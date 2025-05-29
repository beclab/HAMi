package client

import (
	gpuv1alpha1 "github.com/Project-HAMi/HAMi/pkg/api/gpu/v1alpha1"
	"k8s.io/klog/v2"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	GPUClient ctrlclient.Client
)

func NewGPUClient() (ctrlclient.Client, error) {
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		return nil, err
	}
	client, err := ctrlclient.New(config, ctrlclient.Options{})
	if err != nil {
		return nil, err
	}
	if err := gpuv1alpha1.AddToScheme(client.Scheme()); err != nil {
		return nil, err
	}
	return client, nil
}

func InitGlobalGPUClient() {
	client, err := NewGPUClient()
	if err != nil {
		klog.Fatalf("new GPU client error: %v", err)
	}
	GPUClient = client
}
