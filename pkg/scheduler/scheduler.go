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
	"context"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/device/nvidia"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"

	"github.com/Project-HAMi/HAMi/pkg/api/gpu/v1alpha1"
	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/k8sutil"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/config"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/policy"
	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Scheduler struct {
	*nodeManager
	*podManager

	stopCh     chan struct{}
	kubeClient kubernetes.Interface
	podLister  listerscorev1.PodLister
	nodeLister listerscorev1.NodeLister
	//Node status returned by filter
	cachedstatus map[string]*NodeUsage
	nodeNotify   chan struct{}
	//Node Overview
	overviewstatus map[string]*NodeUsage

	eventRecorder record.EventRecorder
}

func NewScheduler() *Scheduler {
	klog.InfoS("Initializing HAMi scheduler")
	s := &Scheduler{
		stopCh:       make(chan struct{}),
		cachedstatus: make(map[string]*NodeUsage),
		nodeNotify:   make(chan struct{}, 1),
	}
	s.nodeManager = newNodeManager()
	s.podManager = newPodManager()
	klog.V(2).InfoS("Scheduler initialized successfully")
	return s
}

func (s *Scheduler) doNodeNotify() {
	select {
	case s.nodeNotify <- struct{}{}:
	default:
	}
}

func (s *Scheduler) onAddPod(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(fmt.Errorf("invalid pod object"), "Failed to process pod addition")
		return
	}
	klog.V(5).InfoS("Pod added", "pod", pod.Name, "namespace", pod.Namespace)
	nodeID, ok := pod.Annotations[util.AssignedNodeAnnotations]
	if !ok {
		return
	}
	if k8sutil.IsPodInTerminatedState(pod) || pod.DeletionTimestamp != nil {
		s.delPod(pod)
		return
	}
	podDev, _ := util.DecodePodDevices(util.SupportDevices, pod.Annotations)
	s.addPod(pod, nodeID, podDev)
}

func (s *Scheduler) onUpdatePod(_, newObj any) {
	s.onAddPod(newObj)
}

func (s *Scheduler) onDelPod(obj any) {
	var pod *corev1.Pod
	switch t := obj.(type) {
	case *corev1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		// Handle tombstone objects to avoid missing deletes on relist/reconnect
		if p, ok := t.Obj.(*corev1.Pod); ok {
			pod = p
		} else {
			klog.Errorf("tombstone contained object that is not a Pod: %#v", t.Obj)
			return
		}
	default:
		klog.Errorf("unknown delete object type: %#v", obj)
		return
	}
	s.delPod(pod)

	// release node lock if this pod owned one on a best-effort basis.
	// this is safe because ReleaseNodeLock checks the lock owner and no-ops if different.
	nodeName := ""
	if pod.Annotations != nil {
		nodeName = pod.Annotations[util.AssignedNodeAnnotations]
	}
	if nodeName == "" {
		return
	}
	p := pod.DeepCopy()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := s.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Error("Skip releasing node lock: failed to get node", "node", nodeName, "pod", klog.KObj(p), "err", err)
		return
	}
	for _, dev := range device.GetDevices() {
		if err := dev.ReleaseNodeLock(node, p); err != nil {
			klog.Error("ReleaseNodeLock returned error", "node", nodeName, "pod", klog.KObj(p), "err", err)
		}
	}
}

func (s *Scheduler) DeletePodFromCluster(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	err := ctrlclient.IgnoreNotFound(s.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	if err != nil {
		err = fmt.Errorf("failed to delete pod %s: %v", pod.Name, err)
		klog.Errorln(err)
		return err
	}
	s.onDelPod(pod)
	return nil
}

func (s *Scheduler) DeletePodsBelongToApp(ctx context.Context, appName string) error {
	pods, err := s.kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", util.AppNameLabelKey, appName)})
	if err != nil {
		err = fmt.Errorf("failed to list pods belonging to app %s: %v", appName, err)
		klog.Errorln(err)
		return err
	}
	for _, pod := range pods.Items {
		err := s.DeletePodFromCluster(ctx, &pod)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) Start() {
	klog.InfoS("Starting HAMi scheduler components")
	s.kubeClient = client.GetClient()
	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.kubeClient, time.Hour*1)
	s.podLister = informerFactory.Core().V1().Pods().Lister()
	s.nodeLister = informerFactory.Core().V1().Nodes().Lister()

	informerFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.onAddPod,
		UpdateFunc: s.onUpdatePod,
		DeleteFunc: s.onDelPod,
	})
	informerFactory.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { s.doNodeNotify() },
		UpdateFunc: func(_, _ any) { s.doNodeNotify() },
		DeleteFunc: func(_ any) { s.doNodeNotify() },
	})
	informerFactory.Start(s.stopCh)
	informerFactory.WaitForCacheSync(s.stopCh)
	s.addAllEventHandlers()
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
}

