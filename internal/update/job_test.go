package update_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

// The API server the update jobs run against, started by the first test that needs it.
var (
	clusterOnce sync.Once
	clusterEnv  *envtest.Environment
	clusterC    client.WithWatch
	clusterErr  error
)

func clusterClient(t *testing.T) client.WithWatch {
	t.Helper()
	clusterOnce.Do(startCluster)
	if clusterErr != nil {
		t.Fatal(clusterErr)
	}
	return clusterC
}

func startCluster() {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		clusterErr = errors.New("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return
	}
	crd, err := platformCRD(filepath.Join("..", "..", "addons", "ilm-operator.yaml"))
	if err != nil {
		clusterErr = err
		return
	}
	clusterEnv = &envtest.Environment{
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{crd},
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := clusterEnv.Start()
	if err != nil {
		clusterErr = err
		return
	}
	cfg.QPS, cfg.Burst = 1000, 1000
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, otilm.AddToScheme} {
		if clusterErr = add(scheme); clusterErr != nil {
			return
		}
	}
	if clusterC, clusterErr = client.NewWithWatch(cfg, client.Options{Scheme: scheme}); clusterErr != nil {
		return
	}
	for _, ns := range []string{kube.SystemNamespace, platform.Namespace, platform.OperatorNamespace} {
		if clusterErr = clusterC.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); clusterErr != nil {
			return
		}
	}
}

func stopCluster() {
	if clusterEnv != nil {
		_ = clusterEnv.Stop()
	}
}

// platformCRD is the Platform CRD of the operator manifest.
func platformCRD(manifest string) (*apiextensionsv1.CustomResourceDefinition, error) {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := decoder.Decode(crd); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no Platform CRD in the operator manifest")
			}
			return nil, err
		}
		if crd.Kind == "CustomResourceDefinition" && crd.Spec.Names.Kind == "Platform" {
			return crd, nil
		}
	}
}

const (
	fromBundle      = "1.0.0"
	toBundle        = "1.1.0"
	oldILM          = "2.19.0"
	newILM          = "2.20.0"
	shippedOperator = "1.0.0"
	newerOperator   = "1.1.0"
	ilmHost         = "ilm.example.com"
)

// fakeBackups is the backup runner's admission and its update backups.
type fakeBackups struct {
	fakeLock
	mu     sync.Mutex
	runs   [][]string
	runErr error
	unpins []string
	// noDatabase makes the backups leave the database out, as while an external database is unreachable.
	noDatabase bool
	// entered, when set, is told when a backup starts; the backup then waits for block.
	entered chan struct{}
	block   chan struct{}
	// local are the local backups, in dir.
	local []v1alpha1.BackupRecord
	dir   string
}

func (b *fakeBackups) Local() ([]v1alpha1.BackupRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.local), nil
}

func (b *fakeBackups) Open(name string) (*os.File, int64, error) {
	path := filepath.Join(b.dir, name)
	if err := os.WriteFile(path, []byte("backup"), 0o600); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path) //nolint:gosec // a test file
	return f, 6, err
}

func (b *fakeBackups) Run(_ context.Context, reason string, targets []string, _ ...backup.RunOption) (v1alpha1.BackupRecord, error) {
	b.mu.Lock()
	b.runs = append(b.runs, append([]string{reason}, targets...))
	b.mu.Unlock()
	if b.entered != nil {
		b.entered <- struct{}{}
		<-b.block
	}
	database := v1alpha1.BackupDatabaseIncluded
	if b.noDatabase {
		database = v1alpha1.BackupDatabaseNotIncluded
	}
	return v1alpha1.BackupRecord{Name: "pre-update", Database: database}, b.runErr
}

func (*fakeBackups) Busy() bool { return false }

func (b *fakeBackups) Unpin(kind, keep string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unpins = append(b.unpins, kind+"/"+keep)
	return nil
}

func (b *fakeBackups) counts() (runs, unpins int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.runs), len(b.unpins)
}

// fakeTagger is zot.
type fakeTagger struct {
	mu     sync.Mutex
	tagged [][]bundle.ImageRef
	err    error
}

func (f *fakeTagger) TagImages(_ context.Context, images []bundle.ImageRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagged = append(f.tagged, images)
	return f.err
}

func (f *fakeTagger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tagged)
}

// fakeFetcher fetches the target bundle online, writing its files as a load does, unless err is set.
type fakeFetcher struct {
	e     *jobEnv
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeFetcher) Fetch(_ context.Context, kind, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	if kind != release.BundlePlatform || version != toBundle {
		return errors.New("no such bundle online")
	}
	f.e.storeBundle()
	return nil
}

// jobEnv is an appliance on the API server running platform 1.0.0 with ILM 2.19.0, with platform bundle 1.1.0 of
// ILM 2.20.0 loaded. A fake reconciler and operator play the cluster while the test runs.
type jobEnv struct {
	t        *testing.T
	c        client.WithWatch
	backups  *fakeBackups
	tagger   *fakeTagger
	fetcher  *fakeFetcher
	audit    *fakeAudit
	logs     *syncBuffer
	dir      string
	operator string
	noSpace  bool
	// wait bounds the wait for ILM to run the new version.
	wait time.Duration
	// jobClient, when set, is the client of the jobs instead of c.
	jobClient client.Client
	// version is the manager's own version, fromBundle when empty; talos, applier and console are the Talos API, the
	// applier of the manager's manifest and the console check of a system update, which gives up its health check
	// after health.
	version string
	talos   update.TalosNode
	// talosErr fails the access to the Talos API.
	talosErr error
	applier  update.Applier
	console  func(context.Context) error
	health   time.Duration
	// rebootRetry is how long the node may keep its boot before it is rebooted again; the default when zero.
	rebootRetry time.Duration
	// upstreams are the registries the registry mirror serves.
	upstreams []string
	// addonSteps are the add-on steps of the running release; addonLock is the add-on pass's lock.
	addonSteps []string
	addonLock  sync.Locker
	// releaseILM is the ILM version the running release embeds, oldILM when empty.
	releaseILM string

	mu sync.Mutex
	// applyOperator lets the add-on pass apply the bundle's operator; reject makes the operator refuse ILM 2.20.0.
	applyOperator bool
	reject        bool
	// stall keeps ILM from running the new version.
	stall bool
}

