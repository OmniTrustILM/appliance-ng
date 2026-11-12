package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// platformResource is the Platform custom resource defined by the ILM operator CRD.
var platformResource = schema.GroupVersionResource{Group: "otilm.com", Version: "v1alpha1", Resource: "platforms"}

type waitKind struct {
	resource schema.GroupVersionResource
	// checks maps a For condition to its evaluation on an existing object.
	checks map[string]func(*unstructured.Unstructured) (bool, string)
}

var waitKinds = map[string]waitKind{
	"CustomResourceDefinition": {
		resource: schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
		checks:   map[string]func(*unstructured.Unstructured) (bool, string){"Established": conditionTrue("Established")},
	},
	"Deployment": {
		resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		checks:   map[string]func(*unstructured.Unstructured) (bool, string){"Available": conditionTrue("Available")},
	},
	"DaemonSet": {
		resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		checks:   map[string]func(*unstructured.Unstructured) (bool, string){"Rollout": daemonSetRolledOut},
	},
	"Node": {
		resource: schema.GroupVersionResource{Version: "v1", Resource: "nodes"},
		checks:   map[string]func(*unstructured.Unstructured) (bool, string){"Ready": conditionTrue("Ready")},
	},
	"Platform": {
		resource: platformResource,
		checks:   map[string]func(*unstructured.Unstructured) (bool, string){"Available": platformAvailable},
	},
}

func (c *cluster) Ready(ctx context.Context, w WaitFor) (bool, string, error) {
	kind, ok := waitKinds[w.Kind]
	if !ok {
		return false, "", fmt.Errorf("wait: unsupported kind %q", w.Kind)
	}
	check, ok := kind.checks[w.For]
	if !ok {
		return false, "", fmt.Errorf("wait for %s: unsupported condition %q", w.Kind, w.For)
	}
	if w.Kind == "Node" && w.Name == "" {
		return c.allNodesReady(ctx, check)
	}

	client := c.dyn.Resource(kind.resource)
	var obj *unstructured.Unstructured
	var err error
	if w.Namespace != "" {
		obj, err = client.Namespace(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	} else {
		obj, err = client.Get(ctx, w.Name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return false, "not found", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get %s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
	}
	ready, reason := check(obj)
	return ready, reason, nil
}

func (c *cluster) allNodesReady(ctx context.Context, check func(*unstructured.Unstructured) (bool, string)) (bool, string, error) {
	nodes, err := c.dyn.Resource(waitKinds["Node"].resource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "", fmt.Errorf("list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return false, "no nodes", nil
	}
	for i := range nodes.Items {
		if ready, reason := check(&nodes.Items[i]); !ready {
			return false, fmt.Sprintf("node %s: %s", nodes.Items[i].GetName(), reason), nil
		}
	}
	return true, "", nil
}

// conditionTrue returns a check that passes when the named status condition is True.
// The reason of a condition that is not True is returned.
func conditionTrue(condType string) func(*unstructured.Unstructured) (bool, string) {
	return func(obj *unstructured.Unstructured) (bool, string) {
		conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, item := range conditions {
			cond, ok := item.(map[string]any)
			if !ok || cond["type"] != condType {
				continue
			}
			if cond["status"] == "True" {
				return true, ""
			}
			if reason, _ := cond["reason"].(string); reason != "" {
				return false, reason
			}
			return false, condType + " is not true"
		}
		return false, "no " + condType + " condition"
	}
}

// platformAvailable passes when the Available condition is True and was set for the current generation.
func platformAvailable(obj *unstructured.Unstructured) (bool, string) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if ok && cond["type"] == "Available" {
			if observed, _ := cond["observedGeneration"].(int64); observed != obj.GetGeneration() {
				return false, "generation not observed"
			}
		}
	}
	return conditionTrue("Available")(obj)
}

func daemonSetRolledOut(obj *unstructured.Unstructured) (bool, string) {
	count := func(field string) int64 {
		n, _, _ := unstructured.NestedInt64(obj.Object, "status", field)
		return n
	}
	if count("observedGeneration") != obj.GetGeneration() {
		return false, "generation not observed"
	}
	desired, updated, available := count("desiredNumberScheduled"), count("updatedNumberScheduled"), count("numberAvailable")
	if desired != updated || desired != available {
		return false, fmt.Sprintf("desired %d, updated %d, available %d", desired, updated, available)
	}
	return true, ""
}
