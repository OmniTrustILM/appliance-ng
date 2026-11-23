package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

// secretRef locates a Secret; required Secrets are an error when missing.
type secretRef struct {
	namespace, name string
	required        bool
}

// CollectSecrets returns the Secrets a restore needs, without the metadata of the cluster they came from, sorted by
// namespace and name: every Secret the Appliance references (a missing one is an error), the appliance's own
// account, key and recipient Secrets, the CA Secrets, and in the platform namespace the CA key pairs and the key
// Secret of every Connector and of every connector named in keyed. A removed connector's key Secret stays, and so
// does what the connector stored with it in the database. Other Secrets that do not exist are skipped.
func CollectSecrets(ctx context.Context, c client.Reader, a *v1alpha1.Appliance, keyed []string) ([]corev1.Secret, error) {
	refs := referenced(a)
	for _, name := range []string{auth.UsersSecret, auth.UsersKeySecret, recovery.RecipientSecret} {
		refs = append(refs, secretRef{namespace: kube.SystemNamespace, name: name})
	}
	refs = append(refs, secretRef{namespace: certs.ClusterResourceNamespace, name: certs.RootCAName})
	for _, name := range []string{platform.EdgeCASecret, platform.AdminCASecret} {
		refs = append(refs, secretRef{namespace: platform.Namespace, name: name})
	}
	connectors := &otilm.ConnectorList{}
	if err := c.List(ctx, connectors, client.InNamespace(platform.Namespace)); err != nil {
		return nil, fmt.Errorf("list connectors: %w", err)
	}
	withKeys := slices.Clone(keyed)
	for _, connector := range connectors.Items {
		withKeys = append(withKeys, connector.Name)
	}
	for _, name := range withKeys {
		refs = append(refs, secretRef{namespace: platform.Namespace, name: name + "-keys"})
	}

	slices.SortFunc(refs, func(x, y secretRef) int {
		return strings.Compare(x.namespace+"/"+x.name, y.namespace+"/"+y.name)
	})
	refs = dedupe(refs)

	var out []corev1.Secret
	for _, ref := range refs {
		s := &corev1.Secret{}
		err := c.Get(ctx, client.ObjectKey{Namespace: ref.namespace, Name: ref.name}, s)
		switch {
		case apierrors.IsNotFound(err) && !ref.required:
			continue
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("referenced secret %s/%s not found", ref.namespace, ref.name)
		case err != nil:
			return nil, fmt.Errorf("read secret %s/%s: %w", ref.namespace, ref.name, err)
		}
		out = append(out, portable(s))
	}
	return out, nil
}

// dedupe merges references to the same Secret; one required reference makes it required.
func dedupe(refs []secretRef) []secretRef {
	var out []secretRef
	for _, ref := range refs {
		if n := len(out); n > 0 && out[n-1].namespace == ref.namespace && out[n-1].name == ref.name {
			out[n-1].required = out[n-1].required || ref.required
			continue
		}
		out = append(out, ref)
	}
	return out
}

// referenced lists the Secrets of the Appliance's spec, all in the appliance namespace.
func referenced(a *v1alpha1.Appliance) []secretRef {
	var refs []secretRef
	add := func(name string) {
		if name != "" {
			refs = append(refs, secretRef{namespace: kube.SystemNamespace, name: name, required: true})
		}
	}
	addKey := func(ref *v1alpha1.SecretKeyRef) {
		if ref != nil {
			add(ref.Name)
		}
	}
	spec := a.Spec
	add(spec.TLS.SecretRef)
	addKey(spec.Trust)
	for _, r := range spec.Registries {
		add(r.CredentialsRef)
	}
	if spec.Mail != nil {
		add(spec.Mail.CredentialsRef)
	}
	if spec.Backup != nil {
		for _, t := range spec.Backup.Targets {
			add(t.CredentialsRef)
			if t.S3 != nil {
				addKey(t.S3.CA)
			}
		}
	}
	return refs
}

// portable copies what a restore applies: name, namespace, labels, annotations, type and data. It drops the
// identity the source cluster gave the object, which the target cluster rejects or must set itself.
func portable(s *corev1.Secret) corev1.Secret {
	annotations := make(map[string]string, len(s.Annotations))
	for k, v := range s.Annotations {
		if k != corev1.LastAppliedConfigAnnotation {
			annotations[k] = v
		}
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	return corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, Labels: s.Labels, Annotations: annotations},
		Immutable:  s.Immutable,
		Data:       s.Data,
		Type:       s.Type,
	}
}
