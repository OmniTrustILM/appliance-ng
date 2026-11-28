package update_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const (
	systemTo        = "1.1.0"
	oldKubernetes   = "v1.37.1"
	newKubernetes   = "v1.38.0"
	oldTalos        = "v1.14.5"
	newTalos        = "v1.15.0"
	builtRegistry   = "registry.example.com:5005"
	managerHolder   = "appliance.invalid/manager:release"
	nodeName        = "appliance-node"
	kubeSystem      = "kube-system"
	installerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	previousManager = builtRegistry + "/manager:1.0.0-" + testArch
	managerName     = "appliance-manager"
	etcdRef         = "registry.k8s.io/etcd:v3.8.0"
)

var (
	installerRef = builtRegistry + "/installer:" + systemTo + "-" + testArch
	managerRef   = builtRegistry + "/manager:" + systemTo + "-" + testArch
)

// staticPods are the control plane components Talos runs as static pods, by their k8s-app label.
var staticPods = map[string]string{
	"apiServer":         "kube-apiserver",
	"controllerManager": "kube-controller-manager",
	"scheduler":         "kube-scheduler",
}

// componentImages are the images of the Kubernetes components of version.
func componentImages(version string) map[string]string {
	return map[string]string{
		"apiServer":         "registry.k8s.io/kube-apiserver:" + version,
		"controllerManager": "registry.k8s.io/kube-controller-manager:" + version,
		"scheduler":         "registry.k8s.io/kube-scheduler:" + version,
		"kubelet":           "ghcr.io/siderolabs/kubelet:" + version,
	}
}

// fakeNode is the Talos API of the node: a staged upgrade reboots the node into the new Talos with the new release's
// first-boot extension and a rollback into the old one, each giving the Kubernetes node a new boot ID; a Kubernetes
// image moves its static pod or the kubelet to it.
type fakeNode struct {
	e *systemEnv

	mu    sync.Mutex
	calls []string
	boot  int
	kube  string
	// talos and extension are the Talos version and the first-boot extension's version the node runs.
	talos, extension string
	// stuck keeps the node from rebooting; oldTalosBoots boots the old Talos after an upgrade.
	stuck         bool
	oldTalosBoots bool
	upgradeErr    error
	rollbackErr   error
	// rollbackCutOff rolls back and reboots, but reports the call as cut off; rollbackBootsNew reboots into the new
	// Talos again.
	rollbackCutOff   bool
	rollbackBootsNew bool
	// installed is the new Talos installed as the next boot entry.
	installed bool
	// installTime is how long an install takes; deadlineAtInstall the deadline the status showed when it began.
	installTime       time.Duration
	deadlineAtInstall time.Time
	rebootErr         error
	// readErr fails the reads of the Talos and extension versions.
	readErr error
	// setErr fails the component it names.
	setErr map[string]error
	// pointOfNoReturn records, for each component change, whether the status had passed the point of no return.
	pointOfNoReturn []bool
}

func (f *fakeNode) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeNode) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeNode) count(prefix string) int {
	n := 0
	for _, c := range f.called() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeNode) Upgrade(_ context.Context, image string) error {
	f.record("upgrade " + image)
	if u := f.e.appliance().Status.Update; u != nil && u.Deadline != nil {
		f.mu.Lock()
		f.deadlineAtInstall = u.Deadline.Time
		f.mu.Unlock()
	}
	time.Sleep(f.installTime)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upgradeErr != nil {
		return f.upgradeErr
	}
	f.installed = true
	return nil
}

func (f *fakeNode) PullSystemImage(_ context.Context, image string) error {
	f.record("pull " + image)
	return nil
}

func (f *fakeNode) SetInstallerImage(_ context.Context, image string) error {
	f.record("installer " + image)
	return nil
}

// RebootPowercycle boots the installed Talos, or the old one when it does not boot.
func (f *fakeNode) RebootPowercycle(context.Context) error {
	f.record("reboot")
	f.mu.Lock()
	installed, old, err := f.installed, f.oldTalosBoots, f.rebootErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if installed && !old {
		f.runs(newTalos, systemTo)
	}
	f.reboot()
	return nil
}

// runs makes the node run Talos version talos with the first-boot extension of version extension.
func (f *fakeNode) runs(talos, extension string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.talos, f.extension = talos, extension
}

func (f *fakeNode) ExtensionVersion(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != talos.FirstbootExtension {
		return "", nil
	}
	return f.extension, f.readErr
}

func (f *fakeNode) Rollback(context.Context) error {
	f.record("rollback")
	f.mu.Lock()
	err, cutOff, bootsNew := f.rollbackErr, f.rollbackCutOff, f.rollbackBootsNew
	f.mu.Unlock()
	if err == nil || cutOff {
		if !bootsNew {
			f.runs(oldTalos, fromBundle)
		}
		f.reboot()
	}
	return err
}

// reboot gives the node a new boot ID, unless it is stuck.
func (f *fakeNode) reboot() {
	f.mu.Lock()
	if f.stuck {
		f.mu.Unlock()
		return
	}
	f.boot++
	boot := f.boot
	f.mu.Unlock()
	f.e.setNode(func(n *corev1.Node) { n.Status.NodeInfo.BootID = bootID(boot) })
}

func (f *fakeNode) Versions(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.talos, f.kube, f.readErr
}

func (f *fakeNode) SetKubernetesImages(ctx context.Context, component, image string) error {
	f.record("set " + component + " " + image)
	a := &v1alpha1.Appliance{}
	if err := f.e.c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return err
	}
	f.mu.Lock()
	f.pointOfNoReturn = append(f.pointOfNoReturn, a.Status.Update != nil && a.Status.Update.PointOfNoReturn)
	err := f.setErr[component]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.e.moveComponent(component, image)
	return nil
}

func bootID(n int) string { return "boot-" + string(rune('a'+n)) }

// fakeManifests is the Kubernetes API applying the manager's manifest, which runs the manager of the bundle.
type fakeManifests struct {
	e       *systemEnv
	mu      sync.Mutex
	applied [][]byte
}

func (f *fakeManifests) Apply(_ context.Context, docs []byte) error {
	f.mu.Lock()
	f.applied = append(f.applied, docs)
	f.mu.Unlock()
	f.e.runManager(managerRef)
	return nil
}

func (f *fakeManifests) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

// systemEnv is the jobEnv with system bundle 1.1.0 loaded, a node running Kubernetes 1.37.1 and its control plane.
// A fake reconciler reports the add-ons and the platform ready, holding the add-on pass while the update asks it to.
type systemEnv struct {
	*jobEnv
	node      *fakeNode
	manifests *fakeManifests
	// kubernetes is the Kubernetes version of the bundle.
	kubernetes string

	smu sync.Mutex
	// consoleDown keeps the console from answering; addonsDown keeps the add-ons from being ready; stallAddons keeps
	// the add-on pass the update releases from finishing.
	consoleDown bool
	addonsDown  bool
	stallAddons bool
	// newLock is the admission of the bundle's manager, another process than the manager of the running release.
	newLock *processBackups
	// old is the manager of the running release the test started.
	old *update.Jobs
}

func newSystemEnv(t *testing.T) *systemEnv {
	t.Helper()
	return newSystemEnvWith(t, newKubernetes)
}

