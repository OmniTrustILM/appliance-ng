package manager

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// managerPods selects the pods of the manager's Deployment.
var managerPods = client.MatchingLabels{"app.kubernetes.io/name": "appliance-manager"}

// RemoveFailedPods deletes the manager's Failed pods now and then every interval until ctx ends. The manager
// tolerates the not-ready taint so that it starts before the network does; while the node shuts down, the
// scheduler keeps placing replacement pods on it, the kubelet rejects each one, and Kubernetes keeps Failed pods
// far beyond what one appliance ever needs.
func RemoveFailedPods(ctx context.Context, c client.Writer, interval time.Duration) error {
	logger := log.FromContext(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := c.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(SystemNamespace), managerPods,
			client.MatchingFields{"status.phase": string(corev1.PodFailed)})
		if err != nil && ctx.Err() == nil {
			logger.Error(err, "remove the manager's failed pods")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
