package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	wizard "github.com/OmniTrustILM/appliance-ng/internal/setup"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	hashMarker = "$argon2id$v=19$m=65536,t=3,p=4$HASH-MARKER"
	seedMarker = "SEEDMARKERSEEDMARKER"
	adminName  = "admin"
)

func handoverAnswers() wizard.HandoverFile {
	f := wizard.HandoverFile{Version: wizard.HandoverVersion, Node: wizard.NodeAnswers{
		HostName: "pki.example.com",
		Address:  "192.0.2.10/24",
		Gateway:  "192.0.2.1",
		DNS:      []string{"192.0.2.53"},
		NTP:      []string{"ntp.example.com"},
		Proxy:    &wizard.Proxy{HTTP: "http://proxy.example.com:3128", NoProxy: ".example.com"},
	}}
	f.Admin.User, f.Admin.PasswordHash, f.Admin.TOTPSeed = adminName, hashMarker, seedMarker
	return f
}

// importedSpec is the Appliance the answers of handoverAnswers describe.
func importedSpec() v1alpha1.ApplianceSpec {
	return v1alpha1.ApplianceSpec{
		Node:  v1alpha1.NodeSpec{HostName: "pki.example.com", Address: "192.0.2.10/24", Gateway: "192.0.2.1"},
		DNS:   v1alpha1.DNSSpec{Servers: []string{"192.0.2.53"}},
		NTP:   v1alpha1.NTPSpec{Servers: []string{"ntp.example.com"}},
		Proxy: &v1alpha1.ProxySpec{HTTP: "http://proxy.example.com:3128", NoProxy: ".example.com"},
		TLS:   v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
	}
}

// cleanupHandover removes the Appliance and the Secrets of the system namespace when the test ends.
func cleanupHandover(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = testClient.Delete(ctx, &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}})
		_ = testClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(SystemNamespace))
	})
}

func createHandoverSecret(t *testing.T, data []byte) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: wizard.HandoverSecretName},
		Data:       map[string][]byte{wizard.HandoverSecretKey: data},
	}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func handoverJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(handoverAnswers())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fakeTalos is a node whose machine configuration mounts the hand-over file until it is removed.
type fakeTalos struct {
	talos.Node
	mu       sync.Mutex
	mounted  bool
	removals int   // removals that took the file out of the configuration
	failWith error // returned once by the next removal, after it took effect
	calls    []string
	closed   int
}

func (f *fakeTalos) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeTalos) RemoveExtensionConfig(_ context.Context, name, file string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+file)
	if f.mounted {
		f.mounted = false
		f.removals++
	}
	if err := f.failWith; err != nil {
		f.failWith = nil
		return err
	}
	return nil
}

func newHandoverReconciler(c client.Client, node *fakeTalos) *HandoverReconciler {
	return &HandoverReconciler{
		Client:   c,
		Users:    auth.NewSecretUserStore(c, SystemNamespace),
		Talos:    func(context.Context) (talos.Node, error) { return node, nil },
		Recorder: events.NewFakeRecorder(10),
	}
}

var handoverRequest = ctrl.Request{NamespacedName: client.ObjectKey{Namespace: SystemNamespace, Name: wizard.HandoverSecretName}}

func reconcileHandover(ctx context.Context, r *HandoverReconciler) (ctrl.Result, error) {
	return r.Reconcile(ctx, handoverRequest)
}

// watchClient is a client with watch support, which the interceptor needs.
func watchClient(t *testing.T) client.WithWatch {
	t.Helper()
	c, err := client.NewWithWatch(testConfig, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var errCrash = errors.New("connection reset by peer")

// failingUsers is a user store whose Create fails, after storing the user when stored is set.
type failingUsers struct {
	auth.UserStore
	stored bool
	err    error
}

func (s failingUsers) Create(ctx context.Context, u auth.User) error {
	if s.stored {
		if err := s.UserStore.Create(ctx, u); err != nil {
			return err
		}
	}
	return s.err
}

func storedUsers(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: auth.UsersSecret}, s); err != nil {
		t.Fatalf("read the users: %v", err)
	}
	users := map[string]json.RawMessage{}
	if err := json.Unmarshal(s.Data["users.json"], &users); err != nil {
		t.Fatal(err)
	}
	return users
}

