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
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// copyFileAtomic copies a file atomically by writing to a temporary file first, then renaming.
func copyFileAtomic(srcFile, dstPath string) error {
	// #nosec G304 -- srcFile is from trusted command-line flag, validated in verifyPaths()
	src, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer src.Close()

	// Get source file permissions
	srcInfo, err := src.Stat()
	if err != nil {
		return err
	}

	// Create temporary file in same directory as destination
	dstDir := filepath.Dir(dstPath)
	finalName := filepath.Base(dstPath)
	tempPath := filepath.Join(dstDir, finalName+".temp")

	// #nosec G304 -- dstPath is from trusted command-line flag, validated in verifyPaths()
	dst, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	defer os.Remove(tempPath)

	// Copy file contents
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}

	// Sync to disk
	if err := dst.Sync(); err != nil {
		return err
	}

	if err := dst.Close(); err != nil {
		return err
	}

	// Set permissions to match source file
	if err := os.Chmod(tempPath, srcInfo.Mode()); err != nil {
		return err
	}

	// Atomic rename
	if err := os.Rename(tempPath, dstPath); err != nil {
		return err
	}

	return nil
}

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
	NoSleep           bool
}

func (o *Options) addFlags() {
	flag.StringVar(&o.CNIBinDir, "cni-bin-dir", "/host/opt/cni/bin", "CNI binary directory")
	flag.StringVar(&o.IBSriovCNIBinFile, "ib-sriov-cni-bin-file", "/usr/bin/ib-sriov", "InfiniBand SR-IOV CNI binary file path")
	flag.BoolVar(&o.NoSleep, "no-sleep", false, "Exit after copying binary instead of sleeping") // Used for testing

	flag.Usage = func() {
		fmt.Printf("This is a thin entrypoint for InfiniBand SR-IOV CNI to copy its\n")
		fmt.Printf("binary into the CNI bin directory on the host filesystem.\n")
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
	srcFile := o.IBSriovCNIBinFile
	dstFile := filepath.Join(o.CNIBinDir, "ib-sriov")

	fmt.Printf("Copying %q to %q\n", srcFile, dstFile)

	if err := copyFileAtomic(srcFile, dstFile); err != nil {
		return fmt.Errorf("failed to copy binary: %v", err)
	}

	fmt.Printf("Successfully copied IB SR-IOV CNI binary to %q\n", dstFile)
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

	// Exit immediately if --no-sleep is specified
	if opt.NoSleep {
		fmt.Println("Binary copied successfully, exiting (--no-sleep)")
		return
	}

	// Set up signal handling
	// This provides graceful shutdown on SIGTERM/SIGINT
	ctx := setupSignalHandler()

	fmt.Println("Entering sleep... (success)")

	// Wait until signal received
	<-ctx.Done()

	fmt.Println("Received signal, exiting...")
}