// newSystemEnvWith prepares the system update to a bundle of Kubernetes kubernetes.
func newSystemEnvWith(t *testing.T, kubernetes string) *systemEnv {
	t.Helper()
	e := &systemEnv{jobEnv: newJobEnv(t, newerOperator), kubernetes: kubernetes}
	e.manifests = &fakeManifests{e: e}
	e.node = &fakeNode{e: e, kube: oldKubernetes, talos: oldTalos, extension: fromBundle}
	e.newLock = &processBackups{fakeBackups: e.backups}
	e.health = 10 * time.Second
	e.upstreams = []string{builtRegistry, "registry.k8s.io"}
	e.addonSteps = []string{"cilium", "cert-manager"}
	e.talos, e.applier, e.console = e.node, e.manifests, e.consoleCheck
	e.resetNode()
	e.storeSystemBundle()
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Versions.Kubernetes = oldKubernetes
		st.Updates.Loaded = append(st.Updates.Loaded, v1alpha1.BundleInfo{
			Kind: release.BundleSystem, Version: systemTo, Arch: testArch, OperatorRange: ">=1.0.0 <2.0.0", SizeBytes: 2000,
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { e.reconcile(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return e
}

func (e *systemEnv) consoleCheck(context.Context) error {
	e.smu.Lock()
	defer e.smu.Unlock()
	if e.consoleDown {
		return errors.New("connection refused")
	}
	return nil
}

func (e *systemEnv) setSystem(change func(e *systemEnv)) {
	e.smu.Lock()
	defer e.smu.Unlock()
	change(e)
}

// storeSystemBundle writes the files of the loaded system bundle, as a load stores them.
func (e *systemEnv) storeSystemBundle(without ...string) {
	e.t.Helper()
	images := []bundle.ImageRef{
		{Ref: sourceHost + "/system/tool:" + systemTo, Digest: "sha256:" + sha("system-tool"), Size: 1000},
	}
	for _, built := range []bundle.ImageRef{
		{Ref: installerRef, Digest: installerDigest, Size: 500},
		{Ref: managerRef, Digest: "sha256:" + sha("manager"), Size: 500},
		{Ref: etcdRef, Digest: "sha256:" + sha("etcd"), Size: 100},
	} {
		if !slices.Contains(without, built.Ref) {
			images = append(images, built)
		}
	}
	for _, c := range []string{"apiServer", "controllerManager", "scheduler", "kubelet"} {
		images = append(images, bundle.ImageRef{Ref: componentImages(e.kubernetes)[c], Digest: "sha256:" + sha(c), Size: 10})
	}
	md := bundle.Metadata{
		Format: 1, Kind: release.BundleSystem, Version: systemTo, Arch: testArch, Talos: newTalos, Kubernetes: e.kubernetes,
		OperatorRange: ">=1.0.0 <2.0.0", Images: images, Files: []string{"release.yaml", "manager.yaml"},
	}
	data, err := json.Marshal(md)
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.dir, release.BundleSystem, systemTo+"-"+testArch)
	files := map[string][]byte{
		bundle.MetadataFile: data,
		"release.yaml":      []byte("version: " + systemTo + "\nfutureField: kept\nmanagerImage: " + managerHolder + "\n"),
		"manager.yaml":      []byte("kind: DaemonSet\nspec:\n  image: " + managerHolder + "\n"),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
}

// resetNode leaves the one node of the cluster at its first boot, running Kubernetes 1.37.1 with its control plane
// pods ready.
func (e *systemEnv) resetNode() {
	e.t.Helper()
	ctx := e.t.Context()
	if err := e.c.DeleteAllOf(ctx, &corev1.Node{}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(kubeSystem), client.GracePeriodSeconds(0)); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.DeleteAllOf(ctx, &appsv1.DaemonSet{}, client.InNamespace(kube.SystemNamespace)); err != nil {
		e.t.Fatal(err)
	}
	labels := map[string]string{"app": managerName}
	manager := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: managerName},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: previousManager}}},
			},
		},
	}
	if err := e.c.Create(ctx, manager); err != nil {
		e.t.Fatal(err)
	}
	e.setNode(func(n *corev1.Node) {
		n.Status.NodeInfo = corev1.NodeSystemInfo{BootID: bootID(0), KubeletVersion: oldKubernetes}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	})
	for component, app := range staticPods {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: kubeSystem, Name: app + "-" + nodeName, Labels: map[string]string{"k8s-app": app}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: app, Image: componentImages(oldKubernetes)[component],
			}}},
		}
		if err := e.c.Create(ctx, pod); err != nil {
			e.t.Fatal(err)
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := e.c.Status().Update(ctx, pod); err != nil {
			e.t.Fatal(err)
		}
	}
}

// runManager sets the image of the manager's DaemonSet, as an apply or a first boot does.
func (e *systemEnv) runManager(image string) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d := &appsv1.DaemonSet{}
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: kube.SystemNamespace, Name: managerName}, d); err != nil {
			return err
		}
		d.Spec.Template.Spec.Containers[0].Image = image
		return e.c.Update(context.Background(), d)
	})
	if err != nil {
		e.t.Error(err)
	}
}

func (e *systemEnv) managerImage() string {
	e.t.Helper()
	d := &appsv1.DaemonSet{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: kube.SystemNamespace, Name: managerName}, d); err != nil {
		e.t.Fatal(err)
	}
	return d.Spec.Template.Spec.Containers[0].Image
}

// processBackups is the admission of another manager process, with the backups of the env.
type processBackups struct {
	*fakeBackups
	lock fakeLock
}

func (p *processBackups) Hold() (func(), error) { return p.lock.Hold() }

func (e *systemEnv) setNode(change func(n *corev1.Node)) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n := &corev1.Node{}
		if err := e.c.Get(context.Background(), client.ObjectKey{Name: nodeName}, n); err != nil {
			return err
		}
		change(n)
		return e.c.Status().Update(context.Background(), n)
	})
	if err != nil {
		e.t.Error(err)
	}
}

// moveComponent runs the Kubernetes component at image, as Talos and the kubelet do.
func (e *systemEnv) moveComponent(component, image string) {
	if component == "kubelet" {
		version := image[strings.LastIndex(image, ":")+1:]
		e.node.mu.Lock()
		e.node.kube = version
		e.node.mu.Unlock()
		e.setNode(func(n *corev1.Node) { n.Status.NodeInfo.KubeletVersion = version })
		return
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod := &corev1.Pod{}
		key := client.ObjectKey{Namespace: kubeSystem, Name: staticPods[component] + "-" + nodeName}
		if err := e.c.Get(context.Background(), key, pod); err != nil {
			return err
		}
		pod.Spec.Containers[0].Image = image
		return e.c.Update(context.Background(), pod)
	})
	if err != nil {
		e.t.Error(err)
	}
}

// runsKubernetes leaves the node running Kubernetes version, as after the update's kubernetes step.
func (e *systemEnv) runsKubernetes(version string) {
	for component, image := range componentImages(version) {
		e.moveComponent(component, image)
	}
}

// reconcile plays the Appliance reconciler: the add-ons and the platform are ready, checked while a system update
// holds the add-on pass and passed again once it releases it.
func (e *systemEnv) reconcile(ctx context.Context) {
	for ctx.Err() == nil {
		e.reconcileOnce(ctx)
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *systemEnv) reconcileOnce(ctx context.Context) {
	a := &v1alpha1.Appliance{}
	if e.c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a) != nil {
		return
	}
	held := a.Status.Update.HoldsAddons()
	reason := "Ready"
	if held {
		reason = v1alpha1.ReasonUpdateHeld
	}
	changed := false
	for _, kind := range []string{v1alpha1.ConditionAddonsReady, v1alpha1.ConditionPlatformReady} {
		c := meta.FindStatusCondition(a.Status.Conditions, kind)
		if kind == v1alpha1.ConditionAddonsReady && e.addonsStalled(held) || c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason {
			continue
		}
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, Reason: reason, Message: "ready", ObservedGeneration: a.Generation,
		})
		changed = true
	}
	if changed {
		_ = e.c.Status().Update(ctx, a)
	}
}

// addonsStalled tells whether the add-ons are left as they are: never ready, or the released pass does not finish.
func (e *systemEnv) addonsStalled(held bool) bool {
	e.smu.Lock()
	defer e.smu.Unlock()
	return e.addonsDown || !held && e.stallAddons
}

// oldManager and newManager are the Jobs of the manager of the running release and of the bundle's.
func (e *systemEnv) oldManager() *update.Jobs {
	e.version = fromBundle
	e.old = e.newJobs()
	return e.old
}

func (e *systemEnv) newManager() *update.Jobs {
	e.version = systemTo
	return e.newJobsWith(e.newLock)
}

// waitHandedOver waits until the old manager applied the new manager and waits for it to take the update over.
func (e *systemEnv) waitHandedOver() {
	e.t.Helper()
	e.waitFor("the hand-over", func(u *v1alpha1.UpdateStatus) bool { return u.Step == update.StepTalos || u.Message != "" })
}

// waitStopped waits until jobs runs no update.
func (e *systemEnv) waitStopped(jobs *update.Jobs) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for jobs.Running() {
		if time.Now().After(deadline) {
			e.t.Fatal("the manager still runs the update")
		}
		time.Sleep(time.Millisecond)
	}
}

