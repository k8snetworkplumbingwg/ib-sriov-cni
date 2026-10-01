package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	cniVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/gofrs/flock"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/sriov"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
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

// cniLockRetryInterval paces retries while another attachment holds the lock.
// The lock covers device operations only, never a wait on Kubernetes, so
// contention clears in the time a rebind takes.
const cniLockRetryInterval = 50 * time.Millisecond

func lockCNIExecution(ctx context.Context, phase string) (*flock.Flock, error) {
	// Note: Unbind/Bind VF and move RDMA device to namespace causes rdma resources to be re-created for the VF.
	// CNI may be invoked in parallel and kernel may provide the VF's RDMA resources under a different name.
	// As the mapping of RDMA resources is done in Device plugin prior to CNI invocation, it must not change here.
	// We serialize the CNI's operation causing kernel to allocate the VF's RDMA resources under the same name.
	// In the future, Systems should use udev PCI based RDMA device names, ensuring consistent RDMA resources names.
	return acquireObservedLock(ctx, config.CniFileLockDir, config.CniFileLockName, phase)
}

// acquireLock waits for the node's CNI lock under ctx. Acquisition is bounded
// by the caller's context rather than blocking outright, so a daemon request
// whose caller has already given up stops waiting instead of occupying one of
// the daemon's slots until the node is restarted.
//
// The kernel releases this lock when the holding process exits, so no signal
// handler is installed for it: a handler per invocation would take termination
// away from a long-lived daemon and leak a goroutine for every request served.
func acquireLock(ctx context.Context, dir, name string) (*flock.Flock, error) {
	if err := os.MkdirAll(dir, utils.OwnerReadWriteExecuteAttrs); err != nil {
		return nil, fmt.Errorf("failed to create ib-sriov-cni lock file directory(%q): %v", dir, err)
	}
	lock := flock.New(filepath.Join(dir, name))
	locked, err := lock.TryLockContext(ctx, cniLockRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire the ib-sriov-cni lock: %w", err)
	}
	if !locked {
		return nil, errors.New("failed to acquire the ib-sriov-cni lock")
	}
	return lock, nil
}

func acquireObservedLock(ctx context.Context, dir, name, phase string) (*flock.Flock, error) {
	finish := observability.Phase(ctx, phase)
	lock, err := acquireLock(ctx, dir, name)
	finish(err)
	return lock, err
}

func unlockCNIExecution(lock *flock.Flock) {
	_ = lock.Unlock()
}

func handleVfioPciDetection(netConf *localtypes.NetConf) error {
	isVfioPci, err := utils.IsVfioPciDevice(netConf.DeviceID)
	if err != nil {
		return fmt.Errorf("failed to check vfio-pci driver binding for device %s: %v", netConf.DeviceID, err)
	}

	// If vfioPciMode is explicitly set to true, validate the device is actually bound to vfio-pci
	if netConf.VfioPciMode {
		if !isVfioPci {
			return fmt.Errorf("vfioPciMode is enabled but device %s is not bound to vfio-pci driver", netConf.DeviceID)
		}
	} else {
		// Auto-detect: if device is bound to vfio-pci, enable vfioPciMode
		if isVfioPci {
			netConf.VfioPciMode = true
		}
	}
	return nil
}

// cniArgKVParts is the field count of a CNI_ARGS "KEY=VALUE" entry.
const cniArgKVParts = 2

// parsePodIdentity extracts K8S_POD_NAMESPACE / K8S_POD_NAME / K8S_POD_UID from the
// semicolon-separated CNI_ARGS string multus passes on every invocation.
func parsePodIdentity(cniArgs string) (namespace, name, uid string) {
	for _, kv := range strings.Split(cniArgs, ";") {
		parts := strings.SplitN(kv, "=", cniArgKVParts)
		if len(parts) != cniArgKVParts {
			continue
		}
		switch parts[0] {
		case "K8S_POD_NAMESPACE":
			namespace = parts[1]
		case "K8S_POD_NAME":
			name = parts[1]
		case "K8S_POD_UID":
			uid = parts[1]
		}
	}
	return namespace, name, uid
}

// publisher writes this delegate's connection details and, when the handshake
// is enabled, waits for the controller to report the attachment ready. It is
// injected so a standalone invocation can reach the API directly while a
// daemon serves the same call from its shared node-local cache.
type publisher func(*skel.CmdArgs, *localtypes.NetConf) error

