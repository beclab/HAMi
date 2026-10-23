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

// Package intel schedules Intel discrete GPUs registered by the Intel GPU
// device plugin in HAMi mode. Cards are either Exclusive or MemorySlice
// (memory quotas enforced by libvxpu); there is no time slicing.
package intel

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/device/common"
	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

const (
	IntelGPUDevice     = "Intel"
	IntelGPUCommonWord = "Intel"
	RegisterAnnos      = "hami.io/node-intel-register"
	HandshakeAnnos     = "hami.io/node-handshake-intel"
	NodeLockIntel      = "hami.io/mutex.lock"
	InRequestAnnos     = "hami.io/intel-vgpu-devices-to-allocate"
	AllocatedAnnos     = "hami.io/intel-vgpu-devices-allocated"
	// GPUUseUUID lists the cards a pod may use, set from its GPUBindings.
	GPUUseUUID = "intel.com/use-gpuuuid"
	// MemoryReserveAnnotation is the memory in MiB booked per container on
	// top of its quota for what libvxpu cannot count (driver and runtime
	// allocations). Set on a pod it overrides the configured default; the
	// scheduler writes the value it used so the device plugin can subtract
	// it from the booked memory to get the quota.
	MemoryReserveAnnotation = "gpu.bytetrade.io/vxpu-memory-reserve"

	defaultResourceCountName  = "intel.com/vgpu"
	defaultResourceMemoryName = "intel.com/vgpumem"
	defaultMemoryReserve      = 256
)

type IntelConfig struct {
	ResourceCountName  string `yaml:"resourceCountName"`
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	// MemoryReserve is the default MemoryReserveAnnotation, in MiB.
	MemoryReserve int32 `yaml:"memoryReserve"`
}

type IntelGPUDevices struct {
	config IntelConfig
}

func InitIntelDevice(config IntelConfig) *IntelGPUDevices {
	if config.ResourceCountName == "" {
		config.ResourceCountName = defaultResourceCountName
	}
	if config.ResourceMemoryName == "" {
		config.ResourceMemoryName = defaultResourceMemoryName
	}
	if config.MemoryReserve <= 0 {
		config.MemoryReserve = defaultMemoryReserve
	}
	klog.InfoS("initializing intel device", "resourceName", config.ResourceCountName,
		"resourceMem", config.ResourceMemoryName, "memoryReserve", config.MemoryReserve)
	util.InRequestDevices[IntelGPUDevice] = InRequestAnnos
	util.SupportDevices[IntelGPUDevice] = AllocatedAnnos
	util.HandshakeAnnos[IntelGPUDevice] = HandshakeAnnos
	return &IntelGPUDevices{config: config}
}

func ParseConfig(fs *flag.FlagSet) {
}

func (dev *IntelGPUDevices) CommonWord() string {
	return IntelGPUCommonWord
}

func (dev *IntelGPUDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	_, ok := ctr.Resources.Limits[corev1.ResourceName(dev.config.ResourceCountName)]
	return ok, nil
}

func (dev *IntelGPUDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return util.CheckHealth(devType, n)
}

func (dev *IntelGPUDevices) NodeCleanUp(nn string) error {
	return util.MarkAnnotationsToDelete(HandshakeAnnos, nn)
}

func (dev *IntelGPUDevices) GetNodeDevices(n corev1.Node) ([]*util.DeviceInfo, error) {
	devEncoded, ok := n.Annotations[RegisterAnnos]
	if !ok {
		return []*util.DeviceInfo{}, errors.New("annos not found " + RegisterAnnos)
	}
	nodedevices, err := util.DecodeNodeDevices(devEncoded)
	if err != nil {
		klog.ErrorS(err, "failed to decode node devices", "node", n.Name, "device annotation", devEncoded)
		return []*util.DeviceInfo{}, err
	}
	if len(nodedevices) == 0 {
		return []*util.DeviceInfo{}, errors.New("no intel gpu found on node")
	}
	for _, val := range nodedevices {
		if n.Annotations[fmt.Sprintf(util.ShareModeAnnotationTpl, val.ID)] == util.ShareModeMemSlicing {
			val.ShareMode = util.ShareModeMemSlicing
		} else {
			val.ShareMode = util.ShareModeExclusive
		}
	}
	return nodedevices, nil
}

func (dev *IntelGPUDevices) requestsIntel(p *corev1.Pod) bool {
	return slices.ContainsFunc(p.Spec.Containers, func(ctr corev1.Container) bool {
		return dev.GenerateResourceRequests(&ctr).Nums > 0
	})
}

func (dev *IntelGPUDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	if !dev.requestsIntel(p) {
		return nil
	}
	return nodelock.LockNode(n.Name, NodeLockIntel, p)
}

func (dev *IntelGPUDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	if !dev.requestsIntel(p) {
		return nil
	}
	return nodelock.ReleaseNodeLock(n.Name, NodeLockIntel, p, false)
}