// update runs the system update to its end: the old manager to the hand-over, the new one from there.
func (e *systemEnv) update() *v1alpha1.UpdateStatus {
	e.t.Helper()
	old := e.oldManager()
	if err := old.StartSystem(e.t.Context(), systemTo, "admin"); err != nil {
		e.t.Fatal(err)
	}
	e.waitHandedOver()
	return e.takeOver()
}

// takeOver resumes the update in the new manager, waits for its end and for the old manager to stop. An update the
// new manager handed back ends in the previous manager, which its hand-back runs again.
func (e *systemEnv) takeOver() *v1alpha1.UpdateStatus {
	e.t.Helper()
	jobs := e.newManager()
	if err := jobs.Resume(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	u := e.waitFor("its end or hand-back", func(u *v1alpha1.UpdateStatus) bool {
		return u.Step == update.StepDone || u.Message != "" || u.HandBack != ""
	})
	if u.HandBack != "" && u.Message == "" {
		e.waitStopped(jobs)
		if got := e.managerImage(); got != previousManager {
			e.t.Errorf("manager image %s after the hand-back, want the previous one", got)
		}
		old := e.oldManager()
		if err := old.Resume(e.t.Context()); err != nil {
			e.t.Fatal(err)
		}
		u = e.waitEnded(old)
	}
	e.waitStopped(jobs)
	if e.old != nil {
		e.waitStopped(e.old)
	}
	return u
}

// wantHoldFree checks that neither manager holds its admission.
func (e *systemEnv) wantHoldFree() {
	e.t.Helper()
	e.jobEnv.wantHoldFree()
	release, err := e.newLock.Hold()
	if err != nil {
		e.t.Fatalf("the new manager still holds its admission: %v", err)
	}
	release()
}

// handedBack waits until the new manager handed the update back with reason and runs the previous manager again,
// whose resumption fails the update with that reason and releases the add-on pass.
func (e *systemEnv) handedBack(jobs *update.Jobs, reason string) {
	e.t.Helper()
	e.waitFor("the hand-back", func(u *v1alpha1.UpdateStatus) bool { return u.HandBack != "" })
	e.waitStopped(jobs)
	if u := e.appliance().Status.Update; u.HandBack != reason || u.Message != "" || !u.HoldsAddons() {
		e.t.Errorf("handed back %+v, want %s with the add-on pass held", u, reason)
	}
	if got := e.managerImage(); got != previousManager {
		e.t.Errorf("manager image %s, want the previous one", got)
	}
	old := e.oldManager()
	if err := old.Resume(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	if u := e.waitEnded(old); u.Message != reason || u.HoldsAddons() {
		e.t.Errorf("update %+v, want failed with %s", u, reason)
	}
	e.wantHoldFree()
}

// wantCalls checks the calls to the Talos API.
func (e *systemEnv) wantCalls(want ...string) {
	e.t.Helper()
	if got := e.node.called(); !slices.Equal(got, want) {
		e.t.Errorf("Talos calls\n%q\nwant\n%q", got, want)
	}
}

// kubernetesCalls are the calls that move the node to Kubernetes version, one component after the other.
func kubernetesCalls(version string) []string {
	images := componentImages(version)
	var calls []string
	for _, c := range []string{"apiServer", "controllerManager", "scheduler", "kubelet"} {
		calls = append(calls, "set "+c+" "+images[c])
	}
	return calls
}

var upgradeCalls = []string{
	// etcd is pulled by its tag, the name Talos looks it up by.
	"pull " + etcdRef, "installer " + installerRef + "@" + installerDigest, "upgrade " + installerRef + "@" + installerDigest, "reboot",
}

func TestSystemUpdateRunsEveryStep(t *testing.T) {
	e := newSystemEnv(t)
	u := e.update()
	if u.Step != update.StepDone || u.Message != "" || u.Kind != release.BundleSystem || u.From != fromBundle ||
		u.To != systemTo || u.Backup != "pre-update" || !u.PointOfNoReturn {
		t.Errorf("update %+v", u)
	}
	e.wantCalls(append(slices.Clone(upgradeCalls), kubernetesCalls(newKubernetes)...)...)
	if u.PreviousManager != previousManager || e.managerImage() != managerRef {
		t.Errorf("previous manager %q, manager image %q", u.PreviousManager, e.managerImage())
	}
	if !slices.Equal(u.PreviousAddonSteps, e.addonSteps) || e.appliance().Status.Versions.RequiredManager != systemTo {
		t.Errorf("previous add-on steps %v, required manager %q", u.PreviousAddonSteps, e.appliance().Status.Versions.RequiredManager)
	}
	if e.manifests.count() != 1 {
		t.Fatalf("%d applies of the manager's manifest, want 1", e.manifests.count())
	}
	applied := string(e.manifests.applied[0])
	if !strings.Contains(applied, "image: "+managerRef+"\n") || strings.Contains(applied, managerHolder) {
		t.Errorf("applied manifest %q, want the bundle's manager image", applied)
	}
	if e.tagger.calls() != 1 || len(e.tagger.tagged[0]) != 8 {
		t.Errorf("tagged %v, want the bundle's images once", e.tagger.tagged)
	}
	if runs, _ := e.backups.counts(); runs != 1 || !slices.Equal(e.backups.unpins, []string{release.BundleSystem + "/pre-update"}) {
		t.Errorf("%d backups, unpins %v", runs, e.backups.unpins)
	}
	want := []string{update.AuditSystemUpdateStart + ":success::", update.AuditSystemUpdate + ":success::"}
	if got := e.auditActions(); !slices.Equal(got, want) {
		t.Errorf("audit %v, want %v", got, want)
	}
	for _, step := range []string{update.StepBackup, update.StepManager, update.StepKubernetes, update.StepAddons} {
		if !strings.Contains(e.logs.String(), "step="+step) {
			t.Errorf("the log names no step %s:\n%s", step, e.logs)
		}
	}
	e.wantHoldFree()
}

// TestSystemUpdateAppliesManagerDaemonSet records the image the manager's DaemonSet runs before it applies the
// bundle's manager manifest, which defines that DaemonSet with the bundle's manager image.
func TestSystemUpdateAppliesManagerDaemonSet(t *testing.T) {
	e := newSystemEnv(t)
	u := e.update()
	if u.Step != update.StepDone || u.PreviousManager != previousManager {
		t.Errorf("update %+v, want done with the previous manager %s", u, previousManager)
	}
	if e.manifests.count() != 1 {
		t.Fatalf("%d applies of the manager's manifest, want 1", e.manifests.count())
	}
	if applied := string(e.manifests.applied[0]); !strings.HasPrefix(applied, "kind: DaemonSet\n") ||
		!strings.Contains(applied, "image: "+managerRef+"\n") {
		t.Errorf("applied manifest %q, want the bundle's DaemonSet with its manager image", applied)
	}
	if got := e.managerImage(); got != managerRef {
		t.Errorf("the manager's DaemonSet runs %s, want %s", got, managerRef)
	}
}

// crashedAtSystem leaves the status as a manager stopped at the i-th step of a system update leaves it.
func (e *systemEnv) crashedAtSystem(step string, i int) {
	e.t.Helper()
	if i >= 3 {
		e.runManager(managerRef)
	}
	if i >= 4 {
		// The node rebooted into the new Talos.
		e.setNode(func(n *corev1.Node) { n.Status.NodeInfo.BootID = bootID(1) })
		e.node.mu.Lock()
		e.node.boot = 1
		e.node.mu.Unlock()
		e.node.runs(newTalos, systemTo)
	}
	if i >= 6 {
		e.runsKubernetes(newKubernetes)
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update = &v1alpha1.UpdateStatus{
			Kind: release.BundleSystem, From: fromBundle, To: systemTo, Step: step, StartedAt: metav1.Now(),
			PointOfNoReturn: i >= 6,
		}
		if i > 0 {
			st.Update.Backup = "pre-update"
		}
		if i >= 3 {
			deadline := metav1.NewTime(time.Now().Add(time.Minute))
			st.Update.PreviousManager, st.Update.Deadline = previousManager, &deadline
		}
		if i >= 4 {
			st.Update.BootID = bootID(0)
		}
	})
}

// TestSystemUpdateResumesAtEveryStep restarts the managers at each step: the manager of the running release resumes
// the steps up to the hand-over and leaves the rest to the bundle's manager, which resumes from there; no step runs
// twice.
func TestSystemUpdateResumesAtEveryStep(t *testing.T) {
	steps := []string{
		update.StepBackup, update.StepImages, update.StepManager, update.StepTalos, update.StepHealth,
		update.StepKubernetes, update.StepAddons,
	}
	for i, step := range steps {
		t.Run(step, func(t *testing.T) {
			e := newSystemEnv(t)
			e.crashedAtSystem(step, i)
			old := e.oldManager()
			if err := old.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			if i <= 3 {
				e.waitHandedOver()
			} else {
				e.waitStopped(old)
			}
			if len(e.node.called()) != 0 || e.appliance().Status.Update.Message != "" {
				t.Fatalf("the old manager ran a step past the hand-over: %q", e.node.called())
			}
			u := e.takeOver()
			if u.Step != update.StepDone || u.Message != "" || u.Backup != "pre-update" {
				t.Errorf("update %+v", u)
			}
			e.wantSteps(i)
			if got := e.auditActions(); !slices.Equal(got, []string{update.AuditSystemUpdate + ":success::"}) {
				t.Errorf("audit %v", got)
			}
			e.wantHoldFree()
		})
	}
}

// wantSteps checks that each step from the i-th on ran once and none before it.
func (e *systemEnv) wantSteps(i int) {
	e.t.Helper()
	once := func(from int) int {
		if i <= from {
			return 1
		}
		return 0
	}
	runs, unpins := e.backups.counts()
	if runs != once(0) || e.tagger.calls() != once(1) || e.manifests.count() != once(2) || unpins != 1 {
		e.t.Errorf("%d backups, %d taggings, %d manifests, %d unpins", runs, e.tagger.calls(), e.manifests.count(), unpins)
	}
	if e.node.count("upgrade") != once(3) || e.node.count("installer") != once(3) || e.node.count("reboot") != once(3) ||
		e.node.count("rollback") != 0 {
		e.t.Errorf("Talos calls %q", e.node.called())
	}
	if e.node.count("set") != 4*once(5) {
		e.t.Errorf("Kubernetes calls %q", e.node.called())
	}
}

// TestSystemUpdateNewManagerResumesAtTheManagerStep is a restart at the manager step that finds the bundle's manager
// running: the manifest was applied, so the step is done.
func TestSystemUpdateNewManagerResumesAtTheManagerStep(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepManager, 2)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.PreviousManager = previousManager })
	if u := e.takeOver(); u.Step != update.StepDone || u.Message != "" {
		t.Errorf("update %+v", u)
	}
	if e.manifests.count() != 0 {
		t.Error("the bundle's manager applied its manifest again")
	}
}

