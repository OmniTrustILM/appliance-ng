package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// RoleAdministrator is the only role a console account has.
	RoleAdministrator = "administrator"
	// UsersSecret holds the console accounts.
	UsersSecret = "appliance-users"
	// UsersKeySecret holds the key that encrypts the TOTP seeds in UsersSecret.
	UsersKeySecret = "appliance-users-key"

	usersField = "users.json"
	keyField   = "key"
	keyLen     = 32
)

// Errors of UserStore.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrUserExists   = errors.New("user already exists")
)

// User is a console account. TOTPSeed is the plaintext seed; stores keep it
// only encrypted.
type User struct {
	Name         string
	PasswordHash string
	TOTPSeed     string
	Role         string
	Created      time.Time
}

// UserStore keeps the console accounts.
type UserStore interface {
	Get(ctx context.Context, name string) (User, error)
	Create(ctx context.Context, u User) error
	Update(ctx context.Context, u User) error
}

// record is a User as stored in the Secret.
type record struct {
	PasswordHash string    `json:"passwordHash"`
	TOTPSeedEnc  []byte    `json:"totpSeedEnc"`
	Role         string    `json:"role"`
	Created      time.Time `json:"created"`
}

// SecretUserStore keeps the accounts in the Secret UsersSecret. It encrypts
// each TOTP seed with AES-256-GCM under the key in the Secret UsersKeySecret,
// which it creates on first use, and binds the ciphertext to the user name.
type SecretUserStore struct {
	c  client.Client
	ns string
}

// NewSecretUserStore returns a store on the Secrets in namespace ns.
func NewSecretUserStore(c client.Client, ns string) UserStore {
	return &SecretUserStore{c: c, ns: ns}
}

// Get returns the user called name, or ErrUserNotFound.
func (s *SecretUserStore) Get(ctx context.Context, name string) (User, error) {
	secret, err := s.usersSecret(ctx)
	if apierrors.IsNotFound(err) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	records, err := decodeRecords(secret)
	if err != nil {
		return User{}, err
	}
	r, ok := records[name]
	if !ok {
		return User{}, ErrUserNotFound
	}
	aead, err := s.cipher(ctx)
	if err != nil {
		return User{}, err
	}
	seed, err := openSeed(aead, name, r.TOTPSeedEnc)
	if err != nil {
		return User{}, err
	}
	return User{Name: name, PasswordHash: r.PasswordHash, TOTPSeed: seed, Role: r.Role, Created: r.Created}, nil
}

// Create adds u, or returns ErrUserExists. An empty role becomes
// RoleAdministrator.
func (s *SecretUserStore) Create(ctx context.Context, u User) error {
	return s.write(ctx, u, true)
}

// Update replaces the stored u, or returns ErrUserNotFound.
func (s *SecretUserStore) Update(ctx context.Context, u User) error {
	return s.write(ctx, u, false)
}

func (s *SecretUserStore) write(ctx context.Context, u User, create bool) error {
	if u.Name == "" {
		return errors.New("user name is empty")
	}
	if u.Role == "" {
		u.Role = RoleAdministrator
	}
	if u.Role != RoleAdministrator {
		return fmt.Errorf("role %q is not supported; the only role is %s", u.Role, RoleAdministrator)
	}
	aead, err := s.cipher(ctx)
	if err != nil {
		return err
	}
	r := record{
		PasswordHash: u.PasswordHash,
		TOTPSeedEnc:  sealSeed(aead, u.Name, u.TOTPSeed),
		Role:         u.Role,
		Created:      u.Created.UTC(),
	}

	secret, err := s.usersSecret(ctx)
	switch {
	case apierrors.IsNotFound(err) && create:
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: UsersSecret}}
		if err := encodeRecords(secret, map[string]record{u.Name: r}); err != nil {
			return err
		}
		if err := s.c.Create(ctx, secret); !apierrors.IsAlreadyExists(err) {
			return wrap("create Secret "+UsersSecret, err)
		}
		// Another writer created the Secret first; add the user to it.
		return s.write(ctx, u, create)
	case apierrors.IsNotFound(err):
		return ErrUserNotFound
	case err != nil:
		return err
	}

	records, err := decodeRecords(secret)
	if err != nil {
		return err
	}
	_, exists := records[u.Name]
	if create && exists {
		return ErrUserExists
	}
	if !create && !exists {
		return ErrUserNotFound
	}
	records[u.Name] = r
	if err := encodeRecords(secret, records); err != nil {
		return err
	}
	// The read resourceVersion makes a concurrent change fail with a conflict.
	return wrap("update Secret "+UsersSecret, s.c.Update(ctx, secret))
}

func (s *SecretUserStore) usersSecret(ctx context.Context) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: UsersSecret}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("read Secret %s: %w", UsersSecret, err)
	}
	return secret, nil
}

// cipher returns the AEAD under the key in UsersKeySecret, creating the key
// when the Secret does not exist yet.
func (s *SecretUserStore) cipher(ctx context.Context) (cipher.AEAD, error) {
	key, err := s.key(ctx)
	if err != nil {
		return nil, err
	}
	return newAEAD(key)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seed key: %w", err)
	}
	return cipher.NewGCM(block)
}

func (s *SecretUserStore) key(ctx context.Context) ([]byte, error) {
	name := types.NamespacedName{Namespace: s.ns, Name: UsersKeySecret}
	secret := &corev1.Secret{}
	err := s.c.Get(ctx, name, secret)
	if apierrors.IsNotFound(err) {
		key := make([]byte, keyLen)
		_, _ = rand.Read(key) // crypto/rand.Read never returns an error.
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: UsersKeySecret},
			Data:       map[string][]byte{keyField: key},
		}
		err = s.c.Create(ctx, secret)
		if apierrors.IsAlreadyExists(err) {
			// Another writer created the key first; use theirs.
			err = s.c.Get(ctx, name, secret)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read Secret %s: %w", UsersKeySecret, err)
	}
	key := secret.Data[keyField]
	if len(key) != keyLen {
		return nil, fmt.Errorf("key in Secret %s has %d bytes, want %d", UsersKeySecret, len(key), keyLen)
	}
	return key, nil
}

// sealSeed encrypts seed under a fresh random nonce and returns nonce and
// ciphertext. The user name is authenticated data, so a ciphertext moved to
// another user does not open.
func sealSeed(aead cipher.AEAD, user, seed string) []byte {
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error.
	return aead.Seal(nonce, nonce, []byte(seed), []byte(user))
}

func openSeed(aead cipher.AEAD, user string, enc []byte) (string, error) {
	if len(enc) < aead.NonceSize() {
		return "", fmt.Errorf("TOTP seed of user %q is malformed", user)
	}
	n := aead.NonceSize()
	seed, err := aead.Open(nil, enc[:n], enc[n:], []byte(user))
	if err != nil {
		return "", fmt.Errorf("TOTP seed of user %q does not decrypt", user)
	}
	return string(seed), nil
}

// UserNames returns the sorted account names in a UsersSecret.
func UserNames(secret *corev1.Secret) ([]string, error) {
	records, err := decodeRecords(secret)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(records)), nil
}

func decodeRecords(secret *corev1.Secret) (map[string]record, error) {
	records := map[string]record{}
	raw, ok := secret.Data[usersField]
	if !ok {
		return records, nil
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("users in Secret %s key %s are not valid: %w", UsersSecret, usersField, err)
	}
	return records, nil
}

func encodeRecords(secret *corev1.Secret, records map[string]record) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("encode users: %w", err)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[usersField] = raw
	return nil
}

// wrap adds context to err, keeping nil as nil.
func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}