// newJobEnv prepares the appliance with a loaded bundle whose ILM operator is operator.
func newJobEnv(t *testing.T, operator string) *jobEnv {
	t.Helper()
	e := &jobEnv{
		t: t, c: clusterClient(t), backups: &fakeBackups{dir: t.TempDir()}, tagger: &fakeTagger{}, audit: &fakeAudit{},
		logs: &syncBuffer{}, dir: t.TempDir(), operator: operator, applyOperator: true, wait: 10 * time.Second,
	}
	e.fetcher = &fakeFetcher{e: e}
	e.resetCluster()
	e.createAppliance()
	e.createPlatform()
	e.createOperator()
	e.storeBundle()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { e.play(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return e
}

func (e *jobEnv) resetCluster() {
	e.t.Helper()
	ctx := e.t.Context()
	err := e.c.Delete(ctx, &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}})
	if err != nil && !apierrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	for _, del := range []func() error{
		func() error { return e.c.DeleteAllOf(ctx, &otilm.Platform{}, client.InNamespace(platform.Namespace)) },
		func() error {
			return e.c.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace(platform.OperatorNamespace))
		},
		func() error { return e.c.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(kube.SystemNamespace)) },
	} {
		if err := del(); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *jobEnv) createAppliance() {
	e.t.Helper()
	ctx := e.t.Context()
	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec: v1alpha1.ApplianceSpec{
			Node:     v1alpha1.NodeSpec{HostName: "appliance.example.com"},
			Platform: &v1alpha1.PlatformSpec{ILMVersion: oldILM, HostName: ilmHost},
			Backup: &v1alpha1.BackupSpec{Targets: []v1alpha1.BackupTarget{{
				Name: "nas", Type: "nfs", Retention: 3, NFS: &v1alpha1.NFSTarget{Server: "nas.example.com", Export: "/backups"},
			}}},
		},
	}
	if err := e.c.Create(ctx, a); err != nil {
		e.t.Fatal(err)
	}
	a.Status = v1alpha1.ApplianceStatus{
		Versions: v1alpha1.Versions{Appliance: fromBundle, Operator: shippedOperator, ILM: oldILM},
		Updates: &v1alpha1.UpdatesStatus{Loaded: []v1alpha1.BundleInfo{{
			Kind: release.BundlePlatform, Version: toBundle, Arch: testArch, ILM: newILM, Operator: e.operator,
			MinAppliance: "1.0.0", SizeBytes: 1000,
		}}},
	}
	e.setCondition(a, v1alpha1.ConditionConnectorsReady)
	if err := e.c.Status().Update(ctx, a); err != nil {
		e.t.Fatal(err)
	}
}

func (e *jobEnv) setCondition(a *v1alpha1.Appliance, kind string) {
	meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
		Type: kind, Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready", ObservedGeneration: a.Generation,
	})
}

func (e *jobEnv) createPlatform() {
	e.t.Helper()
	ctx := e.t.Context()
	p, _, err := platform.Render(e.appliance(), &release.Manifest{
		Platform: release.PlatformDefaults{ILMVersion: oldILM, HostName: ilmHost},
	}, platform.SecretNames{}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Create(ctx, p); err != nil {
		e.t.Fatal(err)
	}
	runVersion(p, oldILM)
	if err := e.c.Status().Update(ctx, p); err != nil {
		e.t.Fatal(err)
	}
}

// runVersion makes the operator report p running version.
func runVersion(p *otilm.Platform, version string) {
	p.Status.ObservedGeneration, p.Status.ObservedVersion = p.Generation, version
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: "Available", Status: metav1.ConditionTrue, Reason: "AllComponentsReady", ObservedGeneration: p.Generation,
	})
}

func (e *jobEnv) createOperator() {
	e.t.Helper()
	ctx := e.t.Context()
	labels := map[string]string{"app": "ilm-operator"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.OperatorNamespace, Name: platform.OperatorDeployment},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "ilm-operator:" + shippedOperator}}},
			},
		},
	}
	if err := e.c.Create(ctx, d); err != nil {
		e.t.Fatal(err)
	}
	e.rollOut(true)
}

// rollOut reports the operator's Deployment rolled out, or rolling out.
func (e *jobEnv) rollOut(done bool) {
	e.t.Helper()
	d := &appsv1.Deployment{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: platform.OperatorNamespace, Name: platform.OperatorDeployment}, d); err != nil {
		e.t.Fatal(err)
	}
	var updated int32
	if done {
		updated = 1
	}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: updated, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := e.c.Status().Update(e.t.Context(), d); err != nil {
		e.t.Fatal(err)
	}
}

// storeBundle writes the metadata of the loaded bundle, as a load stores it.
func (e *jobEnv) storeBundle() {
	md := bundle.Metadata{
		Format: 1, Kind: release.BundlePlatform, Version: toBundle, Arch: testArch, Operator: e.operator, ILM: newILM,
		MinAppliance: "1.0.0",
		Images: []bundle.ImageRef{
			{Ref: sourceHost + "/platform/tool:" + toBundle, Digest: "sha256:" + sha("tool"), Size: 600},
			{Ref: sourceHost + "/platform/shared:1.0", Digest: "sha256:" + sha("shared"), Size: 400},
		},
		Files: []string{bundle.OperatorFile},
	}
	data, err := json.Marshal(md)
	dir := filepath.Join(e.dir, release.BundlePlatform, toBundle+"-"+testArch)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, bundle.MetadataFile), data, 0o600)
	}
	if err != nil {
		e.t.Error(err)
	}
}

