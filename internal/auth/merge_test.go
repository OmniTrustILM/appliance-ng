package auth

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// accountStore returns a store on its own fake cluster with the named accounts, each with its name as the password
// hash prefixed by origin.
func accountStore(t *testing.T, origin string, names ...string) (client.Client, UserStore) {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	s := NewSecretUserStore(c, "appliance-system")
	for _, n := range names {
		if err := s.Create(context.Background(), User{Name: n, PasswordHash: origin + "-" + n, TOTPSeed: origin + "-seed-" + n, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return c, s
}

func storedSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "appliance-system", Name: name}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMergeUsersBackupWinsAndKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	cur, _ := accountStore(t, "current", "admin", "first")
	bak, _ := accountStore(t, "backup", "admin", "alice")
	currentKey := storedSecret(t, cur, UsersKeySecret)

	merged, err := MergeUsers(storedSecret(t, cur, UsersSecret), currentKey, storedSecret(t, bak, UsersSecret), storedSecret(t, bak, UsersKeySecret))
	if err != nil {
		t.Fatal(err)
	}
	// The merged accounts are read with the current key, which the restore keeps.
	out := fake.NewClientBuilder().WithObjects(merged, currentKey.DeepCopy()).Build()
	store := NewSecretUserStore(out, "appliance-system")
	names, err := UserNames(merged)
	if err != nil || !slices.Equal(names, []string{"admin", "alice", "first"}) {
		t.Fatalf("accounts = %v (%v)", names, err)
	}
	for name, want := range map[string]string{"admin": "backup", "alice": "backup", "first": "current"} {
		u, err := store.Get(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u.PasswordHash != want+"-"+name || u.TOTPSeed != want+"-seed-"+name {
			t.Errorf("%s = %+v, want the %s account", name, u, want)
		}
	}
}

func TestMergeUsersAgainGivesTheSameAccounts(t *testing.T) {
	ctx := context.Background()
	cur, _ := accountStore(t, "current", "first")
	bak, _ := accountStore(t, "backup", "alice")
	restored, restoredKey := storedSecret(t, bak, UsersSecret), storedSecret(t, bak, UsersKeySecret)
	once, err := MergeUsers(storedSecret(t, cur, UsersSecret), storedSecret(t, cur, UsersKeySecret), restored, restoredKey)
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted restore wrote the merged accounts: the merge runs again on them.
	twice, err := MergeUsers(once, storedSecret(t, cur, UsersKeySecret), restored, restoredKey)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSecretUserStore(fake.NewClientBuilder().WithObjects(twice, storedSecret(t, cur, UsersKeySecret)).Build(), "appliance-system")
	for name, seed := range map[string]string{"first": "current-seed-first", "alice": "backup-seed-alice"} {
		if u, err := store.Get(ctx, name); err != nil || u.TOTPSeed != seed {
			t.Errorf("%s = %+v, %v", name, u, err)
		}
	}
}

func TestMergeUsersOpensCurrentSeedsUnderTheBackupsKey(t *testing.T) {
	cur, _ := accountStore(t, "current", "first")
	bak, _ := accountStore(t, "backup", "alice")
	restoredKey := storedSecret(t, bak, UsersKeySecret)
	// An earlier restore wrote the backup's key after accounts sealed under it.
	sealed, err := MergeUsers(nil, restoredKey, storedSecret(t, cur, UsersSecret), storedSecret(t, cur, UsersKeySecret))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeUsers(sealed, storedSecret(t, cur, UsersKeySecret), storedSecret(t, bak, UsersSecret), restoredKey)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSecretUserStore(fake.NewClientBuilder().WithObjects(merged, storedSecret(t, cur, UsersKeySecret)).Build(), "appliance-system")
	if u, err := store.Get(context.Background(), "first"); err != nil || u.TOTPSeed != "current-seed-first" {
		t.Errorf("first = %+v, %v", u, err)
	}
}

func TestMergeUsersWithoutCurrentAccounts(t *testing.T) {
	bak, _ := accountStore(t, "backup", "alice")
	merged, err := MergeUsers(nil, nil, storedSecret(t, bak, UsersSecret), storedSecret(t, bak, UsersKeySecret))
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := UserNames(merged); !slices.Equal(names, []string{"alice"}) {
		t.Errorf("accounts = %v", names)
	}
	store := NewSecretUserStore(fake.NewClientBuilder().WithObjects(merged, storedSecret(t, bak, UsersKeySecret)).Build(), "appliance-system")
	if u, err := store.Get(context.Background(), "alice"); err != nil || u.TOTPSeed != "backup-seed-alice" {
		t.Errorf("alice = %+v, %v; want it sealed under the backup's key", u, err)
	}
}

func TestMergeUsersRefusesUnreadableInput(t *testing.T) {
	cur, _ := accountStore(t, "current", "first")
	bak, _ := accountStore(t, "backup", "alice")
	restored, restoredKey := storedSecret(t, bak, UsersSecret), storedSecret(t, bak, UsersKeySecret)
	current := storedSecret(t, cur, UsersSecret)
	otherKey := storedSecret(t, cur, UsersKeySecret)
	otherKey.Data[keyField] = make([]byte, keyLen)
	broken := restored.DeepCopy()
	broken.Data[usersField] = []byte("{")
	shortKey := restoredKey.DeepCopy()
	shortKey.Data[keyField] = []byte("short")
	for name, args := range map[string][4]*corev1.Secret{
		"seed under neither key": {current, otherKey, restored, restoredKey},
		"restored not json":      {current, storedSecret(t, cur, UsersKeySecret), broken, restoredKey},
		"current not json":       {broken, storedSecret(t, cur, UsersKeySecret), restored, restoredKey},
		"restored key short":     {current, storedSecret(t, cur, UsersKeySecret), restored, shortKey},
		"current key short":      {current, shortKey, restored, restoredKey},
	} {
		if _, err := MergeUsers(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
}

func TestOnlyUsersKeepsTheNamedAccounts(t *testing.T) {
	cur, _ := accountStore(t, "current", "admin", "first", "second")
	users := storedSecret(t, cur, UsersSecret)
	only, err := OnlyUsers(users, "first", "absent")
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := UserNames(only); !slices.Equal(names, []string{"first"}) {
		t.Errorf("accounts = %v, want first", names)
	}
	if names, _ := UserNames(users); len(names) != 3 {
		t.Errorf("the input changed: %v", names)
	}
	broken := users.DeepCopy()
	broken.Data[usersField] = []byte("{")
	if _, err := OnlyUsers(broken, "first"); err == nil {
		t.Error("kept accounts of an unreadable Secret")
	}
}
