// Package manager reconciles the add-ons, the ILM Platform and its Connectors from the Appliance.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	fieldManager = kube.ManagerFieldManager
	// SystemNamespace holds the Appliance's own Secrets, which the spec references by name.
	SystemNamespace = kube.SystemNamespace
	// addonsRequeue is how often a pass is repeated while an add-on is not ready; add-on objects are not watched.
	addonsRequeue = 10 * time.Second
	// resync is how often the add-on pass is repeated once everything is ready, because add-on objects are not
	// watched and drift in them is corrected only by a pass.
	resync = 5 * time.Minute
	// keyBytes is the length of a generated connector key before base64 encoding.
	keyBytes = 32
)

// Names of the copies, in the platform namespace, of the Secrets the Appliance references.
const (
	TLSSecretName   = platform.TLSSecretName
	TrustSecretName = certs.TrustSecretName
	// PullSecretName is the dockerconfigjson Secret with the credentials of every registry that has some.
	PullSecretName = "appliance-registry-credentials"
	// MailSecretName holds the SMTP credentials of the mail server.
	MailSecretName = "appliance-mail"
)

// Keys of a registry credentials Secret in the system namespace.
const (
	RegistryUsernameKey = v1alpha1.CredentialsUsernameKey
	RegistryPasswordKey = v1alpha1.CredentialsPasswordKey
)

// Keys of the mail credentials Secret in the system namespace and of its copy, which the email connector reads.
const (
	MailUsernameKey = v1alpha1.CredentialsUsernameKey
	MailPasswordKey = v1alpha1.CredentialsPasswordKey
)

// ApplianceReconciler applies the add-ons and the ILM Platform and Connectors the Appliance describes.
type ApplianceReconciler struct {
	Client client.Client
	// APIReader reads from the API server directly, for objects outside the cache such as Secrets in the platform
	// namespace.
	APIReader client.Reader
	Cluster   kube.Cluster
	Steps     []kube.Step
	Addons    fs.FS
	Release   *release.Manifest
	Recorder  events.EventRecorder
	// Version is the manager's own version, set when it is built.
	Version string
	// DataDir is the directory the data volume is mounted at, whose free space the status reports.
	DataDir string
	// Audit records the expired kubeconfigs the reconciler revokes.
	Audit AuditWriter
	// Schedule, when set, schedules the backups of the targets on every reconcile.
	Schedule BackupScheduler

	// The operator's kinds are watched only once its CRDs are served: the reconciler installs them itself.
	controller  controller.Controller
	cache       cache.Cache
	watchMu     sync.Mutex
	watchedKind map[string]bool

	// The last completed full add-on pass. The controller runs one reconcile at a time, so no lock is needed.
	passed     bool
	passedAt   time.Time
	passedGen  int64
	passedHeld bool
	now        func() time.Time
}

// AuditWriter appends entries to the audit log.
type AuditWriter interface {
	Write(e audit.Entry) error
}

// BackupScheduler schedules the backups of the targets; Sync is called on every reconcile.
type BackupScheduler interface {
	Sync(targets []v1alpha1.BackupTarget)
}

// AuditActor is the user of the audit entries the manager writes on its own.
const AuditActor = audit.SystemUser

const (
	auditKubeconfigRevoke = "kubeconfig-revoke"
	auditSuccess          = "success"
)

// CacheOptions limits the informer cache of Secrets to the system namespace: the reconciler reads only the
// Secrets the Appliance references there, and its RBAC grants no cluster-wide read of Secrets.
func CacheOptions() cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}: {Namespaces: map[string]cache.Config{SystemNamespace: {}}},
	}}
}

// SetupWithManager watches the Appliance and the Secrets it may reference. The Platform and Connectors it owns
// are watched from the first reconcile that finds the operator's CRDs served, see watchOperator.
func (r *ApplianceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Appliance{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(applianceRequest), builder.WithPredicates(inSystemNamespace)).
		Build(r)
	if err != nil {
		return err
	}
	r.controller, r.cache = c, mgr.GetCache()
	return nil
}