func (e *jobEnv) newJobs() *update.Jobs {
	return e.newJobsWith(e.backups)
}

func (e *jobEnv) newJobsWith(backups update.JobBackups) *update.Jobs {
	return e.newJobsWithRestorer(backups, nil)
}

func (e *jobEnv) newJobsWithRestorer(backups update.JobBackups, restorer update.Restorer) *update.Jobs {
	return update.NewJobs(update.JobDeps{
		Restore:  restorer,
		Client:   cmp.Or[client.Client](e.jobClient, e.c),
		Backups:  backups,
		Registry: func(context.Context) (update.ImageTagger, error) { return e.tagger, nil },
		Fetcher:  e.fetcher,
		Audit:    e.audit,
		Log:      slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Release: &release.Manifest{
			Version:       fromBundle,
			Platform:      release.PlatformDefaults{ILMVersion: cmp.Or(e.releaseILM, oldILM)},
			Compatibility: release.Compatibility{Operator: ">=1.0.0 <2.0.0", MinAppliance: "1.0.0"},
			Addons:        []release.AddonFile{{Name: "ilm-operator", File: "ilm-operator.yaml", Version: shippedOperator}},
		},
		Dir:              e.dir,
		Arch:             testArch,
		Space:            e.space,
		Poll:             5 * time.Millisecond,
		OperatorTimeout:  10 * time.Second,
		WaitTimeout:      e.wait,
		Version:          cmp.Or(e.version, fromBundle),
		Talos:            func(context.Context) (update.TalosNode, error) { return e.talos, e.talosErr },
		Manifests:        e.applier,
		Console:          e.console,
		HealthTimeout:    e.health,
		ComponentTimeout: 10 * time.Second,
		RebootRetry:      e.rebootRetry,
		Upstreams:        e.upstreams,
		AddonSteps:       e.addonSteps,
		AddonLock:        e.addonLock,
	})
}

func (e *jobEnv) space(string, uint64) error {
	if e.noSpace {
		return storage.ErrNoSpace
	}
	return nil
}

func (e *jobEnv) set(change func(e *jobEnv)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	change(e)
}

func (e *jobEnv) behaviour() (applyOperator, reject, stall bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyOperator, e.reject, e.stall
}

// play plays the reconciler and the operator until ctx ends.
func (e *jobEnv) play(ctx context.Context) {
	for ctx.Err() == nil {
		e.playOnce(ctx)
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *jobEnv) playOnce(ctx context.Context) {
	a := &v1alpha1.Appliance{}
	p := &otilm.Platform{}
	if e.c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a) != nil ||
		e.c.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p) != nil {
		return
	}
	applyOperator, reject, stall := e.behaviour()
	if applyOperator && a.Status.Versions.PlatformBundle == toBundle && a.Status.Versions.Operator != e.operator {
		a.Status.Versions.Operator = e.operator
		_ = e.c.Status().Update(ctx, a)
		return
	}
	if a.Spec.Platform != nil && p.Spec.Version != a.Spec.Platform.ILMVersion {
		p.Spec.Version = a.Spec.Platform.ILMVersion
		_ = e.c.Update(ctx, p)
		return
	}
	e.operate(ctx, a, p, reject, stall)
}

// operate plays the operator moving the Platform to its version, and the reconciler reporting the connectors.
func (e *jobEnv) operate(ctx context.Context, a *v1alpha1.Appliance, p *otilm.Platform, reject, stall bool) {
	switch {
	case p.Spec.Version != newILM || p.Status.ObservedVersion == newILM && p.Status.ObservedGeneration == p.Generation:
	case reject:
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: "Degraded", Status: metav1.ConditionTrue, Reason: "UnsupportedVersion", ObservedGeneration: p.Generation,
		})
		p.Status.ObservedGeneration = p.Generation
		_ = e.c.Status().Update(ctx, p)
		return
	case !stall:
		runVersion(p, newILM)
		_ = e.c.Status().Update(ctx, p)
		return
	}
	c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionConnectorsReady)
	if c != nil && c.ObservedGeneration != a.Generation {
		e.setCondition(a, v1alpha1.ConditionConnectorsReady)
		_ = e.c.Status().Update(ctx, a)
	}
}

