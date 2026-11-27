package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	wizard "github.com/OmniTrustILM/appliance-ng/internal/setup"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Reasons of the HandoverComplete condition. Its messages are fixed texts: first-boot shows them on the console,
// so they never carry an error, which could quote anything.
const (
	reasonImported              = "Imported"
	reasonCreatingAdministrator = "CreatingAdministrator"
	reasonWaitingForTalosAccess = "WaitingForTalosAccess"
	reasonRemovingHandoverFile  = "RemovingHandoverFile"
)

// talosAccessRequeue is how often the import checks again for access to the Talos API.
const talosAccessRequeue = 10 * time.Second

// ErrNoTalosAccess is returned by a Talos provider while the talosconfig of the manager is not mounted yet.
var ErrNoTalosAccess = errors.New("no Talos API access yet")

// HandoverReconciler imports the answers of wizard part 1 from the hand-over Secret, once: it creates the Appliance
// and the administrator, removes the hand-over file from the machine configuration, marks the hand-over complete
// and deletes the Secret. Each step is skipped when it is already done, so a crash at any point resumes. A Secret
// without a valid hand-over file is deleted with an Event.
type HandoverReconciler struct {
	Client   client.Client
	Users    auth.UserStore
	Recorder events.EventRecorder
	// Talos returns the Talos API of the node, or ErrNoTalosAccess while there is none yet.
	Talos func(context.Context) (talos.Node, error)
}

// SetupWithManager watches the hand-over Secret only.
func (r *HandoverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("handover").
		For(&corev1.Secret{}, builder.WithPredicates(predicate.NewPredicateFuncs(isHandoverSecret))).
		Complete(r)
}

func isHandoverSecret(o client.Object) bool {
	return o.GetNamespace() == SystemNamespace && o.GetName() == wizard.HandoverSecretName
}

// Reconcile runs the import steps that are not done yet. The Secret and the answers are never logged or reported:
// they hold the administrator's password hash and TOTP seed.
func (r *HandoverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, req.NamespacedName, secret); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	answers, invalid := parseHandover(secret)
	if invalid != "" {
		return ctrl.Result{}, r.deleteInvalid(ctx, secret, invalid)
	}

	a, err := r.ensureAppliance(ctx, answers)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionHandoverComplete) {
		result, err := r.importAnswers(ctx, a, answers)
		if err != nil || !result.IsZero() {
			return result, err
		}
	}
	if err := r.Client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("delete the hand-over Secret: %w", err)
	}
	logger.Info("hand-over complete")
	return ctrl.Result{}, nil
}

// deleteInvalid deletes a hand-over Secret that holds no valid hand-over file: it cannot be imported, and it may hold
// a password hash and a TOTP seed. The log and the Event name the fixed reason only, never the content, and only
// when this reconcile deleted the Secret. A Secret changed since it was read conflicts and is read again.
func (r *HandoverReconciler) deleteInvalid(ctx context.Context, secret *corev1.Secret, reason string) error {
	err := r.Client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete the invalid hand-over Secret: %w", err)
	}
	log.FromContext(ctx).Error(nil, "deleted the hand-over Secret, which holds no valid hand-over file", "reason", reason)
	r.Recorder.Eventf(secret, nil, corev1.EventTypeWarning, "InvalidHandover", "Delete",
		"deleted the hand-over Secret, which holds no valid hand-over file: %s", reason)
	return nil
}

// importAnswers creates the administrator, removes the hand-over file and marks the hand-over complete. A failed
// step is reported with a fixed reason and message; the error itself goes only to the log.
func (r *HandoverReconciler) importAnswers(ctx context.Context, a *v1alpha1.Appliance, answers *wizard.HandoverFile) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if err := r.ensureRestoreSetup(ctx, a, answers); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureAdministrator(ctx, answers); err != nil {
		logger.Error(err, "could not create the administrator")
		return ctrl.Result{}, r.failed(ctx, a, reasonCreatingAdministrator, "creating the administrator failed, retrying", err)
	}
	node, err := r.Talos(ctx)
	if err != nil {
		logger.Info("waiting for access to the Talos API", "error", err)
		return ctrl.Result{RequeueAfter: talosAccessRequeue},
			r.setHandover(ctx, a, metav1.ConditionFalse, reasonWaitingForTalosAccess, messageWaitingForTalosAccess)
	}
	if err := node.RemoveExtensionConfig(ctx, machineconfig.HandoverService, machineconfig.HandoverPath); err != nil {
		logger.Error(err, "could not remove the hand-over file from the machine configuration")
		return ctrl.Result{}, r.failed(ctx, a, reasonRemovingHandoverFile, "removing the hand-over file failed, retrying", err)
	}
	if err := r.setHandover(ctx, a, metav1.ConditionTrue, reasonImported, "the setup answers are imported"); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("setup answers imported", "user", answers.Admin.User)
	return ctrl.Result{}, nil
}

// ensureRestoreSetup puts an Appliance of a restore hand-over in setup state restore, which keeps wizard part 2
// closed. The appliance reconciler may have started part 2 between the creation and this step, so that state is
// taken over as well; an Appliance further along is left alone.
func (r *HandoverReconciler) ensureRestoreSetup(ctx context.Context, a *v1alpha1.Appliance, f *wizard.HandoverFile) error {
	if f.Mode != wizard.ModeRestore || (a.Status.Setup != "" && a.Status.Setup != v1alpha1.SetupPart2) {
		return nil
	}
	a.Status.Setup = v1alpha1.SetupRestore
	if err := r.Client.Status().Update(ctx, a, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("set the setup state to restore: %w", err)
	}
	log.FromContext(ctx).Info("the appliance restores a backup instead of running wizard part 2")
	return nil
}

