package manager

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	other := createPod(t, "other-failed", map[string]string{"app.kubernetes.io/name": "other"}, corev1.PodFailed)

	c := managerClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RemoveFailedPods(ctx, c, time.Hour) }()
	deadline := time.Now().Add(10 * time.Second)
	for podExists(t, failed) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("RemoveFailedPods: %v", err)
	}
	if podExists(t, failed) {
		t.Error("the manager's failed pod was kept")
	}
	if !podExists(t, running) || !podExists(t, other) {
		t.Error("a running manager pod or another failed pod was removed")
	}
}
