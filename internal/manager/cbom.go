package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
)

// Message of a False CBOMRepositoryApplied; the error goes to the manager's log only.
const messageCBOMFailed = "applying the CBOM repository failed, retrying; the manager log has the details"

// setCBOMCondition records the outcome err of applying the CBOM repository, and drops the condition while none was
// ever chosen.
func setCBOMCondition(a *v1alpha1.Appliance, err error) {
	switch {
	case err != nil:
		setCondition(a, v1alpha1.ConditionCBOMRepositoryApplied, metav1.ConditionFalse, reasonFailed, messageCBOMFailed)
	case a.Spec.Platform == nil || a.Spec.Platform.CBOM == nil:
		meta.RemoveStatusCondition(&a.Status.Conditions, v1alpha1.ConditionCBOMRepositoryApplied)
	default:
		setCondition(a, v1alpha1.ConditionCBOMRepositoryApplied, metav1.ConditionTrue, reasonApplied, "the CBOM repository is applied")
	}
}

// Lengths, before hex encoding, of the root keys generated for the CBOM store.
const (
	storeAccessKeyBytes = 16
	storeSecretKeyBytes = 32
)

// reconcileCBOM applies the CBOM repository the platform describes and deletes its workloads that the spec no longer
// renders. The store on the appliance runs while its storage is the appliance and the repository is on, or turned off
// while the store's volume keeps the CBOMs, so that backups still hold them; its volume and keys are created once and
// never owned by the Appliance. The copy of an external store's credentials is deleted once it is not used.
func (r *ApplianceReconciler) reconcileCBOM(ctx context.Context, a *v1alpha1.Appliance) error {
	var spec *v1alpha1.CBOMSpec
	if a.Spec.Platform != nil {
		spec = a.Spec.Platform.CBOM
	}
	in := cbom.Inputs{ILMHost: ilmHostName(a)}
	var err error
	if in.CA, err = r.prepareCBOMStorage(ctx, a, spec); err != nil {
		return err
	}
	objs, err := r.renderCBOM(ctx, a, spec, in)
	if err != nil {
		return err
	}
	rendered := map[string]bool{}
	for _, obj := range objs {
		if _, err := r.apply(ctx, a, obj); err != nil {
			return err
		}
		rendered[obj.GetKind()+"/"+obj.GetName()] = true
	}
	return r.deleteUnrenderedCBOM(ctx, rendered)
}

// prepareCBOMStorage readies the storage of the repository spec turns on and returns the CA of an external store,
// empty without one: the copy of an external store's credentials, deleted while none is used, or the store's volume.
func (r *ApplianceReconciler) prepareCBOMStorage(ctx context.Context, a *v1alpha1.Appliance, spec *v1alpha1.CBOMSpec) (string, error) {
	on := spec != nil && spec.Enabled
	if on && spec.Storage == v1alpha1.CBOMStorageS3 {
		return r.copyCBOMCredentials(ctx, a, spec)
	}
	if err := r.deleteCopy(ctx, cbom.ExternalSecret); err != nil {
		return "", err
	}
	if on {
		return "", r.createCBOMClaim(ctx)
	}
	return "", nil
}