func quantityValue(ctr *corev1.Container, name string) (int64, bool) {
	q, ok := ctr.Resources.Limits[corev1.ResourceName(name)]
	if !ok {
		q, ok = ctr.Resources.Requests[corev1.ResourceName(name)]
	}
	if !ok {
		return 0, false
	}
	return q.AsInt64()
}

func (dev *IntelGPUDevices) GenerateResourceRequests(ctr *corev1.Container) util.ContainerDeviceRequest {
	n, ok := quantityValue(ctr, dev.config.ResourceCountName)
	if !ok || n <= 0 {
		return util.ContainerDeviceRequest{}
	}
	mem, _ := quantityValue(ctr, dev.config.ResourceMemoryName)
	return util.ContainerDeviceRequest{
		Nums:             int32(n),
		Type:             IntelGPUDevice,
		Memreq:           int32(mem),
		MemPercentagereq: 101,
	}
}

// memoryReserve is the MemoryReserveAnnotation in effect for the pod.
func (dev *IntelGPUDevices) memoryReserve(pod *corev1.Pod) int32 {
	if pod != nil {
		if v, ok := pod.Annotations[MemoryReserveAnnotation]; ok {
			if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
				return int32(n)
			}
			klog.InfoS("ignoring invalid memory reserve", "pod", klog.KObj(pod), "value", v)
		}
	}
	return dev.config.MemoryReserve
}

func (dev *IntelGPUDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd util.PodDevices) map[string]string {
	devlist, ok := pd[IntelGPUDevice]
	if ok && len(devlist) > 0 {
		deviceStr := util.EncodePodSingleDevice(devlist)
		(*annoinput)[InRequestAnnos] = deviceStr
		(*annoinput)[AllocatedAnnos] = deviceStr
		(*annoinput)[MemoryReserveAnnotation] = strconv.Itoa(int(dev.memoryReserve(pod)))
	}
	return *annoinput
}

func (dev *IntelGPUDevices) ScoreNode(node *corev1.Node, podDevices util.PodSingleDevice, previous []*util.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *IntelGPUDevices) AddResourceUsage(pod *corev1.Pod, n *util.DeviceUsage, ctr *util.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem
	return nil
}

// boundUUIDs are the cards the pod's GPUBindings allow. Without a binding no
// card fits: unlike NVIDIA there is no time-sliced fallback.
func boundUUIDs(annos map[string]string) []string {
	v := annos[GPUUseUUID]
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// bindingMemory is the quota from the app's GPUBinding for this card, in MiB.
func bindingMemory(annos map[string]string, uuid string) int32 {
	v := annos[fmt.Sprintf(util.AppGPUMemAnnotationTpl, uuid)]
	if v == "" {
		return 0
	}
	q, err := resource.ParseQuantity(v)
	if err != nil || q.Value() <= 0 {
		return 0
	}
	return int32(q.Value())
}

func (dev *IntelGPUDevices) Fit(devices []*util.DeviceUsage, request util.ContainerDeviceRequest, annos map[string]string, pod *corev1.Pod, nodeInfo *util.NodeInfo, allocated *util.PodDevices) (bool, map[string]util.ContainerDevices, string) {
	k := request
	tmpDevs := make(map[string]util.ContainerDevices)
	reason := make(map[string]int)
	bound := boundUUIDs(annos)
	reserve := dev.memoryReserve(pod)

	for i := len(devices) - 1; i >= 0 && k.Nums > 0; i-- {
		d := devices[i]
		if !slices.Contains(bound, d.ID) {
			reason[common.CardUUIDMismatch]++
			continue
		}
		if d.Count <= d.Used {
			reason[common.CardTimeSlicingExhausted]++
			continue
		}

		var usedmem, usedcores int32
		switch d.ShareMode {
		case util.ShareModeMemSlicing:
			quota := bindingMemory(annos, d.ID)
			if quota == 0 {
				quota = k.Memreq
			}
			if quota <= 0 {
				reason[common.CardInsufficientMemory]++
				klog.V(5).InfoS("memory slice without a quota", "pod", klog.KObj(pod), "device", d.ID)
				continue
			}
			usedmem = quota + reserve
		default:
			if d.Used > 0 {
				reason[common.ExclusiveDeviceAllocateConflict]++
				continue
			}
			usedmem, usedcores = d.Totalmem, 100
		}
		if d.Totalmem-d.Usedmem < usedmem {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS(common.CardInsufficientMemory, "pod", klog.KObj(pod), "device", d.ID,
				"total", d.Totalmem, "used", d.Usedmem, "request", usedmem)
			continue
		}

		k.Nums--
		tmpDevs[k.Type] = append(tmpDevs[k.Type], util.ContainerDevice{
			Idx:       int(d.Index),
			UUID:      d.ID,
			Type:      k.Type,
			Usedmem:   usedmem,
			Usedcores: usedcores,
			ShareMode: d.ShareMode,
		})
	}
	if k.Nums == 0 {
		return true, tmpDevs, ""
	}
	if len(tmpDevs) > 0 {
		reason[common.AllocatedCardsInsufficientRequest] = len(tmpDevs[k.Type])
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}
