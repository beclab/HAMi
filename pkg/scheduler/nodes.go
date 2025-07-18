/*
Copyright 2024 The HAMi Authors.

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

package scheduler

import (
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/policy"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

type NodeUsage struct {
	Node    *corev1.Node
	Devices policy.DeviceUsageList
}

type nodeManager struct {
	nodes map[string]*util.NodeInfo
	mutex sync.RWMutex
}

func newNodeManager() *nodeManager {
	return &nodeManager{
		nodes: make(map[string]*util.NodeInfo),
	}
}

func (m *nodeManager) addNode(nodeID string, nodeInfo *util.NodeInfo) {
	if nodeInfo == nil || len(nodeInfo.Devices) == 0 {
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	_, ok := m.nodes[nodeID]
	if ok {
		if len(nodeInfo.Devices) > 0 {
			tmp := make([]util.DeviceInfo, 0, len(nodeInfo.Devices))
			devices := device.GetDevices()
			deviceType := ""
			for _, val := range devices {
				if strings.Contains(nodeInfo.Devices[0].Type, val.CommonWord()) {
					deviceType = val.CommonWord()
				}
			}
			for _, val := range m.nodes[nodeID].Devices {
				if !strings.Contains(val.Type, deviceType) {
					tmp = append(tmp, val)
				}
			}
			m.nodes[nodeID].Devices = tmp
			m.nodes[nodeID].Devices = append(m.nodes[nodeID].Devices, nodeInfo.Devices...)
		}
		m.nodes[nodeID].Node = nodeInfo.Node
	} else {
		m.nodes[nodeID] = nodeInfo
	}
}

func (m *nodeManager) rmNodeDevices(nodeID string, deviceVendor string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	nodeInfo := m.nodes[nodeID]
	if nodeInfo == nil {
		return
	}

	devices := make([]util.DeviceInfo, 0)
	for _, val := range nodeInfo.Devices {
		if val.DeviceVendor != deviceVendor {
			devices = append(devices, val)
		}
	}

	if len(devices) == 0 {
		delete(m.nodes, nodeID)
	} else {
		nodeInfo.Devices = devices
	}
	klog.InfoS("Removing device from node", "nodeName", nodeID, "deviceVendor", deviceVendor, "remainingDevices", devices)
}

func (m *nodeManager) GetNode(nodeID string) (*util.NodeInfo, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	if n, ok := m.nodes[nodeID]; ok {
		return n, nil
	}
	return &util.NodeInfo{}, fmt.Errorf("node %v not found", nodeID)
}

func (m *nodeManager) ListNodes() (map[string]*util.NodeInfo, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.nodes, nil
}

func (m *nodeManager) UpdateDeviceShareMode(uuid, mode string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, node := range m.nodes {
		for _, dev := range node.Devices {
			if dev.ID == uuid {
				dev.ShareMode = mode
				return nil
			}
		}
	}
	return fmt.Errorf("GPU %s not found", uuid)
}