// deleteUnrenderedCBOM deletes the workloads of the CBOM repository that are not among rendered, by kind/name.
func (r *ApplianceReconciler) deleteUnrenderedCBOM(ctx context.Context, rendered map[string]bool) error {
	for _, obj := range cbom.Workloads() {
		if rendered[obj.GetKind()+"/"+obj.GetName()] {
			continue
		}
		err := r.Client.Delete(ctx, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
		log.FromContext(ctx).Info("deleted a part of the CBOM repository the spec no longer has", "kind", obj.GetKind(), "name", obj.GetNamespace()+"/"+obj.GetName())
	}
	return nil
}

// renderCBOM renders the store on the appliance while it runs, with its keys created once, and the repository from
// the pinned chart and images while it is on.
func (r *ApplianceReconciler) renderCBOM(ctx context.Context, a *v1alpha1.Appliance, spec *v1alpha1.CBOMSpec, in cbom.Inputs) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	runs, err := cbom.StoreRuns(ctx, r.APIReader, a.Spec.Platform)
	if err != nil {
		return nil, err
	}
	if runs {
		if err := r.createCBOMKeys(ctx); err != nil {
			return nil, err
		}
		image, err := r.Release.PinnedImage(cbom.StoreImage)
		if err != nil {
			return nil, err
		}
		if out, err = cbom.StoreObjects(image); err != nil {
			return nil, err
		}
	}
	if spec == nil || !spec.Enabled {
		return out, nil
	}
	if in.RepositoryImage, err = r.Release.PinnedImage(cbom.RepositoryImage); err != nil {
		return nil, err
	}
	pin, err := r.Release.Chart(cbom.ChartName)
	if err != nil {
		return nil, err
	}
	chart, err := fs.ReadFile(r.Addons, pin.File)
	if err != nil {
		return nil, fmt.Errorf("read the %s chart: %w", cbom.ChartName, err)
	}
	repository, err := cbom.Render(spec, chart, in)
	if err != nil {
		return nil, err
	}
	return append(out, repository...), nil
}

// copyCBOMCredentials copies the access and secret key of an external store into the platform namespace, in the
// keys the chart reads, and returns its CA, empty without one.
func (r *ApplianceReconciler) copyCBOMCredentials(ctx context.Context, a *v1alpha1.Appliance, spec *v1alpha1.CBOMSpec) (string, error) {
	src, err := systemSecret(ctx, r.Client, spec.CredentialsRef)
	if err != nil {
		return "", err
	}
	data := map[string][]byte{}
	for key, from := range map[string]string{cbom.AccessKeyKey: v1alpha1.CredentialsAccessKey, cbom.SecretKeyKey: v1alpha1.CredentialsSecretKey} {
		if data[key], err = secretValue(src, from); err != nil {
			return "", err
		}
	}
	if err := r.applySecret(ctx, a, cbom.ExternalSecret, corev1.SecretTypeOpaque, data); err != nil {
		return "", err
	}
	if spec.S3 == nil || spec.S3.CA == nil {
		return "", nil
	}
	ca, err := caOf(ctx, r.Client, spec.S3.CA)
	return string(ca), err
}

// createCBOMKeys creates the store's root keys once. They have no owner: the CBOMs in the store outlive the
// repository being turned off, and the keys they are kept under with them.
func (r *ApplianceReconciler) createCBOMKeys(ctx context.Context) error {
	exists, err := r.secretExists(ctx, cbom.Namespace, cbom.StoreSecret)
	if err != nil || exists {
		return err
	}
	access, secret := make([]byte, storeAccessKeyBytes), make([]byte, storeSecretKeyBytes)
	if _, err := rand.Read(access); err != nil {
		return fmt.Errorf("generate the CBOM store's keys: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate the CBOM store's keys: %w", err)
	}
	keys := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: cbom.Namespace, Name: cbom.StoreSecret},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			cbom.AccessKeyKey: []byte(hex.EncodeToString(access)), cbom.SecretKeyKey: []byte(hex.EncodeToString(secret)),
		},
	}
	return r.createOnce(ctx, keys)
}

// createCBOMClaim creates the store's volume claim once, without an owner, like its keys.
func (r *ApplianceReconciler) createCBOMClaim(ctx context.Context) error {
	claim := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cbom.Namespace, Name: cbom.StoreClaim}, claim)
	if apierrors.IsNotFound(err) {
		return r.createOnce(ctx, cbom.Claim())
	}
	if err != nil {
		return fmt.Errorf("read claim %s/%s: %w", cbom.Namespace, cbom.StoreClaim, err)
	}
	return nil
}

// createOnce creates obj, which another pass may have created already.
func (r *ApplianceReconciler) createOnce(ctx context.Context, obj client.Object) error {
	err := r.Client.Create(ctx, obj, client.FieldOwner(fieldManager))
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	log.FromContext(ctx).Info("created a part of the CBOM store", "name", obj.GetNamespace()+"/"+obj.GetName())
	return nil
}
