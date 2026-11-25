package manager

import (
	"context"
	"fmt"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
)

// ensureRegistrySecret creates the Secret with zot's htpasswd file and the push user's password once and never
// replaces it. zot needs bcrypt hashes in its htpasswd file.
func ensureRegistrySecret(ctx context.Context, c client.Client) error {
	key := client.ObjectKey{Namespace: SystemNamespace, Name: registry.SecretName}
	err := c.Get(ctx, key, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf(errReadSecret, key.Namespace, key.Name, err)
	}
	password, err := platform.NewAdminPassword()
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash the registry password: %w", err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Data: map[string][]byte{
			"htpasswd": []byte(registry.PushUser + ":" + string(hash) + "\n"),
			"password": []byte(password),
		},
	}
	switch err := c.Create(ctx, s, client.FieldOwner(fieldManager)); {
	case apierrors.IsAlreadyExists(err):
		return nil
	case err != nil:
		return fmt.Errorf("create secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	log.FromContext(ctx).Info("created the registry push credentials", "secret", key.Namespace+"/"+key.Name)
	return nil
}
