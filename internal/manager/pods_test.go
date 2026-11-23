package manager

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createPod creates a pod in the system namespace with labels and phase.
func createPod(t *testing.T, name string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: SystemNamespace, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.invalid/c"}}},
	}
	if err := testClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), pod) })
	pod.Status.Phase = phase
	if err := testClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func podExists(t *testing.T, pod *corev1.Pod) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRemoveFailedPodsKeepsAllButTheManagersFailedPods(t *testing.T) {
	manager := map[string]string{"app.kubernetes.io/name": "appliance-manager"}
	failed := createPod(t, "manager-failed", manager, corev1.PodFailed)
	running := createPod(t, "manager-running", manager, corev1.PodRunning)
	dump := createPod(t, "dump-failed", map[string]string{"app.kubernetes.io/name": "appliance-pgdump"}, corev1.PodFailed)
	nfs := createPod(t, "nfs-failed", map[string]string{"app.kubernetes.io/name": "appliance-nfs"}, corev1.PodFailed)
	other := createPod(t, "other-failed", map[string]string{"app.kubernetes.io/name": "other"}, corev1.PodFailed)

	c := managerClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RemoveFailedPods(ctx, c, c, time.Hour) }()
	deadline := time.Now().Add(10 * time.Second)
	for podExists(t, failed) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("RemoveFailedPods: %v", err)
	}
	if podExists(t, failed) || podExists(t, dump) || podExists(t, nfs) {
		t.Error("a failed manager, dump or NFS pod was kept")
	}
	if !podExists(t, running) || !podExists(t, other) {
		t.Error("a running manager pod or another failed pod was removed")
	}
}

// nfsVolume creates a volume and, with claim, a claim of name as an NFS target operation does.
func nfsVolume(t *testing.T, name string, labels map[string]string, claim bool) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	t.Helper()
	ctx := context.Background()
	size := corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	rwx := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.PersistentVolumeSpec{Capacity: size, AccessModes: rwx, PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "192.0.2.20", Path: "/export"},
		}},
	}
	if err := testClient.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFinalizers(pv) })
	if !claim {
		return pv, nil
	}
	class := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: SystemNamespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: rwx, VolumeName: name, StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: size}},
	}
	if err := testClient.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFinalizers(pvc) })
	return pv, pvc
}

// exists reports whether obj exists and is not being deleted: without the controller manager the protection
// finalizers of volumes and claims stay.
func exists(t *testing.T, obj client.Object) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil && obj.GetDeletionTimestamp() == nil
}

func TestRemoveFailedPodsRemovesNFSLeftovers(t *testing.T) {
	nfs := map[string]string{"app.kubernetes.io/name": "appliance-nfs"}
	leftPV, leftPVC := nfsVolume(t, "appliance-nfs-left", nfs, true)
	orphanPV, _ := nfsVolume(t, "appliance-nfs-orphan", nfs, false)
	busyPV, busyPVC := nfsVolume(t, "appliance-nfs-busy", nfs, true)
	createPod(t, "appliance-nfs-busy", nfs, corev1.PodRunning)
	otherPV, _ := nfsVolume(t, "other-volume", map[string]string{"app.kubernetes.io/name": "other"}, false)
	c := managerClient(t)

	if err := removeNFSLeftovers(context.Background(), c, c); err != nil {
		t.Fatal(err)
	}
	if !exists(t, leftPV) || !exists(t, leftPVC) || !exists(t, orphanPV) {
		t.Fatal("a volume younger than the leftover age was removed")
	}

	old := nfsLeftoverAge
	nfsLeftoverAge = 0
	t.Cleanup(func() { nfsLeftoverAge = old })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RemoveFailedPods(ctx, c, c, time.Hour) }()
	deadline := time.Now().Add(10 * time.Second)
	for (exists(t, leftPV) || exists(t, leftPVC) || exists(t, orphanPV)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("RemoveFailedPods: %v", err)
	}
	if exists(t, leftPV) || exists(t, leftPVC) || exists(t, orphanPV) {
		t.Error("an NFS volume or claim without its pod was kept")
	}
	if !exists(t, busyPV) || !exists(t, busyPVC) || !exists(t, otherPV) {
		t.Error("a volume in use or of another kind was removed")
	}
}

// removeFinalizers deletes obj for good.
func removeFinalizers(obj client.Object) {
	ctx := context.Background()
	if testClient.Get(ctx, client.ObjectKeyFromObject(obj), obj) == nil {
		obj.SetFinalizers(nil)
		_ = testClient.Update(ctx, obj)
		_ = testClient.Delete(ctx, obj)
	}
}
