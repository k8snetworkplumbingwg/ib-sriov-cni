package main

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/Mellanox/ib-sriov-cni/pkg/config"
	"github.com/Mellanox/ib-sriov-cni/pkg/sriov"
	"github.com/Mellanox/ib-sriov-cni/pkg/utils"
)

const (
	infiniBandAnnotation = "mellanox.infiniband.app"
	configuredInfiniBand = "configured"
	ipamDHCP             = "dhcp"
)

//nolint:gochecknoinits
func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func cmdAdd(args *skel.CmdArgs) error {
	netConf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("infiniBand SRI-OV CNI failed to load netconf: %v", err)
	}

	cniArgs := netConf.Args.CNI
	if cniArgs[infiniBandAnnotation] != configuredInfiniBand {
		return fmt.Errorf(
			"infiniBand SRIOV-CNI failed, InfiniBand status \"%s\" is not \"%s\" please check mellanox ib-kubernets",
			infiniBandAnnotation, configuredInfiniBand)
	}

	guid, ok := cniArgs["guid"]
	if !ok {
		return fmt.Errorf(
			"infiniBand SRIOV-CNI failed, no guid found from cni-args, please check mellanox ib-kubernets")
	}
	netConf.GUID = guid

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

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "")
}