// invocation is how one command reaches Kubernetes and what this process is
// able to run on its behalf.
type invocation struct {
	// kube publishes or withdraws this delegate's connection details.
	kube publisher
	// delegates reports whether a delegate plugin such as IPAM can be
	// executed here. The daemon's request carries everything a delegate needs
	// as data, but the helpers that exec one read the process environment and
	// build their own context, so in a shared daemon they would address
	// whichever container that environment happens to describe. Executing a
	// delegate request-locally is possible and not implemented; until it is,
	// the daemon declines rather than guesses.
	delegates bool
}

// standalone runs the command in the plugin process, which holds the CNI
// environment and reads Kubernetes directly.
func standalone(kube publisher) invocation {
	return invocation{kube: kube, delegates: true}
}

// checkDelegates rejects a configuration whose setup or teardown would exec a
// delegate this process cannot give a CNI environment to. A vfio-pci device has
// no network interface, so its IPAM section is never executed and is allowed.
func (inv invocation) checkDelegates(netConf *localtypes.NetConf) error {
	if inv.delegates || netConf.IPAM.Type == "" || netConf.VfioPciMode {
		return nil
	}
	return fmt.Errorf("ipam type %q requires the standalone plugin: this process does not exec delegates",
		netConf.IPAM.Type)
}

// refuseDelegatesFromStdin applies the delegate rule to the configuration the
// runtime handed a DEL, before the DEL has any side effect, with the same VFIO
// auto-detection ADD applied: a vfio-pci device's IPAM section is never
// executed, so a configuration the daemon accepted for ADD is not refused for
// DEL. The cached configuration is checked again later. An unreadable
// configuration or a failed detection is not refused here: DEL stays lenient
// and the later paths decide. The deviceID is validated before it names a
// sysfs path, so only a PCI address can select the driver link that is read.
func (inv invocation) refuseDelegatesFromStdin(stdinData []byte) error {
	stdinConf, err := config.LoadConf(stdinData)
	if err != nil {
		return nil
	}
	if !stdinConf.VfioPciMode && connectiondetails.ValidateDeviceID(stdinConf.DeviceID) == nil {
		if isVfio, detectErr := utils.IsVfioPciDevice(stdinConf.DeviceID); detectErr == nil && isVfio {
			stdinConf.VfioPciMode = true
		}
	}
	return inv.checkDelegates(stdinConf)
}

// configureIBKubernetes replaces only the PF/VFIO readiness gate with publication.
// VFs still require the controller-assigned GUID and the legacy readiness gate.
func configureIBKubernetes(args *skel.CmdArgs, netConf *localtypes.NetConf,
	publish publisher,
) error {
	if !netConf.IBKubernetesEnabled {
		return nil
	}
	if netConf.PublishPfGUID && !netConf.IsVFDevice && netConf.VfioPciMode {
		return publish(args, netConf)
	}
	if netConf.Args.CNI[infiniBandAnnotation] != configuredInfiniBand {
		return fmt.Errorf(
			"infiniBand SRIOV-CNI failed, InfiniBand status \"%s\" is not \"%s\" please check mellanox ib-kubernetes",
			infiniBandAnnotation, configuredInfiniBand)
	}
	return nil
}

