package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/containernetworking/cni/pkg/skel"

	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

// executeCNICommand measures this host callback, including routing and result
// printing. Daemon request duration is a different interval, measured by the
// Unix server. The shim never exports a second sample into that histogram.
func executeCNICommand(args *skel.CmdArgs, command string, local func(context.Context) error) (retErr error) {
	// Every normal return replaces this value. A panic still propagates, but
	// cannot be mislabeled as a successful callback by the completion defer.
	retErr = errors.New("CNI callback did not complete")
	ctx, request := observability.Start(context.Background(), "standalone", command, nil)
	defer func() { request.Finish(retErr) }()
	observeCommandIdentity(ctx, args)
	socket, timeout, discovered := daemonRoute(args.StdinData)
	if socket == "" {
		return local(ctx)
	}
	if !discovered {
		if err := checkConfiguredSocket(socket); err != nil {
			return err
		}
	}
	request.SetMode("shim")
	finish := observability.Phase(ctx, "transport")
	result, err := forwardCommand(ctx, args, command, socket, timeout)
	finish(err)
	if err != nil {
		if discovered && daemonUnreachable(err) {
			// The discovered socket was a hint that a daemon is here; nothing
			// listens on it, so the attachment is served as it would be had the
			// file been absent. A configured daemonSocketPath never falls back.
			klog.InfoS("node daemon unreachable on the discovered socket, serving in-process",
				"socket", socket, "err", err.Error(), "readinessPollsTheAPIServer", true)
			request.SetMode("standalone")
			return local(ctx)
		}
		return err
	}
	if command == daemon.AddCommand {
		// Only the CNI result goes to stdout; observations use the existing log.
		_, err = fmt.Fprintln(os.Stdout, string(result))
	}
	return err
}

func observeCommandIdentity(ctx context.Context, args *skel.CmdArgs) {
	namespace, name, uid := parsePodIdentity(args.Args)
	// Read just the log-safe identifier. Config validation remains in its
	// original path, including DEL's leniency for malformed configurations.
	var conf struct {
		DeviceID string `json:"deviceID"`
	}
	_ = json.Unmarshal(args.StdinData, &conf)
	observability.SetIdentity(ctx, observability.Identity{
		Namespace: namespace, Pod: name, UID: uid, Interface: args.IfName,
		DeviceID: conf.DeviceID, ContainerID: args.ContainerID,
	})
}