func (e *jobEnv) appliance() *v1alpha1.Appliance {
	e.t.Helper()
	a := &v1alpha1.Appliance{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *jobEnv) updateStatus(change func(st *v1alpha1.ApplianceStatus)) {
	e.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := e.appliance()
		change(&a.Status)
		return e.c.Status().Update(e.t.Context(), a)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// waitFor waits until the update status satisfies done.
func (e *jobEnv) waitFor(what string, done func(u *v1alpha1.UpdateStatus) bool) *v1alpha1.UpdateStatus {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		u := e.appliance().Status.Update
		if u != nil && done(u) {
			return u
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the update did not reach %s: %+v", what, u)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitEnded waits until the update is done or failed and jobs has released the admission.
func (e *jobEnv) waitEnded(jobs *update.Jobs) *v1alpha1.UpdateStatus {
	e.t.Helper()
	u := e.waitFor("its end", func(u *v1alpha1.UpdateStatus) bool { return u.Step == update.StepDone || u.Message != "" })
	deadline := time.Now().Add(5 * time.Second)
	for jobs.Running() {
		if time.Now().After(deadline) {
			e.t.Fatal("the job still runs after its end was recorded")
		}
		time.Sleep(time.Millisecond)
	}
	return u
}

func (e *jobEnv) start(jobs *update.Jobs) {
	e.t.Helper()
	if err := jobs.StartPlatform(e.t.Context(), toBundle, "admin"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *jobEnv) wantHoldFree() {
	e.t.Helper()
	release, err := e.backups.Hold()
	if err != nil {
		e.t.Fatalf("the admission is still held: %v", err)
	}
	release()
}

func (e *jobEnv) auditActions() []string {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	var out []string
	for _, entry := range e.audit.entries {
		out = append(out, entry.Action+":"+entry.Result+":"+entry.Details["step"]+":"+entry.Details["reason"])
	}
	return out
}

func TestPlatformUpdateRunsEveryStep(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	jobs := e.newJobs()
	e.start(jobs)
	u := e.waitEnded(jobs)

	if u.Step != update.StepDone || u.Message != "" || u.Kind != release.BundlePlatform || u.From != fromBundle ||
		u.To != toBundle || u.Backup != "pre-update" || !u.PointOfNoReturn || u.StartedAt.IsZero() {
		t.Errorf("update status %+v", u)
	}
	a := e.appliance()
	if a.Spec.Platform.ILMVersion != newILM || a.Status.Versions.PlatformBundle != toBundle {
		t.Errorf("ILM version %s, platform bundle %s", a.Spec.Platform.ILMVersion, a.Status.Versions.PlatformBundle)
	}
	if !slices.ContainsFunc(a.ManagedFields, func(f metav1.ManagedFieldsEntry) bool {
		return f.Manager == kube.ManagerFieldManager && f.Operation == metav1.ManagedFieldsOperationUpdate && f.Subresource == ""
	}) {
		t.Error("the ILM version was not written by the manager's field manager")
	}
	if runs := e.backups.runs; len(runs) != 1 || !slices.Equal(runs[0], []string{backup.RunUpdate, "nas"}) {
		t.Errorf("backups %v, want one update backup to every target", runs)
	}
	if runs, unpins := e.backups.counts(); runs != 1 || unpins != 1 || e.backups.unpins[0] != release.BundlePlatform+"/pre-update" {
		t.Errorf("%d backups, unpins %v", runs, e.backups.unpins)
	}
	if e.tagger.calls() != 1 || len(e.tagger.tagged[0]) != 2 || e.fetcher.calls != 0 {
		t.Errorf("tagged %v, fetched %d times", e.tagger.tagged, e.fetcher.calls)
	}
	e.wantHoldFree()
	want := []string{update.AuditUpdateStart + ":success::", update.AuditUpdate + ":success::"}
	if got := e.auditActions(); !slices.Equal(got, want) || e.audit.entries[0].User != "admin" {
		t.Errorf("audit %v, want %v", got, want)
	}
}

// TestUpdateNeverDowngradesTheOperator refuses a bundle with a newer ILM but an older operator than the one an
// earlier update installed, which the add-on pass would otherwise replace with the embedded one.
func TestUpdateNeverDowngradesTheOperator(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone || e.appliance().Status.Versions.Operator != newerOperator {
		t.Fatalf("first update %+v", u)
	}
	const next = "1.2.0"
	e.storeBundleAt(next, "2.21.0", shippedOperator)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Updates.Loaded = append(st.Updates.Loaded, v1alpha1.BundleInfo{
			Kind: release.BundlePlatform, Version: next, Arch: testArch, ILM: "2.21.0", Operator: shippedOperator,
			MinAppliance: "1.0.0", SizeBytes: 1000,
		})
	})

	err := jobs.StartPlatform(t.Context(), next, "admin")
	if update.Reason(err) != update.OperatorOlderThanRunning {
		t.Fatalf("start of a bundle with an older operator: %v, want %s", err, update.OperatorOlderThanRunning)
	}
	if a := e.appliance(); a.Status.Versions.PlatformBundle != toBundle || a.Status.Update.To != toBundle {
		t.Errorf("platform bundle %s, update %+v after the refusal", a.Status.Versions.PlatformBundle, a.Status.Update)
	}
	e.wantHoldFree()
}

// TestUpdateRefusesAnOlderOperatorWithoutAReportedOne judges the operator against the installed platform bundle when
// the status reports none.
func TestUpdateRefusesAnOlderOperatorWithoutAReportedOne(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Fatalf("first update %+v", u)
	}
	const next = "1.2.0"
	e.storeBundleAt(next, "2.21.0", shippedOperator)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Versions.Operator = ""
		st.Updates.Loaded = append(st.Updates.Loaded, v1alpha1.BundleInfo{
			Kind: release.BundlePlatform, Version: next, Arch: testArch, ILM: "2.21.0", Operator: shippedOperator,
			MinAppliance: "1.0.0", SizeBytes: 1000,
		})
	})

	if err := jobs.StartPlatform(t.Context(), next, "admin"); update.Reason(err) != update.OperatorOlderThanRunning {
		t.Fatalf("start of a bundle with an older operator: %v, want %s", err, update.OperatorOlderThanRunning)
	}
	e.wantHoldFree()
}

func TestPlatformUpdateFetchesABundleNotLoaded(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	if err := os.RemoveAll(filepath.Join(e.dir, release.BundlePlatform)); err != nil {
		t.Fatal(err)
	}
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone || e.fetcher.calls != 1 || e.tagger.calls() != 1 {
		t.Errorf("update %+v after %d fetches", u, e.fetcher.calls)
	}
}