func handoverSecretExists(t *testing.T) bool {
	t.Helper()
	err := testClient.Get(context.Background(), handoverRequest.NamespacedName, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// TestImportResumesAtEveryStep stops the import at every step, as a crash or a power cut would, and runs it again
// with a fresh reconciler: the hand-over still finishes exactly once.
func TestImportResumesAtEveryStep(t *testing.T) {
	tests := []struct {
		name string
		// crash prepares the first reconciler to stop at its step; nil runs the first pass undisturbed.
		crash func(t *testing.T, r *HandoverReconciler, node *fakeTalos)
		// late creates the Secret only after the first pass, as when the manager starts before first-boot hands over.
		late bool
	}{
		{name: "configuration applied, nothing handed over yet", late: true},
		{name: "Appliance created, answer lost", crash: func(t *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*v1alpha1.Appliance); ok {
						if err := c.Create(ctx, obj, opts...); err != nil {
							return err
						}
						return errCrash
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			r.Users = auth.NewSecretUserStore(r.Client, SystemNamespace)
		}},
		{name: "creating the administrator", crash: func(_ *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Users = failingUsers{UserStore: r.Users, err: errCrash}
		}},
		{name: "administrator created, answer lost", crash: func(_ *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Users = failingUsers{UserStore: r.Users, stored: true, err: errCrash}
		}},
		{name: "no Talos access yet", crash: func(_ *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Talos = func(context.Context) (talos.Node, error) { return nil, ErrNoTalosAccess }
		}},
		{name: "file removed, answer lost", crash: func(_ *testing.T, _ *HandoverReconciler, node *fakeTalos) {
			node.failWith = errCrash
		}},
		{name: "hand-over marked complete, answer lost", crash: func(t *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
						return err
					}
					return errCrash
				},
			})
			r.Users = auth.NewSecretUserStore(r.Client, SystemNamespace)
		}},
		{name: "Secret deleted, answer lost", crash: func(t *testing.T, r *HandoverReconciler, _ *fakeTalos) {
			r.Client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					return errCrash
				},
			})
			r.Users = auth.NewSecretUserStore(r.Client, SystemNamespace)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanupHandover(t)
			ctx := context.Background()
			node := &fakeTalos{mounted: true}
			if !tt.late {
				createHandoverSecret(t, handoverJSON(t))
			}

			first := newHandoverReconciler(testClient, node)
			if tt.crash != nil {
				tt.crash(t, first, node)
			}
			_, _ = reconcileHandover(ctx, first)
			if tt.late {
				if err := testClient.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, &v1alpha1.Appliance{}); !apierrors.IsNotFound(err) {
					t.Fatalf("Appliance read %v before the hand-over, want not found", err)
				}
				createHandoverSecret(t, handoverJSON(t))
			}

			// The restarted manager runs until the Secret is gone, as the watch and the requeues would.
			restarted := newHandoverReconciler(testClient, node)
			for i := 0; handoverSecretExists(t); i++ {
				if i == 3 {
					t.Fatal("the hand-over Secret is still there after three passes")
				}
				if _, err := reconcileHandover(ctx, restarted); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}
			if _, err := reconcileHandover(ctx, restarted); err != nil {
				t.Fatalf("reconcile without the Secret: %v", err)
			}

			list := &v1alpha1.ApplianceList{}
			if err := testClient.List(ctx, list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 1 || !equality.Semantic.DeepEqual(list.Items[0].Spec, importedSpec()) {
				t.Fatalf("Appliances %+v, want one with spec %+v", list.Items, importedSpec())
			}
			if users := storedUsers(t); len(users) != 1 {
				t.Errorf("%d users stored, want one", len(users))
			}
			u, err := restarted.Users.Get(ctx, adminName)
			if err != nil || u.PasswordHash != hashMarker || u.TOTPSeed != seedMarker || u.Role != auth.RoleAdministrator {
				t.Errorf("administrator %q (%v), want the hand-over answers", u.Name, err)
			}
			if node.removals != 1 || node.mounted {
				t.Errorf("file removed %d times, mounted %v: want removed once", node.removals, node.mounted)
			}
			for _, call := range node.calls {
				if call != machineconfig.HandoverService+" "+machineconfig.HandoverPath {
					t.Errorf("RemoveExtensionConfig(%s), want the hand-over file of first-boot", call)
				}
			}
			c := condition(t, getAppliance(t), v1alpha1.ConditionHandoverComplete)
			if c.Status != metav1.ConditionTrue || c.Reason != reasonImported {
				t.Errorf("HandoverComplete %s %s, want True %s", c.Status, c.Reason, reasonImported)
			}
		})
	}
}