func TestOldManagerDoesNotContinue(t *testing.T) {
	e := newSystemEnv(t)
	old := e.oldManager()
	if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
		t.Fatal(err)
	}
	e.waitHandedOver()
	time.Sleep(50 * time.Millisecond)
	u := e.appliance().Status.Update
	if u.Step != update.StepTalos || !u.Unfinished() || !u.HoldsAddons() || u.PointOfNoReturn || u.Deadline == nil {
		t.Errorf("update %+v, want waiting at the talos step with the add-on pass held", u)
	}
	if !old.Running() || len(e.node.called()) != 0 || e.manifests.count() != 1 {
		t.Errorf("the old manager went on: running %v, Talos calls %q, %d applies", old.Running(), e.node.called(),
			e.manifests.count())
	}
	if !strings.Contains(e.logs.String(), "waits for the manager of the new release") {
		t.Errorf("the log does not say the old manager waits:\n%s", e.logs)
	}
	// Another update, in another manager, is refused while this one waits for the new manager.
	if err := e.newJobsWith(&processBackups{fakeBackups: e.backups}).StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("platform update while the system update waits: %v", err)
	}
	// A failed health check rolled Talos back and the old manager runs again: it ends the update.
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update.Step, st.Update.BootID = update.StepRolledBack, bootID(5)
	})
	e.waitStopped(old)
	if err := old.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(old); u.Message != update.HealthCheckFailed || u.HoldsAddons() {
		t.Errorf("update %+v, want failed with the add-on pass released", u)
	}
	if len(e.node.called()) != 0 {
		t.Errorf("the old manager called Talos: %q", e.node.called())
	}
}

// TestManagerHandOver restarts the old manager at the talos step after a reboot of the old Talos put it back before
// the new manager took over: before the deadline it applies the new manager again and leaves the update to it; past
// it, the update fails with Talos and Kubernetes untouched and the old manager stays.
func TestManagerHandOver(t *testing.T) {
	t.Run("before the deadline", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.runManager(previousManager)
		old := e.oldManager()
		if err := old.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for e.managerImage() != managerRef {
			if time.Now().After(deadline) {
				t.Fatal("the old manager did not apply the new manager again")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if u := e.takeOver(); u.Step != update.StepDone || e.manifests.count() != 1 {
			t.Errorf("update %+v, %d applies", u, e.manifests.count())
		}
		e.wantHoldFree()
	})
	t.Run("past the deadline", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			past := metav1.NewTime(time.Now().Add(-time.Second))
			st.Update.Deadline = &past
		})
		old := e.oldManager()
		if err := old.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		if u := e.waitEnded(old); u.Message != update.ManagerDidNotStart || u.HoldsAddons() {
			t.Errorf("update %+v", u)
		}
		if e.managerImage() != previousManager || len(e.node.called()) != 0 || e.manifests.count() != 0 {
			t.Errorf("manager %s, Talos calls %q, %d applies", e.managerImage(), e.node.called(), e.manifests.count())
		}
		e.wantHoldFree()
	})
}

// TestHandBackBeforeTheNewTalos fails an update before the node booted the new Talos through the previous manager:
// the new one runs it again, which records the failure and releases the add-on pass.
func TestHandBackBeforeTheNewTalos(t *testing.T) {
	t.Run("upgrade refused", func(t *testing.T) {
		e := newSystemEnv(t)
		e.node.upgradeErr = status.Error(codes.FailedPrecondition, "no such image")
		old := e.oldManager()
		if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
			t.Fatal(err)
		}
		e.waitHandedOver()
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.handedBack(jobs, update.TalosUpgradeRefused)
	})
	t.Run("the old Talos booted", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = 500 * time.Millisecond
		e.node.oldTalosBoots = true
		old := e.oldManager()
		if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
			t.Fatal(err)
		}
		e.waitHandedOver()
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.handedBack(jobs, update.TalosNotRebooted)
		if e.node.count("rollback") != 0 || e.node.count("set") != 0 {
			t.Errorf("Talos calls %q", e.node.called())
		}
	})
	t.Run("the new manager restarts", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.HandBack = update.TalosUpgradeRefused })
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.handedBack(jobs, update.TalosUpgradeRefused)
	})
	// The node rebooted into the old Talos and the old manager runs there before any hand-back.
	t.Run("old manager after a reboot into the old Talos", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.BootID = bootID(0) })
		e.node.reboot()
		old := e.oldManager()
		if err := old.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		if u := e.waitEnded(old); u.Message != update.TalosNotRebooted || u.HoldsAddons() {
			t.Errorf("update %+v", u)
		}
		e.wantCalls()
	})
	// An upgrade call cut off is tried again once resumed, not handed back.
	t.Run("interrupted upgrade call", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.node.upgradeErr = status.Error(codes.Unavailable, "connection reset")
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.waitFor("the recorded boot", func(u *v1alpha1.UpdateStatus) bool { return u.BootID != "" })
		e.waitStopped(jobs)
		if u := e.appliance().Status.Update; u.Step != update.StepTalos || u.HandBack != "" || u.Message != "" {
			t.Errorf("update %+v, want waiting at the talos step", u)
		}
		e.node.mu.Lock()
		e.node.upgradeErr = nil
		e.node.mu.Unlock()
		if u := e.takeOver(); u.Step != update.StepDone || e.node.count("upgrade") != 2 || e.node.count("reboot") != 1 {
			t.Errorf("update %+v, Talos calls %q", u, e.node.called())
		}
	})
}