func TestPlatformUpdateFailsWithoutItsImages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(e *jobEnv)
		reason string
	}{
		{"not loaded nor online", func(e *jobEnv) {
			_ = os.RemoveAll(filepath.Join(e.dir, release.BundlePlatform))
			e.fetcher.err = &update.Error{Reason: update.NotFound}
		}, update.BundleUnavailable},
		{"missing in zot", func(e *jobEnv) { e.tagger.err = errors.New("manifest unknown") }, update.ImagesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newJobEnv(t, newerOperator)
			tc.setup(e)
			jobs := e.newJobs()
			e.start(jobs)
			u := e.waitEnded(jobs)
			if u.Step != update.StepImages || u.Message != tc.reason {
				t.Errorf("update %+v, want failed at images with %s", u, tc.reason)
			}
			if a := e.appliance(); a.Spec.Platform.ILMVersion != oldILM || a.Status.Versions.PlatformBundle != "" {
				t.Error("the update went on past the images")
			}
			e.wantHoldFree()
		})
	}
}

func TestPlatformUpdateAbortsOnBackupFailure(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.backups.runErr = backup.ErrFailed
	jobs := e.newJobs()
	e.start(jobs)
	u := e.waitEnded(jobs)

	if u.Step != update.StepBackup || u.Message != update.BackupFailed || u.Backup != "" {
		t.Errorf("update %+v, want failed at the backup", u)
	}
	if a := e.appliance(); a.Spec.Platform.ILMVersion != oldILM || a.Status.Versions.PlatformBundle != "" {
		t.Error("the update went on after the backup failed")
	}
	if _, unpins := e.backups.counts(); e.tagger.calls() != 0 || unpins != 0 {
		t.Errorf("tagged %d times, unpinned %d times after the failed backup", e.tagger.calls(), unpins)
	}
	e.wantHoldFree()
	want := []string{
		update.AuditUpdateStart + ":success::",
		update.AuditUpdate + ":failure:" + update.StepBackup + ":" + update.BackupFailed,
	}
	if got := e.auditActions(); !slices.Equal(got, want) {
		t.Errorf("audit %v, want %v", got, want)
	}
}

func TestPlatformUpdateAbortsOnBackupWithoutDatabase(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.backups.noDatabase = true
	jobs := e.newJobs()
	e.start(jobs)
	u := e.waitEnded(jobs)

	if u.Step != update.StepBackup || u.Message != update.BackupIncomplete || u.Backup != "" {
		t.Errorf("update %+v, want failed at the backup as incomplete", u)
	}
	if a := e.appliance(); a.Spec.Platform.ILMVersion != oldILM || a.Status.Versions.PlatformBundle != "" {
		t.Error("the update went on after a backup without the database")
	}
	if e.tagger.calls() != 0 {
		t.Errorf("tagged %d times after a backup without the database", e.tagger.calls())
	}
	e.wantHoldFree()
}

// TestPlatformUpdateResumesAtEveryStep restarts the manager at each step: a new Jobs resumes the update at the step
// the status records and does none of the steps before it again.
func TestPlatformUpdateResumesAtEveryStep(t *testing.T) {
	steps := []string{update.StepBackup, update.StepImages, update.StepOperator, update.StepVersion, update.StepWait}
	for i, step := range steps {
		t.Run(step, func(t *testing.T) {
			e := newJobEnv(t, newerOperator)
			e.crashedAt(step, i)
			jobs := e.newJobs()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			u := e.waitEnded(jobs)
			if u.Step != update.StepDone || u.Message != "" || u.Backup != "pre-update" {
				t.Errorf("update %+v", u)
			}
			runs, unpins := e.backups.counts()
			wantRuns, wantTags := 0, 0
			if i == 0 {
				wantRuns = 1
			}
			if i <= 1 {
				wantTags = 1
			}
			if runs != wantRuns || e.tagger.calls() != wantTags || unpins != 1 {
				t.Errorf("%d backups, %d taggings, %d unpins; want %d, %d, 1", runs, e.tagger.calls(), unpins, wantRuns, wantTags)
			}
			if a := e.appliance(); a.Spec.Platform.ILMVersion != newILM || a.Status.Versions.PlatformBundle != toBundle {
				t.Error("the resumed update did not finish its steps")
			}
			if got := e.auditActions(); !slices.Equal(got, []string{update.AuditUpdate + ":success::"}) {
				t.Errorf("audit %v", got)
			}
			e.wantHoldFree()
		})
	}
}

// crashedAt leaves the status as a manager stopped at step, the i-th, leaves it.
func (e *jobEnv) crashedAt(step string, i int) {
	e.t.Helper()
	if i >= 3 {
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Versions.PlatformBundle = toBundle })
	}
	if i >= 4 {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			a := e.appliance()
			a.Spec.Platform.ILMVersion = newILM
			return e.c.Update(e.t.Context(), a)
		})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update = &v1alpha1.UpdateStatus{
			Kind: release.BundlePlatform, From: fromBundle, To: toBundle, Step: step, StartedAt: metav1.Now(),
			PointOfNoReturn: i >= 4,
		}
		if i > 0 {
			st.Update.Backup = "pre-update"
		}
	})
}

func TestResumeLeavesEndedUpdates(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); err != nil || jobs.Running() {
		t.Fatalf("Resume without an update: %v, running %v", err, jobs.Running())
	}
	for _, u := range []v1alpha1.UpdateStatus{
		{Kind: release.BundlePlatform, From: fromBundle, To: toBundle, Step: update.StepDone, StartedAt: metav1.Now()},
		{Kind: release.BundlePlatform, From: fromBundle, To: toBundle, Step: update.StepWait, Message: update.Timeout, StartedAt: metav1.Now()},
	} {
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update = &u })
		if err := jobs.Resume(t.Context()); err != nil || jobs.Running() {
			t.Errorf("Resume of %+v: %v, running %v", u, err, jobs.Running())
		}
	}
	e.wantHoldFree()
	if runs, _ := e.backups.counts(); runs != 0 {
		t.Error("an ended update ran again")
	}
}