func (s *Scheduler) RegisterFromNodeAnnotations() {
	klog.InfoS("Entering RegisterFromNodeAnnotations")
	defer klog.InfoS("Exiting RegisterFromNodeAnnotations")

	labelSelector := labels.Set(config.NodeLabelSelector).AsSelector()
	klog.InfoS("Using label selector for list nodes", "selector", labelSelector.String())

	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()
	printedLog := map[string]bool{}
	for {
		select {
		case <-s.nodeNotify:
			klog.V(5).InfoS("Received node notification")
		case <-ticker.C:
			klog.InfoS("Ticker triggered")
		case <-s.stopCh:
			klog.InfoS("Received stop signal, exiting RegisterFromNodeAnnotations")
			return
		}
		rawNodes, err := s.nodeLister.List(labelSelector)
		if err != nil {
			klog.ErrorS(err, "Failed to list nodes with selector", "selector", labelSelector.String())
			continue
		}
		klog.V(5).InfoS("Listed nodes", "nodeCount", len(rawNodes))
		var nodeNames []string
		for _, val := range rawNodes {
			nodeNames = append(nodeNames, val.Name)
			klog.V(5).InfoS("Processing node", "nodeName", val.Name)

			for devhandsk, devInstance := range device.GetDevices() {
				klog.V(5).InfoS("Checking device health", "nodeName", val.Name, "deviceVendor", devhandsk)

				nodedevices, err := devInstance.GetNodeDevices(*val)
				if err != nil {
					klog.V(5).InfoS("Failed to get node devices", "nodeName", val.Name, "deviceVendor", devhandsk)
					continue
				}

				health, needUpdate := devInstance.CheckHealth(devhandsk, val)
				klog.V(5).InfoS("Device health check result", "nodeName", val.Name, "deviceVendor", devhandsk, "health", health, "needUpdate", needUpdate)

				if !health {
					klog.Warning("Device is unhealthy, cleaning up node", "nodeName", val.Name, "deviceVendor", devhandsk)
					err := devInstance.NodeCleanUp(val.Name)
					if err != nil {
						klog.ErrorS(err, "Node cleanup failed", "nodeName", val.Name, "deviceVendor", devhandsk)
					}

					s.rmNodeDevices(val.Name, devhandsk)
					continue
				}

				for _, nodedevice := range nodedevices {
					if err := s.UpdateDeviceShareMode(nodedevice.ID, nodedevice.ShareMode); err != nil {
						klog.V(5).InfoS("Skipping share mode sync, device not registered yet", "nodeName", val.Name, "deviceID", nodedevice.ID)
					}
				}

				if !needUpdate {
					klog.V(5).InfoS("No update needed for device", "nodeName", val.Name, "deviceVendor", devhandsk)
					continue
				}
				_, ok := util.HandshakeAnnos[devhandsk]
				if ok {
					tmppat := make(map[string]string)
					tmppat[util.HandshakeAnnos[devhandsk]] = "Requesting_" + time.Now().Format(time.DateTime)
					klog.InfoS("New timestamp for annotation", "nodeName", val.Name, "annotationKey", util.HandshakeAnnos[devhandsk], "annotationValue", tmppat[util.HandshakeAnnos[devhandsk]])
					n, err := util.GetNode(val.Name)
					if err != nil {
						klog.ErrorS(err, "Failed to get node", "nodeName", val.Name)
						continue
					}
					klog.V(5).InfoS("Patching node annotations", "nodeName", val.Name, "annotations", tmppat)
					if err := util.PatchNodeAnnotations(n, tmppat); err != nil {
						klog.ErrorS(err, "Failed to patch node annotations", "nodeName", val.Name)
					}
				}
				nodeInfo := &util.NodeInfo{}
				nodeInfo.ID = val.Name
				nodeInfo.Node = val
				klog.V(5).InfoS("Fetching node devices", "nodeName", val.Name, "deviceVendor", devhandsk)
				nodeInfo.Devices = make([]util.DeviceInfo, 0)
				for _, deviceinfo := range nodedevices {
					nodeInfo.Devices = append(nodeInfo.Devices, *deviceinfo)
				}
				s.addNode(val.Name, nodeInfo)
				if s.nodes[val.Name] != nil && len(nodeInfo.Devices) > 0 {
					if printedLog[val.Name] {
						klog.V(5).InfoS("Node device updated", "nodeName", val.Name, "deviceVendor", devhandsk, "nodeInfo", nodeInfo, "totalDevices", s.nodes[val.Name].Devices)
					} else {
						klog.InfoS("Node device added", "nodeName", val.Name, "deviceVendor", devhandsk, "nodeInfo", nodeInfo, "totalDevices", s.nodes[val.Name].Devices)
						printedLog[val.Name] = true
					}
				}
			}
		}
		activeNodes := make(map[string]struct{}, len(nodeNames))
		for _, name := range nodeNames {
			activeNodes[name] = struct{}{}
		}
		allRegistered, _ := s.ListNodes()
		for id := range allRegistered {
			if _, exists := activeNodes[id]; !exists {
				klog.InfoS("Removing stale node from scheduler", "nodeName", id)
				s.removeNode(id)
			}
		}

		_, _, err = s.getNodesUsage(&nodeNames, nil)
		if err != nil {
			klog.ErrorS(err, "Failed to get node usage", "nodeNames", nodeNames)
		}
	}
}