func TestImportKeepsExistingAppliance(t *testing.T) {
	cleanupHandover(t)
	existing := v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "existing.example.com"},
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
		Platform: &v1alpha1.PlatformSpec{ILMVersion: "2.17.0"},
	}
	ctx := context.Background()
	if err := testClient.Create(ctx, &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}, Spec: existing}); err != nil {
		t.Fatal(err)
	}
	createHandoverSecret(t, handoverJSON(t))
	node := &fakeTalos{mounted: true}
	r := newHandoverReconciler(testClient, node)

	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatal(err)
	}
	a := getAppliance(t)
	if !equality.Semantic.DeepEqual(a.Spec, existing) {
		t.Errorf("spec %+v, want the existing %+v", a.Spec, existing)
	}
	if _, err := r.Users.Get(ctx, adminName); err != nil {
		t.Errorf("administrator not created: %v", err)
	}
	if c := condition(t, a, v1alpha1.ConditionHandoverComplete); c.Status != metav1.ConditionTrue {
		t.Errorf("HandoverComplete %s, want True", c.Status)
	}
	if handoverSecretExists(t) || node.mounted {
		t.Errorf("Secret left %v, file mounted %v: want both gone", handoverSecretExists(t), node.mounted)
	}
}

// The Appliance is in setup state restore also when the appliance reconciler started part 2 between the creation and
// the import, and the administrator is created.
func TestHandoverModeRestore(t *testing.T) {
	cleanupHandover(t)
	answers := handoverAnswers()
	answers.Mode = wizard.ModeRestore
	data, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	createHandoverSecret(t, data)
	ctx := context.Background()
	node := &fakeTalos{mounted: true}
	r := newHandoverReconciler(testClient, node)
	node.failWith = errCrash

	if _, err := reconcileHandover(ctx, r); !errors.Is(err, errCrash) {
		t.Fatalf("first import: %v, want the crash", err)
	}
	a := getAppliance(t)
	a.Status.Setup = v1alpha1.SetupPart2
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatal(err)
	}

	a = getAppliance(t)
	if a.Status.Setup != v1alpha1.SetupRestore {
		t.Errorf("setup %q, want restore", a.Status.Setup)
	}
	if c := condition(t, a, v1alpha1.ConditionHandoverComplete); c.Status != metav1.ConditionTrue {
		t.Errorf("HandoverComplete %s, want True", c.Status)
	}
	if _, err := r.Users.Get(ctx, adminName); err != nil {
		t.Errorf("administrator not created: %v", err)
	}
}

// A hand-over file without a mode leaves setup to the appliance reconciler, which starts part 2.
func TestHandoverWithoutModeIsNew(t *testing.T) {
	cleanupHandover(t)
	data := handoverJSON(t)
	if bytes.Contains(data, []byte(`"mode"`)) {
		t.Fatalf("the hand-over file of a new appliance carries a mode: %s", data)
	}
	createHandoverSecret(t, data)

	if _, err := reconcileHandover(context.Background(), newHandoverReconciler(testClient, &fakeTalos{mounted: true})); err != nil {
		t.Fatal(err)
	}
	if got := getAppliance(t).Status.Setup; got != "" {
		t.Errorf("setup %q after the import, want it unset for part 2", got)
	}
}

// unusedUsers fails every call: the import must not touch the user store.
type unusedUsers struct{ calls *int }

func (u unusedUsers) Get(context.Context, string) (auth.User, error) {
	*u.calls++
	return auth.User{}, errCrash
}
func (u unusedUsers) Create(context.Context, auth.User) error { *u.calls++; return errCrash }
func (u unusedUsers) Update(context.Context, auth.User) error { *u.calls++; return errCrash }