// TestResumeRefusedWhileARestoreIsUnsettled leaves an unfinished update alone while a restore that a restart cut off
// has not ended: the two would interleave.
func TestResumeRefusedWhileARestoreIsUnsettled(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.crashedAt(update.StepWait, 4)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{
			Backup: "b", InPlace: true, Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepDatabase,
			StartedAt: metav1.Now(),
		}
	})
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); update.Reason(err) != update.RestoreRunning {
		t.Fatalf("Resume while a restore is unsettled: %v, want %s", err, update.RestoreRunning)
	}
	time.Sleep(20 * time.Millisecond)
	if u := e.appliance().Status.Update; u.Step != update.StepWait || u.Message != "" {
		t.Errorf("update %+v, want it left at its recorded step", u)
	}
	e.wantHoldFree()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := e.appliance()
		a.Status.Restore.Step = v1alpha1.RestoreStepAbandoned
		return e.c.Status().Update(t.Context(), a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
}

func TestResumeWaitsForTheAdmission(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.crashedAt(update.StepWait, 4)
	release, err := e.backups.Hold()
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); err == nil {
		t.Fatal("Resume while a backup holds the admission: want an error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- jobs.KeepResuming(ctx) }()
	time.Sleep(20 * time.Millisecond)
	release()
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
	cancel()
	if err := <-ended; err != nil {
		t.Error(err)
	}
}

// TestOperatorAppliedOnlyWhenNewer records the bundle for the add-on pass and waits for the operator it applies
// only when the bundle's operator is newer than the embedded one.
func TestOperatorAppliedOnlyWhenNewer(t *testing.T) {
	t.Run("newer", func(t *testing.T) {
		e := newJobEnv(t, newerOperator)
		e.set(func(e *jobEnv) { e.applyOperator = false })
		jobs := e.newJobs()
		e.start(jobs)
		e.waitFor("the operator step", func(u *v1alpha1.UpdateStatus) bool { return u.Step == update.StepOperator })
		deadline := time.Now().Add(5 * time.Second)
		for e.appliance().Status.Versions.PlatformBundle != toBundle {
			if time.Now().After(deadline) {
				t.Fatal("the platform bundle was not recorded for the add-on pass")
			}
			time.Sleep(time.Millisecond)
		}
		e.rollOut(false)
		e.set(func(e *jobEnv) { e.applyOperator = true })
		time.Sleep(50 * time.Millisecond)
		if u := e.appliance().Status.Update; u.Step != update.StepOperator || e.appliance().Spec.Platform.ILMVersion != oldILM {
			t.Fatalf("the update went past the operator before its Deployment rolled out: %+v", u)
		}
		e.rollOut(true)
		if u := e.waitEnded(jobs); u.Step != update.StepDone || e.appliance().Status.Versions.Operator != newerOperator {
			t.Errorf("update %+v, operator %s", u, e.appliance().Status.Versions.Operator)
		}
	})
	t.Run("not newer", func(t *testing.T) {
		e := newJobEnv(t, shippedOperator)
		e.set(func(e *jobEnv) { e.applyOperator = false })
		e.rollOut(false)
		jobs := e.newJobs()
		e.start(jobs)
		u := e.waitEnded(jobs)
		a := e.appliance()
		if u.Step != update.StepDone || a.Status.Versions.Operator != shippedOperator || a.Status.Versions.PlatformBundle != toBundle {
			t.Errorf("update %+v, operator %s, bundle %s", u, a.Status.Versions.Operator, a.Status.Versions.PlatformBundle)
		}
	})
}

func TestWaitTimeoutOffersRollback(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.set(func(e *jobEnv) { e.stall = true })
	e.wait = 100 * time.Millisecond
	jobs := e.newJobs()
	e.start(jobs)
	u := e.waitEnded(jobs)
	if u.Step != update.StepWait || u.Message != update.Timeout || !u.PointOfNoReturn || u.Backup != "pre-update" {
		t.Errorf("update %+v, want timed out waiting, with the backup to roll back to", u)
	}
	if _, unpins := e.backups.counts(); unpins != 0 {
		t.Error("the backup a rollback needs was unpinned")
	}
	e.wantHoldFree()
	if got := e.auditActions(); got[len(got)-1] != update.AuditUpdate+":failure:"+update.StepWait+":"+update.Timeout {
		t.Errorf("audit %v", got)
	}
}

func TestRejectedVersionFailsFast(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.set(func(e *jobEnv) { e.reject = true })
	jobs := e.newJobs()
	start := time.Now()
	e.start(jobs)
	u := e.waitEnded(jobs)
	if u.Step != update.StepWait || u.Message != update.VersionRejected || time.Since(start) > 5*time.Second {
		t.Errorf("update %+v after %v, want the rejected version at once", u, time.Since(start))
	}
}

func TestUpdateAfterAnAbandonedRestore(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepAbandoned, Message: "failed", StartedAt: metav1.Now()}
	})
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Errorf("update %+v after an abandoned restore", u)
	}
}

// busyBackups is the admission while a backup runs.
type busyBackups struct{ fakeBackups }

func (*busyBackups) Hold() (func(), error) { return nil, backup.ErrBusy }
func (*busyBackups) Busy() bool            { return true }

// admittingBackups is the admission while it admits another operation, such as a restore.
type admittingBackups struct{ fakeBackups }

func (*admittingBackups) Hold() (func(), error) { return nil, backup.ErrBusy }

// runnerBackups is the backup runner, with update backups that wait for block.
type runnerBackups struct {
	*backup.Runner
	fake *fakeBackups
}