// Get network config, updated with GUID, device info and network namespace.
func getNetConfNetns(ctx context.Context, args *skel.CmdArgs, inv invocation) (_ *localtypes.NetConf, _ ns.NetNS, retErr error) {
	finish := observability.Phase(ctx, "config_identity")
	defer func() { finish(retErr) }()
	netConf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return nil, nil, fmt.Errorf("infiniBand SRI-OV CNI failed to load netconf: %v", err)
	}
	observability.SetIdentity(ctx, observability.Identity{DeviceID: netConf.DeviceID})

	// Validate deviceID is provided. Multus injects it from the device the
	// runtime allocated for this attachment's resource; none means the node
	// allocated nothing, which the network cannot be set up without.
	if netConf.DeviceID == "" {
		return nil, nil, fmt.Errorf("deviceID is required: the runtime allocated no device for this attachment; " +
			"check the node's device plugin pool for the network's resourceName")
	}

	// Handle vfio-pci detection
	if err := handleVfioPciDetection(netConf); err != nil {
		return nil, nil, err
	}

	// Refuse an unservable configuration before any hardware is inspected.
	if err := inv.checkDelegates(netConf); err != nil {
		return nil, nil, err
	}

	// Check if device is PF or VF to load appropriate device info
	isVF, err := utils.IsVirtualFunction(netConf.DeviceID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to determine if device %s is VF or PF: %v", netConf.DeviceID, err)
	}
	netConf.IsVFDevice = isVF
	finish(nil)
	if err := configureIBKubernetes(args, netConf, inv.kube); err != nil {
		return nil, nil, err
	}
	finish = observability.Phase(ctx, "netns_setup")
	if netConf.RdmaIsolation {
		if err := utils.EnsureRdmaSystemMode(); err != nil {
			return nil, nil, err
		}
	}

	netConf.GUID = getGUIDFromConf(netConf)

	// Ensure GUID was provided if ib-kubernetes integration is enabled
	// Note: PF devices already have their own GUID, so only check for VF devices
	if netConf.IBKubernetesEnabled && netConf.IsVFDevice && netConf.GUID == "" {
		return nil, nil, fmt.Errorf(
			"infiniband SRIOV-CNI failed, Unexpected error. GUID must be provided by ib-kubernetes")
	}

	// Only load VF device info for VF devices (PF device that is bound to vfio dont need this)
	if netConf.IsVFDevice {
		err = config.LoadDeviceInfo(netConf)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get VF device information: %v", err)
		}
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	return netConf, netns, nil
}

// Applies VF config and performs VF setup. if RdmaIsolation is configured, moves RDMA device into namespace
func doVFConfig(sm localtypes.Manager, netConf *localtypes.NetConf, netns ns.NetNS, args *skel.CmdArgs) (retErr error) {
	// Timed separately from the readiness wait: one is this node's own work and
	// the other is the controller's turnaround, and a slow start needs them
	// told apart.
	started := time.Now()
	defer func() {
		klog.InfoS("device configured", "device", netConf.DeviceID, "interface", args.IfName,
			"vfio", netConf.VfioPciMode, "failed", retErr != nil,
			"elapsed", time.Since(started).Round(time.Millisecond))
	}()
	err := sm.ApplyVFConfig(netConf)
	if err != nil {
		return fmt.Errorf("infiniBand SRI-OV CNI failed to configure VF %q", err)
	}

	// VFIO devices don't have network interfaces, skip SetupVF
	if netConf.VfioPciMode {
		return nil
	}

	// Note(adrianc): We do this here as ApplyVFCOnfig is rebinding the VF, causing the RDMA device to be recreated.
	// We do this here due to some un-intuitive kernel behavior (which i hope will change), moving an RDMA device
	// to namespace causes all of its associated ULP devices (IPoIB) to be recreated in the default namespace,
	// hence SetupVF needs to occur after moving RDMA device to namespace
	if netConf.RdmaIsolation {
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
			if retErr != nil {
				_ = utils.MoveRdmaDevFromNs(netConf.RdmaNetState.ContainerRdmaDevName, netns)
			}
		}()
	}

	err = sm.SetupVF(netConf, args.IfName, args.ContainerID, netns)
	if err != nil {
		nsErr := netns.Do(func(_ ns.NetNS) error {
			_, innerErr := netlink.LinkByName(args.IfName)
			return innerErr
		})
		if nsErr == nil {
			_ = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
		}
		return fmt.Errorf("failed to set up pod interface %q from the device %q: %v",
			args.IfName, netConf.DeviceID, err)
	}
	return nil
}

// Run the IPAM plugin
func runIPAMPlugin(stdinData []byte, netConf *localtypes.NetConf) (_ *current.Result, retErr error) {
	if netConf.IPAM.Type == ipamDHCP {
		return nil, fmt.Errorf("ipam type dhcp is not supported")
	}
	r, err := ipam.ExecAdd(netConf.IPAM.Type, stdinData)
	if err != nil {
		return nil, fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v",
			netConf.IPAM.Type, netConf.DeviceID, err)
	}

	defer func() {
		if retErr != nil {
			_ = ipam.ExecDel(netConf.IPAM.Type, stdinData)
		}
	}()

	// Convert the IPAM result into the current Result type
	newResult, err := current.NewResultFromResult(r)
	if err != nil {
		return nil, err
	}

	if len(newResult.IPs) == 0 {
		return nil, errors.New("IPAM plugin returned missing IP config")
	}

	for _, ipc := range newResult.IPs {
		// All addresses apply to the container interface (move from host)
		ipc.Interface = current.Int(0)
	}

	return newResult, nil
}