// watchOperator starts watching the Platform and Connectors the Appliance owns, like Owns would, once each kind
// is served. A kind is watched once the RESTMapping check passes; until then every call returns an error, and
// the next reconcile checks again.
func (r *ApplianceReconciler) watchOperator(ctx context.Context) error {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	// A reconciler not set up with a manager has no controller to add watches to.
	if r.controller == nil {
		return nil
	}
	if r.watchedKind == nil {
		r.watchedKind = map[string]bool{}
	}
	scheme, mapper := r.Client.Scheme(), r.Client.RESTMapper()
	for _, obj := range []client.Object{&otilm.Platform{}, &otilm.Connector{}} {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return err
		}
		if r.watchedKind[gvk.Kind] {
			continue
		}
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			return fmt.Errorf("the operator does not serve %s yet: %w", gvk.Kind, err)
		}
		owner := handler.EnqueueRequestForOwner(scheme, mapper, &v1alpha1.Appliance{}, handler.OnlyControllerOwner())
		if err := r.controller.Watch(source.Kind(r.cache, obj, owner)); err != nil {
			return fmt.Errorf("watch %s: %w", gvk.Kind, err)
		}
		r.watchedKind[gvk.Kind] = true
		log.FromContext(ctx).Info("watching the operator kind", "kind", gvk.Kind)
	}
	return nil
}

// inSystemNamespace lets through the events of the objects in the system namespace.
var inSystemNamespace = predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == SystemNamespace })

// applianceRequest maps any object to the single Appliance.
func applianceRequest(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}}
}

// Reconcile runs one pass over the add-ons and the platform and records the outcome in the status.
func (r *ApplianceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	a := &v1alpha1.Appliance{}
	if err := r.Client.Get(ctx, req.NamespacedName, a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if a.Status.Setup == "" {
		a.Status.Setup = v1alpha1.SetupPart2
		if err := r.Client.Status().Update(ctx, a, client.FieldOwner(fieldManager)); err != nil {
			return ctrl.Result{}, fmt.Errorf("start setup part 2: %w", err)
		}
	}
	if r.Schedule != nil {
		var targets []v1alpha1.BackupTarget
		if a.Spec.Backup != nil {
			targets = a.Spec.Backup.Targets
		}
		r.Schedule.Sync(targets)
	}
	before := a.Status.DeepCopy()
	r.setReleaseVersions(a)
	r.setDataVolume(ctx, a)
	r.removeExpiredAccess(ctx)
	result, err := r.reconcile(ctx, a)
	if serr := r.writeStatus(ctx, a, before); serr != nil {
		return ctrl.Result{}, errors.Join(err, serr)
	}
	return result, err
}

// removeExpiredAccess revokes the issued kubeconfigs that expired and drops the records of expired talosconfigs. A
// failure is logged and tried again on the next pass; it does not hold up the appliance.
func (r *ApplianceReconciler) removeExpiredAccess(ctx context.Context) {
	revoked, err := access.RemoveExpired(ctx, r.Client, time.Now())
	if err != nil {
		log.FromContext(ctx).Error(err, "remove the expired cluster access")
	}
	for _, c := range revoked {
		log.FromContext(ctx).Info("expired kubeconfig revoked", "name", c.Name, "role", c.Role)
		entry := audit.Entry{
			Time: time.Now().UTC(), User: AuditActor, Action: auditKubeconfigRevoke, Result: auditSuccess,
			Details: map[string]string{"name": c.Name, "role": c.Role, "expires": c.Expires.UTC().Format(time.RFC3339)},
		}
		if err := r.Audit.Write(entry); err != nil {
			log.FromContext(ctx).Error(err, "record the revoked kubeconfig in the audit log", "name", c.Name)
		}
	}
}

func (r *ApplianceReconciler) reconcile(ctx context.Context, a *v1alpha1.Appliance) (ctrl.Result, error) {
	progress, err := r.addonsProgress(ctx, a)
	if err != nil {
		setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, reasonFailed, messageAddonsFailed)
		return ctrl.Result{}, err
	}
	certificate := r.reconcileCertificate(ctx, a, progress)
	if !progress.Done {
		setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, reasonProgressing, progress.Step+": "+progress.Message)
		return ctrl.Result{RequeueAfter: addonsRequeue}, nil
	}
	setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionTrue, reasonReady, "all add-ons are ready")
	if a.Spec.Platform == nil {
		return requeue(certificate), nil
	}
	if a.Status.Restore.PausesPlatform() {
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonRestoring, "a restore has stopped the ILM operator")
		return ctrl.Result{RequeueAfter: addonsRequeue}, nil
	}
	if err := r.watchOperator(ctx); err != nil {
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonWaitingForOperator, "waiting for the ILM operator to serve its kinds")
		return ctrl.Result{}, err
	}

	// The operator's status updates of the owned Platform wake the reconciler; the periodic requeue repeats the
	// full add-on pass.
	ready, message, err := r.reconcilePlatform(ctx, a)
	switch {
	case err != nil:
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonFailed, messagePlatformFailed)
		return ctrl.Result{}, err
	case !ready:
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonProgressing, message)
		return requeue(certificate), nil
	}
	setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, reasonReady, message)
	if a.Status.Setup == v1alpha1.SetupPart2 {
		a.Status.Setup = v1alpha1.SetupComplete
		log.FromContext(ctx).Info("setup complete")
	}
	return requeue(certificate), nil
}