// TestHandBackRestoresDaemonSetImage runs the previous manager again in the manager's DaemonSet once the bundle's
// manager hands a failed update back.
func TestHandBackRestoresDaemonSetImage(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepTalos, 3)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.HandBack = update.TalosUpgradeRefused })
	if got := e.managerImage(); got != managerRef {
		t.Fatalf("the manager's DaemonSet runs %s, want the bundle's", got)
	}
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(jobs)
	if got := e.managerImage(); got != previousManager {
		t.Errorf("the manager's DaemonSet runs %s after the hand-back, want %s", got, previousManager)
	}
}

func TestHealthGateFailureRollsBackTalos(t *testing.T) {
	t.Run("console down", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = 300 * time.Millisecond
		e.setSystem(func(e *systemEnv) { e.consoleDown = true })
		u := e.update()
		if u.Step != update.StepRolledBack || u.Message != update.HealthCheckFailed || u.PointOfNoReturn {
			t.Errorf("update %+v", u)
		}
		e.wantCalls(append(slices.Clone(upgradeCalls), "rollback")...)
		if u.HoldsAddons() {
			t.Error("the failed update still holds the add-on pass")
		}
		if got := e.auditActions(); got[len(got)-1] != update.AuditSystemUpdate+":failure:"+update.StepRolledBack+":"+update.HealthCheckFailed {
			t.Errorf("audit %v", got)
		}
		e.wantHoldFree()
	})
	t.Run("addons not ready", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = 300 * time.Millisecond
		e.setSystem(func(e *systemEnv) { e.addonsDown = true })
		u := e.update()
		if u.Step != update.StepRolledBack || u.Message != update.HealthCheckFailed {
			t.Errorf("update %+v", u)
		}
	})
	t.Run("rollback refused", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = 300 * time.Millisecond
		e.setSystem(func(e *systemEnv) { e.consoleDown = true })
		e.node.rollbackErr = errors.New("no fallback")
		u := e.update()
		if u.Message != update.TalosRollbackFailed {
			t.Errorf("update %+v", u)
		}
		e.wantCalls(append(slices.Clone(upgradeCalls), "rollback")...)
	})
	t.Run("rollback call cut off", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = 300 * time.Millisecond
		e.setSystem(func(e *systemEnv) { e.consoleDown = true })
		e.node.rollbackErr, e.node.rollbackCutOff = status.Error(codes.Unavailable, "connection reset"), true
		if u := e.update(); u.Message != update.HealthCheckFailed || u.Step != update.StepRolledBack {
			t.Errorf("update %+v", u)
		}
		e.wantCalls(append(slices.Clone(upgradeCalls), "rollback")...)
	})
	t.Run("deadline kept over a restart", func(t *testing.T) {
		e := newSystemEnv(t)
		e.health = time.Hour
		e.setSystem(func(e *systemEnv) { e.consoleDown = true })
		e.crashedAtSystem(update.StepHealth, 4)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			past := metav1.NewTime(time.Now().Add(-time.Second))
			st.Update.Deadline = &past
		})
		start := time.Now()
		if u := e.takeOver(); u.Message != update.HealthCheckFailed {
			t.Errorf("update %+v", u)
		}
		if time.Since(start) > 10*time.Second {
			t.Error("the restart gave the health check a new deadline")
		}
		e.wantCalls("rollback")
	})
}

// stopReconciler keeps the add-ons from being reported ready again after the reboot.
func (e *systemEnv) stopReconciler() {
	e.setSystem(func(e *systemEnv) { e.stallAddons = true })
}

// TestRolledBackResumesAfterTheReboot restarts the manager after it recorded the Talos rollback. After the reboot into
// the old Talos, the previous manager, run again by the hand-back, fails the update (the bundle's manager hands it
// back, see TestRolledBackHandsBackToThePreviousManager). Before it, the manager never rolls back twice, which would
// switch the boot entry back: it waits for the reboot until the recorded deadline.
func TestRolledBackResumesAfterTheReboot(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		rebooted bool
		// notReady keeps the node that booted the old Talos from being ready.
		notReady bool
		reason   string
	}{
		{"old manager after the reboot", fromBundle, true, false, update.HealthCheckFailed},
		{"no reboot", systemTo, false, false, update.TalosRollbackFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSystemEnv(t)
			e.health = time.Hour
			e.crashedAtSystem(update.StepRolledBack, 4)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
				deadline := metav1.NewTime(time.Now().Add(300 * time.Millisecond))
				st.Update.BootID, st.Update.Deadline = bootID(1), &deadline
			})
			if tc.rebooted {
				e.setNode(func(n *corev1.Node) {
					n.Status.NodeInfo.BootID = bootID(2)
					if tc.notReady {
						n.Status.Conditions[0].Status = corev1.ConditionFalse
					}
				})
				e.node.runs(oldTalos, fromBundle)
			}
			e.version = tc.version
			jobs := e.newJobs()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			u := e.waitEnded(jobs)
			if u.Message != tc.reason || u.Step != update.StepRolledBack || u.HoldsAddons() {
				t.Errorf("update %+v", u)
			}
			e.wantCalls()
			e.wantHoldFree()
		})
	}
}

// TestRolledBackHandsBackToThePreviousManager hands an update whose health check failed back to the previous manager
// once the node booted the old Talos again: a first boot leaves the manager's DaemonSet alone, so the bundle's
// manager would run on the old Talos otherwise.
func TestRolledBackHandsBackToThePreviousManager(t *testing.T) {
	for name, notReady := range map[string]bool{"old Talos ready": false, "old Talos not ready past the deadline": true} {
		t.Run(name, func(t *testing.T) {
			e := newSystemEnv(t)
			e.crashedAtSystem(update.StepRolledBack, 4)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
				deadline := metav1.NewTime(time.Now().Add(300 * time.Millisecond))
				st.Update.BootID, st.Update.Deadline = bootID(1), &deadline
			})
			e.setNode(func(n *corev1.Node) {
				n.Status.NodeInfo.BootID = bootID(2)
				if notReady {
					n.Status.Conditions[0].Status = corev1.ConditionFalse
				}
			})
			e.node.runs(oldTalos, fromBundle)
			jobs := e.newManager()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.handedBack(jobs, update.HealthCheckFailed)
			if u := e.appliance().Status.Update; u.Step != update.StepRolledBack {
				t.Errorf("update %+v, want failed at the rolled back step", u)
			}
			e.wantCalls()
		})
	}
}

// TestRolledBackUnreadableIsDecidedLater neither fails the rolled back update as rolled back nor as a failed rollback
// past its deadline while the Talos the node runs cannot be read.
func TestRolledBackUnreadableIsDecidedLater(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepRolledBack, 4)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.BootID = bootID(1) })
	e.pastDeadline()
	e.node.readErr = status.Error(codes.Unavailable, "no route to host")
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(jobs)
	if u := e.appliance().Status.Update; u.Step != update.StepRolledBack || u.Message != "" {
		t.Fatalf("update %+v: decided without a read", u)
	}
	e.node.mu.Lock()
	e.node.readErr = nil
	e.node.mu.Unlock()
	jobs = e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Message != update.TalosRollbackFailed {
		t.Errorf("update %+v", u)
	}
	e.wantCalls()
}

// TestTalosStepNeverRebootsTwice restarts the manager at the talos step after it recorded the boot ID: after the
// reboot into the new Talos it goes on; before it, it installs and reboots again, once; a node that does not reboot
// into the installed Talos in time hands the update back saying so; one that booted the old Talos hands it back at
// once.
func TestTalosStepNeverRebootsTwice(t *testing.T) {
	cases := []struct {
		name     string
		rebooted bool
		stuck    bool
		upgrades int
	}{
		{"after the reboot", true, false, 0},
		{"before the reboot", false, false, 1},
		{"no reboot", false, true, 1},
	}
	t.Run("booted the old Talos", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepTalos, 3)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.BootID = bootID(0) })
		e.node.reboot()
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.handedBack(jobs, update.TalosNotRebooted)
		e.wantCalls()
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSystemEnv(t)
			e.crashedAtSystem(update.StepTalos, 3)
			// A node that never reboots is given up soon.
			wait := 10 * time.Second
			if tc.stuck {
				wait, e.health = 2*time.Second, 300*time.Millisecond
			}
			deadline := metav1.NewTime(time.Now().Add(wait))
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.BootID, st.Update.Deadline = bootID(0), &deadline })
			if tc.rebooted {
				e.node.runs(newTalos, systemTo)
				e.node.reboot()
			}
			e.node.stuck = tc.stuck
			jobs := e.newManager()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.stuck {
				e.handedBack(jobs, update.TalosInstalledNotRebooted)
			} else if u := e.waitEnded(jobs); u.Step != update.StepDone {
				t.Errorf("update %+v", u)
			}
			if e.node.count("upgrade") != tc.upgrades || e.node.count("reboot") != tc.upgrades || e.node.count("rollback") != 0 {
				t.Errorf("Talos calls %q", e.node.called())
			}
		})
	}
}