// handleVFAdd handles VF device configuration in cmdAdd
func handleVFAdd(args *skel.CmdArgs, netConf *localtypes.NetConf, netns ns.NetNS, result *current.Result) (retErr error) {
	sm := sriov.NewSriovManager()

	err := doVFConfig(sm, netConf, netns, args)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			nsErr := netns.Do(func(_ ns.NetNS) error {
				_, innerErr := netlink.LinkByName(args.IfName)
				return innerErr
			})
			if nsErr == nil {
				_ = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
			}
			if netConf.RdmaIsolation {
				_ = utils.MoveRdmaDevFromNs(netConf.RdmaNetState.ContainerRdmaDevName, netns)
			}
		}
	}()

	// VFIO devices don't have network interfaces, skip IPAM configuration
	if netConf.IPAM.Type != "" && !netConf.VfioPciMode {
		var newResult *current.Result
		newResult, err = runIPAMPlugin(args.StdinData, netConf)
		if err != nil {
			return err
		}
		// If runIPAMPlugin failed, than ExecDel was called. Defer if no error
		defer func() {
			if retErr != nil {
				_ = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
			}
		}()

		newResult.Interfaces = result.Interfaces

		for _, ipc := range newResult.IPs {
			// only a single interface is handled by ib-sriov-cni, point ip IPConfig.Interface to it.
			ipc.Interface = current.Int(0)
		}

		err = netns.Do(func(_ ns.NetNS) error {
			return ipam.ConfigureIface(args.IfName, newResult)
		})
		if err != nil {
			return err
		}

		// Update result pointer to point to the new result
		*result = *newResult
	}

	// Cache NetConf for CmdDel
	if err = utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, netConf); err != nil {
		return fmt.Errorf("error saving NetConf: %v", err)
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	return executeCNICommand(args, daemon.AddCommand, func(ctx context.Context) error {
		publish := func(args *skel.CmdArgs, netConf *localtypes.NetConf) error {
			return publishConnectionDetails(ctx, args, netConf)
		}
		result, configVersion, err := addNetwork(ctx, args, standalone(publish))
		if err != nil {
			return err
		}
		return types.PrintResult(result, configVersion)
	})
}

// addNetwork performs one ADD and returns its result rather than printing it,
// so the same code serves a one-shot plugin process and a daemon handler that
// must hand the result back over its socket.
func addNetwork(ctx context.Context, args *skel.CmdArgs, inv invocation) (_ *current.Result, _ string, retErr error) {
	netConf, netns, err := getNetConfNetns(ctx, args, inv)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = netns.Close() }()

	// Lock CNI operation to serialize the operation
	lock, err := lockCNIExecution(ctx, "final_lock")
	if err != nil {
		return nil, "", err
	}
	defer unlockCNIExecution(lock)
	finish := observability.Phase(ctx, "device_finalize")
	defer func() { finish(retErr) }()

	result := &current.Result{}
	result.Interfaces = []*current.Interface{{
		Name:    args.IfName,
		Sandbox: netns.Path(),
	}}

	// Check if device is PF (Physical Function) - flag was set in getNetConfNetns
	// PF passthrough devices don't need VF configuration
	if !netConf.IsVFDevice {
		if !netConf.VfioPciMode {
			return nil, "", fmt.Errorf("PF device %s requires vfioPciMode to be enabled", netConf.DeviceID)
		}
		// PF device - just cache config and return success
		if err = utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, netConf); err != nil {
			return nil, "", fmt.Errorf("error saving NetConf: %v", err)
		}
	} else {
		// VF device - continue with normal VF configuration
		err = handleVFAdd(args, netConf, netns, result)
		if err != nil {
			return nil, "", err
		}
	}

	return result, netConf.CNIVersion, nil
}

func handleIPAMCleanup(netConf *localtypes.NetConf, stdinData []byte) error {
	// VFIO devices don't use IPAM
	if netConf.VfioPciMode {
		return nil
	}
	if netConf.IPAM.Type == ipamDHCP {
		return fmt.Errorf("ipam type dhcp is not supported")
	}
	return ipam.ExecDel(netConf.IPAM.Type, stdinData)
}