// Reasons a hand-over file is refused; fixed texts, so they quote nothing of the file.
const (
	invalidNotJSON = "it is not JSON"
	invalidVersion = "its version is not supported"
	invalidMode    = "its mode is not known"
	invalidMissing = "answers are missing"
)

// parseHandover decodes and checks the hand-over file of the Secret and returns it, or the reason it is refused.
// Decoding errors may quote the content, so they are dropped.
func parseHandover(s *corev1.Secret) (f *wizard.HandoverFile, invalid string) {
	f = &wizard.HandoverFile{}
	if err := json.Unmarshal(s.Data[wizard.HandoverSecretKey], f); err != nil {
		return nil, invalidNotJSON
	}
	switch {
	case f.Version != wizard.HandoverVersion:
		return nil, invalidVersion
	case f.Mode != "" && f.Mode != wizard.ModeNew && f.Mode != wizard.ModeRestore:
		return nil, invalidMode
	// An empty seed would be a TOTP key everyone knows, so an account without a hash and a seed is refused.
	case f.Node.HostName == "" || f.Admin.User == "" || f.Admin.PasswordHash == "" || f.Admin.TOTPSeed == "":
		return nil, invalidMissing
	}
	return f, ""
}

// ensureAppliance creates the Appliance from the answers unless one exists, which is never changed.
func (r *HandoverReconciler) ensureAppliance(ctx context.Context, f *wizard.HandoverFile) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a)
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return nil, fmt.Errorf("read the Appliance: %w", err)
		}
		return a, nil
	}
	a = importedAppliance(&f.Node)
	if err := r.Client.Create(ctx, a, client.FieldOwner(fieldManager)); err != nil {
		return nil, fmt.Errorf("create the Appliance: %w", err)
	}
	log.FromContext(ctx).Info("created the Appliance from the setup answers")
	return a, nil
}

// importedAppliance is the Appliance of the node answers: the built-in certificate and no platform yet, which
// wizard part 2 adds.
func importedAppliance(n *wizard.NodeAnswers) *v1alpha1.Appliance {
	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec: v1alpha1.ApplianceSpec{
			Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{
				{HostName: n.HostName, Address: n.Address, Gateway: n.Gateway, Role: v1alpha1.NodeRoleVoting},
			}},
			DNS: v1alpha1.DNSSpec{Servers: n.DNS},
			NTP: v1alpha1.NTPSpec{Servers: n.NTP},
			TLS: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
		},
	}
	if p := n.Proxy; p != nil {
		a.Spec.Proxy = &v1alpha1.ProxySpec{HTTP: p.HTTP, HTTPS: p.HTTPS, NoProxy: p.NoProxy}
	}
	return a
}

// ensureAdministrator creates the administrator unless a user of that name exists, which is never changed.
func (r *HandoverReconciler) ensureAdministrator(ctx context.Context, f *wizard.HandoverFile) error {
	_, err := r.Users.Get(ctx, f.Admin.User)
	if !errors.Is(err, auth.ErrUserNotFound) {
		return err
	}
	err = r.Users.Create(ctx, auth.User{
		Name:         f.Admin.User,
		PasswordHash: f.Admin.PasswordHash,
		TOTPSeed:     f.Admin.TOTPSeed,
		Role:         auth.RoleAdministrator,
		Created:      time.Now(),
	})
	if err != nil && !errors.Is(err, auth.ErrUserExists) {
		return err
	}
	return nil
}

// failed reports a failed step and returns cause, so the request is retried with back-off.
func (r *HandoverReconciler) failed(ctx context.Context, a *v1alpha1.Appliance, reason, message string, cause error) error {
	if err := r.setHandover(ctx, a, metav1.ConditionFalse, reason, message); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// setHandover writes the HandoverComplete condition when it changes.
func (r *HandoverReconciler) setHandover(ctx context.Context, a *v1alpha1.Appliance, status metav1.ConditionStatus, reason, message string) error {
	before := a.Status.DeepCopy()
	setCondition(a, v1alpha1.ConditionHandoverComplete, status, reason, message)
	if equality.Semantic.DeepEqual(before, &a.Status) {
		return nil
	}
	if err := r.Client.Status().Update(ctx, a, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update the Appliance status: %w", err)
	}
	return nil
}

// TalosAccess returns a provider of the Talos API at endpoint, dialled with dial and the talosconfig at path. The
// talosconfig Secret is mounted only once Talos created it, so until the file exists the provider returns
// ErrNoTalosAccess. The file is read on every call: the node is kept while it is unchanged, and dialled again when
// Talos re-issues it. A replaced or dropped client is closed.
func TalosAccess(path, endpoint string, dial func(context.Context, string, []byte) (talos.Node, error)) func(context.Context) (talos.Node, error) {
	var (
		mu      sync.Mutex
		node    talos.Node
		dialled []byte
	)
	drop := func() {
		if c, ok := node.(io.Closer); ok {
			_ = c.Close()
		}
		node, dialled = nil, nil
	}
	return func(ctx context.Context) (talos.Node, error) {
		mu.Lock()
		defer mu.Unlock()
		cfg, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			drop()
			return nil, ErrNoTalosAccess
		case err != nil:
			return nil, fmt.Errorf("read the talosconfig: %w", err)
		case node != nil && bytes.Equal(cfg, dialled):
			return node, nil
		}
		drop()
		n, err := dial(ctx, endpoint, cfg)
		if err != nil {
			return nil, err
		}
		node, dialled = n, cfg
		return node, nil
	}
}
