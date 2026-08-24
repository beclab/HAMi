/**
# Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package plugin

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/device/nvidia"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

func TestGenerateMigTemplate(t *testing.T) {
	sconfig := nvidia.NvidiaConfig{
		MigGeometriesList: []util.AllowedMigGeometries{
			{
				Models: []string{"A30"},
				Geometries: []util.Geometry{
					{util.MigTemplate{Name: "1g.6gb", Memory: 6144, Count: 4}},
					{util.MigTemplate{Name: "2g.12gb", Memory: 12288, Count: 2}},
					{util.MigTemplate{Name: "4g.24gb", Memory: 24576, Count: 1}},
				},
			},
			{
				Models: []string{"A100-SXM4-40GB", "A100-40GB-PCIe", "A100-PCIE-40GB", "A100-SXM4-40GB"},
				Geometries: []util.Geometry{
					{util.MigTemplate{Name: "1g.5gb", Memory: 5120, Count: 7}},
					{util.MigTemplate{Name: "2g.10gb", Memory: 10240, Count: 3}},
					{util.MigTemplate{Name: "1g.5gb", Memory: 5120, Count: 1}},
					{util.MigTemplate{Name: "3g.20gb", Memory: 20480, Count: 2}},
					{util.MigTemplate{Name: "7g.40gb", Memory: 40960, Count: 1}},
				},
			},
			{
				Models: []string{"A100-SXM4-80GB", "A100-80GB-PCIe", "A100-PCIE-80GB"},
				Geometries: []util.Geometry{
					{util.MigTemplate{Name: "1g.10gb", Memory: 10240, Count: 7}},
					{util.MigTemplate{Name: "2g.20gb", Memory: 20480, Count: 3}},
					{util.MigTemplate{Name: "1g.10gb", Memory: 10240, Count: 1}},
					{util.MigTemplate{Name: "3g.40gb", Memory: 40960, Count: 2}},
					{util.MigTemplate{Name: "7g.80gb", Memory: 81920, Count: 1}},
				},
			},
		},
	}

	plugin := NvidiaDevicePlugin{
		operatingMode:   "mig",
		schedulerConfig: sconfig,
	}
	plugin.migCurrent = nvidia.MigPartedSpec{
		Version:    "v1",
		MigConfigs: make(map[string]nvidia.MigConfigSpecSlice),
	}
	plugin.migCurrent.MigConfigs["current"] = nvidia.MigConfigSpecSlice{
		nvidia.MigConfigSpec{
			Devices:    []int32{0, 1},
			MigEnabled: true,
			MigDevices: make(map[string]int32), // Ensure this map is initialized
		},
	}

	testCases := []struct {
		name          string
		model         string
		deviceIdx     int
		containerDev  util.ContainerDevice
		expectedPos   int
		expectedReset bool
		expectedMig   map[string]int32
	}{
		{
			name:      "2g.10gb template",
			model:     "A100-SXM4-40GB",
			deviceIdx: 0,
			containerDev: util.ContainerDevice{
				Idx:     0,
				UUID:    "aaaaabbbb[1-1]",
				Usedmem: 3000,
			},
			expectedPos:   1,
			expectedReset: true,
			expectedMig: map[string]int32{
				"2g.10gb": 3,
			},
		},
		{
			name:      "1g.5gb template",
			model:     "A100-SXM4-40GB",
			deviceIdx: 0,
			containerDev: util.ContainerDevice{
				Idx:     0,
				UUID:    "aaaaabbbb[0-1]",
				Usedmem: 3000,
			},
			expectedPos:   1,
			expectedReset: true,
			expectedMig: map[string]int32{
				"1g.5gb": 7,
			},
		},
		{
			name:      "no reset needed",
			model:     "A100-SXM4-40GB",
			deviceIdx: 0,
			containerDev: util.ContainerDevice{
				Idx:     0,
				UUID:    "aaaaabbbb[0-2]",
				Usedmem: 3000,
			},
			expectedPos:   2,
			expectedReset: false,
			expectedMig: map[string]int32{
				"1g.5gb": 8,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pos, needsreset := plugin.GenerateMigTemplate(tc.model, tc.deviceIdx, tc.containerDev)

			// Check if the position matches the expected value
			if pos != tc.expectedPos {
				t.Errorf("expected position %d, got %d", tc.expectedPos, pos)
			}

			// Check if the reset flag matches the expected value
			if needsreset != tc.expectedReset {
				t.Errorf("expected reset %v, got %v", tc.expectedReset, needsreset)
			}

			// Check if the mig devices match the expected values
			migDevices := plugin.migCurrent.MigConfigs["current"][0].MigDevices
			for k, v := range tc.expectedMig {
				actual, ok := migDevices[k]
				if !ok || actual != v {
					t.Errorf("expected %s count %d, got %d", k, v, actual)
				}
			}
		})
	}
}

// TestGetNextDeviceRequestContainerIndex covers the Olares pod shape, where a
// service-mesh sidecar is injected ahead of the workload. The device plugin
// names the libvgpu shared-cache directory after the container this returns, so
// picking the sidecar silently files every per-container GPU metric under the
// wrong container name.
func TestGetNextDeviceRequestContainerIndex(t *testing.T) {
	util.InRequestDevices[nvidia.NvidiaGPUDevice] = "hami.io/vgpu-devices-to-allocate"

	tests := []struct {
		name       string
		containers []string
		anno       string
		wantCtr    string
		wantErr    bool
	}{
		{
			name:       "gpu container first",
			containers: []string{"llamacpp", "olares-mesh-in-agent"},
			anno:       "GPU-be013ee7,NVIDIA,0,0,2:;;",
			wantCtr:    "llamacpp",
		},
		{
			name:       "gpu container behind an injected sidecar",
			containers: []string{"linkerd-proxy", "llamacpp", "olares-mesh-in-agent"},
			anno:       ";GPU-be013ee7,NVIDIA,0,0,2:;;",
			wantCtr:    "llamacpp",
		},
		{
			name:       "gpu container last",
			containers: []string{"linkerd-proxy", "olares-mesh-in-agent", "llamacpp"},
			anno:       ";;GPU-be013ee7,NVIDIA,0,0,2:;",
			wantCtr:    "llamacpp",
		},
		{
			name:       "no container requests a device",
			containers: []string{"linkerd-proxy", "llamacpp"},
			anno:       ";;",
			wantErr:    true,
		},
		{
			name:       "more device entries than containers is rejected, not a panic",
			containers: []string{"linkerd-proxy"},
			anno:       ";GPU-be013ee7,NVIDIA,0,0,2:;",
			wantErr:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "pod",
					Namespace:   "ns",
					Annotations: map[string]string{"hami.io/vgpu-devices-to-allocate": test.anno},
				},
			}
			for _, name := range test.containers {
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: name})
			}

			ctr, devs, err := GetNextDeviceRequest(nvidia.NvidiaGPUDevice, pod)
			if test.wantErr {
				if err == nil {
					t.Fatalf("got container %q, want an error", ctr.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetNextDeviceRequest: %v", err)
			}
			if ctr.Name != test.wantCtr {
				t.Errorf("container = %q, want %q", ctr.Name, test.wantCtr)
			}
			if len(devs) != 1 || devs[0].UUID != "GPU-be013ee7" {
				t.Errorf("devices = %+v, want the single GPU-be013ee7 entry", devs)
			}
		})
	}
}