// handleVFCleanup performs VF-specific cleanup operations
func handleVFCleanup(sm localtypes.Manager, netConf *localtypes.NetConf, args *skel.CmdArgs, netns ns.NetNS) error {
	started := time.Now()
	defer func() {
		klog.InfoS("device released", "device", netConf.DeviceID, "interface", args.IfName,
			"vfio", netConf.VfioPciMode, "elapsed", time.Since(started).Round(time.Millisecond))
	}()
	// VFIO devices don't have network interfaces to release
	if !netConf.VfioPciMode {
		err := sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
		if err != nil {
			return err
		}
	}

	// Move RDMA device to default namespace
	// Note(adrianc): Due to some un-intuitive kernel behavior (which i hope will change), moving an RDMA device
	// to namespace causes all of its associated ULP devices (IPoIB) to be recreated in the default namespace.
	// we strategically place this here to allow:
	//   1. netedv cleanup during ReleaseVF.
	//   2. rdma dev netns cleanup as ResetVFConfig will rebind the VF.
	// Doing anything would have yielded the same results however ResetVFConfig will eventually not trigger VF rebind.
	if netConf.RdmaIsolation {
		err := utils.MoveRdmaDevFromNs(netConf.RdmaNetState.ContainerRdmaDevName, netns)
		if err != nil {
			return fmt.Errorf(
				"failed to restore RDMA device %s to default namespace. %v",
				netConf.RdmaNetState.ContainerRdmaDevName, err)
		}
	}

	if err := sm.ResetVFConfig(netConf); err != nil {
		return fmt.Errorf("cmdDel() error resetting VF: %v", err)
	}
	return nil
}

func cmdDel(args *skel.CmdArgs) error {
	return executeCNICommand(args, daemon.DelCommand, func(ctx context.Context) error {
		withdraw := func(args *skel.CmdArgs, netConf *localtypes.NetConf) error {
			return removeConnectionDetails(ctx, args, netConf)
		}
		return delNetwork(ctx, args, standalone(withdraw))
	})
}

// delNetwork performs one DEL. The withdrawal step is injected for the same
// reason ADD's publication is.
func delNetwork(ctx context.Context, args *skel.CmdArgs, inv invocation) (retErr error) {
	finish := observability.Phase(ctx, "withdrawal")
	defer func() { finish(retErr) }()
	// Refuse before touching anything: see refuseDelegatesFromStdin.
	if err := inv.refuseDelegatesFromStdin(args.StdinData); err != nil {
		return err
	}
	// Withdraw the published entry before anything that can exit early. The
	// netconf comes from stdin rather than the cache, so teardown still works
	// when ADD failed before it completed; the cache, written at publication,
	// only adds the Pod UID that fences the withdrawal when CNI_ARGS carry
	// none. The annotation is not left behind claiming an attachment that no
	// longer exists.
	keepRecord, err := withdrawConnectionDetails(ctx, args, inv.kube)
	if err != nil {
		return err
	}
	finish(nil)
	finish = observability.Phase(ctx, "cleanup")

	// https://github.com/kubernetes/kubernetes/pull/35240
	if args.Netns == "" {
		return nil
	}

	netConf, cRefPath, err := config.LoadConfFromCache(args)
	if err != nil {
		// According to the CNI spec, a DEL action should complete without errors
		// even if there are some resources missing. For more details, see
		//nolint
		// https://github.com/containernetworking/cni/blob/main/SPEC.md#del-remove-container-from-network-or-un-apply-modifications
		return nil
	}
	observability.SetIdentity(ctx, observability.Identity{DeviceID: netConf.DeviceID})

	// The cached configuration can differ from stdin's, for instance by a VFIO
	// mode detected at ADD, so the rule is applied to it as well. Reaching a
	// refusal here means the withdrawal above already ran; the stdin check
	// above is what keeps that from being the common case.
	if delegateErr := inv.checkDelegates(netConf); delegateErr != nil {
		return delegateErr
	}

	// Device cleanup may succeed while ownership is still indeterminate.
	// Keep that evidence so a repeated DEL can retry the withdrawal.
	if cRefPath != "" && !keepRecord {
		defer func() {
			if retErr == nil {
				_ = utils.CleanCachedNetConf(cRefPath)
			}
		}()
	}

	sm := sriov.NewSriovManager()

	if netConf.IPAM.Type != "" {
		err = handleIPAMCleanup(netConf, args.StdinData)
		if err != nil {
			return err
		}
	}

	return teardownDevice(ctx, args, netConf, sm, &finish)
}

