package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var testClient client.WithWatch

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return 1
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		return 1
	}
	defer func() { _ = env.Stop() }()
	testClient, err = client.NewWithWatch(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		println("create client:", err.Error())
		return 1
	}
	return m.Run()
}

// newNamespace creates a namespace of its own for a test.
func newNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "auth-"}}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSecretUserStoreEncryptsSeed(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	store := NewSecretUserStore(testClient, ns)

	seed, _, err := NewTOTP("admin", "ILM Appliance")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	want := User{Name: "admin", PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA", TOTPSeed: seed, Created: created}
	if err := store.Create(ctx, want); err != nil {
		t.Fatal(err)
	}

	var secret corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersSecret}, &secret); err != nil {
		t.Fatal(err)
	}
	for k, v := range secret.Data {
		if bytes.Contains(v, []byte(seed)) || bytes.Contains(bytes.ToLower(v), []byte(strings.ToLower(seed))) {
			t.Fatalf("Secret %s key %q holds the plaintext seed", UsersSecret, k)
		}
	}
	var key corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersKeySecret}, &key); err != nil {
		t.Fatalf("key Secret was not created: %v", err)
	}
	if len(key.Data[keyField]) != 32 {
		t.Fatalf("key Secret holds %d bytes, want 32", len(key.Data[keyField]))
	}

	got, err := store.Get(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	want.Role = RoleAdministrator
	if got.Name != want.Name || got.PasswordHash != want.PasswordHash || got.TOTPSeed != want.TOTPSeed ||
		got.Role != want.Role || !got.Created.Equal(want.Created) {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}

	// A second store reads the same key and so the same seed.
	if got, err := NewSecretUserStore(testClient, ns).Get(ctx, "admin"); err != nil || got.TOTPSeed != seed {
		t.Fatalf("second store: seed matches %v, err %v", got.TOTPSeed == seed, err)
	}
}

func TestSecretUserStoreCreateGetUpdate(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	store := NewSecretUserStore(testClient, ns)

	if _, err := store.Get(ctx, "admin"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Get on an empty store = %v, want ErrUserNotFound", err)
	}
	if err := store.Update(ctx, User{Name: "admin"}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Update on an empty store = %v, want ErrUserNotFound", err)
	}

	admin := User{Name: "admin", PasswordHash: "h1", TOTPSeed: "JBSWY3DPEHPK3PXP", Created: time.Unix(1000, 0)}
	if err := store.Create(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, admin); !errors.Is(err, ErrUserExists) {
		t.Fatalf("second Create = %v, want ErrUserExists", err)
	}
	second := User{Name: "second", PasswordHash: "h2", TOTPSeed: "KRSXG5CTMVRXEZLU", Role: RoleAdministrator}
	if err := store.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Get of an unknown user = %v, want ErrUserNotFound", err)
	}

	admin.PasswordHash = "h3"
	admin.TOTPSeed = "GEZDGNBVGY3TQOJQ"
	if err := store.Update(ctx, admin); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != "h3" || got.TOTPSeed != "GEZDGNBVGY3TQOJQ" || got.Role != RoleAdministrator {
		t.Fatalf("after Update Get = %+v", got)
	}
	if got, err := store.Get(ctx, "second"); err != nil || got.TOTPSeed != "KRSXG5CTMVRXEZLU" {
		t.Fatalf("other user changed: %+v, %v", got, err)
	}
}

func TestSecretUserStoreRefusesOtherRolesAndBadInput(t *testing.T) {
	ctx := testContext(t)
	store := NewSecretUserStore(testClient, newNamespace(ctx, t))
	if err := store.Create(ctx, User{Name: "viewer", Role: "viewer", TOTPSeed: "JBSWY3DPEHPK3PXP"}); err == nil {
		t.Fatal("Create accepted a role other than administrator")
	}
	if err := store.Create(ctx, User{TOTPSeed: "JBSWY3DPEHPK3PXP"}); err == nil {
		t.Fatal("Create accepted an empty user name")
	}
	if err := store.Update(ctx, User{Name: "admin", Role: "viewer"}); err == nil {
		t.Fatal("Update accepted a role other than administrator")
	}
}

