// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"sync"
)

// ProviderFactory is a function that creates a BlockVolumeProvider from
// StorageClass parameters.
type ProviderFactory func(params map[string]string) (BlockVolumeProvider, error)

var (
	mu       sync.Mutex
	registry = map[string]ProviderFactory{}
)

// RegisterProvider registers a provider factory under the given name.
// Each provider package calls this in its init() function.
func RegisterProvider(name string, factory ProviderFactory) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = factory
}

// NewBlockVolumeProvider creates the appropriate provider based on the
// "cloudProvider" StorageClass parameter.
func NewBlockVolumeProvider(params map[string]string) (BlockVolumeProvider, error) {
	name := params["cloudProvider"]
	if name == "" {
		return nil, fmt.Errorf("StorageClass parameter 'cloudProvider' is required (e.g., 'libvirt', 'aws')")
	}

	mu.Lock()
	factory, ok := registry[name]
	mu.Unlock()

	if !ok {
		supported := make([]string, 0, len(registry))
		for k := range registry {
			supported = append(supported, k)
		}
		return nil, fmt.Errorf("unsupported cloud provider %q, supported: %v", name, supported)
	}

	return factory(params)
}