// CleanupPodsWithMissingDevicesLoop periodically cleans up pods that are assigned
// devices which no longer exist in the cluster.
func (s *Scheduler) CleanupPodsWithMissingDevicesLoop() {
	klog.InfoS("CleanupPodsWithMissingDevicesLoop: delaying start", "delay", config.CleanupStartupDelay)
	timer := time.NewTimer(config.CleanupStartupDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.stopCh:
		return
	}

	klog.InfoS("Starting CleanupPodsWithMissingDevicesLoop")
	defer klog.InfoS("Exiting CleanupPodsWithMissingDevicesLoop")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			util.GPUManageLock.Lock()
			func() {
				defer util.GPUManageLock.Unlock()

				// Get all scheduled pods with their device assignments FIRST
				// This ordering prevents race conditions: if a new device is hot-plugged
				// after we get the pods list, any pod scheduled to that device won't be
				// in our list yet, so we won't incorrectly delete it.
				scheduledPods := s.ListPodsInfo()
				if len(scheduledPods) == 0 {
					return
				}

				// Get all valid device UUIDs from nodes
				nodes, err := s.ListNodes()
				if err != nil {
					klog.ErrorS(err, "CleanupPodsWithMissingDevicesLoop: failed to list nodes")
					return
				}
				validUUIDs := make(map[string]struct{})
				for _, n := range nodes {
					for _, d := range n.Devices {
						validUUIDs[d.ID] = struct{}{}
					}
				}

				podsToDelete := make([]*podInfo, 0)
				for _, pod := range scheduledPods {
					if len(pod.Devices) == 0 {
						continue
					}

					// Check if any assigned device no longer exists
					hasMissingDevice := false
					for _, deviceList := range pod.Devices {
						for _, containerDevices := range deviceList {
							for _, device := range containerDevices {
								if device.UUID == "" {
									continue
								}
								if _, exists := validUUIDs[device.UUID]; !exists {
									klog.InfoS("CleanupPodsWithMissingDevicesLoop: pod has missing device",
										"pod", klog.KRef(pod.Namespace, pod.Name),
										"deviceUUID", device.UUID,
									)
									hasMissingDevice = true
									break
								}
							}
							if hasMissingDevice {
								break
							}
						}
						if hasMissingDevice {
							break
						}
					}

					if hasMissingDevice {
						podsToDelete = append(podsToDelete, pod)
					}
				}

				if len(podsToDelete) == 0 {
					return
				}

				klog.InfoS("CleanupPodsWithMissingDevicesLoop: deleting pods with missing devices", "count", len(podsToDelete))
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				for _, pod := range podsToDelete {

					err := s.kubeClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
					if err != nil {
						if kerrors.IsNotFound(err) {
							klog.InfoS("CleanupPodsWithMissingDevicesLoop: pod not found",
								"pod", klog.KRef(pod.Namespace, pod.Name),
							)
							continue
						}
						klog.ErrorS(err, "CleanupPodsWithMissingDevicesLoop: failed to delete pod",
							"pod", klog.KRef(pod.Namespace, pod.Name),
						)
					} else {
						klog.InfoS("CleanupPodsWithMissingDevicesLoop: deleted pod with missing device",
							"pod", klog.KRef(pod.Namespace, pod.Name),
						)
					}
				}
			}()
		case <-s.stopCh:
			return
		}
	}
}

// InspectAllNodesUsage is used by metrics monitor.
func (s *Scheduler) InspectAllNodesUsage() *map[string]*NodeUsage {
	return &s.overviewstatus
}

