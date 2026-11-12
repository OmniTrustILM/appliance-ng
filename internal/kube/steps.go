// Package kube applies the add-on manifests to a cluster and waits for them to become ready.
package kube

import (
	"fmt"
	"io/fs"

	"sigs.k8s.io/yaml"
)

// Step is one entry of order.yaml: files applied in order, then waits that must pass.
// With CreateOnly the objects of the files are created when missing and never modified afterwards.
type Step struct {
	Name            string                       `json:"name"`
	Files           []string                     `json:"files"`
	CreateOnly      bool                         `json:"createOnly"`
	NamespaceLabels map[string]map[string]string `json:"namespaceLabels"`
	Wait            []WaitFor                    `json:"wait"`
}

// WaitFor names an object and the condition it must reach.
type WaitFor struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	For       string `json:"for"`
}

// LoadSteps parses order.yaml from fsys.
func LoadSteps(fsys fs.FS) ([]Step, error) {
	data, err := fs.ReadFile(fsys, "order.yaml")
	if err != nil {
		return nil, fmt.Errorf("read order.yaml: %w", err)
	}
	var order struct {
		Steps []Step `json:"steps"`
	}
	if err := yaml.UnmarshalStrict(data, &order); err != nil {
		return nil, fmt.Errorf("parse order.yaml: %w", err)
	}
	return order.Steps, nil
}
