package routes

import (
	"encoding/json"
	"fmt"
	"github.com/Project-HAMi/HAMi/pkg/util/client"
	"net/http"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"

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
	Apps            []GPUAppInfo `json:"apps"`
	MemoryAllocated *int64       `json:"memoryAllocated,omitempty"`
	MemoryAvailable *int64       `json:"memoryAvailable,omitempty"`
}

type AssignGPURequest struct {
	AppName string             `json:"appName"`
	Memory  *resource.Quantity `json:"memory,omitempty"`
}

type SwitchModeRequest struct {
	Mode string `json:"mode"`
}

func ListGPUInfos(s *scheduler.Scheduler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		klog.Infoln("Listing all GPUs")
		util.GPUManageLock.RLock()
		defer util.GPUManageLock.RUnlock()

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
		util.GPUManageLock.RLock()
		defer util.GPUManageLock.RUnlock()

		nodes, err := s.ListNodes()
		if err != nil {
			klog.Errorln(err)
			http.Error(w, fmt.Sprintf("failed to list nodes: %v", err), http.StatusInternalServerError)
			return
		}

		// if there is any GPU in timeslicing mode
		// we get the apps with no GPUBinding that fall back to the GPU
		// by listing all pods
		// otherwise, we can avoid the unnecessary API call
		var timeslicingGPUExists bool
		var allPods []corev1.Pod
		uuidToGPUDetails := make(map[string]*GPUDetail)

		for _, node := range nodes {
			for _, device := range node.Devices {
				uuidToGPUDetails[device.ID] = &GPUDetail{
					GPUInfo: GPUInfo{
						NodeName:   node.Node.Name,
						DeviceInfo: device,
					},
				}
				if device.ShareMode == util.ShareModeTimeSlicing {
					timeslicingGPUExists = true
				}
			}
		}

		if timeslicingGPUExists {
			podList, err := client.GetClient().CoreV1().Pods(metav1.NamespaceAll).List(r.Context(), metav1.ListOptions{})
			if err != nil {
				err = fmt.Errorf("failed to list pods: %v", err)
				klog.Errorln(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			allPods = podList.Items
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

		for uuid, gpuDetail := range uuidToGPUDetails {
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

			// find pods with no explicit GPUBinding and has fallen back to this GPU
			if gpuDetail.ShareMode == util.ShareModeTimeSlicing {
				for _, pod := range allPods {
					for _, annotation := range pod.Annotations {
						if strings.Contains(annotation, uuid) {
							var appName string
							if pod.Labels != nil {
								appName = pod.Labels[util.AppNameLabelKey]
							}

							// this should never happen
							if appName == "" {
								klog.Errorf("failed to find app name for pod %s/%s", pod.Namespace, pod.Name)
								appName = fmt.Sprintf("pod/%s/%s", pod.Namespace, pod.Name)
							}

							// do not duplicate the apps already found by GPUBindings
							// as GPUs in timeslicing mode can also be assigned to apps by user
							var hasBinding bool
							for _, appInfo := range gpuDetail.Apps {
								if appInfo.AppName == appName {
									hasBinding = true
									break
								}
							}
							if hasBinding {
								continue
							}
							gpuDetail.Apps = append(gpuDetail.Apps, GPUAppInfo{AppName: appName})
						}
					}
				}
			}
		}

		var gpuDetails []GPUDetail
		for _, gpuDetail := range uuidToGPUDetails {
			gpuDetails = append(gpuDetails, *gpuDetail)
		}

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
		for _, node := range nodes {
			for _, device := range node.Devices {
				if device.ID == uuid {
					targetDevice = &device
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

		if targetDevice.ShareMode != util.ShareModeMemSlicing && req.Memory != nil {
			klog.Warningf("Attempting to request memory %d when assigning app %s to GPU %s that's not in memory slicing mode, clearing ...", req.Memory.Value(), req.AppName, uuid)
			req.Memory = nil
		}

		// validate request based on share mode
		if targetDevice.ShareMode == util.ShareModeExclusive {
			for _, binding := range bindings {
				if binding.Spec.UUID == uuid && binding.Spec.AppName != req.AppName {
					err = fmt.Errorf("GPU %s is in exclusive mode and already assigned to app %s, refuse assigning to app %s", uuid, binding.Spec.AppName, req.AppName)
					klog.Warningln(err)
					http.Error(w, err.Error(), http.StatusConflict)
					return
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
		}

		// delete existing pods for this app
		err = ctrlclient.IgnoreNotFound(util.DeletePodsBelongToApp(r.Context(), req.AppName))
		if err != nil {
			err = fmt.Errorf("failed to delete existing pods of app %s: %v", req.AppName, err)
			klog.Errorln(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// delete existing binding for this app if any
		for _, binding := range bindings {
			if binding.Spec.AppName == req.AppName {
				if err := ctrlclient.IgnoreNotFound(util.DeleteGPUBinding(r.Context(), binding.Name)); err != nil {
					err = fmt.Errorf("failed to delete existing binding for app %s: %v", req.AppName, err)
					klog.Errorln(err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
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