// returns all nodes and its device memory usage, and we filter it with nodeSelector, taints, nodeAffinity
// unschedulerable and nodeName.
func (s *Scheduler) getNodesUsage(nodes *[]string, task *corev1.Pod) (*map[string]*NodeUsage, map[string]string, error) {
	overallnodeMap := make(map[string]*NodeUsage)
	cachenodeMap := make(map[string]*NodeUsage)
	failedNodes := make(map[string]string)
	allNodes, err := s.ListNodes()
	if err != nil {
		return &overallnodeMap, failedNodes, err
	}

	for _, node := range allNodes {
		nodeInfo := &NodeUsage{}
		userGPUPolicy := util.GetGPUSchedulerPolicyByPod(config.GPUSchedulerPolicy, task)
		nodeInfo.Node = node.Node
		nodeInfo.Devices = policy.DeviceUsageList{
			Policy:      userGPUPolicy,
			DeviceLists: make([]*policy.DeviceListsScore, 0),
		}
		for _, d := range node.Devices {
			nodeInfo.Devices.DeviceLists = append(nodeInfo.Devices.DeviceLists, &policy.DeviceListsScore{
				Score: 0,
				Device: &util.DeviceUsage{
					ID:        d.ID,
					Index:     d.Index,
					Used:      0,
					Count:     d.Count,
					Usedmem:   0,
					Totalmem:  d.Devmem,
					Totalcore: d.Devcore,
					Usedcores: 0,
					MigUsage: util.MigInUse{
						Index:     0,
						UsageList: make(util.MIGS, 0),
					},
					MigTemplate: d.MIGTemplate,
					Mode:        d.Mode,
					Type:        d.Type,
					Numa:        d.Numa,
					Health:      d.Health,
					CustomInfo:  maps.Clone(d.CustomInfo),
					ShareMode:   d.ShareMode,
				},
			})
		}
		overallnodeMap[node.ID] = nodeInfo
	}

	podsInfo := s.ListPodsInfo()
	for _, p := range podsInfo {
		node, ok := overallnodeMap[p.NodeID]
		if !ok {
			klog.V(5).InfoS("pod allocated unknown node resources",
				"pod", klog.KRef(p.Namespace, p.Name), "nodeID", p.NodeID)
			continue
		}
		for _, podsingleds := range p.Devices {
			for _, ctrdevs := range podsingleds {
				for _, udevice := range ctrdevs {
					for _, d := range node.Devices.DeviceLists {
						deviceID := udevice.UUID
						if strings.Contains(deviceID, "[") {
							deviceID = strings.Split(deviceID, "[")[0]
						}
						if d.Device.ID == deviceID {
							d.Device.Used++
							d.Device.Usedmem += udevice.Usedmem
							d.Device.Usedcores += udevice.Usedcores
							if strings.Contains(udevice.UUID, "[") {
								if strings.Compare(d.Device.Mode, "hami-core") == 0 {
									klog.Errorf("found a mig task running on a hami-core GPU\n")
									d.Device.Health = false
									continue
								}
								tmpIdx, Instance, _ := util.ExtractMigTemplatesFromUUID(udevice.UUID)
								if len(d.Device.MigUsage.UsageList) == 0 {
									util.PlatternMIG(&d.Device.MigUsage, d.Device.MigTemplate, tmpIdx)
								}
								d.Device.MigUsage.UsageList[Instance].InUse = true
								klog.V(5).Infoln("add mig usage", d.Device.MigUsage, "template=", d.Device.MigTemplate, "uuid=", d.Device.ID)
							}
						}
					}
				}
			}
		}
		klog.V(5).Infof("usage: pod %v assigned %v %v", p.Name, p.NodeID, p.Devices)
	}
	s.overviewstatus = overallnodeMap
	for _, nodeID := range *nodes {
		node, err := s.GetNode(nodeID)
		if err != nil {
			// The identified node does not have a gpu device, so the log here has no practical meaning,increase log priority.
			klog.V(5).InfoS("node unregistered", "node", nodeID, "error", err)
			failedNodes[nodeID] = "node unregistered"
			continue
		}
		cachenodeMap[node.ID] = overallnodeMap[node.ID]
	}
	s.cachedstatus = cachenodeMap
	return &cachenodeMap, failedNodes, nil
}

func (s *Scheduler) getPodUsage() (map[string]PodUseDeviceStat, error) {
	podUsageStat := make(map[string]PodUseDeviceStat)
	pods, err := s.podLister.List(labels.NewSelector())
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		podUseDeviceNum := 0
		if v, ok := pod.Annotations[util.DeviceBindPhase]; ok && v == util.DeviceBindSuccess {
			podUseDeviceNum = 1
		}
		nodeName := pod.Spec.NodeName
		if _, ok := podUsageStat[nodeName]; !ok {
			podUsageStat[nodeName] = PodUseDeviceStat{
				TotalPod:     1,
				UseDevicePod: podUseDeviceNum,
			}
		} else {
			exist := podUsageStat[nodeName]
			podUsageStat[nodeName] = PodUseDeviceStat{
				TotalPod:     exist.TotalPod + 1,
				UseDevicePod: exist.UseDevicePod + podUseDeviceNum,
			}
		}
	}
	return podUsageStat, nil
}

type nvidiaRequestSummary struct {
	requested     int
	hasMemory     bool
	memoryByte    int64
	memoryPercent int32
}

func summarizeNVIDIARequests(resourceReqs util.PodDeviceRequests) nvidiaRequestSummary {
	sum := nvidiaRequestSummary{}
	for _, ctrReqs := range resourceReqs {
		for _, req := range ctrReqs {
			if req.Type != nvidia.NvidiaGPUDevice || req.Nums <= 0 {
				continue
			}
			sum.requested += int(req.Nums)
			if req.Memreq > 0 {
				sum.hasMemory = true
				// similar to the memory request in pod spec, we only consider the maximum memory request for now
				// this works with our current assumption that only one container in the pod has a memory request
				if int64(req.Memreq) > sum.memoryByte {
					sum.memoryByte = int64(req.Memreq)
				}
				continue
			}
			if req.MemPercentagereq != 0 && req.MemPercentagereq != 101 {
				sum.hasMemory = true
				// use the max percentage across containers for a conservative single-value summary
				if req.MemPercentagereq > sum.memoryPercent {
					sum.memoryPercent = req.MemPercentagereq
				}
			}
		}
	}
	return sum
}