func (r runnerBackups) Run(ctx context.Context, reason string, targets []string, opts ...backup.RunOption) (v1alpha1.BackupRecord, error) {
	return r.fake.Run(ctx, reason, targets, opts...)
}

func (r runnerBackups) Unpin(kind, keep string) error { return r.fake.Unpin(kind, keep) }

// TestUpdateAdmission refuses an update while another long operation runs, and every other long operation while
// an update runs; a scheduled backup waits for the update to end.
func TestUpdateAdmission(t *testing.T) {
	e := newSystemEnv(t).jobEnv
	runner := backup.NewRunner(backup.RunnerDeps{
		Client: e.c, Reader: e.c, Audit: e.audit, Dir: t.TempDir(), Version: fromBundle,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)),
	})
	backups := runnerBackups{Runner: runner, fake: e.backups}
	jobs := e.newJobsWith(backups)
	refused := func(what, reason string) {
		t.Helper()
		err := jobs.StartPlatform(t.Context(), toBundle, "admin")
		if update.Reason(err) != reason || err.Error() == "" {
			t.Errorf("start while %s: %v, want %s", what, err, reason)
		}
		if err := jobs.StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != reason {
			t.Errorf("system update while %s: %v, want %s", what, err, reason)
		}
	}

	release, err := runner.Hold()
	if err != nil {
		t.Fatal(err)
	}
	refused("a bundle loads", update.BundleLoading)
	release()
	if err := e.newJobsWith(&busyBackups{}).StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.BackupRunning {
		t.Errorf("start while a backup runs: %v", err)
	}
	if err := e.newJobsWith(&busyBackups{}).StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != update.BackupRunning {
		t.Errorf("system update while a backup runs: %v", err)
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now()}
	})
	refused("a restore runs", update.RestoreRunning)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Restore.Message = "failed" })
	refused("a failed restore is neither resumed nor abandoned", update.RestoreRunning)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Restore = nil })
	e.upgrading(true)
	refused("the operator upgrades the platform", update.PlatformUpgrading)
	e.upgrading(false)
	e.noSpace = true
	refused("the data volume is full", update.NoSpace)
	e.noSpace = false
	if err := e.newJobsWith(&admittingBackups{}).StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.Busy {
		t.Errorf("start while another operation is admitted: %v", err)
	}
	if err := e.newJobsWith(&admittingBackups{}).StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != update.Busy {
		t.Errorf("system update while another operation is admitted: %v", err)
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Updates.Loaded[0].SizeBytes = 0 })
	if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.InvalidMetadata {
		t.Errorf("start while the bundle's size is unknown: %v", err)
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Updates.Loaded[0].SizeBytes = 1000 })
	if err := jobs.StartPlatform(t.Context(), "9.9.9", "admin"); update.Reason(err) != update.NotFound {
		t.Errorf("start of an unknown bundle: %v", err)
	}
	if runs, _ := e.backups.counts(); runs != 0 || e.appliance().Status.Update != nil {
		t.Fatal("a refused update started")
	}

	e.backups.entered, e.backups.block = make(chan struct{}), make(chan struct{})
	e.start(jobs)
	<-e.backups.entered
	refused("an update runs", update.UpdateRunning)
	if err := e.newJobsWith(backups).StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("start from another Jobs while an update runs: %v", err)
	}
	if err := e.newJobsWith(backups).StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("system update from another Jobs while an update runs: %v", err)
	}
	if _, err := runner.Hold(); !errors.Is(err, backup.ErrHeld) {
		t.Errorf("a bundle load while an update runs: %v", err)
	}
	if err := runner.Exclusive(func() error { return nil }); !errors.Is(err, backup.ErrHeld) {
		t.Errorf("a restore while an update runs: %v", err)
	}
	if _, err := runner.Run(t.Context(), backup.RunManual, nil); !errors.Is(err, backup.ErrHeld) {
		t.Errorf("a manual backup while an update runs: %v", err)
	}
	if err := runner.Start(t.Context(), backup.RunSchedule, []string{"nas"}); !errors.Is(err, backup.ErrHeld) {
		t.Errorf("a scheduled backup while an update runs: %v", err)
	}
	if scheduledBackups(e) != 0 {
		t.Fatal("the scheduled backup ran while the update runs")
	}
	close(e.backups.block)
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Fatalf("update %+v", u)
	}
	deadline := time.Now().Add(10 * time.Second)
	for scheduledBackups(e) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduled backup did not start once the update ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scheduledBackups counts the scheduled backups the audit log records, refused ones included.
func scheduledBackups(e *jobEnv) int {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	n := 0
	for _, entry := range e.audit.entries {
		if entry.Action == "backup" && entry.Details["reason"] == backup.RunSchedule {
			n++
		}
	}
	return n
}