// requeue repeats the add-on pass after resync, or sooner when the certificate asks for it.
func requeue(certificate time.Duration) ctrl.Result {
	if certificate > 0 && certificate < resync {
		return ctrl.Result{RequeueAfter: certificate}
	}
	return ctrl.Result{RequeueAfter: resync}
}

// addonsProgress runs the full add-on pass, which applies every step, on the first reconcile, when the
// Appliance's generation changed since the last complete pass and when resync has elapsed since then. Other
// reconciles, woken by Platform, Connector and Secret events, only check that the steps are still ready. A pass
// that did not complete is not recorded, so the next reconcile applies again.
func (r *ApplianceReconciler) addonsProgress(ctx context.Context, a *v1alpha1.Appliance) (addons.Progress, error) {
	if r.now == nil {
		r.now = time.Now
	}
	held, err := r.holdsOperator(ctx)
	if err != nil {
		return addons.Progress{}, err
	}
	if r.passed && a.Generation == r.passedGen && held == r.passedHeld && r.now().Sub(r.passedAt) < resync {
		return addons.Check(ctx, r.Cluster, r.Steps)
	}
	progress, err := addons.Pass(ctx, r.Cluster, r.Steps, r.Addons, restoreHolds(held))
	r.passed = err == nil && progress.Done
	if r.passed {
		r.passedAt, r.passedGen, r.passedHeld = r.now(), a.Generation, held
	}
	return progress, err
}

// holdsOperator tells whether a restore needs the ILM operator stopped, from the Appliance as the API server has it:
// the cached one may not show yet the step that stopped the operator.
func (r *ApplianceReconciler) holdsOperator(ctx context.Context) (bool, error) {
	live := &v1alpha1.Appliance{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, live); err != nil {
		return false, fmt.Errorf("read the restore status: %w", err)
	}
	return live.Status.Restore.HoldsOperator(), nil
}

// restoreHolds keeps the ILM operator at zero replicas while held.
func restoreHolds(held bool) addons.Holds {
	if !held {
		return nil
	}
	return addons.Holds{platform.OperatorNamespace + "/" + platform.OperatorDeployment: 0}
}