func requiredNvidiaMemoryBytes(sum nvidiaRequestSummary, totalMemory int64) int64 {
	if !sum.hasMemory {
		return 0
	}
	if sum.memoryByte > 0 {
		return sum.memoryByte
	}
	if sum.memoryPercent > 0 && totalMemory > 0 {
		return totalMemory * int64(sum.memoryPercent) / 100
	}
	return 0
}

func normalizeGPUUUID(uuid string) string {
	if strings.Contains(uuid, "[") {
		return strings.Split(uuid, "[")[0]
	}
	return uuid
}

type appBindingIdentity struct {
	appName   string
	owner     string
	namespace string
}

func podBindingIdentity(pod *corev1.Pod) appBindingIdentity {
	id := appBindingIdentity{}
	if pod == nil {
		return id
	}
	id.namespace = pod.Namespace
	if pod.Labels != nil {
		id.appName = pod.Labels[util.AppNameLabelKey]
		id.owner = pod.Labels[util.AppOwnerLabelKey]
	}
	return id
}

func bindingMatchesIdentity(binding *v1alpha1.GPUBinding, id appBindingIdentity) bool {
	if binding == nil || binding.Spec.UUID == "" || binding.Spec.AppName != id.appName {
		return false
	}
	if binding.Spec.Owner != "" && binding.Spec.Owner != id.owner {
		return false
	}
	if binding.Spec.Namespace != "" && binding.Spec.Namespace != id.namespace {
		return false
	}
	return true
}

// buildGPUUUIDToNodeMap returns a mapping from GPU UUID to the name of the node
// the GPU is registered on. It is used to locate which node an app's bound GPUs
// live on so a pod can be pinned to a single node.
func (s *Scheduler) buildGPUUUIDToNodeMap() map[string]string {
	res := make(map[string]string)
	nodes, err := s.ListNodes()
	if err != nil {
		klog.ErrorS(err, "failed to list nodes for GPU UUID to node mapping")
		return res
	}
	for nodeID, n := range nodes {
		if n == nil {
			continue
		}
		for _, d := range n.Devices {
			uuid := normalizeGPUUUID(d.ID)
			if uuid == "" {
				continue
			}
			res[uuid] = nodeID
		}
	}
	return res
}

func (s *Scheduler) collectConsumedGPUUUIDsByApp(identity appBindingIdentity, currentPod *corev1.Pod) map[string]struct{} {
	consumed := make(map[string]struct{})
	for _, p := range s.ListPodsInfo() {
		if p.Labels == nil || p.Labels[util.AppNameLabelKey] != identity.appName {
			continue
		}
		if identity.owner != "" && p.Labels[util.AppOwnerLabelKey] != identity.owner {
			continue
		}
		if identity.namespace != "" && p.Namespace != identity.namespace {
			continue
		}
		if currentPod != nil && p.Namespace == currentPod.Namespace && p.Name == currentPod.Name {
			continue
		}
		for _, podDevices := range p.Devices {
			for _, containerDevices := range podDevices {
				for _, assigned := range containerDevices {
					uuid := normalizeGPUUUID(assigned.UUID)
					if uuid != "" {
						consumed[uuid] = struct{}{}
					}
				}
			}
		}
	}
	return consumed
}

func (s *Scheduler) Bind(args extenderv1.ExtenderBindingArgs) (*extenderv1.ExtenderBindingResult, error) {
	klog.InfoS("Attempting to bind pod to node", "pod", args.PodName, "namespace", args.PodNamespace, "node", args.Node)
	var res *extenderv1.ExtenderBindingResult

	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: args.PodName, UID: args.PodUID},
		Target:     corev1.ObjectReference{Kind: "Node", Name: args.Node},
	}
	current, err := s.kubeClient.CoreV1().Pods(args.PodNamespace).Get(context.Background(), args.PodName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get pod", "pod", args.PodName, "namespace", args.PodNamespace)
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}, err
	}
	klog.InfoS("Trying to get the target node for pod", "pod", args.PodName, "namespace", args.PodNamespace, "node", args.Node)
	node, err := s.kubeClient.CoreV1().Nodes().Get(context.Background(), args.Node, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get node", "node", args.Node)
		s.recordScheduleBindingResultEvent(current, EventReasonBindingFailed, []string{}, fmt.Errorf("failed to get node %s", args.Node))
		res = &extenderv1.ExtenderBindingResult{Error: err.Error()}
		return res, nil
	}

	tmppatch := map[string]string{
		util.DeviceBindPhase:     "allocating",
		util.BindTimeAnnotations: strconv.FormatInt(time.Now().Unix(), 10),
	}

	for _, val := range device.GetDevices() {
		err = val.LockNode(node, current)
		if err != nil {
			klog.ErrorS(err, "Failed to lock node", "node", args.Node, "device", val)
			goto ReleaseNodeLocks
		}
	}

	err = util.PatchPodAnnotations(current, tmppatch)
	if err != nil {
		klog.ErrorS(err, "Failed to patch pod annotations", "pod", klog.KObj(current))
		return &extenderv1.ExtenderBindingResult{Error: err.Error()}, err
	}

	err = s.kubeClient.CoreV1().Pods(args.PodNamespace).Bind(context.Background(), binding, metav1.CreateOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to bind pod", "pod", args.PodName, "namespace", args.PodNamespace, "node", args.Node)
		goto ReleaseNodeLocks
	}

	s.recordScheduleBindingResultEvent(current, EventReasonBindingSucceed, []string{args.Node}, nil)
	klog.InfoS("Successfully bound pod to node", "pod", args.PodName, "namespace", args.PodNamespace, "node", args.Node)
	return &extenderv1.ExtenderBindingResult{Error: ""}, nil

