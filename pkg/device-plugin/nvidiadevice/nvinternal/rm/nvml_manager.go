/*
 * SPDX-License-Identifier: Apache-2.0
 *
 * The HAMi Contributors require contributions made to
 * this file be licensed under the Apache-2.0 license or a
 * compatible open source license.
 */

/*
 * Licensed to NVIDIA CORPORATION under one or more contributor
 * license agreements. See the NOTICE file distributed with
 * this work for additional information regarding copyright
 * ownership. NVIDIA CORPORATION licenses this file to you under
 * the Apache License, Version 2.0 (the "License"); you may
 * not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

/*
 * Modifications Copyright The HAMi Authors. See
 * GitHub history for details.
 */

package rm

import (
	"fmt"
	"sync"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/device/nvidia"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

type nvmlResourceManager struct {
	resourceManager
	nvml nvml.Interface

	mu             sync.RWMutex
	lastRescan     time.Time
	rescanInterval time.Duration
}

var _ ResourceManager = (*nvmlResourceManager)(nil)

// NewNVMLResourceManagers returns a set of ResourceManagers, one for each NVML resource in 'config'.
func NewNVMLResourceManagers(nvmllib nvml.Interface, config *nvidia.DeviceConfig) ([]ResourceManager, error) {
	ret := nvmllib.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML: %v", ret)
	}
	defer func() {
		ret := nvmllib.Shutdown()
		if ret != nvml.SUCCESS {
			klog.Infof("Error shutting down NVML: %v", ret)
		}
	}()

	deviceMap, err := NewDeviceMap(nvmllib, config)
	if err != nil {
		return nil, fmt.Errorf("error building device map: %v", err)
	}

	var rms []ResourceManager
	for resourceName, devices := range deviceMap {
		if len(devices) == 0 {
			continue
		}
		for key, value := range devices {
			if nvidia.FilterDeviceToRegister(value.ID, value.Index) {
				klog.V(5).InfoS("Filtering device", "device", value.ID)
				delete(devices, key)
				continue
			}
		}
		r := &nvmlResourceManager{
			resourceManager: resourceManager{
				config:   config,
				resource: resourceName,
				devices:  devices,
			},
			nvml: nvmllib,
		}
		r.rescanInterval = 30 * time.Second
		r.lastRescan = time.Now()
		rms = append(rms, r)
	}

	return rms, nil
}

// GetPreferredAllocation runs an allocation algorithm over the inputs.
// The algorithm chosen is based both on the incoming set of available devices and various config settings.
func (r *nvmlResourceManager) GetPreferredAllocation(available, required []string, size int) ([]string, error) {
	return r.getPreferredAllocation(available, required, size)
}

// GetDevicePaths returns the required and optional device nodes for the requested resources
func (r *nvmlResourceManager) GetDevicePaths(ids []string) []string {
	paths := []string{
		"/dev/nvidiactl",
		"/dev/nvidia-uvm",
		"/dev/nvidia-uvm-tools",
		"/dev/nvidia-modeset",
	}

	for _, p := range r.Devices().Subset(ids).GetPaths() {
		paths = append(paths, p)
	}

	return paths
}

// Devices returns a snapshot of devices for this resource.
//
// It also performs a throttled rescan to detect hot-plug/hot-unplug events.
// Thread-safety rules:
// - the internal map is always protected by r.mu
// - callers get a shallow copy, so external iteration can't race with internal updates
func (r *nvmlResourceManager) Devices() Devices {
	r.maybeRescan()
	return r.devicesSnapshot()
}

// CheckHealth performs health checks on a set of devices, writing to the 'unhealthy' channel with any unhealthy devices
func (r *nvmlResourceManager) CheckHealth(stop <-chan any, unhealthy chan<- *Device, disableNVML <-chan bool, ackDisableHealthChecks chan<- bool) error {
	for {
		// first check if disableNVML channel signal is pass close into checkHealth function
		// if signal is pass close, return error "close signal received"
		err := r.checkHealth(stop, unhealthy, disableNVML)
		if err.Error() == "close signal received" {
			ackDisableHealthChecks <- true
			klog.Info("Check Health has been closed")
			// when disableNVML channel signal is pass restart, continue to restart checkHealth function
			// when disableNVML channel signal is not pass restart, wait for restart signal
			<-disableNVML
			klog.Info("Restarting Check Health")
			continue

		}
		return err
	}
}

func (r *nvmlResourceManager) devicesSnapshot() Devices {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return copyDevicesMap(r.resourceManager.devices)
}

func copyDevicesMap(in Devices) Devices {
	out := make(Devices, len(in))
	for id, dev := range in {
		out[id] = dev
	}
	return out
}

func (r *nvmlResourceManager) maybeRescan() {
	// Fast path: check without lock.
	if r.rescanInterval > 0 && !r.lastRescan.IsZero() && time.Since(r.lastRescan) < r.rescanInterval {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check under lock (double-checked locking).
	if r.rescanInterval > 0 && !r.lastRescan.IsZero() && time.Since(r.lastRescan) < r.rescanInterval {
		return
	}

	if err := r.rescanLocked(); err != nil {
		// Rescan failures should not break the device plugin; log and keep last known-good devices.
		klog.ErrorS(err, "Failed to rescan NVML devices; keeping existing device list", "resource", r.resource)
		return
	}
	r.lastRescan = time.Now()
}

func (r *nvmlResourceManager) rescanLocked() error {
	ret := r.nvml.Init()
	if ret != nvml.SUCCESS {
		if r.config != nil && r.config.Flags.FailOnInitError != nil && *r.config.Flags.FailOnInitError {
			return fmt.Errorf("failed to initialize NVML for rescan: %v", ret)
		}
		return nil
	}
	defer func() {
		ret := r.nvml.Shutdown()
		if ret != nvml.SUCCESS {
			klog.Infof("Error shutting down NVML after rescan: %v", ret)
		}
	}()

	newDeviceMap, err := NewDeviceMap(r.nvml, r.config)
	if err != nil {
		return fmt.Errorf("error building device map during rescan: %v", err)
	}

	newDevices, exists := newDeviceMap[r.resource]
	if !exists {
		newDevices = make(Devices)
	}

	for key, value := range newDevices {
		if nvidia.FilterDeviceToRegister(value.ID, value.Index) {
			klog.V(5).InfoS("Filtering device during rescan", "device", value.ID)
			delete(newDevices, key)
		}
	}

	// Merge: preserve existing *Device pointers to keep Health state.
	oldDevices := r.resourceManager.devices
	if oldDevices == nil {
		oldDevices = make(Devices)
	}

	// Add/update.
	for id, newDev := range newDevices {
		if old, ok := oldDevices[id]; ok && old != nil {
			// Preserve health, but refresh metadata that may change across rescans.
			old.Paths = newDev.Paths
			old.Index = newDev.Index
			old.Topology = newDev.Topology
			continue
		}
		oldDevices[id] = newDev
		klog.InfoS("Hot-plug: new device detected", "resource", r.resource, "deviceID", id, "index", newDev.Index)
	}

	// Remove.
	for id, old := range oldDevices {
		if _, ok := newDevices[id]; ok {
			continue
		}
		if old != nil {
			klog.InfoS("Hot-unplug: device removed", "resource", r.resource, "deviceID", id, "index", old.Index)
		} else {
			klog.InfoS("Hot-unplug: device removed", "resource", r.resource, "deviceID", id)
		}
		delete(oldDevices, id)
	}

	r.resourceManager.devices = oldDevices
	return nil
}