// reconcilePlatform applies the Platform and Connectors with the Secrets they read and reports whether the
// Platform is ready.
func (r *ApplianceReconciler) reconcilePlatform(ctx context.Context, a *v1alpha1.Appliance) (bool, string, error) {
	secrets, err := r.copySecrets(ctx, a)
	if err != nil {
		return false, "", err
	}
	ip, ok, err := certs.NodeAddress(ctx, r.Client, a)
	if err != nil {
		return false, "", err
	}
	nodeIP := ""
	if ok {
		nodeIP = ip.String()
	}
	p, connectors, err := platform.Render(a, r.Release, secrets, nodeIP)
	if err != nil {
		return false, "", fmt.Errorf("render the platform: %w", err)
	}
	if err := r.createKeySecrets(ctx, connectors); err != nil {
		return false, "", err
	}
	if _, err := r.apply(ctx, a, platform.TLSOption(p)); err != nil {
		return false, "", err
	}
	content, err := platform.Unstructured(p)
	if err != nil {
		return false, "", err
	}
	applied, err := r.apply(ctx, a, content)
	if err != nil {
		return false, "", err
	}
	observed := make([]otilm.Connector, len(connectors))
	for i := range connectors {
		appliedConnector, err := r.apply(ctx, a, &connectors[i])
		if err != nil {
			return false, "", err
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(appliedConnector.Object, &observed[i]); err != nil {
			return false, "", fmt.Errorf("read the applied connector %s: %w", connectors[i].Name, err)
		}
	}
	if err := r.deleteRemovedConnectors(ctx, a, connectors); err != nil {
		return false, "", err
	}
	setConnectorsCondition(a, observed)
	current := &otilm.Platform{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(applied.Object, current); err != nil {
		return false, "", fmt.Errorf("read the applied Platform: %w", err)
	}
	keepKnown(&a.Status.Versions.ILM, current.Status.ObservedVersion)
	ready, message := platformReady(current)
	return ready, message, nil
}

// deleteRemovedConnectors deletes the Connectors the Appliance controls that are no longer among its components.
// Their key Secrets have no owner and stay. The list is read from the API server: the Connector cache exists only
// once the operator's CRDs are served.
func (r *ApplianceReconciler) deleteRemovedConnectors(ctx context.Context, a *v1alpha1.Appliance, desired []otilm.Connector) error {
	current := &otilm.ConnectorList{}
	if err := r.APIReader.List(ctx, current, client.InNamespace(platform.Namespace)); err != nil {
		return fmt.Errorf("list connectors in %s: %w", platform.Namespace, err)
	}
	for i := range current.Items {
		c := &current.Items[i]
		wanted := slices.ContainsFunc(desired, func(d otilm.Connector) bool { return d.Name == c.Name })
		if wanted || !metav1.IsControlledBy(c, a) {
			continue
		}
		if err := r.Client.Delete(ctx, c, client.Preconditions{UID: &c.UID}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete connector %s/%s: %w", c.Namespace, c.Name, err)
		}
		log.FromContext(ctx).Info("deleted a connector no longer among the components", "connector", c.Namespace+"/"+c.Name)
	}
	return nil
}

// copySecrets copies the certificate and key of the TLS Secret, the referenced key of the trust Secret, the
// registry credentials and the mail credentials the spec references into the platform namespace, and returns the
// names of the copies. The copy of trust, registry or mail credentials the spec no longer references is deleted.
func (r *ApplianceReconciler) copySecrets(ctx context.Context, a *v1alpha1.Appliance) (platform.SecretNames, error) {
	var names platform.SecretNames
	src, err := r.tlsSecret(ctx, a)
	if err != nil {
		return names, err
	}
	if src != nil {
		if src.Type != corev1.SecretTypeTLS {
			return names, fmt.Errorf("secret %s/%s has type %q, want %s", SystemNamespace, src.Name, src.Type, corev1.SecretTypeTLS)
		}
		pair := map[string][]byte{corev1.TLSCertKey: src.Data[corev1.TLSCertKey], corev1.TLSPrivateKeyKey: src.Data[corev1.TLSPrivateKeyKey]}
		if err := r.applySecret(ctx, a, TLSSecretName, corev1.SecretTypeTLS, pair); err != nil {
			return names, err
		}
		names.TLS = TLSSecretName
	}
	if names.Trust, err = r.copyTrust(ctx, a); err != nil {
		return names, err
	}
	pull, err := r.copyRegistryCredentials(ctx, a)
	if err != nil {
		return names, err
	}
	names.PullSecret = pull
	if names.Mail, err = r.copyMailCredentials(ctx, a); err != nil {
		return names, err
	}
	return names, nil
}

// tlsSecret returns the Secret the spec references for TLS, or the built-in certificate's Secret once cert-manager
// issued it; nil means the built-in certificate is not issued yet.
func (r *ApplianceReconciler) tlsSecret(ctx context.Context, a *v1alpha1.Appliance) (*corev1.Secret, error) {
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		return systemSecret(ctx, r.Client, a.Spec.TLS.SecretRef)
	}
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.SecretName}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", certs.Namespace, certs.SecretName, err)
	case len(s.Data[corev1.TLSCertKey]) == 0 || len(s.Data[corev1.TLSPrivateKeyKey]) == 0:
		return nil, nil
	}
	return s, nil
}

