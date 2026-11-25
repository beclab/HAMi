package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/api/gpu/v1alpha1"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

func appPod(consumer string) *corev1.Pod {
	labels := map[string]string{
		util.AppNameLabelKey:  "llm",
		util.AppOwnerLabelKey: "alice",
	}
	if consumer != "" {
		labels[util.AppGPUConsumerLabelKey] = consumer
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "llm-alice", Labels: labels}}
}

func appBinding(consumer string) *v1alpha1.GPUBinding {
	return &v1alpha1.GPUBinding{Spec: v1alpha1.GPUBindingSpec{
		UUID: "GPU-1", AppName: "llm", Owner: "alice", Consumer: consumer,
	}}
}

func TestBindingMatchesConsumer(t *testing.T) {
	for _, tc := range []struct {
		name            string
		bindingConsumer string
		podConsumer     string
		want            bool
	}{
		{"legacy binding, legacy pod", "", "", true},
		{"legacy binding, consumer pod", "", "server", true},
		{"consumer binding, legacy pod", "server", "", true},
		{"same consumer", "server", "server", true},
		{"other consumer", "server", "embed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bindingMatchesIdentity(appBinding(tc.bindingConsumer), podBindingIdentity(appPod(tc.podConsumer)))
			if got != tc.want {
				t.Errorf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBindingMatchesIdentityKeepsAppAndOwner(t *testing.T) {
	id := podBindingIdentity(appPod("server"))
	other := appBinding("server")
	other.Spec.Owner = "bob"
	if bindingMatchesIdentity(other, id) {
		t.Error("binding of another owner matched")
	}
	other = appBinding("server")
	other.Spec.AppName = "embed"
	if bindingMatchesIdentity(other, id) {
		t.Error("binding of another app matched")
	}
}

func TestPodSharesConsumer(t *testing.T) {
	server := podBindingIdentity(appPod("server"))
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"same consumer occupies", appPod("server"), true},
		{"other consumer does not occupy", appPod("embed"), false},
		{"legacy pod occupies", appPod(""), true},
		{"other namespace does not occupy", func() *corev1.Pod {
			p := appPod("server")
			p.Namespace = "other"
			return p
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podSharesConsumer(tc.pod.Labels, tc.pod.Namespace, server); got != tc.want {
				t.Errorf("shares = %v, want %v", got, tc.want)
			}
		})
	}
}
