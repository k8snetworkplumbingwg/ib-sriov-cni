package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	cniVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/gofrs/flock"
	"github.com/vishvananda/netlink"

	"github.com/Mellanox/ib-sriov-cni/pkg/config"
	"github.com/Mellanox/ib-sriov-cni/pkg/sriov"
	localtypes "github.com/Mellanox/ib-sriov-cni/pkg/types"
	"github.com/Mellanox/ib-sriov-cni/pkg/utils"
)

const (
	infiniBandAnnotation = "mellanox.infiniband.app"
	configuredInfiniBand = "configured"
	ipamDHCP             = "dhcp"
)

var (
	version = "master@git"
	commit  = "unknown commit"
	date    = "unknown date"
)

//nolint:gochecknoinits
func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func getGUIDFromConf(netConf *localtypes.NetConf) string {
	// Take from runtime config if available
	if netConf.RuntimeConfig.InfinibandGUID != "" {
		return netConf.RuntimeConfig.InfinibandGUID
	}
	// Take from CNI_ARGS if available
	if guid, ok := netConf.Args.CNI["guid"]; ok {
		return guid
	}

	// No guid provided
	return ""
}

func lockCNIExecution() (*flock.Flock, error) {
	// Note: Unbind/Bind VF and move RDMA device to namespace causes rdma resources to be re-created for the VF.
	// CNI may be invoked in parallel and kernel may provide the VF's RDMA resources under a different name.
	// As the mapping of RDMA resources is done in Device plugin prior to CNI invocation, it must not change here.
	// We serialize the CNI's operation causing kernel to allocate the VF's RDMA resources under the same name.
	// In the future, Systems should use udev PCI based RDMA device names, ensuring consistent RDMA resources names.
	err := os.MkdirAll(config.CniFileLockDir, 0700)
	if err != nil {
		return nil, fmt.Errorf("failed to create ib-sriov-cni lock file directory(%q): %v", config.CniFileLockDir, err)
	}

	lock := flock.New(filepath.Join(config.CniFileLockDir, config.CniFileLockName))
	err = lock.Lock()
	if err != nil {
		return nil, err
	}
	// unlock on signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func(sigC chan os.Signal, l *flock.Flock) {
		// This goroutine will die when the process dies (main exits)
		sig := <-sigC
		_ = l.Unlock()
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		close(sigC)
		// Re-raise the signal
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(sig)
	}(sigChan, lock)
	return lock, nil
}

func unlockCNIExecution(lock *flock.Flock) {
	_ = lock.Unlock()
}