// TestKubernetesTalosFailuresAreResumed keeps the update at the kubernetes step, past the point of no return, when a
// call to the Talos API fails, and goes on once resumed.
func TestKubernetesTalosFailuresAreResumed(t *testing.T) {
	t.Run("component change cut off", func(t *testing.T) {
		e := newSystemEnv(t)
		e.node.setErr = map[string]error{"scheduler": status.Error(codes.Unavailable, "connection reset")}
		old := e.oldManager()
		if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
			t.Fatal(err)
		}
		e.waitHandedOver()
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.waitFor("the scheduler change", func(*v1alpha1.UpdateStatus) bool { return e.node.count("set scheduler") > 0 })
		e.waitStopped(jobs)
		if u := e.appliance().Status.Update; u.Step != update.StepKubernetes || u.Message != "" || !u.PointOfNoReturn {
			t.Fatalf("update %+v, want waiting at the kubernetes step", u)
		}
		if !slices.Equal(e.node.pointOfNoReturn, []bool{true, true, true}) {
			t.Errorf("point of no return at each component change: %v", e.node.pointOfNoReturn)
		}
		e.node.mu.Lock()
		e.node.setErr = nil
		e.node.mu.Unlock()
		if u := e.takeOver(); u.Step != update.StepDone || e.node.count("rollback") != 0 || e.node.count("upgrade") != 1 {
			t.Errorf("update %+v, Talos calls %q", u, e.node.called())
		}
	})
	t.Run("component change refused", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepKubernetes, 5)
		e.node.setErr = map[string]error{"scheduler": status.Error(codes.InvalidArgument, "invalid image")}
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		if u := e.waitEnded(jobs); u.Step != update.StepKubernetes || u.Message != update.Failed || !u.PointOfNoReturn || u.HoldsAddons() {
			t.Errorf("update %+v, want failed at the kubernetes step", u)
		}
	})
	t.Run("Talos API unavailable", func(t *testing.T) {
		e := newSystemEnv(t)
		e.crashedAtSystem(update.StepKubernetes, 5)
		e.talosErr = status.Error(codes.Unavailable, "no route to host")
		jobs := e.newManager()
		if err := jobs.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.waitStopped(jobs)
		if u := e.appliance().Status.Update; u.Step != update.StepKubernetes || u.Message != "" {
			t.Fatalf("update %+v, want waiting at the kubernetes step", u)
		}
		e.talosErr = nil
		if u := e.takeOver(); u.Step != update.StepDone {
			t.Errorf("update %+v", u)
		}
	})
}

// TestFailedSystemUpdateRunsAgain starts a system update that failed past its point of no return again in the manager
// of its version, from the step it failed at: no backup, no images, no Talos install. Any other failed update of the
// running version is refused.
func TestFailedSystemUpdateRunsAgain(t *testing.T) {
	failedAt := func(e *systemEnv, step, reason string, pointOfNoReturn bool) {
		e.crashedAtSystem(update.StepKubernetes, 5)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			st.Update.Step, st.Update.Message, st.Update.PointOfNoReturn = step, reason, pointOfNoReturn
			st.Versions.Appliance = systemTo
		})
	}
	refused := func(t *testing.T, e *systemEnv) {
		t.Helper()
		if err := e.newManager().StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != update.SameVersion {
			t.Errorf("start: %v, want %s", err, update.SameVersion)
		}
	}
	t.Run("past the point of no return", func(t *testing.T) {
		e := newSystemEnv(t)
		failedAt(e, update.StepKubernetes, update.Failed, true)
		jobs := e.newManager()
		if err := jobs.StartSystem(t.Context(), systemTo, "admin"); err != nil {
			t.Fatal(err)
		}
		u := e.waitEnded(jobs)
		if u.Step != update.StepDone || u.From != fromBundle || u.To != systemTo || u.Backup != "pre-update" {
			t.Errorf("update %+v", u)
		}
		e.wantCalls(kubernetesCalls(newKubernetes)...)
		if runs, _ := e.backups.counts(); runs != 0 || e.tagger.calls() != 0 || e.manifests.count() != 0 {
			t.Errorf("%d backups, %d taggings, %d manifests: the update did not resume at its step", runs, e.tagger.calls(), e.manifests.count())
		}
		want := []string{update.AuditSystemUpdateStart + ":success::", update.AuditSystemUpdate + ":success::"}
		if got := e.auditActions(); !slices.Equal(got, want) {
			t.Errorf("audit %v, want %v", got, want)
		}
		e.wantHoldFree()
	})
	t.Run("talos rollback failed", func(t *testing.T) {
		e := newSystemEnv(t)
		failedAt(e, update.StepRolledBack, update.TalosRollbackFailed, false)
		refused(t, e)
	})
	t.Run("another version", func(t *testing.T) {
		e := newSystemEnv(t)
		failedAt(e, update.StepKubernetes, update.Failed, true)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.To = "1.0.5" })
		refused(t, e)
	})
	t.Run("done", func(t *testing.T) {
		e := newSystemEnv(t)
		failedAt(e, update.StepDone, "", true)
		refused(t, e)
	})
}

// TestHealthStepPastThePointOfNoReturn ends the health step at once when a restart cut it off after the check passed:
// Talos is never rolled back then.
func TestHealthStepPastThePointOfNoReturn(t *testing.T) {
	e := newSystemEnv(t)
	e.setSystem(func(e *systemEnv) { e.consoleDown = true })
	e.crashedAtSystem(update.StepHealth, 4)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.PointOfNoReturn = true })
	e.pastDeadline()
	if u := e.takeOver(); u.Step != update.StepDone || u.Message != "" {
		t.Errorf("update %+v", u)
	}
	e.wantCalls(kubernetesCalls(newKubernetes)...)
}

// TestKubernetesComponentsSkippedAtTheirImage resumes the kubernetes step with the API server already moved: it is
// not changed again.
func TestKubernetesComponentsSkippedAtTheirImage(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepKubernetes, 5)
	e.moveComponent("apiServer", componentImages(newKubernetes)["apiServer"])
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Step != update.StepDone || !u.PointOfNoReturn {
		t.Errorf("update %+v", u)
	}
	e.wantCalls(kubernetesCalls(newKubernetes)[1:]...)
}

func TestSkipsUnchangedVersions(t *testing.T) {
	e := newSystemEnvWith(t, oldKubernetes)
	u := e.update()
	// The passed health check is the point of no return, also without a change of Kubernetes.
	if u.Step != update.StepDone || u.Message != "" || !u.PointOfNoReturn {
		t.Errorf("update %+v", u)
	}
	// Talos is upgraded at the same version too: the installer carries the new release's image cache.
	e.wantCalls(upgradeCalls...)
}

// TestAddonsStepWaitsForTheAddonPass releases the add-on pass at the addons step and ends only once the pass of the
// new release finished.
func TestAddonsStepWaitsForTheAddonPass(t *testing.T) {
	e := newSystemEnv(t)
	e.stopReconciler()
	e.crashedAtSystem(update.StepAddons, 6)
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if u := e.appliance().Status.Update; u.Step != update.StepAddons || u.HoldsAddons() {
		t.Fatalf("update %+v, want at the addons step with the add-on pass released", u)
	}
	e.setSystem(func(e *systemEnv) { e.stallAddons = false })
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
}