// teardownDevice is the device half of DEL: open the sandbox's namespace and,
// for a VF, release it under the node lock. A namespace that no longer exists
// ends the DEL successfully, as the CNI specification asks when the node has
// restarted; a PF has nothing to release. The caller's cleanup phase is closed
// and reopened around the lock, so the lock wait is not counted as cleanup.
func teardownDevice(ctx context.Context, args *skel.CmdArgs, netConf *localtypes.NetConf,
	sm localtypes.Manager, finish *func(error),
) error {
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		if _, gone := err.(ns.NSPathNotExistErr); gone {
			return nil
		}
		return fmt.Errorf("failed to open netns %s: %q", args.Netns, err)
	}
	defer func() { _ = netns.Close() }()

	// Detect if device is VF or PF at runtime during Del
	isVF, err := utils.IsVirtualFunction(netConf.DeviceID)
	if err != nil {
		return fmt.Errorf("failed to determine if device %s is VF or PF: %v", netConf.DeviceID, err)
	}
	// PF devices don't need VF cleanup
	if !isVF {
		return nil
	}

	// Lock CNI operation to serialize the operation
	(*finish)(nil)
	lock, err := lockCNIExecution(ctx, "final_lock")
	if err != nil {
		return err
	}
	defer unlockCNIExecution(lock)
	*finish = observability.Phase(ctx, "cleanup")

	return handleVFCleanup(sm, netConf, args, netns)
}

// withdrawConnectionDetails removes this delegate's connection-details member
// when the startup handshake is enabled. It is a no-op for every other
// configuration, so teardown is unchanged unless the feature was turned on.
// keepRecord tells teardown to preserve evidence of indeterminate ownership.
func withdrawConnectionDetails(ctx context.Context, args *skel.CmdArgs, withdraw publisher) (keepRecord bool, err error) {
	netConf, err := config.LoadConf(args.StdinData)
	if err != nil {
		// An unreadable config cannot identify an entry to remove. DEL stays
		// lenient here; the cache path below applies the same rule.
		return false, nil
	}
	if !netConf.AwaitsReadiness() {
		return false, nil
	}
	if netConf.DeviceID == "" {
		// The runtime allocated no device for this attachment, so its ADD
		// never published and there is nothing to withdraw. That is a node
		// fault worth an error, not a reason to fail the teardown.
		namespace, name, _ := parsePodIdentity(args.Args)
		klog.ErrorS(nil, "DEL carries no deviceID: the runtime allocated no device for this attachment, "+
			"nothing was published and nothing is withdrawn; check the node's device plugin pool",
			"namespace", namespace, "pod", name, "interface", args.IfName, "container", args.ContainerID)
		return false, nil
	}
	if err := connectiondetails.ValidateDeviceID(netConf.DeviceID); err != nil {
		// ADD refused the same value before publishing anything, so there
		// is nothing to withdraw; the value never names a sysfs path.
		namespace, name, _ := parsePodIdentity(args.Args)
		klog.ErrorS(err, "DEL carries a deviceID that is not a PCI address: nothing was published and nothing is withdrawn",
			"namespace", namespace, "pod", name, "interface", args.IfName, "container", args.ContainerID)
		return false, nil
	}
	isVF, err := utils.IsVirtualFunction(netConf.DeviceID)
	if err != nil || isVF {
		return false, nil
	}
	return withdrawOwned(ctx, args, netConf, withdraw)
}

