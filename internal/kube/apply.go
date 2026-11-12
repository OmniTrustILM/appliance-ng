package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	fieldManager = "appliance-firstboot"
	// mappingTimeout bounds the wait for a kind that a CRD in the same call has not yet made available.
	mappingTimeout  = 30 * time.Second
	mappingInterval = 500 * time.Millisecond
)

// Cluster applies manifests to a Kubernetes cluster and reports readiness.
type Cluster interface {
	// Apply server-side-applies every document of a multi-document YAML in order.
	Apply(ctx context.Context, docs []byte) error
	// LabelNamespace creates the namespace if missing and merges labels into it.
	LabelNamespace(ctx context.Context, ns string, labels map[string]string) error
	// Ready reports whether the object meets the condition, with a short reason when it does not.
	Ready(ctx context.Context, w WaitFor) (bool, string, error)
}

type cluster struct {
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

// NewCluster connects to the cluster of the given kubeconfig.
func NewCluster(kubeconfig []byte) (Cluster, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	return &cluster{dyn: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))}, nil
}

func (c *cluster) Apply(ctx context.Context, docs []byte) error {
	defer c.mapper.Reset()

	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(docs), 4096)
	for {
		var obj map[string]any
		if err := decoder.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		if err := c.applyObject(ctx, &unstructured.Unstructured{Object: obj}); err != nil {
			return err
		}
	}
}

func describe(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return fmt.Sprintf("%s %s/%s", obj.GetKind(), ns, obj.GetName())
	}
	return fmt.Sprintf("%s %s", obj.GetKind(), obj.GetName())
}

func (c *cluster) applyObject(ctx context.Context, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" || obj.GetName() == "" {
		return fmt.Errorf("apply: document without kind or name (%s)", describe(obj))
	}
	mapping, err := c.mappingFor(ctx, gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("apply %s: %w", describe(obj), err)
	}

	var resource dynamic.ResourceInterface = c.dyn.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = c.dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return fmt.Errorf("apply %s: %w", describe(obj), err)
	}
	force := true
	if _, err := resource.Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		return fmt.Errorf("apply %s: %w", describe(obj), err)
	}
	slog.Debug("applied", "kind", obj.GetKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
	return nil
}

// mappingFor resolves a kind, waiting for a CRD applied earlier in the call to be served.
func (c *cluster) mappingFor(ctx context.Context, gk schema.GroupKind, version string) (*meta.RESTMapping, error) {
	ctx, cancel := context.WithTimeout(ctx, mappingTimeout)
	defer cancel()
	for {
		mapping, err := c.mapper.RESTMapping(gk, version)
		if err == nil {
			return mapping, nil
		}
		if !meta.IsNoMatchError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kind %s is not served: %w", gk, err)
		case <-time.After(mappingInterval):
		}
		c.mapper.Reset()
	}
}

func (c *cluster) LabelNamespace(ctx context.Context, ns string, labels map[string]string) error {
	namespaces := c.dyn.Resource(corev1.SchemeGroupVersion.WithResource("namespaces"))
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("label namespace %s: %w", ns, err)
	}
	_, err = namespaces.Patch(ctx, ns, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	if apierrors.IsNotFound(err) {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": ns, "labels": toAny(labels)},
		}}
		_, err = namespaces.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
	}
	if err != nil {
		return fmt.Errorf("label namespace %s: %w", ns, err)
	}
	return nil
}

func toAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