// TestImportDeletesAReappearedSecretOnly covers first-boot re-creating the Secret from a still mounted file after the
// hand-over completed: the Secret is deleted without touching the users or the Talos API.
func TestImportDeletesAReappearedSecretOnly(t *testing.T) {
	cleanupHandover(t)
	ctx := context.Background()
	a := &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}, Spec: importedSpec()}
	if err := testClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	setCondition(a, v1alpha1.ConditionHandoverComplete, metav1.ConditionTrue, reasonImported, "the setup answers are imported")
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	createHandoverSecret(t, handoverJSON(t))
	userCalls, talosCalls := 0, 0
	r := &HandoverReconciler{
		Client: testClient,
		Users:  unusedUsers{calls: &userCalls},
		Talos: func(context.Context) (talos.Node, error) {
			talosCalls++
			return nil, errCrash
		},
	}

	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if handoverSecretExists(t) {
		t.Error("the re-created hand-over Secret was not deleted")
	}
	if userCalls != 0 || talosCalls != 0 {
		t.Errorf("%d user store and %d Talos calls, want none once the hand-over is complete", userCalls, talosCalls)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionHandoverComplete); c.Status != metav1.ConditionTrue {
		t.Errorf("HandoverComplete %s, want it kept True", c.Status)
	}
}

func TestImportKeepsExistingAdministrator(t *testing.T) {
	cleanupHandover(t)
	ctx := context.Background()
	createHandoverSecret(t, handoverJSON(t))
	r := newHandoverReconciler(testClient, &fakeTalos{mounted: true})
	if err := r.Users.Create(ctx, auth.User{Name: adminName, PasswordHash: "other-hash", TOTPSeed: "OTHERSEED"}); err != nil {
		t.Fatal(err)
	}

	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatal(err)
	}
	if u, err := r.Users.Get(ctx, adminName); err != nil || u.PasswordHash != "other-hash" {
		t.Errorf("administrator changed (%v), want the existing one kept", err)
	}
}

// TestImportReportsAFailedStep checks that a failed step sets a fixed reason and message and that neither the
// condition nor the log carries the error text or a secret.
func TestImportReportsAFailedStep(t *testing.T) {
	const detail = "rpc error: code = Unavailable desc = DETAIL-MARKER"
	tests := []struct {
		name        string
		prepare     func(r *HandoverReconciler, node *fakeTalos)
		wantReason  string
		wantError   bool
		wantRequeue bool
	}{
		{"administrator", func(r *HandoverReconciler, _ *fakeTalos) {
			r.Users = failingUsers{UserStore: r.Users, err: errors.New(detail)}
		}, reasonCreatingAdministrator, true, false},
		{"Talos access", func(r *HandoverReconciler, _ *fakeTalos) {
			r.Talos = func(context.Context) (talos.Node, error) { return nil, ErrNoTalosAccess }
		}, reasonWaitingForTalosAccess, false, true},
		{"Talos dial", func(r *HandoverReconciler, _ *fakeTalos) {
			r.Talos = func(context.Context) (talos.Node, error) { return nil, errors.New(detail) }
		}, reasonWaitingForTalosAccess, false, true},
		{"file removal", func(_ *HandoverReconciler, node *fakeTalos) {
			node.failWith = errors.New(detail)
		}, reasonRemovingHandoverFile, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanupHandover(t)
			createHandoverSecret(t, handoverJSON(t))
			node := &fakeTalos{mounted: true}
			r := newHandoverReconciler(testClient, node)
			tt.prepare(r, node)
			var logs bytes.Buffer
			ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
				logs.WriteString(prefix + args + "\n")
			}, funcr.Options{Verbosity: 10}))

			res, err := reconcileHandover(ctx, r)
			if (err != nil) != tt.wantError {
				t.Errorf("error %v, want one: %v", err, tt.wantError)
			}
			if (res.RequeueAfter > 0) != tt.wantRequeue {
				t.Errorf("requeue after %v, want one: %v", res.RequeueAfter, tt.wantRequeue)
			}
			c := condition(t, getAppliance(t), v1alpha1.ConditionHandoverComplete)
			if c.Status != metav1.ConditionFalse || c.Reason != tt.wantReason {
				t.Errorf("HandoverComplete %s %s, want False %s", c.Status, c.Reason, tt.wantReason)
			}
			for _, marker := range []string{"DETAIL-MARKER", hashMarker, seedMarker} {
				if strings.Contains(c.Message, marker) {
					t.Errorf("condition message %q contains %s", c.Message, marker)
				}
			}
			for _, marker := range []string{hashMarker, seedMarker} {
				if strings.Contains(logs.String(), marker) {
					t.Errorf("log contains %s:\n%s", marker, logs.String())
				}
			}
			if !handoverSecretExists(t) {
				t.Error("the hand-over Secret was deleted before the hand-over completed")
			}
		})
	}
}

