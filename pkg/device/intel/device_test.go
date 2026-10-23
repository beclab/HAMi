/*
Copyright 2026 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package intel

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/util"
)

const (
	cardA = "intel-1a2b3c4d-0000-03-00.0"
	cardB = "intel-1a2b3c4d-0000-83-00.0"
)

func TestGetNodeDevices(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{})
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Annotations: map[string]string{
		RegisterAnnos: cardA + ",50,32768,100,Intel-Arc-Pro-B70,0,true,0,vxpu,0:" +
			cardB + ",50,32768,100,Intel-Arc-Pro-B70,1,true,1,vxpu,0:",
		fmt.Sprintf(util.ShareModeAnnotationTpl, cardA): util.ShareModeMemSlicing,
		fmt.Sprintf(util.ShareModeAnnotationTpl, cardB): util.ShareModeTimeSlicing,
	}}}

	devices, err := dev.GetNodeDevices(node)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || devices[0].Devmem != 32768 || devices[0].Count != 50 {
		t.Fatalf("devices %+v", devices)
	}
	if devices[0].ShareMode != util.ShareModeMemSlicing {
		t.Errorf("card A share mode %q", devices[0].ShareMode)
	}
	if devices[1].ShareMode != util.ShareModeExclusive {
		t.Errorf("time slicing is not supported, card B got %q", devices[1].ShareMode)
	}
}

func TestGenerateResourceRequests(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{})
	ctr := &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
		"intel.com/vgpu":    resource.MustParse("1"),
		"intel.com/vgpumem": resource.MustParse("4096"),
	}}}

	req := dev.GenerateResourceRequests(ctr)
	if req.Nums != 1 || req.Type != IntelGPUDevice || req.Memreq != 4096 {
		t.Errorf("request %+v", req)
	}
	if dev.GenerateResourceRequests(&corev1.Container{}).Nums != 0 {
		t.Error("a container without intel.com/vgpu requests a card")
	}
}

func usage(id, shareMode string, used, usedmem int32) *util.DeviceUsage {
	return &util.DeviceUsage{
		ID: id, Count: 50, Used: used, Totalmem: 32768, Usedmem: usedmem, Totalcore: 100,
		Type: "Intel-Arc-Pro-B70", ShareMode: shareMode,
	}
}

func TestFit(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{MemoryReserve: 256})
	request := util.ContainerDeviceRequest{Nums: 1, Type: IntelGPUDevice, Memreq: 4096}
	bound := map[string]string{GPUUseUUID: cardA}
	vllm := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{MemoryReserveAnnotation: "1024"}}}

	for _, tc := range []struct {
		name    string
		devices []*util.DeviceUsage
		annos   map[string]string
		pod     *corev1.Pod
		fit     bool
		usedmem int32
	}{
		{
			name:    "memory slice books quota plus reserve",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeMemSlicing, 0, 0)},
			annos:   bound, fit: true, usedmem: 4096 + 256,
		},
		{
			name:    "pod annotation overrides the reserve",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeMemSlicing, 0, 0)},
			annos:   bound, pod: vllm, fit: true, usedmem: 4096 + 1024,
		},
		{
			name:    "binding memory wins over the container request",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeMemSlicing, 0, 0)},
			annos: map[string]string{
				GPUUseUUID: cardA,
				fmt.Sprintf(util.AppGPUMemAnnotationTpl, cardA): "8192",
			},
			fit: true, usedmem: 8192 + 256,
		},
		{
			name:    "reserve counts against free memory",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeMemSlicing, 3, 32768-4096)},
			annos:   bound,
		},
		{
			name:    "no binding, no card",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeMemSlicing, 0, 0)},
			annos:   map[string]string{},
		},
		{
			name:    "card bound to another app",
			devices: []*util.DeviceUsage{usage(cardB, util.ShareModeMemSlicing, 0, 0)},
			annos:   bound,
		},
		{
			name:    "exclusive books the whole card",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeExclusive, 0, 0)},
			annos:   bound, fit: true, usedmem: 32768,
		},
		{
			name:    "exclusive card already in use",
			devices: []*util.DeviceUsage{usage(cardA, util.ShareModeExclusive, 1, 32768)},
			annos:   bound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := tc.pod
			if pod == nil {
				pod = &corev1.Pod{}
			}
			fit, devs, _ := dev.Fit(tc.devices, request, tc.annos, pod, nil, nil)
			if fit != tc.fit {
				t.Fatalf("fit %v, want %v", fit, tc.fit)
			}
			if !fit {
				return
			}
			got := devs[IntelGPUDevice]
			if len(got) != 1 || got[0].UUID != tc.devices[0].ID || got[0].Usedmem != tc.usedmem {
				t.Errorf("allocated %+v, want %d MiB on %s", got, tc.usedmem, tc.devices[0].ID)
			}
		})
	}
}

func TestFitMultipleCards(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{})
	devices := []*util.DeviceUsage{
		usage(cardA, util.ShareModeMemSlicing, 0, 0),
		usage(cardB, util.ShareModeMemSlicing, 0, 0),
	}
	request := util.ContainerDeviceRequest{Nums: 2, Type: IntelGPUDevice, Memreq: 1024}

	fit, devs, _ := dev.Fit(devices, request, map[string]string{GPUUseUUID: cardA + "," + cardB}, &corev1.Pod{}, nil, nil)
	if !fit || len(devs[IntelGPUDevice]) != 2 {
		t.Errorf("fit %v, devices %+v", fit, devs)
	}

	if fit, _, _ = dev.Fit(devices, request, map[string]string{GPUUseUUID: cardA}, &corev1.Pod{}, nil, nil); fit {
		t.Error("two cards fitted with one bound")
	}
}

func TestPatchAnnotationsRecordsReserve(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{})
	pd := util.PodDevices{IntelGPUDevice: util.PodSingleDevice{
		{},
		{{UUID: cardA, Type: IntelGPUDevice, Usedmem: 4096 + 256, ShareMode: util.ShareModeMemSlicing}},
	}}
	annos := map[string]string{}

	dev.PatchAnnotations(&corev1.Pod{}, &annos, pd)
	if annos[MemoryReserveAnnotation] != "256" {
		t.Errorf("reserve %q", annos[MemoryReserveAnnotation])
	}
	want := ";" + cardA + ",Intel,4352,0,1:;"
	if annos[InRequestAnnos] != want || annos[AllocatedAnnos] != want {
		t.Errorf("to allocate %q, want %q", annos[InRequestAnnos], want)
	}
}

func TestInvalidReserveFallsBack(t *testing.T) {
	dev := InitIntelDevice(IntelConfig{MemoryReserve: 300})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{MemoryReserveAnnotation: "1Gi"}}}
	if got := dev.memoryReserve(pod); got != 300 {
		t.Errorf("reserve %d, want the default", got)
	}
}