ReleaseNodeLocks:
	klog.InfoS("Release node locks", "node", args.Node)
	for _, val := range device.GetDevices() {
		val.ReleaseNodeLock(node, current)
	}
	s.recordScheduleBindingResultEvent(current, EventReasonBindingFailed, []string{}, err)
	return &extenderv1.ExtenderBindingResult{Error: err.Error()}, nil
}

func (s *Scheduler) Filter(args extenderv1.ExtenderArgs) (*extenderv1.ExtenderFilterResult, error) {
	klog.InfoS("Starting schedule filter process", "pod", args.Pod.Name, "uuid", args.Pod.UID, "namespace", args.Pod.Namespace)
	resourceReqs := k8sutil.Resourcereqs(args.Pod)
	resourceReqTotal := 0
	for _, n := range resourceReqs {
		for _, k := range n {
			resourceReqTotal += int(k.Nums)
		}
	}
	if resourceReqTotal == 0 {
		klog.V(1).InfoS("Pod does not request any resources",
			"pod", args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", fmt.Errorf("does not request any resource"))
		return &extenderv1.ExtenderFilterResult{
			NodeNames:   args.NodeNames,
			FailedNodes: nil,
			Error:       "",
		}, nil
	}

	// Always serialize Filter under GPUManageLock to avoid races with mode switches/binding changes
	util.GPUManageLock.Lock()
	defer util.GPUManageLock.Unlock()
	annos := args.Pod.Annotations
	if annos == nil {
		annos = make(map[string]string)
	}
	identity := podBindingIdentity(args.Pod)
	appName := identity.appName
	if appName == "" {
		err := fmt.Errorf("cannot schedule pod without %s label", util.AppNameLabelKey)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		failedNodes := make(map[string]string)
		if args.NodeNames != nil {
			for _, nodeName := range *args.NodeNames {
				failedNodes[nodeName] = "pod has no owner application"
			}
		}
		return &extenderv1.ExtenderFilterResult{
			FailedNodes: failedNodes,
		}, nil
	}

	bindings, err := s.ListGPUBindings()
	if err != nil {
		klog.ErrorS(err, "Failed to list GPUBindings for Filter", "pod", klog.KObj(args.Pod))
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		return nil, err
	}

	appBoundByUUID := make(map[string]*v1alpha1.GPUBinding)
	matchedBindings := make([]*v1alpha1.GPUBinding, 0)
	for _, b := range bindings {
		if !bindingMatchesIdentity(b, identity) {
			continue
		}
		matchedPod := b.MatchPod(args.Pod)
		// todo: restrict binding operation on specific nodes
		// bindingNode, ok := uuidToNode[b.Spec.UUID]
		// if !ok {
		// 	if matchedPod {
		// 		err := fmt.Errorf("GPU binding %s references unknown GPU %s for pod %s/%s", b.Name, b.Spec.UUID, args.Pod.Namespace, args.Pod.Name)
		// 		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		// 		return &extenderv1.ExtenderFilterResult{
		// 			FailedNodes: map[string]string{},
		// 		}, nil
		// 	}
		// 	continue
		// }
		// if _, eligible := eligibleNodes[bindingNode]; !eligible {
		// 	if matchedPod {
		// 		err := fmt.Errorf("GPU binding %s (uuid=%s) targets node %s, which conflicts with scheduler filtered nodes for pod %s/%s", b.Name, b.Spec.UUID, bindingNode, args.Pod.Namespace, args.Pod.Name)
		// 		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		// 		return &extenderv1.ExtenderFilterResult{
		// 			FailedNodes: map[string]string{},
		// 		}, nil
		// 	}
		// 	continue
		// }
		if _, ok := appBoundByUUID[b.Spec.UUID]; !ok {
			appBoundByUUID[b.Spec.UUID] = b
		}
		// todo: maybe we can remove this check, because the pod selector currently only matches the app name
		if !matchedPod {
			continue
		}
		matchedBindings = append(matchedBindings, b)
		// todo: currently this will conflict if the pod has multiple containers, or requires multiple GPUs with different memory requests in bindings
		if b.Spec.Memory != nil {
			annos[fmt.Sprintf(nvidia.AppGPUMemAnnotationTpl, b.Spec.UUID)] = b.Spec.Memory.String()
		}
	}

	policyMode := ""
	if args.Pod.Labels != nil {
		policyMode = args.Pod.Labels[nvidia.AppPodGPUConsumePolicyKey]
	}
	consumedByApp := s.collectConsumedGPUUUIDsByApp(identity, args.Pod)

	selectedUUIDs := make([]string, 0)
	selectedUUIDSet := make(map[string]struct{})
	appendSelectedUUID := func(uuid string) {
		if uuid == "" {
			return
		}
		if _, ok := selectedUUIDSet[uuid]; ok {
			return
		}
		selectedUUIDSet[uuid] = struct{}{}
		selectedUUIDs = append(selectedUUIDs, uuid)
	}

	if policyMode == "" || policyMode == nvidia.AppPodGPUConsumePolicyAll {
		// "all" policy: a single pod consumes all GPUs bound to this app that live on
		// one node. Because a pod can only run on a single node, the app's bound GPUs
		// may span several nodes. We therefore group the bound GPUs by node, skip nodes
		// already occupied by other pods of this app (i.e. nodes whose bound GPUs are
		// already consumed), and pin this pod to one remaining free node so that it
		// takes all of that node's bound GPUs.
		if len(matchedBindings) > 0 {
			uuidToNode := s.buildGPUUUIDToNodeMap()
			nodeToBoundUUIDs := make(map[string][]string)
			nodeOrder := make([]string, 0)
			for _, b := range matchedBindings {
				nodeName, ok := uuidToNode[normalizeGPUUUID(b.Spec.UUID)]
				if !ok {
					klog.V(4).InfoS("bound GPU not found on any registered node, skipping",
						"app", appName, "uuid", b.Spec.UUID)
					continue
				}
				if _, seen := nodeToBoundUUIDs[nodeName]; !seen {
					nodeOrder = append(nodeOrder, nodeName)
				}
				nodeToBoundUUIDs[nodeName] = append(nodeToBoundUUIDs[nodeName], b.Spec.UUID)
			}
			// deterministic node selection order so concurrent pods of the same app
			// fill nodes predictably (occupied nodes are skipped as they fill up).
			sort.Strings(nodeOrder)

			candidate := make(map[string]struct{})
			if args.NodeNames != nil {
				for _, nn := range *args.NodeNames {
					candidate[nn] = struct{}{}
				}
			}

			targetNode := ""
			var targetUUIDs []string
			for _, nodeName := range nodeOrder {
				// only consider nodes that survived the default scheduler's predicates
				if len(candidate) > 0 {
					if _, ok := candidate[nodeName]; !ok {
						continue
					}
				}
				// skip nodes already occupied by another pod of this app
				occupied := false
				for _, u := range nodeToBoundUUIDs[nodeName] {
					if _, c := consumedByApp[normalizeGPUUUID(u)]; c {
						occupied = true
						break
					}
				}
				if occupied {
					continue
				}
				targetNode = nodeName
				targetUUIDs = nodeToBoundUUIDs[nodeName]
				break
			}

			if targetNode == "" {
				err := fmt.Errorf("no free node with GPUs bound to app %s (all bound nodes are occupied by existing pods or unschedulable)", appName)
				s.recordScheduleFilterResultEvent(args.Pod, EventReasonInsufficientGPU, "", err)
				return &extenderv1.ExtenderFilterResult{
					FailedNodes: map[string]string{},
				}, nil
			}

			// pin the pod to the chosen node and request exactly its bound GPUs
			args.NodeNames = &[]string{targetNode}
			for ctrIdx := range resourceReqs {
				for reqIdx, req := range resourceReqs[ctrIdx] {
					if req.Type != nvidia.NvidiaGPUDevice || req.Nums <= 0 {
						continue
					}
					// this assumes only one container in the pod has a gpu request
					req.Nums = int32(len(targetUUIDs))
					resourceReqs[ctrIdx][reqIdx] = req
				}
			}
			for _, u := range targetUUIDs {
				appendSelectedUUID(u)
			}
			klog.InfoS("app consume-policy=all: pinning pod to node holding all bound GPUs",
				"app", appName, "node", targetNode, "gpuCount", len(targetUUIDs), "uuids", targetUUIDs)
		}
	} else {
		// other policies (e.g. "single"): keep the pod's own requested GPU count and
		// only constrain it to the app's bound GPUs that are still free.
		for _, b := range matchedBindings {
			if _, occupied := consumedByApp[normalizeGPUUUID(b.Spec.UUID)]; occupied {
				continue
			}
			appendSelectedUUID(b.Spec.UUID)
		}
	}

	nvidiaSummary := summarizeNVIDIARequests(resourceReqs)
	if nvidiaSummary.requested > 0 && len(selectedUUIDs) < nvidiaSummary.requested {
		err := fmt.Errorf("insufficient GPUBindings for app %s, requested=%d, bound=%d", appName, nvidiaSummary.requested, len(selectedUUIDs))
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonInsufficientGPU, "", err)
		return &extenderv1.ExtenderFilterResult{
			FailedNodes: map[string]string{},
		}, nil
	}
	if len(selectedUUIDs) > 0 {
		annos[nvidia.GPUUseUUID] = strings.Join(selectedUUIDs, ",")
	} else {
		annos[nvidia.GPUUseUUID] = ""
	}
	s.delPod(args.Pod)
	nodeUsage, failedNodes, err := s.getNodesUsage(args.NodeNames, args.Pod)
	if err != nil {
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		return nil, err
	}
	if len(failedNodes) != 0 {
		klog.V(5).InfoS("Nodes failed during usage retrieval",
			"nodes", failedNodes)
	}
	nodeScores, err := s.calcScore(nodeUsage, resourceReqs, annos, args.Pod, failedNodes)
	if err != nil {
		err := fmt.Errorf("calcScore failed %v for pod %v", err, args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		return nil, err
	}
	if len((*nodeScores).NodeList) == 0 {
		klog.V(4).InfoS("No available nodes meet the required scores",
			"pod", args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonInsufficientGPU, "", fmt.Errorf("no available GPU resources on all %d nodes", len(*args.NodeNames)))
		return &extenderv1.ExtenderFilterResult{
			FailedNodes: failedNodes,
		}, nil
	}
	klog.V(4).Infoln("nodeScores_len=", len((*nodeScores).NodeList))
	sort.Sort(nodeScores)
	m := (*nodeScores).NodeList[len((*nodeScores).NodeList)-1]

	devlist, ok := m.Devices[nvidia.NvidiaGPUDevice]
	if ok && len(devlist) > 0 {
		allocatedUUIDs := make(map[string]struct{})
		for _, cdev := range devlist {
			for _, dev := range cdev {
				uuid := normalizeGPUUUID(dev.UUID)
				if uuid == "" {
					continue
				}
				allocatedUUIDs[uuid] = struct{}{}
			}
		}
		for uuid := range allocatedUUIDs {
			if _, exists := appBoundByUUID[uuid]; !exists {
				err := fmt.Errorf("allocated GPU %s for app %s has no matching GPUBinding", uuid, appName)
				s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
				return nil, err
			}
		}
	}

	klog.InfoS("Scheduling pod to node",
		"podNamespace", args.Pod.Namespace,
		"podName", args.Pod.Name,
		"nodeID", m.NodeID,
		"devices", m.Devices)
	annotations := make(map[string]string)
	annotations[util.AssignedNodeAnnotations] = m.NodeID
	annotations[util.AssignedTimeAnnotations] = strconv.FormatInt(time.Now().Unix(), 10)

	for _, val := range device.GetDevices() {
		val.PatchAnnotations(args.Pod, &annotations, m.Devices)
	}

	s.addPod(args.Pod, m.NodeID, m.Devices)
	err = util.PatchPodAnnotations(args.Pod, annotations)
	if err != nil {
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, "", err)
		s.delPod(args.Pod)
		return nil, err
	}
	successMsg := genSuccessMsg(len(*args.NodeNames), m.NodeID, nodeScores.NodeList)
	s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringSucceed, successMsg, nil)
	res := extenderv1.ExtenderFilterResult{NodeNames: &[]string{m.NodeID}}
	return &res, nil
}