func TestSecretUserStoreRefusesBadKey(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: UsersKeySecret},
		Data:       map[string][]byte{keyField: []byte("short")},
	}
	if err := testClient.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	err := NewSecretUserStore(testClient, ns).Create(ctx, User{Name: "admin", TOTPSeed: "JBSWY3DPEHPK3PXP"})
	if err == nil {
		t.Fatal("Create succeeded with a malformed key")
	}
}

func TestSecretUserStoreDetectsTampering(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	store := NewSecretUserStore(testClient, ns)
	if err := store.Create(ctx, User{Name: "admin", TOTPSeed: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	// Moving the encrypted seed to another user must not decrypt: the seed is bound to its user.
	if err := store.Create(ctx, User{Name: "mallory", TOTPSeed: "KRSXG5CTMVRXEZLU"}); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersSecret}, &secret); err != nil {
		t.Fatal(err)
	}
	records, err := decodeRecords(&secret)
	if err != nil {
		t.Fatal(err)
	}
	r := records["mallory"]
	r.TOTPSeedEnc = records["admin"].TOTPSeedEnc
	records["mallory"] = r
	if err := encodeRecords(&secret, records); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "mallory"); err == nil {
		t.Fatal("Get decrypted a seed moved from another user")
	}
}

// raceCreate returns a client that, on the first Create of the Secret name,
// first creates competitor itself, as a concurrent writer would.
func raceCreate(name string, competitor *corev1.Secret) client.Client {
	raced := false
	return interceptor.NewClient(testClient, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == name && !raced {
				raced = true
				if err := c.Create(ctx, competitor.DeepCopy()); err != nil {
					return err
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

func TestSecretUserStoreKeyCreateRace(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	theirs := bytes.Repeat([]byte{7}, 32)
	c := raceCreate(UsersKeySecret, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: UsersKeySecret},
		Data:       map[string][]byte{keyField: theirs},
	})
	if err := NewSecretUserStore(c, ns).Create(ctx, User{Name: "admin", TOTPSeed: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	var key corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersKeySecret}, &key); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key.Data[keyField], theirs) {
		t.Fatal("the key Secret was replaced")
	}
	if got, err := NewSecretUserStore(testClient, ns).Get(ctx, "admin"); err != nil || got.TOTPSeed != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("seed not encrypted under the competitor's key: %v", err)
	}
}

func TestSecretUserStoreUsersCreateRace(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	first := NewSecretUserStore(testClient, ns)
	// Update creates the key, so the raced Create is the one of the users Secret.
	if err := first.Update(ctx, User{Name: "x"}); !errors.Is(err, ErrUserNotFound) {
		t.Fatal(err)
	}
	competitor := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: UsersSecret}}
	if err := encodeRecords(competitor, map[string]record{"other": {Role: RoleAdministrator, TOTPSeedEnc: []byte{1}}}); err != nil {
		t.Fatal(err)
	}
	store := NewSecretUserStore(raceCreate(UsersSecret, competitor), ns)
	if err := store.Create(ctx, User{Name: "admin", TOTPSeed: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersSecret}, &secret); err != nil {
		t.Fatal(err)
	}
	records, err := decodeRecords(&secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := records["other"]; !ok {
		t.Fatal("the competitor's user was lost")
	}
	if _, ok := records["admin"]; !ok {
		t.Fatal("the user was not added to the competitor's Secret")
	}
}

func TestSecretUserStoreMalformedData(t *testing.T) {
	ctx := testContext(t)
	ns := newNamespace(ctx, t)
	store := NewSecretUserStore(testClient, ns)
	if err := store.Create(ctx, User{Name: "admin", TOTPSeed: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: UsersSecret}, &secret); err != nil {
		t.Fatal(err)
	}
	if err := encodeRecords(&secret, map[string]record{"admin": {TOTPSeedEnc: []byte{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "admin"); err == nil {
		t.Fatal("Get accepted a truncated seed")
	}
	secret.Data[usersField] = []byte("{not json")
	if err := testClient.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "admin"); err == nil {
		t.Fatal("Get accepted malformed users data")
	}
	if err := store.Create(ctx, User{Name: "new"}); err == nil {
		t.Fatal("Create accepted malformed users data")
	}
}