// withdrawOwned decides, under the ownership lock, whether this sandbox may
// withdraw, and withdraws while still holding it, so no publication of the
// same key can land between the decision and the write. Only the sandbox that
// published may withdraw, and only while it is the last to have done so: a
// sandbox with no record published nothing, and one a later sandbox of the
// same Pod has superseded would be taking the live record away. A record that
// cannot be read settles nothing; DEL then withdraws nothing, says so, and
// still succeeds, preserving the record through device cleanup for a retry.
// After a rollback whose sandbox has no namespace left, the
// record is removed here, since the teardown path below never reaches it.
func withdrawOwned(ctx context.Context, args *skel.CmdArgs, netConf *localtypes.NetConf, withdraw publisher) (keepRecord bool, err error) {
	namespace, name, _ := parsePodIdentity(args.Args)
	lock, err := acquireOwnershipLock(ctx)
	if err != nil {
		return false, err
	}
	defer unlockCNIExecution(lock)
	own, err := readRecordedPublication(args)
	switch {
	case errors.Is(err, errNoRecord):
		klog.InfoS("connection-details withdrawal skipped: this sandbox has no record of publishing the attachment",
			"namespace", namespace, "pod", name, "container", args.ContainerID, "device", netConf.DeviceID)
		return false, nil
	case err != nil:
		klog.ErrorS(err, "connection-details withdrawal skipped: this sandbox's publication record cannot be read; "+
			"the record is kept and nothing is withdrawn", "namespace", namespace, "pod", name,
			"container", args.ContainerID, "device", netConf.DeviceID)
		return true, nil
	}
	confirmedBy, pending, err := laterPublication(args, own)
	if err != nil {
		klog.ErrorS(err, "connection-details withdrawal skipped: a publication record of this attachment cannot be "+
			"ordered against this sandbox's, so ownership is unsettled and nothing is withdrawn", "namespace", namespace, "pod", name,
			"container", args.ContainerID, "device", netConf.DeviceID)
		return true, nil
	}
	if confirmedBy != "" {
		klog.InfoS("connection-details withdrawal skipped: a later sandbox of the same pod republished the attachment",
			"namespace", namespace, "pod", name, "container", args.ContainerID, "later", confirmedBy, "device", netConf.DeviceID)
		// The superseded record has nothing left to settle; without a netns the
		// teardown below never reaches the cache, so it is removed here.
		if args.Netns == "" {
			removeRecord(args, namespace, name, netConf.DeviceID, "superseded publication record not removed")
		}
		return false, nil
	}
	netConf.PodUID = own.PodUID
	netConf.PublishedPfGUID = own.PublishedPfGUID
	netConf.PublishedDeviceIndex = own.PublishedDeviceIndex
	netConf.PublicationUnconfirmed = own.PublicationUnconfirmed
	netConf.PriorPublishedPfGUID = own.PriorPublishedPfGUID
	netConf.PriorPublishedDeviceIndex = own.PriorPublishedDeviceIndex
	if pending > 0 {
		// Later records are the intent of sandboxes whose ADDs failed, so they
		// settle nothing about the member; this sandbox withdraws only the
		// members it published and leaves one a later write replaced them
		// with for that sandbox's own DEL.
		klog.InfoS("connection-details: later publications of this function are unconfirmed; withdrawing only members this sandbox published",
			"namespace", namespace, "pod", name, "container", args.ContainerID, "pending", pending, "device", netConf.DeviceID)
		netConf.WithdrawExactMember = true
	}
	if err := withdraw(args, netConf); err != nil {
		return false, err
	}
	if args.Netns == "" {
		removeRecord(args, namespace, name, netConf.DeviceID, "publication record not removed after withdrawal")
	}
	return false, nil
}

// removeRecord deletes this sandbox's publication record once a DEL has
// settled it. A record left behind only makes a later DEL for this sandbox
// withdraw nothing, and costs every later publication a read, so a failure
// is logged and not returned.
func removeRecord(args *skel.CmdArgs, namespace, name, deviceID, failure string) {
	path := filepath.Join(config.DefaultCNIDir, args.ContainerID+"-"+args.IfName)
	if err := utils.CleanCachedNetConf(path); err != nil {
		klog.ErrorS(err, failure, "namespace", namespace, "pod", name,
			"container", args.ContainerID, "device", deviceID, "path", path)
	}
}

// recordedPodUID returns the UID the ADD for this attachment kept with its
// cached configuration, or "" when there is no such record.
func recordedPodUID(args *skel.CmdArgs) string {
	if cached, ok := recordedPublication(args); ok {
		return cached.PodUID
	}
	return ""
}

func cmdCheck(args *skel.CmdArgs) error {
	// Forward so a configured daemon accounts for every command it serves.
	// The check itself is a no-op in both modes.
	return executeCNICommand(args, daemon.CheckCommand, func(context.Context) error { return nil })
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
	opt := registerDaemonFlags(flag.CommandLine)
	flag.Parse()
	if versionOpt {
		fmt.Printf("%s\n", printVersionString())
		return
	}
	if opt.enabled {
		os.Exit(daemonExitCode(*opt))
	}

	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add:   cmdAdd,
			Del:   cmdDel,
			Check: cmdCheck,
		},
		cniVersion.All,
		"")
}