// upgrading makes the operator report a messaging migration of the Platform, or none.
func (e *jobEnv) upgrading(on bool) {
	e.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p := &otilm.Platform{}
		if err := e.c.Get(e.t.Context(), client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
			return err
		}
		p.Status.Upgrade = nil
		if on {
			p.Status.Upgrade = &otilm.UpgradeStatus{FromVersion: oldILM, ToVersion: newILM, Phase: otilm.MigrationPhaseDraining,
				StartedAt: metav1.Now(), PhaseStartedAt: metav1.Now()}
		}
		return e.c.Status().Update(e.t.Context(), p)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// TestResumeAtTheBackupTakesItAgain takes the backup again when a restart cut the update off at the backup, even if
// a pinned backup of it was written: its copies to the targets may have failed.
func TestResumeAtTheBackupTakesItAgain(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.crashedAt(update.StepBackup, 0)
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	u := e.waitEnded(jobs)
	if runs, _ := e.backups.counts(); u.Step != update.StepDone || u.Backup != "pre-update" || runs != 1 {
		t.Errorf("update %+v after %d backups, want the backup taken again", u, runs)
	}
}

// unavailable is a transient failure of the API server.
var unavailable = apierrors.NewServiceUnavailable("the API server is unavailable")

// failingStatus fails with err the status writes that record step while fail allows.
func failingStatus(c client.WithWatch, step string, err error, fail func() bool) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if a, ok := obj.(*v1alpha1.Appliance); ok && a.Status.Update != nil && a.Status.Update.Step == step && fail() {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

func TestStatusWriteFailuresDoNotFailTheUpdate(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		e := newJobEnv(t, newerOperator)
		var mu sync.Mutex
		failures := 2
		e.jobClient = failingStatus(e.c, update.StepWait, unavailable, func() bool {
			mu.Lock()
			defer mu.Unlock()
			failures--
			return failures >= 0
		})
		jobs := e.newJobs()
		e.start(jobs)
		if u := e.waitEnded(jobs); u.Step != update.StepDone || u.Message != "" {
			t.Errorf("update %+v, want done after the failed writes", u)
		}
	})
	t.Run("lasting", func(t *testing.T) {
		e := newJobEnv(t, newerOperator)
		var mu sync.Mutex
		down := true
		e.jobClient = failingStatus(e.c, update.StepWait, unavailable, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return down
		})
		jobs := e.newJobs()
		e.start(jobs)
		e.waitFor("the version step", func(u *v1alpha1.UpdateStatus) bool { return u.Step == update.StepVersion })
		deadline := time.Now().Add(10 * time.Second)
		for jobs.Running() {
			if time.Now().After(deadline) {
				t.Fatal("the job did not stop on the lasting failure")
			}
			time.Sleep(time.Millisecond)
		}
		if u := e.appliance().Status.Update; u.Step != update.StepVersion || u.Message != "" {
			t.Errorf("update %+v, want it unfinished at its recorded step", u)
		}
		if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.UpdateRunning {
			t.Errorf("start while an update is unfinished: %v", err)
		}
		mu.Lock()
		down = false
		mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		ended := make(chan error, 1)
		go func() { ended <- jobs.KeepResuming(ctx) }()
		if u := e.waitEnded(jobs); u.Step != update.StepDone || u.Message != "" {
			t.Errorf("update %+v, want it resumed and done", u)
		}
		cancel()
		if err := <-ended; err != nil {
			t.Error(err)
		}
	})
}

func TestPermanentStatusWriteFailures(t *testing.T) {
	t.Run("at the start", func(t *testing.T) {
		e := newJobEnv(t, newerOperator)
		e.jobClient = failingStatus(e.c, update.StepBackup, apierrors.NewForbidden(v1alpha1.GroupVersion.WithResource("appliances").GroupResource(), v1alpha1.ApplianceName, errors.New("denied")), func() bool { return true })
		jobs := e.newJobs()
		start := time.Now()
		if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); err == nil || time.Since(start) > 2*time.Second {
			t.Fatalf("start with a forbidden status write: %v after %v, want an error at once", err, time.Since(start))
		}
		if jobs.Running() || e.appliance().Status.Update != nil || len(e.auditActions()) != 0 {
			t.Error("an update that could not be recorded started")
		}
		e.wantHoldFree()
	})
	t.Run("at a step", func(t *testing.T) {
		e := newJobEnv(t, newerOperator)
		e.jobClient = failingStatus(e.c, update.StepWait, apierrors.NewBadRequest("invalid"), func() bool { return true })
		jobs := e.newJobs()
		start := time.Now()
		e.start(jobs)
		u := e.waitEnded(jobs)
		if u.Step != update.StepVersion || u.Message != update.Failed || time.Since(start) > 5*time.Second {
			t.Errorf("update %+v after %v, want failed at once at the version step", u, time.Since(start))
		}
		want := update.AuditUpdate + ":failure:" + update.StepVersion + ":" + update.Failed
		if got := e.auditActions(); got[len(got)-1] != want {
			t.Errorf("audit %v", got)
		}
	})
}

// failingReads fails the reads of the Platform while fail allows.
func failingReads(c client.WithWatch, fail func() bool) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*otilm.Platform); ok && fail() {
				return unavailable
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

func TestTransientReadErrorsWhileWaiting(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	var mu sync.Mutex
	failures := 0
	e.jobClient = failingReads(e.c, func() bool {
		mu.Lock()
		defer mu.Unlock()
		a := &v1alpha1.Appliance{}
		if e.c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a) != nil ||
			a.Status.Update == nil || a.Status.Update.Step != update.StepWait || failures == 5 {
			return false
		}
		failures++
		return true
	})
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone || u.Message != "" || failures != 5 {
		t.Errorf("update %+v after %d failed reads, want done", u, failures)
	}
}

func TestShutdownLeavesTheUpdateToResume(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.set(func(e *jobEnv) { e.stall = true })
	jobs := e.newJobs()
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- jobs.KeepResuming(ctx) }()
	time.Sleep(20 * time.Millisecond)
	e.start(jobs)
	e.waitFor("the wait step", func(u *v1alpha1.UpdateStatus) bool { return u.Step == update.StepWait })
	cancel()
	if err := <-ended; err != nil {
		t.Error(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for jobs.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the job still runs after the shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	if u := e.appliance().Status.Update; u.Step != update.StepWait || u.Message != "" {
		t.Errorf("update %+v, want it unfinished at its recorded step", u)
	}
	if got := e.auditActions(); len(got) != 1 {
		t.Errorf("audit %v, want the start alone", got)
	}
}

// syncBuffer is a log buffer the managers of a system update, each with its own handler, write at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