func cmdAdd(args *skel.CmdArgs) error {
	netConf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("infiniBand SRI-OV CNI failed to load netconf: %v", err)
	}

	if netConf.IBKubernetesEnabled && netConf.Args.CNI[infiniBandAnnotation] != configuredInfiniBand {
		return fmt.Errorf(
			"infiniBand SRIOV-CNI failed, InfiniBand status \"%s\" is not \"%s\" please check mellanox ib-kubernets",
			infiniBandAnnotation, configuredInfiniBand)
	}

	netConf.GUID = getGUIDFromConf(netConf)

	if netConf.RdmaIso {
		err = utils.EnsureRdmaSystemMode()
		if err != nil {
			return err
		}
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	err = config.LoadDeviceInfo(netConf)
	if err != nil {
		return fmt.Errorf("failed to get device specific information. %v", err)
	}

	sm := sriov.NewSriovManager()

	// Lock CNI operation to serialize the operation
	lock, err := lockCNIExecution()
	if err != nil {
		return err
	}
	defer unlockCNIExecution(lock)

	err = sm.ApplyVFConfig(netConf)
	if err != nil {
		return fmt.Errorf("infiniBand SRI-OV CNI failed to configure VF %q", err)
	}

	// Note(adrianc): We do this here as ApplyVFCOnfig is rebinding the VF, causing the RDMA device to be recreated.
	// We do this here due to some un-intuitive kernel behavior (which i hope will change), moving an RDMA device
	// to namespace causes all of its associated ULP devices (IPoIB) to be recreated in the default namespace,
	// hence SetupVF needs to occur after moving RDMA device to namespace
	if netConf.RdmaIso {
		var rdmaDev string
		rdmaDev, err = utils.MoveRdmaDevToNsPci(netConf.DeviceID, netns)
		if err != nil {
			return err
		}
		// Save RDMA state
		netConf.RdmaNetState.DeviceID = netConf.DeviceID
		netConf.RdmaNetState.SandboxRdmaDevName = rdmaDev
		netConf.RdmaNetState.ContainerRdmaDevName = rdmaDev
		// restore RDMA device back to default namespace in case of error
		// Note(adrianc): as there is no logging, we have little visibility if the restore operation failed.
		defer func() {
			if err != nil {
				_ = utils.MoveRdmaDevFromNs(netConf.RdmaNetState.ContainerRdmaDevName, netns)
			}
		}()
	}

	result := &current.Result{}
	result.Interfaces = []*current.Interface{{
		Name:    args.IfName,
		Sandbox: netns.Path(),
	}}

	err = sm.SetupVF(netConf, args.IfName, args.ContainerID, netns)
	defer func() {
		if err != nil {
			nsErr := netns.Do(func(_ ns.NetNS) error {
				_, innerErr := netlink.LinkByName(args.IfName)
				return innerErr
			})
			if nsErr == nil {
				_ = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to set up pod interface %q from the device %q: %v", args.IfName, netConf.Master, err)
	}

	// run the IPAM plugin
	if netConf.IPAM.Type != "" {
		if netConf.IPAM.Type == ipamDHCP {
			return fmt.Errorf("ipam type dhcp is not supported")
		}
		var r types.Result
		r, err = ipam.ExecAdd(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v",
				netConf.IPAM.Type, netConf.Master, err)
		}

		defer func() {
			if err != nil {
				_ = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
			}
		}()

		// Convert the IPAM result into the current Result type
		var newResult *current.Result
		newResult, err = current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		if len(newResult.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}

		newResult.Interfaces = result.Interfaces

		for _, ipc := range newResult.IPs {
			// All addresses apply to the container interface (move from host)
			ipc.Interface = current.Int(0)
		}

		err = netns.Do(func(_ ns.NetNS) error {
			return ipam.ConfigureIface(args.IfName, newResult)
		})
		if err != nil {
			return err
		}
		result = newResult
	}

	// Cache NetConf for CmdDel
	if err = utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, netConf); err != nil {
		return fmt.Errorf("error saving NetConf %q", err)
	}

	return types.PrintResult(result, current.ImplementedSpecVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	// https://github.com/kubernetes/kubernetes/pull/35240
	if args.Netns == "" {
		return nil
	}

	netConf, cRefPath, err := config.LoadConfFromCache(args)
	if err != nil {
		return err
	}

	defer func() {
		if err == nil && cRefPath != "" {
			_ = utils.CleanCachedNetConf(cRefPath)
		}
	}()

	sm := sriov.NewSriovManager()

	if netConf.IPAM.Type != "" {
		if netConf.IPAM.Type == ipamDHCP {
			return fmt.Errorf("ipam type dhcp is not supported")
		}
		err = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		// according to:
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		// if provided path does not exist (e.x. when node was restarted)
		// plugin should silently return with success after releasing
		// IPAM resources
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}

		return fmt.Errorf("failed to open netns %s: %q", netns, err)
	}
	defer netns.Close()

	// Lock CNI operation to serialize the operation
	lock, err := lockCNIExecution()
	if err != nil {
		return err
	}
	defer unlockCNIExecution(lock)

	err = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
	if err != nil {
		return err
	}

	// Move RDMA device to default namespace
	// Note(adrianc): Due to some un-intuitive kernel behavior (which i hope will change), moving an RDMA device
	// to namespace causes all of its associated ULP devices (IPoIB) to be recreated in the default namespace.
	// we strategically place this here to allow:
	//   1. netedv cleanup during ReleaseVF.
	//   2. rdma dev netns cleanup as ResetVFConfig will rebind the VF.
	// Doing anything would have yielded the same results however ResetVFConfig will eventually not trigger VF rebind.
	if netConf.RdmaIso {
		err = utils.MoveRdmaDevFromNs(netConf.RdmaNetState.ContainerRdmaDevName, netns)
		if err != nil {
			return fmt.Errorf(
				"failed to restore RDMA device %s to default namespace. %v",
				netConf.RdmaNetState.ContainerRdmaDevName, err)
		}
	}

	if err := sm.ResetVFConfig(netConf); err != nil {
		return fmt.Errorf("cmdDel() error reseting VF: %q", err)
	}
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func printVersionString() string {
	return fmt.Sprintf("ib-sriov cni version:%s, commit:%s, date:%s", version, commit, date)
}

func main() {
	// Init command line flags to clear vendor packages' flags, especially in init()
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// add version flag
	versionOpt := false
	flag.BoolVar(&versionOpt, "version", false, "Show application version")
	flag.BoolVar(&versionOpt, "v", false, "Show application version")
	flag.Parse()
	if versionOpt {
		fmt.Printf("%s\n", printVersionString())
		return
	}

	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, cniVersion.All, "")
}