// dockerAuth is an entry of the auths of a dockerconfigjson Secret.
type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// copyTrust copies the CA bundle spec.trust references into the platform namespace and returns the copy's name, or
// deletes the copy and returns "" without one.
func (r *ApplianceReconciler) copyTrust(ctx context.Context, a *v1alpha1.Appliance) (string, error) {
	if a.Spec.Trust == nil {
		return "", r.deleteCopy(ctx, TrustSecretName)
	}
	bundle, err := trustBundle(ctx, r.Client, a)
	if err != nil {
		return "", err
	}
	if err := r.applySecret(ctx, a, TrustSecretName, corev1.SecretTypeOpaque, map[string][]byte{platform.TrustKey: bundle}); err != nil {
		return "", err
	}
	return TrustSecretName, nil
}

// copyRegistryCredentials writes the credentials of every registry that references some into one pull Secret in
// the platform namespace and returns its name, or deletes the pull Secret and returns "" when no registry has
// credentials.
func (r *ApplianceReconciler) copyRegistryCredentials(ctx context.Context, a *v1alpha1.Appliance) (string, error) {
	registries, err := registryCredentials(ctx, r.Client, a)
	if err != nil {
		return "", err
	}
	if len(registries) == 0 {
		return "", r.deleteCopy(ctx, PullSecretName)
	}
	auths := make(map[string]dockerAuth, len(registries))
	for _, registry := range registries {
		auths[registry.Host] = dockerAuth{
			Username: registry.Username,
			Password: registry.Password,
			Auth:     base64.StdEncoding.EncodeToString([]byte(registry.Username + ":" + registry.Password)),
		}
	}
	config, err := json.Marshal(map[string]map[string]dockerAuth{"auths": auths})
	if err != nil {
		return "", fmt.Errorf("encode the registry credentials: %w", err)
	}
	if err := r.applySecret(ctx, a, PullSecretName, corev1.SecretTypeDockerConfigJson, map[string][]byte{corev1.DockerConfigJsonKey: config}); err != nil {
		return "", err
	}
	return PullSecretName, nil
}

// copyMailCredentials copies the user name and password of the mail credentials spec.mail references into the
// platform namespace and returns the copy's name, or deletes the copy and returns "" without credentials. The copy
// always holds both keys, so that the connector's references resolve for a user name without a password.
func (r *ApplianceReconciler) copyMailCredentials(ctx context.Context, a *v1alpha1.Appliance) (string, error) {
	if a.Spec.Mail == nil || a.Spec.Mail.CredentialsRef == "" {
		return "", r.deleteCopy(ctx, MailSecretName)
	}
	src, err := systemSecret(ctx, r.Client, a.Spec.Mail.CredentialsRef)
	if err != nil {
		return "", err
	}
	username, err := secretValue(src, MailUsernameKey)
	if err != nil {
		return "", err
	}
	data := map[string][]byte{MailUsernameKey: username, MailPasswordKey: append([]byte{}, src.Data[MailPasswordKey]...)}
	if err := r.applySecret(ctx, a, MailSecretName, corev1.SecretTypeOpaque, data); err != nil {
		return "", err
	}
	return MailSecretName, nil
}