// TestSystemUpdateHoldsTheAddonPass holds the add-on pass from the backup on until the addons step.
func TestSystemUpdateHoldsTheAddonPass(t *testing.T) {
	e := newSystemEnv(t)
	e.backups.entered, e.backups.block = make(chan struct{}), make(chan struct{})
	old := e.oldManager()
	if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
		t.Fatal(err)
	}
	<-e.backups.entered
	if u := e.appliance().Status.Update; u.Step != update.StepBackup || !u.HoldsAddons() {
		t.Errorf("update %+v, want the add-on pass held at the backup", u)
	}
	close(e.backups.block)
	e.waitHandedOver()
	if u := e.takeOver(); u.HoldsAddons() {
		t.Errorf("update %+v holds the add-on pass", u)
	}
}

func TestSystemUpdateAdmission(t *testing.T) {
	refused := func(t *testing.T, e *systemEnv, reason string) {
		t.Helper()
		if err := e.oldManager().StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != reason {
			t.Errorf("start: %v, want %s", err, reason)
		}
		if e.appliance().Status.Update != nil {
			t.Error("a refused update started")
		}
	}
	t.Run("not loaded", func(t *testing.T) {
		e := newSystemEnv(t)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			st.Updates.Available, st.Updates.Loaded = st.Updates.Loaded[1:], st.Updates.Loaded[:1]
		})
		refused(t, e, update.NotFound)
	})
	t.Run("files missing", func(t *testing.T) {
		e := newSystemEnv(t)
		if err := os.RemoveAll(filepath.Join(e.dir, release.BundleSystem)); err != nil {
			t.Fatal(err)
		}
		refused(t, e, update.BundleUnavailable)
	})
	for _, image := range []string{installerRef, managerRef, etcdRef} {
		t.Run("without "+image, func(t *testing.T) {
			e := newSystemEnv(t)
			e.storeSystemBundle(image)
			refused(t, e, update.InvalidBundle)
		})
	}
	t.Run("same version", func(t *testing.T) {
		e := newSystemEnv(t)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Versions.Appliance = systemTo })
		refused(t, e, update.SameVersion)
	})
	t.Run("operator of the active platform bundle out of range", func(t *testing.T) {
		e := newSystemEnv(t)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			st.Updates.Loaded[1].OperatorRange = ">=1.0.0 <1.1.0"
			// The reported operator is empty: the active one, of platform bundle 1.1.0, is checked.
			st.Versions.Operator, st.Versions.PlatformBundle = "", toBundle
		})
		refused(t, e, update.OperatorOutOfRange)
	})
	t.Run("older Kubernetes", func(t *testing.T) {
		e := newSystemEnvWith(t, "v1.36.0")
		refused(t, e, update.KubernetesOlderThanRunning)
	})
	t.Run("node images from a registry not mirrored", func(t *testing.T) {
		e := newSystemEnv(t)
		e.upstreams = []string{"registry.k8s.io"}
		refused(t, e, update.UnmirroredRegistry)
	})
	t.Run("Kubernetes minor skipped", func(t *testing.T) {
		e := newSystemEnvWith(t, "v1.39.0")
		refused(t, e, update.KubernetesMinorSkip)
	})
	t.Run("no space", func(t *testing.T) {
		e := newSystemEnv(t)
		e.noSpace = true
		refused(t, e, update.NoSpace)
	})
	t.Run("embedded operator in range", func(t *testing.T) {
		e := newSystemEnv(t)
		e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
			st.Updates.Loaded[1].OperatorRange = ">=1.0.0 <1.1.0"
			st.Versions.Operator = ""
		})
		if u := e.update(); u.Step != update.StepDone {
			t.Errorf("update %+v", u)
		}
	})
}

// TestUnfinishedSystemUpdateRefusesOthers refuses a platform update and a system update, of a restarted manager too,
// while a system update is unfinished.
func TestUnfinishedSystemUpdateRefusesOthers(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepTalos, 3)
	jobs := e.oldManager()
	if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("platform update: %v", err)
	}
	if err := jobs.StartSystem(t.Context(), systemTo, "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("system update: %v", err)
	}
}

// TestHealthGateDecidesOnTheRunningTalos rolls Talos back, at the deadline of the health check, once the node runs the
// new Talos, ready or not, and hands the update back while it runs the boot it ran before.
func TestHealthGateDecidesOnTheRunningTalos(t *testing.T) {
	cases := []struct {
		name     string
		rebooted bool
		notReady bool
		readErr  bool
		rollback bool
	}{
		{"new Talos not ready", true, true, false, true},
		{"no reboot, Talos API down", false, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSystemEnv(t)
			e.health = 200 * time.Millisecond
			e.crashedAtSystem(update.StepHealth, 4)
			e.pastDeadline()
			if !tc.rebooted {
				e.setNode(func(n *corev1.Node) { n.Status.NodeInfo.BootID = bootID(0) })
				e.node.runs(oldTalos, fromBundle)
			}
			if tc.notReady {
				e.setNode(func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse })
			}
			if tc.readErr {
				e.node.readErr = status.Error(codes.Unavailable, "no route to host")
			}
			jobs := e.newManager()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !tc.rollback {
				e.handedBack(jobs, update.TalosNotRebooted)
				e.wantCalls()
				return
			}
			e.wantRolledBack()
		})
	}
}

func (e *systemEnv) pastDeadline() {
	past := metav1.NewTime(time.Now().Add(-time.Second))
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.Deadline = &past })
}

// wantRolledBack waits for the rollback of Talos, never handed back before it. The step is recorded before the
// rollback is called, so the call is waited for too.
func (e *systemEnv) wantRolledBack() {
	e.t.Helper()
	e.waitFor("the rollback", func(u *v1alpha1.UpdateStatus) bool {
		return u.Step == update.StepRolledBack && e.node.count("rollback") > 0
	})
	if e.node.count("rollback") != 1 {
		e.t.Errorf("Talos calls %q, want a rollback", e.node.called())
	}
	if u := e.appliance().Status.Update; u.HandBack != "" && u.HandBack != update.HealthCheckFailed {
		e.t.Errorf("update %+v handed back on the new Talos", u)
	}
}

// TestUnreadableTalosIsDecidedLater neither rolls back nor hands back at the deadline while the node or the Talos API
// cannot be read: the decision waits for a read, then rolls back the new Talos.
func TestUnreadableTalosIsDecidedLater(t *testing.T) {
	for _, unreadable := range []string{"Talos API", "node"} {
		t.Run(unreadable, func(t *testing.T) {
			e := newSystemEnv(t)
			e.crashedAtSystem(update.StepHealth, 4)
			e.pastDeadline()
			if unreadable == "node" {
				e.jobClient = interceptor.NewClient(e.c, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Node); ok {
							return errors.New("the API server does not answer")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			} else {
				e.node.readErr = status.Error(codes.Unavailable, "no route to host")
			}
			jobs := e.newManager()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.waitStopped(jobs)
			if u := e.appliance().Status.Update; u.Step != update.StepHealth || u.HandBack != "" || u.Message != "" ||
				len(e.node.called()) != 0 {
				t.Fatalf("update %+v, Talos calls %q: decided without a read", u, e.node.called())
			}
			e.jobClient = nil
			e.node.mu.Lock()
			e.node.readErr = nil
			e.node.mu.Unlock()
			if err := e.newManager().Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.wantRolledBack()
		})
	}
}

// TestManagerStepPastTheDeadline restarts the old manager at the manager step past the hand-over deadline: it fails
// the update without applying the new manager again.
func TestManagerStepPastTheDeadline(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepManager, 2)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.PreviousManager = previousManager })
	e.pastDeadline()
	old := e.oldManager()
	if err := old.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(old); u.Message != update.ManagerDidNotStart || u.HoldsAddons() {
		t.Errorf("update %+v", u)
	}
	if e.manifests.count() != 0 || e.managerImage() != previousManager {
		t.Errorf("%d applies, manager %s", e.manifests.count(), e.managerImage())
	}
}

