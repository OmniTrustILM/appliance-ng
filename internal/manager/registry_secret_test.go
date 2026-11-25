package manager

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/internal/registry"
)

func TestZotHtpasswdGenerated(t *testing.T) {
	var mu sync.Mutex
	var logs bytes.Buffer
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		logs.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 10}))
	key := client.ObjectKey{Namespace: SystemNamespace, Name: registry.SecretName}
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
	})

	if err := ensureRegistrySecret(ctx, testClient); err != nil {
		t.Fatal(err)
	}
	first := &corev1.Secret{}
	if err := testClient.Get(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	password := first.Data["password"]
	if len(password) == 0 {
		t.Fatal("the Secret has no password")
	}
	user, hash, ok := strings.Cut(strings.TrimSpace(string(first.Data["htpasswd"])), ":")
	if !ok || user != registry.PushUser {
		t.Fatalf("htpasswd = %q, want a line for %s", first.Data["htpasswd"], registry.PushUser)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), password); err != nil {
		t.Errorf("htpasswd hash does not match the password: %v", err)
	}

	if err := ensureRegistrySecret(ctx, testClient); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := &corev1.Secret{}
	if err := testClient.Get(ctx, key, second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Data["password"], password) || !bytes.Equal(second.Data["htpasswd"], first.Data["htpasswd"]) {
		t.Error("the second pass replaced the Secret")
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), string(password)) || strings.Contains(logs.String(), hash) {
		t.Errorf("the log holds the password or its hash: %s", logs.String())
	}
}
