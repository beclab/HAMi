package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

func (b *GPUBinding) MatchPod(pod *corev1.Pod) bool {
	if pod == nil {
		klog.Errorf("nil pod")
		return false
	}
	if b == nil {
		klog.Error("nil GPUBinding")
		return false
	}
	if b.Spec.PodSelector == nil {
		klog.Error("empty PodSelector")
		return false
	}
	if b.Spec.UUID == "" {
		klog.Error("empty UUID")
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(b.Spec.PodSelector)
	if err != nil {
		klog.Errorf("invalid PodSelector: %v", err)
		return false
	}
	return selector.Matches(labels.Set(pod.Labels))
}