// deleteCopy deletes the copy name in the platform namespace if it exists.
func (r *ApplianceReconciler) deleteCopy(ctx context.Context, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: name}}
	err := r.Client.Delete(ctx, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("delete secret %s/%s: %w", platform.Namespace, name, err)
	}
	log.FromContext(ctx).Info("deleted a copy the Appliance no longer references", "secret", platform.Namespace+"/"+name)
	return nil
}

func (r *ApplianceReconciler) applySecret(ctx context.Context, a *v1alpha1.Appliance, name string, kind corev1.SecretType, data map[string][]byte) error {
	s := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: name},
		Type:       kind,
		Data:       data,
	}
	_, err := r.apply(ctx, a, s)
	return err
}

// createKeySecrets creates the key Secret of every connector whose catalogue entry has keys. An existing key
// Secret is never changed: a connector cannot read what it stored with a lost key. The Secret has no owner, so
// deleting the Appliance does not delete the keys.
func (r *ApplianceReconciler) createKeySecrets(ctx context.Context, connectors []otilm.Connector) error {
	for _, c := range connectors {
		i := slices.IndexFunc(r.Release.Connectors, func(e release.Connector) bool { return e.Name == c.Name })
		if i < 0 || len(r.Release.Connectors[i].Keys) == 0 {
			continue
		}
		exists, err := r.secretExists(ctx, platform.Namespace, c.Name+"-keys")
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		data := make(map[string][]byte, len(r.Release.Connectors[i].Keys))
		for _, key := range r.Release.Connectors[i].Keys {
			value, err := randomKey()
			if err != nil {
				return err
			}
			data[key] = value
		}
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: c.Name + "-keys"}, Data: data}
		err = r.Client.Create(ctx, s, client.FieldOwner(fieldManager))
		switch {
		case apierrors.IsAlreadyExists(err):
			continue
		case err != nil:
			return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		log.FromContext(ctx).Info("created connector keys", "secret", s.Namespace+"/"+s.Name)
	}
	return nil
}

// secretExists reads only the metadata of a Secret, from the API server.
func (r *ApplianceReconciler) secretExists(ctx context.Context, namespace, name string) (bool, error) {
	m := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, m)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read secret %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// randomKey returns 32 random bytes, base64-encoded, the format the connector catalogue asks for.
func randomKey() ([]byte, error) {
	raw := make([]byte, keyBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(raw)), nil
}

// apply server-side-applies obj with the Appliance as its controller, taking every field it sets, and
// emits DriftCorrected when that took spec fields from another manager.
func (r *ApplianceReconciler) apply(ctx context.Context, a *v1alpha1.Appliance, obj client.Object) (*unstructured.Unstructured, error) {
	if err := controllerutil.SetControllerReference(a, obj, r.Client.Scheme()); err != nil {
		return nil, err
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: content}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	name := fmt.Sprintf("%s %s/%s", u.GetKind(), u.GetNamespace(), u.GetName())

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(u.GroupVersionKind())
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(u), current); client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	foreign := foreignSpecFields(current)

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("apply %s: %w", name, err)
	}
	if len(foreign) > 0 && !maps.Equal(foreign, foreignSpecFields(u)) {
		log.FromContext(ctx).Info("corrected drift", "object", name)
		r.Recorder.Eventf(a, nil, corev1.EventTypeNormal, "DriftCorrected", "Apply", "restored %s", name)
	}
	return u, nil
}

// foreignSpecFields returns, per manager other than ours, the fields it owns of an object's spec.
func foreignSpecFields(obj metav1.Object) map[string]string {
	fields := map[string]string{}
	for _, e := range obj.GetManagedFields() {
		if e.Manager == fieldManager || e.Subresource != "" || e.FieldsV1 == nil {
			continue
		}
		if owned := e.FieldsV1.GetRawString(); strings.Contains(owned, `"f:spec"`) {
			fields[e.Manager+"/"+string(e.Operation)] = owned
		}
	}
	return fields
}