func TestUnparseableHandoverDeleted(t *testing.T) {
	without := func(drop func(*wizard.HandoverFile)) []byte {
		f := handoverAnswers()
		drop(&f)
		data, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for name, tc := range map[string]struct {
		data   []byte
		reason string
	}{
		"not JSON":         {[]byte("{" + seedMarker), "it is not JSON"},
		"wrong version":    {[]byte(`{"version":99}`), "its version is not supported"},
		"no password hash": {without(func(f *wizard.HandoverFile) { f.Admin.PasswordHash = "" }), "answers are missing"},
		"no TOTP seed":     {without(func(f *wizard.HandoverFile) { f.Admin.TOTPSeed = "" }), "answers are missing"},
		"unknown mode":     {without(func(f *wizard.HandoverFile) { f.Mode = "upgrade" }), "its mode is not known"},
	} {
		t.Run(name, func(t *testing.T) {
			cleanupHandover(t)
			createHandoverSecret(t, tc.data)
			node := &fakeTalos{mounted: true}
			var logs bytes.Buffer
			ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
				logs.WriteString(prefix + args + "\n")
			}, funcr.Options{}))
			r := newHandoverReconciler(testClient, node)

			if _, err := reconcileHandover(ctx, r); err != nil {
				t.Errorf("reconcile: %v, want the Secret deleted", err)
			}
			if handoverSecretExists(t) {
				t.Error("the invalid hand-over Secret was kept")
			}
			if err := testClient.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, &v1alpha1.Appliance{}); !apierrors.IsNotFound(err) {
				t.Errorf("Appliance read %v, want none created", err)
			}
			if !node.mounted {
				t.Error("the hand-over file was removed for an invalid Secret")
			}
			select {
			case e := <-r.Recorder.(*events.FakeRecorder).Events:
				if !strings.HasPrefix(e, "Warning InvalidHandover ") || !strings.HasSuffix(e, ": "+tc.reason) ||
					strings.Contains(e, seedMarker) || strings.Contains(e, hashMarker) {
					t.Errorf("event %q, want a warning ending in %q that quotes nothing of the Secret", e, tc.reason)
				}
			default:
				t.Error("no event records the deleted Secret")
			}
			for _, marker := range []string{seedMarker, hashMarker} {
				if strings.Contains(logs.String(), marker) {
					t.Errorf("log quotes the Secret:\n%s", logs.String())
				}
			}
		})
	}
}

func TestUnparseableHandoverGoneClaimsNoDeletion(t *testing.T) {
	cleanupHandover(t)
	createHandoverSecret(t, []byte("{"))
	r := newHandoverReconciler(testClient, &fakeTalos{mounted: true})
	r.Client = interceptor.NewClient(watchClient(t), interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewNotFound(corev1.Resource("secrets"), wizard.HandoverSecretName)
		},
	})
	var logs bytes.Buffer
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		logs.WriteString(prefix + args + "\n")
	}, funcr.Options{}))
	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatalf("reconcile: %v, want a Secret deleted meanwhile ignored", err)
	}
	select {
	case e := <-r.Recorder.(*events.FakeRecorder).Events:
		t.Errorf("event %q for a Secret this reconcile did not delete", e)
	default:
	}
	if strings.Contains(logs.String(), "deleted") {
		t.Errorf("log claims a deletion:\n%s", logs.String())
	}
}

