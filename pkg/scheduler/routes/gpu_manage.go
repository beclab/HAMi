package routes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/util/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/julienschmidt/httprouter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/api/gpu/v1alpha1"
	"github.com/Project-HAMi/HAMi/pkg/scheduler"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

type GPUInfo struct {
	NodeName string `json:"nodeName"`
	util.DeviceInfo
}

type GPUAppInfo struct {
	AppName string `json:"appName"`
	Memory  *int64 `json:"memory,omitempty"`
}

type GPUDetail struct {
	GPUInfo
	AllowedShareModes []string     `json:"allowedShareModes,omitempty"`
	Apps              []GPUAppInfo `json:"apps"`
	MemoryAllocated   *int64       `json:"memoryAllocated,omitempty"`
	MemoryAvailable   *int64       `json:"memoryAvailable,omitempty"`
}

type AssignGPURequest struct {
	AppName string             `json:"appName"`
	Memory  *resource.Quantity `json:"memory,omitempty"`
}

type SwitchModeRequest struct {
	Mode string `json:"mode"`
}

type UnassignGPURequest struct {
	AppName string `json:"appName"`
}

type SwitchAssignItem struct {
	ID     string             `json:"id"`
	Memory *resource.Quantity `json:"memory,omitempty"`
}

type SwitchAssignRequest struct {
	AppName  string             `json:"appName"`
	Unassign []SwitchAssignItem `json:"unassign,omitempty"`
	Assign   []SwitchAssignItem `json:"assign,omitempty"`
}

func ListGPUInfos(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		klog.Infoln("Listing all GPUs")
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		var gpus []GPUInfo
		for _, node := range nodes {
			for _, device := range node.Devices {
				gpu := GPUInfo{
					NodeName:   node.Node.Name,
					DeviceInfo: device,
				}
				gpus = append(gpus, gpu)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(gpus)
		if err != nil {
			klog.Errorf("failed to encode response: %v", err)
		}
	}
}

func ListGPUDetails(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		uuidToGPUDetails := make(map[string]*GPUDetail)

		for _, node := range nodes {
			for _, device := range node.Devices {
				allowedShareModes := util.DefaultAllowedShareModes
				config, ok := util.GetCompatibleConfigsByDeviceName(device.Type)
				if ok && len(config.AllowedShareModes) > 0 {
					allowedShareModes = config.AllowedShareModes
				}
				uuidToGPUDetails[device.ID] = &GPUDetail{
					GPUInfo: GPUInfo{
						NodeName:   node.Node.Name,
						DeviceInfo: device,
					},
					AllowedShareModes: allowedShareModes,
				}
			}
		}

		bindings, err := s.ListGPUBindings()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, binding := range bindings {
			gpuDetail := uuidToGPUDetails[binding.Spec.UUID]
			if gpuDetail == nil {
				continue
			}
			appInfo := GPUAppInfo{
				AppName: binding.Spec.AppName,
			}
			if binding.Spec.Memory != nil {
				mem := binding.Spec.Memory.Value()
				appInfo.Memory = &mem
			}
			gpuDetail.Apps = append(gpuDetail.Apps, appInfo)
		}

		for _, gpuDetail := range uuidToGPUDetails {
			if gpuDetail.ShareMode == util.ShareModeMemSlicing {
				var allocated, available int64
				for _, app := range gpuDetail.Apps {
					// normally this check should always equal to true
					if app.Memory != nil {
						allocated += *app.Memory
					}
				}
				available = int64(gpuDetail.Devmem) - allocated
				// this should never happen
				if available < 0 {
					klog.Errorf("error state: GPU %s's allocated memory %d execeeds its total memory %d", gpuDetail.ID, allocated, gpuDetail.Devmem)
					available = 0
				}
				gpuDetail.MemoryAllocated = &allocated
				gpuDetail.MemoryAvailable = &available
			}
		}

		gpuDetails := make([]GPUDetail, 0)
		for _, gpuDetail := range uuidToGPUDetails {
			gpuDetails = append(gpuDetails, *gpuDetail)
		}

		sort.SliceStable(gpuDetails, func(i, j int) bool {
			return gpuDetails[i].NodeName < gpuDetails[j].NodeName
		})
		sort.SliceStable(gpuDetails, func(i, j int) bool {
			return gpuDetails[i].ID < gpuDetails[j].ID
		})

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(gpuDetails)
		if err != nil {
			klog.Errorf("failed to encode response: %v", err)
		}
	}
}

