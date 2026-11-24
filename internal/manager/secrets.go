package manager

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
)

// errReadSecret is the format of a failed read of the Secret namespace/name.
const errReadSecret = "read secret %s/%s: %w"

// trustBundle returns the CA bundle spec.trust references, or nil without one.
func trustBundle(ctx context.Context, c client.Reader, a *v1alpha1.Appliance) ([]byte, error) {
	ref := a.Spec.Trust
	if ref == nil {
		return nil, nil
	}
	src, err := systemSecret(ctx, c, ref.Name)
	if err != nil {
		return nil, err
	}
	return secretValue(src, ref.Key)
}

// registryCredentials returns the credentials of the registries that reference some, in the order of the spec.
func registryCredentials(ctx context.Context, c client.Reader, a *v1alpha1.Appliance) ([]nodeconfig.RegistryAuth, error) {
	var out []nodeconfig.RegistryAuth
	for _, registry := range a.Spec.Registries {
		if registry.CredentialsRef == "" {
			continue
		}
		src, err := systemSecret(ctx, c, registry.CredentialsRef)
		if err != nil {
			return nil, err
		}
		username, err := secretValue(src, RegistryUsernameKey)
		if err != nil {
			return nil, err
		}
		password, err := secretValue(src, RegistryPasswordKey)
		if err != nil {
			return nil, err
		}
		out = append(out, nodeconfig.RegistryAuth{Host: registry.Host, Username: string(username), Password: string(password)})
	}
	return out, nil
}

// nodeSecrets returns what the Secrets the Appliance references add to the node's documents.
func nodeSecrets(ctx context.Context, c client.Reader, a *v1alpha1.Appliance) (nodeconfig.Secrets, error) {
	bundle, err := trustBundle(ctx, c, a)
	if err != nil {
		return nodeconfig.Secrets{}, err
	}
	registries, err := registryCredentials(ctx, c, a)
	if err != nil {
		return nodeconfig.Secrets{}, err
	}
	return nodeconfig.Secrets{TrustedRoots: string(bundle), Registries: registries}, nil
}

// secretValue returns the value of a key of a Secret; an error names the Secret and the key, never a value.
func secretValue(s *corev1.Secret, key string) ([]byte, error) {
	value, ok := s.Data[key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s has no key %s", s.Namespace, s.Name, key)
	}
	return value, nil
}

// systemSecret reads the Secret name of the system namespace.
func systemSecret(ctx context.Context, c client.Reader, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: name}, s); err != nil {
		return nil, fmt.Errorf(errReadSecret, SystemNamespace, name, err)
	}
	return s, nil
}
