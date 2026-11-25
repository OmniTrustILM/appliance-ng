package manager

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// ensureAdminPassword creates the Secret that holds the ILM administrator's password once and never replaces it.
// It creates without reading first: the manager's cache does not hold the platform namespace.
func ensureAdminPassword(ctx context.Context, c client.Client) error {
	password, err := platform.NewAdminPassword()
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret},
		Data:       map[string][]byte{"password": []byte(password)},
	}
	switch err := c.Create(ctx, s, client.FieldOwner(fieldManager)); {
	case apierrors.IsAlreadyExists(err):
		return nil
	case err != nil:
		return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	log.FromContext(ctx).Info("created the administrator password", "secret", s.Namespace+"/"+s.Name)
	return nil
}
