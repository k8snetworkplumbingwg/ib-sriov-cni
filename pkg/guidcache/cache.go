// Copyright (c) 2026 InfiniBand SR-IOV CNI Authors
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
//
// SPDX-License-Identifier: Apache-2.0

// Package guidcache remembers each PF's node GUID for the daemon's lifetime.
//
// Reading a GUID means rebinding the function off vfio-pci and back, which
// takes seconds and may only happen inside CMD ADD, after kubelet has handed
// the function to a Pod: the functions belong to the device plugin's pool and
// the daemon must never touch one on its own. The GUID itself is burned into
// the card, so once read it is good until the card is replaced, and a card
// cannot be replaced while the node is up. The cache therefore lives in
// memory only, filled by what this daemon itself established: the first ADD
// for a function reads the device, and a retry's Pod may carry the GUID the
// controller bound for that Pod in its Ready status. Nothing is written to
// disk, so nothing can outlive the hardware it describes, and nothing is
// learned from Pod annotations at large: a details entry on some Pod says
// what was once read at an address, not that the Pod still holds the function
// there, and the daemon cannot tell the two apart without touching the device.
package guidcache

import "sync"

// Cache maps a PCI address to the node GUID of the function at that address.
type Cache struct {
	mu    sync.Mutex
	guids map[string]string
}

// New returns an empty cache.
func New() *Cache { return &Cache{guids: map[string]string{}} }

// Lookup returns the cached GUID for deviceID.
func (c *Cache) Lookup(deviceID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	guid, ok := c.guids[deviceID]
	return guid, ok
}

// Store remembers a GUID that was just read from the device.
func (c *Cache) Store(deviceID, guid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guids[deviceID] = guid
}

// Forget drops an entry, for a caller that has evidence it is wrong.
func (c *Cache) Forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.guids, deviceID)
}

// Len reports how many addresses are cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.guids)
}
