package kube

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeNameEnv is the environment variable that names the Kubernetes node the manager runs on; the manager's
// Deployment sets it from spec.nodeName.
const NodeNameEnv = "NODE_NAME"

// OwnNode returns the Kubernetes node named by NodeNameEnv, or nil while that node does not exist. Without
// NodeNameEnv it returns the cluster's only node, nil without nodes, and an error when there are several.
func OwnNode(ctx context.Context, c client.Reader) (*corev1.Node, error) {
	if name := os.Getenv(NodeNameEnv); name != "" {
		node := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("get node %s: %w", name, err)
		}
		return node, nil
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	switch len(nodes.Items) {
	case 0:
		return nil, nil
	case 1:
		return &nodes.Items[0], nil
	}
	return nil, fmt.Errorf("%d nodes and %s is not set", len(nodes.Items), NodeNameEnv)
}
