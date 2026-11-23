package manager

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// managerPods selects the pods of the manager's Deployment and the dump and NFS target pods it starts, which
// Kubernetes fails when their deadline passes.
var managerPods = client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(
	*mustRequirement("app.kubernetes.io/name", selection.In, "appliance-manager", "appliance-pgdump", "appliance-nfs"),
)}

func mustRequirement(key string, op selection.Operator, values ...string) *labels.Requirement {
	r, err := labels.NewRequirement(key, op, values)
	if err != nil {
		panic(err)
	}
	return r
}

// nfsLeftoverAge is how old an NFS target volume or claim without its pod must be before it counts as left over: an
// operation creates the pod right after them.
var nfsLeftoverAge = 10 * time.Minute

// nfsObjects selects the volumes, claims and pods of NFS backup target operations, which share one name.
var nfsObjects = client.MatchingLabels{"app.kubernetes.io/name": "appliance-nfs"}

// RemoveFailedPods deletes the Failed pods of the manager and of the pods it starts, every interval until ctx ends. The
// manager tolerates the not-ready taint so that it starts before the network does; while the node shuts down, the
// scheduler keeps placing replacement pods on it, the kubelet rejects each one, and Kubernetes keeps Failed pods far
// beyond what one appliance ever needs. It also deletes the volumes and claims of NFS target operations whose pod is
// gone, which a manager that stopped during an operation leaves behind. r reads without a cache.
func RemoveFailedPods(ctx context.Context, r client.Reader, c client.Writer, interval time.Duration) error {
	logger := log.FromContext(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := c.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(SystemNamespace), managerPods,
			client.MatchingFields{"status.phase": string(corev1.PodFailed)})
		if err != nil && ctx.Err() == nil {
			logger.Error(err, "remove the manager's failed pods")
		}
		if err := removeNFSLeftovers(ctx, r, c); err != nil && ctx.Err() == nil {
			logger.Error(err, "remove the volumes of ended NFS target operations")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// removeNFSLeftovers deletes the NFS target volumes and claims older than nfsLeftoverAge whose pod does not exist.
func removeNFSLeftovers(ctx context.Context, r client.Reader, c client.Writer) error {
	claims, volumes := &corev1.PersistentVolumeClaimList{}, &corev1.PersistentVolumeList{}
	if err := r.List(ctx, claims, client.InNamespace(SystemNamespace), nfsObjects); err != nil {
		return err
	}
	if err := r.List(ctx, volumes, nfsObjects); err != nil {
		return err
	}
	var objects []client.Object
	for i := range claims.Items {
		objects = append(objects, &claims.Items[i])
	}
	for i := range volumes.Items {
		objects = append(objects, &volumes.Items[i])
	}
	var errs []error
	for _, obj := range objects {
		if obj.GetDeletionTimestamp() != nil || time.Since(obj.GetCreationTimestamp().Time) < nfsLeftoverAge {
			continue
		}
		err := r.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: obj.GetName()}, &corev1.Pod{})
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
