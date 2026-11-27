package kube

import (
	"context"
	"errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeNameEnv is the environment variable that names the Kubernetes node the manager runs on; the manager's
// pod sets it from spec.nodeName.
const NodeNameEnv = "NODE_NAME"

// OwnNode returns the Kubernetes node named by NodeNameEnv, or nil while that node does not exist.
func OwnNode(ctx context.Context, c client.Reader) (*corev1.Node, error) {
	return NodeByName(ctx, c, os.Getenv(NodeNameEnv))
}

// NodeByName returns the Kubernetes node name, or nil while that node does not exist.
func NodeByName(ctx context.Context, c client.Reader, name string) (*corev1.Node, error) {
	if name == "" {
		return nil, errors.New("no node name given")
	}
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	return node, nil
}

// InternalAddress returns the internal IP address the Kubernetes node name reports.
func InternalAddress(ctx context.Context, c client.Reader, name string) (string, error) {
	node, err := NodeByName(ctx, c, name)
	if err != nil {
		return "", err
	}
	if node == nil {
		return "", fmt.Errorf("node %s is not registered", name)
	}
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address, nil
		}
	}
	return "", fmt.Errorf("node %s reports no internal address", name)
}
