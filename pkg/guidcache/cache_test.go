// Copyright (c) 2026 InfiniBand SR-IOV CNI Authors
// SPDX-License-Identifier: Apache-2.0

package guidcache

import "testing"

func TestStoreLookupForget(t *testing.T) {
	c := New()
	c.Store("0000:03:00.0", "e0:9d:73:03:00:fe:b0:a0")
	if c.Len() != 1 {
		t.Fatal("store did not add")
	}
	c.Forget("0000:03:00.0")
	if _, ok := c.Lookup("0000:03:00.0"); ok || c.Len() != 0 {
		t.Fatal("forget did not remove")
	}
}
