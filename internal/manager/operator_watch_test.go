package manager

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// waitFor polls cond every 100 ms for up to 30 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestManagerStartsBeforeTheOperatorIsInstalled runs the manager on a cluster without the operator's CRDs, as on a
// fresh appliance where the manager installs the operator itself.
func TestManagerStartsBeforeTheOperatorIsInstalled(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	c, err := client.New(cfg, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{SystemNamespace, platform.Namespace} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testClient.Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   CacheOptions(),
		// TestSetupWithManagerReconcilesTheAppliance registers a controller of the same name in this process.
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newReconciler(mgr.GetClient(), &fakeCluster{})
	r.APIReader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorder(fieldManager)
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec:       v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "ilm.example.com"}, Platform: platformSpec()},
	}
	if err := c.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	read := func() *v1alpha1.Appliance {
		got := &v1alpha1.Appliance{}
		if err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	waitFor(t, "the reconcile without the operator", func() bool {
		got := read()
		p := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionPlatformReady)
		return got.Status.Setup == v1alpha1.SetupPart2 && p != nil && strings.Contains(p.Message, "does not serve Platform")
	})
	select {
	case err := <-done:
		t.Fatalf("the manager stopped without the operator: %v", err)
	default:
	}

	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: []string{operatorCRDs}, ErrorIfPathMissing: true}); err != nil {
		t.Fatal(err)
	}
	p := &otilm.Platform{}
	key := client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}
	waitFor(t, "the Platform", func() bool { return c.Get(ctx, key, p) == nil })

	// Nothing requeues the Appliance once its Platform is applied, so only the watch can undo an edit.
	want := p.Spec.Version
	edited := p.DeepCopy()
	edited.Spec.Version = "9.9.9"
	if err := c.Patch(ctx, edited, client.MergeFrom(p), client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the edit undone through the Platform watch", func() bool {
		return c.Get(ctx, key, p) == nil && p.Spec.Version == want
	})
}