func TestHandoverSetupWithManagerImports(t *testing.T) {
	cleanupHandover(t)
	skipNameValidation := true
	mgr, err := ctrl.NewManager(testConfig, ctrl.Options{
		Scheme:     testClient.Scheme(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Cache:      CacheOptions(),
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatal(err)
	}
	node := &fakeTalos{mounted: true}
	r := newHandoverReconciler(mgr.GetClient(), node)
	r.Users = auth.NewSecretUserStore(testClient, SystemNamespace)
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	// Another Secret in the namespace is not the hand-over.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "other"}}
	if err := testClient.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	createHandoverSecret(t, handoverJSON(t))
	deadline := time.Now().Add(10 * time.Second)
	for handoverSecretExists(t) {
		if time.Now().After(deadline) {
			t.Fatal("the manager did not import the hand-over")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !equality.Semantic.DeepEqual(getAppliance(t).Spec, importedSpec()) {
		t.Errorf("spec %+v, want the imported one", getAppliance(t).Spec)
	}
}

func TestHandoverPredicateSelectsTheSecret(t *testing.T) {
	tests := map[string]struct {
		obj  client.Object
		want bool
	}{
		"hand-over":       {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: wizard.HandoverSecretName}}, true},
		"other name":      {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "other"}}, false},
		"other namespace": {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: wizard.HandoverSecretName}}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isHandoverSecret(tt.obj); got != tt.want {
				t.Errorf("isHandoverSecret = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTalosAccessWaitsForTheTalosconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	var dialled [][]byte
	dialErr := errors.New("dial failed")
	node := &fakeTalos{}
	dial := func(_ context.Context, endpoint string, cfg []byte) (talos.Node, error) {
		if endpoint != "127.0.0.1:50000" {
			t.Errorf("dialled %s, want the configured endpoint", endpoint)
		}
		dialled = append(dialled, cfg)
		if dialErr != nil {
			return nil, dialErr
		}
		return node, nil
	}
	access := TalosAccess(path, "127.0.0.1:50000", dial)
	ctx := context.Background()

	if _, err := access(ctx); !errors.Is(err, ErrNoTalosAccess) {
		t.Fatalf("error %v without a talosconfig, want ErrNoTalosAccess", err)
	}
	if err := os.WriteFile(path, []byte("talosconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := access(ctx); !errors.Is(err, dialErr) {
		t.Fatalf("error %v, want the dial error", err)
	}
	dialErr = nil
	for range 2 {
		got, err := access(ctx)
		if err != nil || got != node {
			t.Fatalf("access = %v, %v, want the dialled node", got, err)
		}
	}
	if len(dialled) != 2 || string(dialled[1]) != "talosconfig" {
		t.Errorf("dialled %q, want twice with the talosconfig: the node is kept once dialled", dialled)
	}

	// Talos re-issues the talosconfig: the old client is closed and the new one dialled.
	if err := os.WriteFile(path, []byte("reissued"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := access(ctx); err != nil || got != node {
		t.Fatalf("access = %v, %v, want the node dialled again", got, err)
	}
	if len(dialled) != 3 || string(dialled[2]) != "reissued" || node.closed != 1 {
		t.Errorf("dialled %q, closed %d times: want the re-issued talosconfig dialled and the old client closed", dialled, node.closed)
	}

	// The Secret is gone again: the client is dropped.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := access(ctx); !errors.Is(err, ErrNoTalosAccess) || node.closed != 2 {
		t.Errorf("error %v, closed %d times: want ErrNoTalosAccess and the client closed", err, node.closed)
	}

	fresh := TalosAccess(t.TempDir(), "127.0.0.1:50000", dial)
	if _, err := fresh(ctx); err == nil || errors.Is(err, ErrNoTalosAccess) {
		t.Errorf("error %v for an unreadable talosconfig, want a read error", err)
	}
}

// TestImportRunsWithTheManagerRBAC runs the import as the manager's ServiceAccount with the RBAC of its manifest.
func TestImportRunsWithTheManagerRBAC(t *testing.T) {
	cleanupHandover(t)
	ctx := context.Background()
	c := managerClient(t)
	createHandoverSecret(t, handoverJSON(t))
	r := newHandoverReconciler(c, &fakeTalos{mounted: true})
	if _, err := reconcileHandover(ctx, r); err != nil {
		t.Fatalf("import as the manager: %v", err)
	}
	if handoverSecretExists(t) {
		t.Error("the hand-over Secret is still there")
	}
	if _, err := r.Users.Get(ctx, adminName); err != nil {
		t.Errorf("administrator: %v", err)
	}
}

// managerClient returns a client that acts as the manager's ServiceAccount with the RBAC of addons/manager.yaml.
func managerClient(t *testing.T) client.Client {
	t.Helper()
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("..", "..", "addons", "manager.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if obj.GroupVersionKind().Group != rbacv1.GroupName || !slices.Contains([]string{"", SystemNamespace, platform.Namespace}, obj.GetNamespace()) {
			continue
		}
		if err := testClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })
	}

	cfg := rest.CopyConfig(testConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + SystemNamespace + ":appliance-manager"}
	c, err := client.New(cfg, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
