package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const testManagerImage = "localhost:5005/manager:0.1.0-arm64"

// fakeReboot is a node that comes back after a number of checks.
type fakeReboot struct {
	talosconfig []byte
	pending     int
	failCycle   error
}

func (f *fakeReboot) bootID(context.Context, []byte) (string, error) { return "boot-a", nil }

func (f *fakeReboot) powercycle(_ context.Context, talosconfig []byte) error {
	f.talosconfig = talosconfig
	return f.failCycle
}

func (f *fakeReboot) back(_ context.Context, _ []byte, before, image string) error {
	if before != "boot-a" || image != testManagerImage {
		return errors.New("unexpected arguments")
	}
	if f.pending > 0 {
		f.pending--
		return errors.New("not back yet")
	}
	return nil
}

// lastUpdateConsole answers GET /updates with update.
func lastUpdateConsole(t *testing.T, update map[string]any) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"update": update})
	}))
	t.Cleanup(srv.Close)
	return newClient(srv.URL, nil, "")
}

func TestDriveRebootComesBack(t *testing.T) {
	fastPolls(t)
	o, _, secrets := updateSecrets(t)
	o.managerImage = testManagerImage
	c := lastUpdateConsole(t, map[string]any{"kind": kindSystem, "from": "0.0.0-test", "to": "0.1.0", "step": restoreStepDone})
	cl := &fakeReboot{pending: 2}
	k := recovery.Kit{Kubeconfig: []byte("token: " + testKubeSecret), Talosconfig: []byte("key: " + testTalosSecret)}
	var err error
	out := captureStdout(t, func() { err = c.reboot(context.Background(), o, k, cl) })
	if err != nil {
		t.Fatalf("reboot = %v\n%s", err, out)
	}
	if string(cl.talosconfig) != string(k.Talosconfig) {
		t.Error("the node was not power-cycled with the kit's talosconfig")
	}
	for _, want := range []string{"runs boot boot-a", "are back after", "system update from 0.0.0-test to 0.1.0 is done"} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed a secret:\n%s", out)
		}
	}
}

func TestDriveRebootFails(t *testing.T) {
	fastPolls(t)
	done := map[string]any{"kind": kindSystem, "step": restoreStepDone}
	for name, tc := range map[string]struct {
		cl     *fakeReboot
		update map[string]any
		want   string
	}{
		"a refused power cycle": {&fakeReboot{failCycle: errors.New("denied")}, done, "denied"},
		"no update":             {&fakeReboot{}, nil, "no update is recorded"},
		"a failed update":       {&fakeReboot{}, map[string]any{"kind": kindSystem, "step": "health", "message": "healthTimeout"}, "healthTimeout"},
		"a platform update":     {&fakeReboot{}, map[string]any{"kind": kindPlatform, "step": restoreStepDone}, "is a platform at step done"},
	} {
		t.Run(name, func(t *testing.T) {
			o := options{managerImage: testManagerImage}
			c := lastUpdateConsole(t, tc.update)
			var err error
			out := captureStdout(t, func() { err = c.reboot(context.Background(), o, recovery.Kit{}, tc.cl) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q\n%s", err, tc.want, out)
			}
		})
	}
}

func TestNodeBack(t *testing.T) {
	ready := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node := func(boot string, conditions []corev1.NodeCondition) *corev1.Node {
		return &corev1.Node{Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Conditions: conditions}}
	}
	if err := nodeBack(node("b", ready), "a"); err != nil {
		t.Errorf("a Ready node with another boot ID = %v", err)
	}
	if err := nodeBack(node("a", ready), "a"); err == nil {
		t.Error("a node with the same boot ID is back")
	}
	if err := nodeBack(node("b", nil), "a"); err == nil {
		t.Error("a node that is not Ready is back")
	}
}

func TestRunsImage(t *testing.T) {
	daemonSet := func(image string, available int32) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: managerContainer, Image: image}},
			}}},
			Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberAvailable: available},
		}
	}
	for _, image := range []string{testManagerImage, testManagerImage + "@sha256:" + strings.Repeat("a", 64)} {
		if err := runsImage(daemonSet(image, 1), testManagerImage); err != nil {
			t.Errorf("runsImage(%s) = %v", image, err)
		}
	}
	if err := runsImage(daemonSet("localhost:5005/manager:0.0.0-test-arm64", 1), testManagerImage); err == nil {
		t.Error("the previous manager image passed")
	}
	if err := runsImage(daemonSet(testManagerImage, 0), testManagerImage); err == nil {
		t.Error("a manager that is not available passed")
	}
}