func AssignGPUToApp(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		var req AssignGPURequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to decode request: %v", err), http.StatusBadRequest)
			return
		}
		uuid := ps.ByName("id")

		if uuid == "" || req.AppName == "" {
			http.Error(w, "UUID and AppName are required", http.StatusBadRequest)
			return
		}

		klog.Infof("Assigning GPU %s to app %s", uuid, req.AppName)
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		var targetDevice *util.DeviceInfo
		var targetNodeName string
		for _, node := range nodes {
			for _, device := range node.Devices {
				if device.ID == uuid {
					targetDevice = &device
					targetNodeName = node.Node.Name
					break
				}
			}
			if targetDevice != nil {
				break
			}
		}

		if targetDevice == nil {
			http.Error(w, fmt.Sprintf("GPU %s not found", uuid), http.StatusNotFound)
			return
		}

		bindings, err := s.ListGPUBindings()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var existingBinding *v1alpha1.GPUBinding

		// validate node consistency for multi-binding: an app cannot bind GPUs across different nodes
		uuidToNodeName := make(map[string]string)
		for _, node := range nodes {
			for _, device := range node.Devices {
				uuidToNodeName[device.ID] = node.Node.Name
			}
		}
		for _, binding := range bindings {
			if binding.Spec.AppName != req.AppName {
				continue
			}
			if binding.Spec.UUID == uuid {
				existingBinding = binding
			}
			existingNode := uuidToNodeName[binding.Spec.UUID]
			if existingNode != "" && existingNode != targetNodeName {
				err = fmt.Errorf("app %s already has GPUBinding on node %s, requested GPU is on node %s; cross-node multi-binding is not allowed", req.AppName, existingNode, targetNodeName)
				klog.Warningln(err)
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}

		if existingBinding != nil {
			klog.Warningf("Attempting to assign app %s to already bound GPU %s in mode %s", req.AppName, uuid, targetDevice.ShareMode)
			if targetDevice.ShareMode != util.ShareModeMemSlicing {
				w.WriteHeader(http.StatusOK)
				return
			}
			if req.Memory == nil || req.Memory.Value() == 0 || req.Memory.Value() == existingBinding.Spec.Memory.Value() {
				w.WriteHeader(http.StatusOK)
				return
			}
		}

		if targetDevice.ShareMode != util.ShareModeMemSlicing && req.Memory != nil {
			klog.Warningf("Attempting to request memory %d when assigning app %s to GPU %s that's not in memory slicing mode, clearing ...", req.Memory.Value(), req.AppName, uuid)
			req.Memory = nil
		}

		// if card is in exclusive mode, force out any already assigned app
		if targetDevice.ShareMode == util.ShareModeExclusive {
			pods := s.ListPodsInfo()
			for _, pod := range pods {
				for _, pdev := range pod.Devices {
					for _, cdevs := range pdev {
						for _, cdev := range cdevs {
							if cdev.UUID == uuid {
								klog.Infof("Forcing out pod %s/%s of exclusive GPU %s in favor of %s", pod.Namespace, pod.Name, uuid, req.AppName)
								err = ctrlclient.IgnoreNotFound(client.GetClient().CoreV1().Pods(pod.Namespace).Delete(r.Context(), pod.Name, metav1.DeleteOptions{}))
								if err != nil {
									err = fmt.Errorf("failed to delete existing pod occupying GPU %s/%s: %v", pod.Namespace, pod.Name, err)
									klog.Errorln(err)
									http.Error(w, err.Error(), http.StatusInternalServerError)
									return
								}
							}
						}
					}
				}
			}
			for _, binding := range bindings {
				if binding.Spec.UUID == uuid && binding.Spec.AppName != req.AppName {
					if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), binding.Name)); err != nil {
						err = fmt.Errorf("failed to delete existing GPUBinding %s: %v", binding.Name, err)
						klog.Errorln(err)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}
			}
		}

		if targetDevice.ShareMode == util.ShareModeMemSlicing {
			if req.Memory == nil || req.Memory.Value() == 0 {
				err = fmt.Errorf("memory allocation is required for GPU %s in memory slicing mode, refuse assigning to app %s", uuid, req.AppName)
				klog.Warningln(err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			totalUsedMem := int64(0)
			for _, binding := range bindings {

				// don't count the memory already allocated to this app
				if binding.Spec.AppName == req.AppName {
					continue
				}
				if binding.Spec.UUID == uuid {
					// normally this check should always equal to true
					if binding.Spec.Memory != nil {
						totalUsedMem += binding.Spec.Memory.Value()
					}
				}
			}

			if totalUsedMem+req.Memory.Value() > int64(targetDevice.Devmem) {
				err = fmt.Errorf("not enough memory available on GPU %s, available: %d, request: %d, refuse assigning to app %s", uuid, int64(targetDevice.Devmem)-totalUsedMem, req.Memory.Value(), req.AppName)
				klog.Warningln(err)
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}

			if existingBinding != nil {
				newBinding := existingBinding.DeepCopy()
				newBinding.Spec.Memory = req.Memory
				err = client.GPUClient.Patch(r.Context(), newBinding, ctrlclient.MergeFrom(existingBinding))
				if err != nil {
					err = fmt.Errorf("failed to patch GPUBinding %s: %v", existingBinding.Name, err)
					klog.Errorln(err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		// delete existing pods for this app
		err = ctrlclient.IgnoreNotFound(util.DeletePodsBelongToApp(r.Context(), req.AppName))
		if err != nil {
			err = fmt.Errorf("failed to delete existing pods of app %s: %v", req.AppName, err)
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if existingBinding != nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		newBinding := &v1alpha1.GPUBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: strings.ToLower(fmt.Sprintf("%s-%s-%d", req.AppName, uuid, time.Now().Unix())),
			},
			Spec: v1alpha1.GPUBindingSpec{
				UUID:    uuid,
				AppName: req.AppName,
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						util.AppNameLabelKey: req.AppName,
					},
				},
				Memory: req.Memory,
			},
		}

		if err := s.CreateGPUBinding(r.Context(), newBinding); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

func SwitchGPUMode(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		var req SwitchModeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("failed to decode request: %v", err), http.StatusBadRequest)
			return
		}
		uuid := ps.ByName("id")

		if uuid == "" || req.Mode == "" {
			http.Error(w, "ID and Mode are required", http.StatusBadRequest)
			return
		}

		if req.Mode != util.ShareModeExclusive && req.Mode != util.ShareModeMemSlicing && req.Mode != util.ShareModeTimeSlicing {
			http.Error(w, "invalid share mode", http.StatusBadRequest)
			return
		}

		klog.Infof("Switching GPU %s to mode %s", uuid, req.Mode)
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		var targetNode *corev1.Node
		for _, node := range nodes {
			for _, device := range node.Devices {
				if device.ID == uuid {
					config, ok := util.GetCompatibleConfigsByDeviceName(device.Type)
					if ok && len(config.AllowedShareModes) > 0 {
						if !slices.Contains(config.AllowedShareModes, req.Mode) {
							klog.Warningf("GPU %s does not support mode %s, refusing to switch", uuid, req.Mode)
							http.Error(w, fmt.Sprintf("GPU %s does not support mode %s", uuid, req.Mode), http.StatusBadRequest)
							return
						}
					}
					targetNode = node.Node
					break
				}
			}
			if targetNode != nil {
				break
			}
		}

		if targetNode == nil {
			http.Error(w, fmt.Sprintf("GPU %s not found", uuid), http.StatusNotFound)
			return
		}

		// delete all pods bound to this GPU
		pods := s.ListPodsInfo()
		for _, pod := range pods {
			for _, pdev := range pod.Devices {
				for _, cdevs := range pdev {
					for _, cdev := range cdevs {
						if cdev.UUID == uuid {
							klog.Infof("Deleting pod %s/%s for mode switch of GPU %s", pod.Namespace, pod.Name, uuid)
							err = ctrlclient.IgnoreNotFound(client.GetClient().CoreV1().Pods(pod.Namespace).Delete(r.Context(), pod.Name, metav1.DeleteOptions{}))
							if err != nil {
								err = fmt.Errorf("failed to delete existing pod occupying GPU %s/%s: %v", pod.Namespace, pod.Name, err)
								klog.Errorln(err)
								http.Error(w, err.Error(), http.StatusInternalServerError)
								return
							}
						}
					}
				}
			}
		}

		// delete all existing GPUBindings of this GPU
		bindings, err := s.ListGPUBindings()
		if err != nil {
			klog.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, binding := range bindings {
			if binding.Spec.UUID == uuid {
				if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), binding.Name)); err != nil {
					err = fmt.Errorf("failed to delete existing GPUBinding %s: %v", binding.Name, err)
					klog.Errorln(err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		patchAnnotations := make(map[string]string)
		patchAnnotations[fmt.Sprintf(util.ShareModeAnnotationTpl, uuid)] = req.Mode
		if err := util.PatchNodeAnnotations(targetNode, patchAnnotations); err != nil {
			err = fmt.Errorf("failed to patch node annotations: %v", err)
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// update scheduler's in-memory knowledge of the device
		// because the update operation in the scheduler's watch loop
		// triggered by node update event has a significant delay
		if err := s.UpdateDeviceShareMode(uuid, req.Mode); err != nil {
			err = fmt.Errorf("failed to update device share mode: %v", err)
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

func UnassignGPUFromApp(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		var req UnassignGPURequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("failed to decode request: %v", err), http.StatusBadRequest)
			return
		}
		uuid := ps.ByName("id")

		if uuid == "" || req.AppName == "" {
			http.Error(w, "UUID and AppName are required", http.StatusBadRequest)
			return
		}

		klog.Infof("Unassigning GPU %s from app %s", uuid, req.AppName)
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		bindings, err := s.ListGPUBindings()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		toDelete := make([]string, 0)
		for _, binding := range bindings {
			if binding.Spec.UUID == uuid && binding.Spec.AppName == req.AppName {
				toDelete = append(toDelete, binding.Name)
			}
		}

		if len(toDelete) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}

		if err := ctrlclient.IgnoreNotFound(util.DeletePodsBelongToApp(r.Context(), req.AppName)); err != nil {
			klog.Errorln(fmt.Errorf("failed to delete pods of app %s: %v", req.AppName, err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, name := range toDelete {
			if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), name)); err != nil {
				klog.Errorln(fmt.Errorf("failed to delete GPUBinding %s: %v", name, err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}

// SwitchAssign performs an atomic switch of GPU assignments for an app:
// - Unassigns specified GPU IDs if currently bound to the app (ignores non-existent bindings)
// - Assigns specified GPU IDs (with optional memory for mem-slicing mode) to the app
// - Enforces single-node binding policy across the app's final bindings
// - For exclusive GPUs, evicts existing app bindings and restarts their pods
// - Restarts the target app's pods only if its binding relationship changes
func BulkManageAssignments(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		var req SwitchAssignRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("failed to decode request: %v", err), http.StatusBadRequest)
			return
		}
		if req.AppName == "" {
			http.Error(w, "AppName is required", http.StatusBadRequest)
			return
		}

		klog.Infof("SwitchAssign request for app %s: unassign=%v assign=%v", req.AppName, req.Unassign, req.Assign)
		util.GPUManageLock.Lock()
		defer util.GPUManageLock.Unlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		// Build device maps
		uuidToDevice := make(map[string]util.DeviceInfo)
		uuidToNodeName := make(map[string]string)
		for _, node := range nodes {
			for _, device := range node.Devices {
				uuidToDevice[device.ID] = device
				uuidToNodeName[device.ID] = node.Node.Name
			}
		}

		bindings, err := s.ListGPUBindings()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Group existing bindings
		currentBindingsByUUID := make(map[string]*v1alpha1.GPUBinding)
		bindingsByUUID := make(map[string][]*v1alpha1.GPUBinding)
		for _, b := range bindings {
			bindingsByUUID[b.Spec.UUID] = append(bindingsByUUID[b.Spec.UUID], b)
			if b.Spec.AppName == req.AppName {
				currentBindingsByUUID[b.Spec.UUID] = b
			}
		}

		// Build a quick lookup for GPUs that will be assigned, so unassign won't remove them
		assignIDs := make(map[string]struct{})
		for _, it := range req.Assign {
			if it.ID != "" {
				assignIDs[it.ID] = struct{}{}
			}
		}

		// Plan unassignments (skip any UUID that is also in the assign list)
		toUnassignNames := make([]string, 0)
		unassignSet := make(map[string]struct{})
		for _, it := range req.Unassign {
			if it.ID == "" {
				continue
			}
			if _, willAssign := assignIDs[it.ID]; willAssign {
				continue
			}
			if b := currentBindingsByUUID[it.ID]; b != nil {
				toUnassignNames = append(toUnassignNames, b.Name)
				unassignSet[it.ID] = struct{}{}
			}
		}

		// Plan assignments (patches for mem changes, creates for new bindings, evictions for exclusive)
		type patchItem struct {
			old *v1alpha1.GPUBinding
			mem *resource.Quantity
		}
		patches := make([]patchItem, 0)
		type createItem struct {
			binding *v1alpha1.GPUBinding
		}
		creates := make([]createItem, 0)
		evictUUIDs := make(map[string]struct{})
		deleteOtherBindingNames := make([]string, 0)

		seenAssign := make(map[string]struct{})
		for _, it := range req.Assign {
			if it.ID == "" {
				continue
			}
			if _, duplicated := seenAssign[it.ID]; duplicated {
				continue
			}
			seenAssign[it.ID] = struct{}{}

			dev, ok := uuidToDevice[it.ID]
			if !ok {
				http.Error(w, fmt.Sprintf("GPU %s not found", it.ID), http.StatusNotFound)
				return
			}

			if existing := currentBindingsByUUID[it.ID]; existing != nil {
				// Already bound to this app
				if dev.ShareMode != util.ShareModeMemSlicing {
					// In exclusive/time-slicing, reassign to same GPU is a no-op
					continue
				}
				// mem-slicing: treat missing/zero or unchanged memory as no-op
				if it.Memory == nil || it.Memory.Value() == 0 ||
					(existing.Spec.Memory != nil && it.Memory.Value() == existing.Spec.Memory.Value()) {
					continue
				}
				// validate memory availability excluding this app's current allocation
				totalUsed := int64(0)
				for _, b := range bindingsByUUID[it.ID] {
					if b.Spec.AppName == req.AppName {
						continue
					}
					if b.Spec.Memory != nil {
						totalUsed += b.Spec.Memory.Value()
					}
				}
				if totalUsed+it.Memory.Value() > int64(dev.Devmem) {
					err = fmt.Errorf("not enough memory on GPU %s, available: %d, request: %d for app %s",
						it.ID, int64(dev.Devmem)-totalUsed, it.Memory.Value(), req.AppName)
					klog.Warningln(err)
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				patches = append(patches, patchItem{old: existing, mem: it.Memory})
				continue
			}

			// Not currently bound to this app
			if dev.ShareMode == util.ShareModeMemSlicing {
				if it.Memory == nil || it.Memory.Value() == 0 {
					err = fmt.Errorf("memory allocation is required for GPU %s in memory slicing mode, refuse assigning to app %s", it.ID, req.AppName)
					klog.Warningln(err)
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				totalUsed := int64(0)
				for _, b := range bindingsByUUID[it.ID] {
					if b.Spec.Memory != nil {
						totalUsed += b.Spec.Memory.Value()
					}
				}
				if totalUsed+it.Memory.Value() > int64(dev.Devmem) {
					err = fmt.Errorf("not enough memory available on GPU %s, available: %d, request: %d, refuse assigning to app %s",
						it.ID, int64(dev.Devmem)-totalUsed, it.Memory.Value(), req.AppName)
					klog.Warningln(err)
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
			} else if dev.ShareMode == util.ShareModeExclusive {
				// Plan eviction of other app(s) holding this GPU
				for _, b := range bindingsByUUID[it.ID] {
					if b.Spec.AppName != req.AppName {
						deleteOtherBindingNames = append(deleteOtherBindingNames, b.Name)
						evictUUIDs[it.ID] = struct{}{}
					}
				}
			}

			// Prepare new binding
			mem := it.Memory
			if dev.ShareMode != util.ShareModeMemSlicing {
				mem = nil
			}
			newBinding := &v1alpha1.GPUBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: strings.ToLower(fmt.Sprintf("%s-%s-%d", req.AppName, it.ID, time.Now().Unix())),
				},
				Spec: v1alpha1.GPUBindingSpec{
					UUID:    it.ID,
					AppName: req.AppName,
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							util.AppNameLabelKey: req.AppName,
						},
					},
					Memory: mem,
				},
			}
			creates = append(creates, createItem{binding: newBinding})
		}

		// Determine final set of UUIDs for the app after changes (for node policy check)
		finalUUIDSet := make(map[string]struct{})
		for uuid := range currentBindingsByUUID {
			if _, toUn := unassignSet[uuid]; !toUn {
				finalUUIDSet[uuid] = struct{}{}
			}
		}
		for _, c := range creates {
			finalUUIDSet[c.binding.Spec.UUID] = struct{}{}
		}

		// If no effective change to app's own bindings, return OK without restarting its pods
		if len(toUnassignNames) == 0 && len(patches) == 0 && len(creates) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Enforce single-node binding policy
		nodeSet := make(map[string]struct{})
		for uuid := range finalUUIDSet {
			nodeName := uuidToNodeName[uuid]
			if nodeName == "" {
				http.Error(w, fmt.Sprintf("GPU %s not found", uuid), http.StatusNotFound)
				return
			}
			nodeSet[nodeName] = struct{}{}
		}
		if len(nodeSet) > 1 {
			err = fmt.Errorf("app %s binding spans multiple nodes which is not allowed", req.AppName)
			klog.Warningln(err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}

		// Execute plan
		// 1) Evict other apps for exclusive GPUs
		if len(evictUUIDs) > 0 {
			pods := s.ListPodsInfo()
			for _, pod := range pods {
				for _, pdev := range pod.Devices {
					for _, cdevs := range pdev {
						for _, cdev := range cdevs {
							if _, needEvict := evictUUIDs[cdev.UUID]; needEvict {
								klog.Infof("Evicting pod %s/%s occupying exclusive GPU %s", pod.Namespace, pod.Name, cdev.UUID)
								if err := ctrlclient.IgnoreNotFound(client.GetClient().CoreV1().Pods(pod.Namespace).Delete(r.Context(), pod.Name, metav1.DeleteOptions{})); err != nil {
									err = fmt.Errorf("failed to delete existing pod occupying GPU %s/%s: %v", pod.Namespace, pod.Name, err)
									klog.Errorln(err)
									http.Error(w, err.Error(), http.StatusInternalServerError)
									return
								}
							}
						}
					}
				}
			}
			for _, name := range deleteOtherBindingNames {
				if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), name)); err != nil {
					err = fmt.Errorf("failed to delete existing GPUBinding %s: %v", name, err)
					klog.Errorln(err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		// 2) Restart this app's pods due to binding changes
		if err := ctrlclient.IgnoreNotFound(util.DeletePodsBelongToApp(r.Context(), req.AppName)); err != nil {
			err = fmt.Errorf("failed to delete existing pods of app %s: %v", req.AppName, err)
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// 3) Apply unassignments
		for _, name := range toUnassignNames {
			if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), name)); err != nil {
				err = fmt.Errorf("failed to delete GPUBinding %s: %v", name, err)
				klog.Errorln(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// 4) Apply memory patches
		for _, p := range patches {
			newBinding := p.old.DeepCopy()
			newBinding.Spec.Memory = p.mem
			if err := client.GPUClient.Patch(r.Context(), newBinding, ctrlclient.MergeFrom(p.old)); err != nil {
				err = fmt.Errorf("failed to patch GPUBinding %s: %v", p.old.Name, err)
				klog.Errorln(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// 5) Create new bindings
		for _, c := range creates {
			if err := s.CreateGPUBinding(r.Context(), c.binding); err != nil {
				klog.Errorln(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}
