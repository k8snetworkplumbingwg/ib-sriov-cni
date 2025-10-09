// Copyright (c) 2024 InfiniBand SR-IOV CNI Authors
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

// This is a thin entrypoint for InfiniBand SR-IOV CNI to replace the shell script
// and enable usage of distroless images.
//
// Design inspired by multus-cni thin_entrypoint:
// https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/cmd/thin_entrypoint/main.go
//
// Key design patterns adopted from multus-cni:
// - Use of cmdutils.CopyFileAtomic for atomic file operations
// - Use of signals.SetupSignalHandler for graceful signal handling
// - Similar command-line flag structure and usage patterns
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/cmdutils"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/signals"
)

// Options stores command line options
type Options struct {
	CNIBinDir         string
	IBSriovCNIBinFile string
}

func (o *Options) addFlags() {
	fs := pflag.CommandLine
	fs.StringVar(&o.CNIBinDir, "cni-bin-dir", "/host/opt/cni/bin", "CNI binary directory")
	fs.StringVar(&o.IBSriovCNIBinFile, "ib-sriov-cni-bin-file", "/usr/src/ib-sriov-cni/bin/ib-sriov", "InfiniBand SR-IOV CNI binary file path")

	// Set custom usage message
	fs.Usage = func() {
		fmt.Printf("This is a thin entrypoint for InfiniBand SR-IOV CNI to copy its\n")
		fmt.Printf("binary into the CNI bin directory on the host filesystem.\n")
		fmt.Printf("\nUsage:\n")
		fs.PrintDefaults()
	}
}

func (o *Options) verifyFileExists() error {
	// Check CNI bin directory
	if _, err := os.Stat(o.CNIBinDir); err != nil {
		return fmt.Errorf("CNI bin directory %q does not exist: %v", o.CNIBinDir, err)
	}

	// Check IB SR-IOV binary file
	if _, err := os.Stat(o.IBSriovCNIBinFile); err != nil {
		return fmt.Errorf("IB SR-IOV CNI binary file %q does not exist: %v", o.IBSriovCNIBinFile, err)
	}

	return nil
}

func (o *Options) copyBinary() error {
	srcFile := o.IBSriovCNIBinFile
	dstFile := filepath.Join(o.CNIBinDir, "ib-sriov")

	fmt.Printf("Copying %q to %q\n", srcFile, dstFile)

	// Use cmdutils.CopyFileAtomic for atomic file copying like multus does
	// This ensures the file is copied atomically (write to temp file, then rename)
	if err := cmdutils.CopyFileAtomic(srcFile, o.CNIBinDir, "_ib-sriov", "ib-sriov"); err != nil {
		return fmt.Errorf("failed to copy binary: %v", err)
	}

	fmt.Printf("Successfully copied IB SR-IOV CNI binary to %q\n", dstFile)
	return nil
}

func main() {
	opt := Options{}
	opt.addFlags()

	pflag.Parse()

	// Verify required files and directories exist
	if err := opt.verifyFileExists(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Copy the binary
	if err := opt.copyBinary(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Set up signal handling like multus does
	// This provides graceful shutdown on SIGTERM/SIGINT
	ctx := signals.SetupSignalHandler()

	fmt.Println("Entering sleep... (success)")

	// Wait until signal received, just like multus
	<-ctx.Done()

	fmt.Println("Received signal, exiting...")
}
