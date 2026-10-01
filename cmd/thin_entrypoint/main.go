// Copyright (c) 2025 InfiniBand SR-IOV CNI Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a thin entrypoint for InfiniBand SR-IOV CNI. Copies the CNI binary to host.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/install"
)

// setupSignalHandler creates a context that gets canceled on SIGTERM/SIGINT.
func setupSignalHandler() context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-c
		cancel()
	}()

	return ctx
}

type Options struct {
	CNIBinDir         string
	IBSriovCNIBinFile string
	CNIConfDir        string
	GenKubeconfig     bool
	NoSleep           bool
}

func (o *Options) addFlags() {
	flag.StringVar(&o.CNIBinDir, "cni-bin-dir", "/host/opt/cni/bin", "CNI binary directory")
	flag.StringVar(&o.IBSriovCNIBinFile, "ib-sriov-cni-bin-file", "/usr/bin/ib-sriov", "InfiniBand SR-IOV CNI binary file path")
	flag.StringVar(&o.CNIConfDir, "cni-conf-dir", "/host/etc/cni/net.d", "CNI config directory on the host")
	flag.BoolVar(&o.GenKubeconfig, "gen-kubeconfig", false, "Generate a node-local kubeconfig from this pod's service account")
	flag.BoolVar(&o.NoSleep, "no-sleep", false, "Exit after copying binary instead of sleeping") // Used for testing

	flag.Usage = func() {
		fmt.Printf("This is a thin entrypoint for InfiniBand SR-IOV CNI to copy its\n")
		fmt.Printf("binary into the CNI bin directory on the host filesystem, for\n")
		fmt.Printf("deployments without the node daemon. With the daemon, run\n")
		fmt.Printf("ib-sriov --daemon --install-cni-bin-dir=... instead; it installs and serves.\n")
		fmt.Printf("\nUsage:\n")
		flag.PrintDefaults()
	}
}

func (o *Options) verifyPaths() error {
	// Check CNI bin directory
	if _, err := os.Stat(o.CNIBinDir); err != nil {
		return fmt.Errorf("CNI bin directory %q does not exist: %v", o.CNIBinDir, err)
	}

	// Check IB SR-IOV binary file
	fileInfo, err := os.Stat(o.IBSriovCNIBinFile)
	if err != nil {
		return fmt.Errorf("IB SR-IOV CNI binary file %q does not exist: %v", o.IBSriovCNIBinFile, err)
	}
	if !fileInfo.Mode().IsRegular() {
		return fmt.Errorf("IB SR-IOV CNI binary file %q is not a regular file", o.IBSriovCNIBinFile)
	}

	return nil
}

func (o *Options) copyBinary() error {
	fmt.Printf("Copying %q to %q\n", o.IBSriovCNIBinFile, filepath.Join(o.CNIBinDir, install.BinaryName))
	dst, err := install.CopyBinary(o.IBSriovCNIBinFile, o.CNIBinDir)
	if err != nil {
		return fmt.Errorf("failed to copy binary: %v", err)
	}
	fmt.Printf("Successfully copied IB SR-IOV CNI binary to %q\n", dst)
	return nil
}

func main() {
	opt := Options{}
	opt.addFlags()

	flag.Parse()

	// Verify required files and directories exist
	if err := opt.verifyPaths(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Copy the binary
	if err := opt.copyBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Generate the kubeconfig once up front so --gen-kubeconfig is honored
	// even together with --no-sleep (used by tests/validation).
	if opt.GenKubeconfig {
		dst, err := install.WriteKubeconfig(opt.CNIConfDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Generated kubeconfig at %q\n", dst)
	}

	// Exit immediately if --no-sleep is specified
	if opt.NoSleep {
		fmt.Println("Binary copied successfully, exiting (--no-sleep)")
		return
	}

	// Set up signal handling for graceful shutdown on SIGTERM/SIGINT
	ctx := setupSignalHandler()

	fmt.Println("Entering sleep... (success)")

	if opt.GenKubeconfig {
		// Bound service account tokens rotate, and the host file must stay
		// valid for in-process CNI invocations.
		install.RefreshKubeconfig(ctx, opt.CNIConfDir, install.KubeconfigRefreshInterval, func(err error) {
			fmt.Fprintf(os.Stderr, "kubeconfig refresh failed: %v\n", err)
		})
	} else {
		<-ctx.Done()
	}

	fmt.Println("Received signal, exiting...")
}
