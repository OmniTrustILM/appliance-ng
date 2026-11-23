// Package recovery builds and opens the recovery kit: the backup private key, break-glass credentials for the
// cluster and the node and the appliance identity, encrypted with a passphrase. The appliance keeps only the public
// key the backups are encrypted to.
package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// The Secret in kube.SystemNamespace that holds the public key the backups are encrypted to and the issuance of the
// latest kit.
const (
	RecipientSecret = "appliance-backup-recipient"
	RecipientKey    = "recipient"
	IssuanceKey     = "issuance"
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

// NewIssuance returns a random identifier for a newly issued kit; it is not derived from the kit's key.
func NewIssuance() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate the kit issuance: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Stored is what the appliance keeps of the latest kit: the public backup key and the kit's issuance.
type Stored struct {
	Recipient, Issuance string
}

// StoredRecipient reads the public backup key, or returns ErrNoRecipient.
func StoredRecipient(ctx context.Context, r client.Reader) (string, error) {
	stored, err := StoredKit(ctx, r)
	return stored.Recipient, err
}

// StoredKit reads the public backup key and the issuance of the latest kit, or returns ErrNoRecipient.
func StoredKit(ctx context.Context, r client.Reader) (Stored, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: RecipientSecret}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return Stored{}, ErrNoRecipient
	case err != nil:
		return Stored{}, fmt.Errorf("read Secret %s/%s: %w", kube.SystemNamespace, RecipientSecret, err)
	}
	stored := Stored{Recipient: string(secret.Data[RecipientKey]), Issuance: string(secret.Data[IssuanceKey])}
	if stored.Recipient == "" {
		return Stored{}, ErrNoRecipient
	}
	return stored, nil
}

// HandedOut tells whether st records the stored kit, which it does once that kit was handed out. Until then the
// stored recipient may be a key nobody holds, such as the key of a replacement whose kit never reached the
// administrator.
func HandedOut(st *v1alpha1.RecoveryKitStatus, stored Stored) bool {
	return st != nil && st.Recipient == stored.Recipient && st.Issuance == stored.Issuance
}

// Usable tells whether backups may be encrypted to the stored key: its kit was handed out and the administrator
// confirmed holding it.
func Usable(st *v1alpha1.RecoveryKitStatus, stored Stored) bool {
	return HandedOut(st, stored) && st.Confirmed
}
