// Package manager reconciles the add-ons, the ILM Platform and its Connectors from the Appliance.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	"github.com/OmniTrustILM/appliance-ng/internal/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	fieldManager = "appliance-manager"
	// SystemNamespace holds the Appliance's own Secrets, which the spec references by name.
	SystemNamespace = "appliance-system"
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
	TrustSecretName = "appliance-trust"
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

	// The operator's kinds are watched only once its CRDs are served: the reconciler installs them itself.
	controller  controller.Controller
	cache       cache.Cache
	watchMu     sync.Mutex
	watchedKind map[string]bool

	// The last completed full add-on pass. The controller runs one reconcile at a time, so no lock is needed.
	passed    bool
	passedAt  time.Time
	passedGen int64
	now       func() time.Time
}

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
	inSystemNamespace := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == SystemNamespace })
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
	before := a.Status.DeepCopy()
	result, err := r.reconcile(ctx, a)
	if serr := r.writeStatus(ctx, a, before); serr != nil {
		return ctrl.Result{}, errors.Join(err, serr)
	}
	return result, err
}

func (r *ApplianceReconciler) reconcile(ctx context.Context, a *v1alpha1.Appliance) (ctrl.Result, error) {
	progress, err := r.addonsProgress(ctx, a)
	if err != nil {
		setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, reasonFailed, err.Error())
		return ctrl.Result{}, err
	}
	if !progress.Done {
		setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, reasonProgressing, progress.Step+": "+progress.Message)
		return ctrl.Result{RequeueAfter: addonsRequeue}, nil
	}
	setCondition(a, v1alpha1.ConditionAddonsReady, metav1.ConditionTrue, reasonReady, "all add-ons are ready")
	if a.Spec.Platform == nil {
		return ctrl.Result{RequeueAfter: resync}, nil
	}
	if err := r.watchOperator(ctx); err != nil {
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonFailed, err.Error())
		return ctrl.Result{}, err
	}

	// The operator's status updates of the owned Platform wake the reconciler; the periodic requeue repeats the
	// full add-on pass.
	ready, message, err := r.reconcilePlatform(ctx, a)
	switch {
	case err != nil:
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonFailed, err.Error())
		return ctrl.Result{}, err
	case !ready:
		setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonProgressing, message)
		return ctrl.Result{RequeueAfter: resync}, nil
	}
	setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, reasonReady, message)
	if a.Status.Setup == v1alpha1.SetupPart2 {
		a.Status.Setup = v1alpha1.SetupComplete
		log.FromContext(ctx).Info("setup complete")
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// addonsProgress runs the full add-on pass, which applies every step, on the first reconcile, when the
// Appliance's generation changed since the last complete pass and when resync has elapsed since then. Other
// reconciles, woken by Platform, Connector and Secret events, only check that the steps are still ready. A pass
// that did not complete is not recorded, so the next reconcile applies again.
func (r *ApplianceReconciler) addonsProgress(ctx context.Context, a *v1alpha1.Appliance) (addons.Progress, error) {
	if r.now == nil {
		r.now = time.Now
	}
	if r.passed && a.Generation == r.passedGen && r.now().Sub(r.passedAt) < resync {
		return addons.Check(ctx, r.Cluster, r.Steps)
	}
	progress, err := addons.Pass(ctx, r.Cluster, r.Steps, r.Addons)
	r.passed = err == nil && progress.Done
	if r.passed {
		r.passedAt, r.passedGen = r.now(), a.Generation
	}
	return progress, err
}

// reconcilePlatform applies the Platform and Connectors with the Secrets they read and reports whether the
// Platform is ready.
func (r *ApplianceReconciler) reconcilePlatform(ctx context.Context, a *v1alpha1.Appliance) (bool, string, error) {
	secrets, err := r.copySecrets(ctx, a)
	if err != nil {
		return false, "", err
	}
	p, connectors, err := platform.Render(a, r.Release, secrets)
	if err != nil {
		return false, "", fmt.Errorf("render the platform: %w", err)
	}
	if err := r.createKeySecrets(ctx, connectors); err != nil {
		return false, "", err
	}
	applied, err := r.apply(ctx, a, p)
	if err != nil {
		return false, "", err
	}
	for i := range connectors {
		if _, err := r.apply(ctx, a, &connectors[i]); err != nil {
			return false, "", err
		}
	}
	current := &otilm.Platform{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(applied.Object, current); err != nil {
		return false, "", fmt.Errorf("read the applied Platform: %w", err)
	}
	ready, message := platformReady(current)
	return ready, message, nil
}

// copySecrets copies the certificate and key of the TLS Secret and the referenced key of the trust Secret the
// spec references into the platform namespace, and returns the names of the copies. Built-in TLS has no Secret.
func (r *ApplianceReconciler) copySecrets(ctx context.Context, a *v1alpha1.Appliance) (platform.SecretNames, error) {
	var names platform.SecretNames
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		src, err := r.systemSecret(ctx, a.Spec.TLS.SecretRef)
		if err != nil {
			return names, err
		}
		if src.Type != corev1.SecretTypeTLS {
			return names, fmt.Errorf("secret %s/%s has type %q, want %s", SystemNamespace, src.Name, src.Type, corev1.SecretTypeTLS)
		}
		pair := map[string][]byte{corev1.TLSCertKey: src.Data[corev1.TLSCertKey], corev1.TLSPrivateKeyKey: src.Data[corev1.TLSPrivateKeyKey]}
		if err := r.applySecret(ctx, a, TLSSecretName, corev1.SecretTypeTLS, pair); err != nil {
			return names, err
		}
		names.TLS = TLSSecretName
	}
	if ref := a.Spec.Trust; ref != nil {
		src, err := r.systemSecret(ctx, ref.Name)
		if err != nil {
			return names, err
		}
		bundle, ok := src.Data[ref.Key]
		if !ok {
			return names, fmt.Errorf("secret %s/%s has no key %s", SystemNamespace, ref.Name, ref.Key)
		}
		if err := r.applySecret(ctx, a, TrustSecretName, corev1.SecretTypeOpaque, map[string][]byte{platform.TrustKey: bundle}); err != nil {
			return names, err
		}
		names.Trust = TrustSecretName
	}
	return names, nil
}

func (r *ApplianceReconciler) systemSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: name}, s); err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", SystemNamespace, name, err)
	}
	return s, nil
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