func genSuccessMsg(totalNodes int, target string, nodes []*policy.NodeScore) string {
	successMsg := "find fit node(%s), %d nodes not fit, %d nodes fit(%s)"
	var scores []string
	for _, no := range nodes {
		scores = append(scores, fmt.Sprintf("%s:%.2f", no.NodeID, no.Score))
	}
	score := strings.Join(scores, ",")
	return fmt.Sprintf(successMsg, target, totalNodes-len(nodes), len(nodes), score)
}

// ListGPUBindings returns all GPU bindings in the cluster
func (s *Scheduler) ListGPUBindings() ([]*v1alpha1.GPUBinding, error) {
	bindings := &v1alpha1.GPUBindingList{}
	if err := client.GPUClient.List(context.Background(), bindings); err != nil {
		return nil, fmt.Errorf("failed to list GPU bindings: %v", err)
	}
	result := make([]*v1alpha1.GPUBinding, len(bindings.Items))
	for i := range bindings.Items {
		result[i] = &bindings.Items[i]
	}
	return result, nil
}

// CreateGPUBinding creates a new GPU binding
func (s *Scheduler) CreateGPUBinding(ctx context.Context, binding *v1alpha1.GPUBinding) error {
	if err := client.GPUClient.Create(ctx, binding); err != nil {
		return fmt.Errorf("failed to create GPU binding: %v", err)
	}
	return nil
}

// UpdateDeviceShareMode updates the share mode for a specific GPU device
func (s *Scheduler) UpdateDeviceShareMode(uuid string, mode string) error {
	nodes, err := s.ListNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}

	for _, node := range nodes {
		for i := range node.Devices {
			if node.Devices[i].ID == uuid {
				node.Devices[i].ShareMode = mode
				return nil
			}
		}
	}
	return fmt.Errorf("GPU device %s not found", uuid)
}
