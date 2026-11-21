// Package recovery builds and opens the recovery kit: the backup private key, break-glass credentials for the
// cluster and the node and the appliance identity, encrypted with a passphrase. The appliance keeps only the public
// key the backups are encrypted to.
package recovery

import (
	"context"
	"errors"
	"fmt"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// The Secret in kube.SystemNamespace that holds the public key the backups are encrypted to.
const (
	RecipientSecret = "appliance-backup-recipient"
	RecipientKey    = "recipient"
)

// ErrNoRecipient is returned while no recovery kit was issued.
var ErrNoRecipient = errors.New("no recovery kit was issued")

// GenerateKey returns a new backup key pair.
func GenerateKey() (*age.X25519Identity, error) {
	key, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate the backup key: %w", err)
	}
	return key, nil
}

// StoredRecipient reads the public backup key, or returns ErrNoRecipient.
func StoredRecipient(ctx context.Context, r client.Reader) (string, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: RecipientSecret}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return "", ErrNoRecipient
	case err != nil:
		return "", fmt.Errorf("read Secret %s/%s: %w", kube.SystemNamespace, RecipientSecret, err)
	}
	recipient := string(secret.Data[RecipientKey])
	if recipient == "" {
		return "", ErrNoRecipient
	}
	return recipient, nil
}