// TestFailuresBeforeTheBootHandBack hands the update back for any failure of the talos step before the node booted the
// new Talos, so that the hold is released by the previous manager only.
func TestFailuresBeforeTheBootHandBack(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(e *systemEnv)
		reason  string
	}{
		{"no installer", func(e *systemEnv) { e.storeSystemBundle(installerRef) }, update.ImagesMissing},
		{"no Talos access", func(e *systemEnv) { e.talosErr = errors.New("no talosconfig") }, update.Failed},
		{"no boot ID", func(e *systemEnv) { e.setNode(func(n *corev1.Node) { n.Status.NodeInfo.BootID = "" }) }, update.Failed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSystemEnv(t)
			e.crashedAtSystem(update.StepTalos, 3)
			tc.prepare(e)
			jobs := e.newManager()
			if err := jobs.Resume(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.handedBack(jobs, tc.reason)
			e.wantCalls()
		})
	}
}

// TestHandOverDeadlineRecordedOnce restarts the old manager at the manager step after it recorded the previous manager
// and the hand-over deadline: it applies the new manager again but keeps the deadline, which no restart extends.
func TestHandOverDeadlineRecordedOnce(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepManager, 2)
	deadline := metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second))
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update.PreviousManager, st.Update.Deadline = previousManager, &deadline
	})
	old := e.oldManager()
	if err := old.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.waitHandedOver()
	if u := e.appliance().Status.Update; !u.Deadline.Equal(&deadline) || u.PreviousManager != previousManager {
		t.Errorf("update %+v, want the deadline %v kept", u, deadline)
	}
	if e.manifests.count() != 1 {
		t.Errorf("%d applies, want 1", e.manifests.count())
	}
	if u := e.takeOver(); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
}

// installedAtTalos leaves the status as a manager stopped at the talos step after the install leaves it: the boot ID
// recorded, Talos installed and the deadline of the health check ahead by wait.
func (e *systemEnv) installedAtTalos(wait time.Duration) {
	e.t.Helper()
	e.crashedAtSystem(update.StepTalos, 3)
	deadline := metav1.NewTime(time.Now().Add(wait))
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update.BootID, st.Update.Installed, st.Update.Deadline = bootID(0), true, &deadline
	})
	e.node.installed = true
}

// TestInstalledTalosOnlyRebootsAgain restarts the manager after the install and before the reboot: it reboots again,
// without installing again or handing back.
func TestInstalledTalosOnlyRebootsAgain(t *testing.T) {
	e := newSystemEnv(t)
	e.installedAtTalos(10 * time.Second)
	if u := e.takeOver(); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
	if got := e.node.called(); len(got) < 1 || got[0] != "reboot" || e.node.count("upgrade") != 0 || e.node.count("installer") != 0 {
		t.Errorf("Talos calls %q, want a reboot alone before Kubernetes", got)
	}
}

// TestRebootFailureAfterTheInstallIsRetried tries a reboot the node refuses again once resumed: the update is not
// handed back while the node runs the boot it recorded.
func TestRebootFailureAfterTheInstallIsRetried(t *testing.T) {
	e := newSystemEnv(t)
	e.installedAtTalos(10 * time.Second)
	e.node.rebootErr = errors.New("reboot refused")
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(jobs)
	if u := e.appliance().Status.Update; u.Step != update.StepTalos || u.HandBack != "" || u.Message != "" {
		t.Fatalf("update %+v, want the reboot tried again", u)
	}
	e.node.mu.Lock()
	e.node.rebootErr = nil
	e.node.mu.Unlock()
	if u := e.takeOver(); u.Step != update.StepDone || e.node.count("upgrade") != 0 {
		t.Errorf("update %+v, Talos calls %q", u, e.node.called())
	}
}

// TestInstalledButNotRebootedHandsBackWithItsReason hands back, at the deadline, an installed Talos the node did not
// reboot into, with the reason that says the next reboot boots it without the health check.
func TestInstalledButNotRebootedHandsBackWithItsReason(t *testing.T) {
	e := newSystemEnv(t)
	e.installedAtTalos(300 * time.Millisecond)
	e.node.stuck = true
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.handedBack(jobs, update.TalosInstalledNotRebooted)
	if e.node.count("upgrade") != 0 || e.node.count("rollback") != 0 {
		t.Errorf("Talos calls %q", e.node.called())
	}
}

// TestHealthDeadlineAfterTheInstall takes the deadline of the health check once Talos is installed, so that a slow
// install does not shorten the health check.
func TestHealthDeadlineAfterTheInstall(t *testing.T) {
	e := newSystemEnv(t)
	e.node.installTime = 1500 * time.Millisecond
	u := e.update()
	if u.Step != update.StepDone || !u.Installed {
		t.Fatalf("update %+v", u)
	}
	if !u.Deadline.After(e.node.deadlineAtInstall) {
		t.Errorf("health deadline %v, want after the install's start, whose deadline was %v", u.Deadline, e.node.deadlineAtInstall)
	}
}

// TestUnavailableTalosBeforeTheBootIsResumed keeps the update at the talos step when the Talos API is unavailable before
// the boot ID is recorded, as right after the new manager started, and goes on once resumed.
func TestUnavailableTalosBeforeTheBootIsResumed(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepTalos, 3)
	e.talosErr = status.Error(codes.Unavailable, "connection refused")
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(jobs)
	if u := e.appliance().Status.Update; u.Step != update.StepTalos || u.HandBack != "" || u.Message != "" || u.BootID != "" {
		t.Fatalf("update %+v, want waiting at the talos step", u)
	}
	e.talosErr = nil
	if u := e.takeOver(); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
}

// TestRebootIssuedAgainWhileTheNodeKeepsItsBoot issues the reboot again, within the wait, when a reboot call was cut
// off before the node took it.
func TestRebootIssuedAgainWhileTheNodeKeepsItsBoot(t *testing.T) {
	e := newSystemEnv(t)
	e.rebootRetry = 50 * time.Millisecond
	e.installedAtTalos(10 * time.Second)
	e.node.rebootErr = status.Error(codes.Unavailable, "connection reset")
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.node.count("reboot") < 2 {
		if time.Now().After(deadline) || !jobs.Running() {
			t.Fatalf("the reboot was not issued again within the wait: Talos calls %q", e.node.called())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if u := e.appliance().Status.Update; u.Step != update.StepTalos || u.HandBack != "" || u.Message != "" {
		t.Fatalf("update %+v", u)
	}
	e.node.mu.Lock()
	e.node.rebootErr = nil
	e.node.mu.Unlock()
	if u := e.waitEnded(jobs); u.Step != update.StepDone || e.node.count("upgrade") != 0 {
		t.Errorf("update %+v, Talos calls %q", u, e.node.called())
	}
}

// TestRolledBackIntoTheNewTalos fails a rollback after which the node booted the new Talos again as a rollback that did
// not take, never as rolled back.
func TestRolledBackIntoTheNewTalos(t *testing.T) {
	e := newSystemEnv(t)
	e.health = 300 * time.Millisecond
	e.setSystem(func(e *systemEnv) { e.consoleDown = true })
	e.node.rollbackBootsNew = true
	if u := e.update(); u.Message != update.TalosRollbackFailed || u.Step != update.StepRolledBack {
		t.Errorf("update %+v", u)
	}
}

// TestKubernetesStepDecidesOnWhatRuns moves every component whose running version is old although the machine
// configuration already names the new Kubernetes, as after a restart that wrote the configuration.
func TestKubernetesStepDecidesOnWhatRuns(t *testing.T) {
	e := newSystemEnv(t)
	e.crashedAtSystem(update.StepKubernetes, 5)
	e.node.mu.Lock()
	e.node.kube = newKubernetes
	e.node.mu.Unlock()
	jobs := e.newManager()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Errorf("update %+v", u)
	}
	e.wantCalls(kubernetesCalls(newKubernetes)...)
}

// TestSystemUpdateWaitsForTheAddonStep starts the backup of a system update only once a step of the add-on pass that
// was being applied when it started ended.
func TestSystemUpdateWaitsForTheAddonStep(t *testing.T) {
	e := newSystemEnv(t)
	lock := &sync.Mutex{}
	e.addonLock = lock
	e.backups.entered, e.backups.block = make(chan struct{}), make(chan struct{})
	lock.Lock()
	old := e.oldManager()
	if err := old.StartSystem(t.Context(), systemTo, "admin"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.backups.entered:
		t.Fatal("the backup began while the add-on pass applied a step")
	case <-time.After(100 * time.Millisecond):
	}
	lock.Unlock()
	<-e.backups.entered
	close(e.backups.block)
	e.waitHandedOver()
}
