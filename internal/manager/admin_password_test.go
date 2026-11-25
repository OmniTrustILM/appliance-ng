package manager

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

func TestEnsureAdminPasswordCreatesTheSecretOnce(t *testing.T) {
	ctx := context.Background()
	key := client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: platform.Namespace}}
	if err := testClient.Create(ctx, ns); client.IgnoreAlreadyExists(err) != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
	})

	if err := ensureAdminPassword(ctx, testClient); err != nil {
		t.Fatal(err)
	}
	first := &corev1.Secret{}
	if err := testClient.Get(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	password := string(first.Data["password"])
	if len(password) != 24 || !strings.ContainsAny(password, "-_.") {
		t.Errorf("password has %d characters, want 24 with one of -_.", len(password))
	}

	if err := ensureAdminPassword(ctx, testClient); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := &corev1.Secret{}
	if err := testClient.Get(ctx, key, second); err != nil {
		t.Fatal(err)
	}
	if string(second.Data["password"]) != password {
		t.Error("the second pass replaced the password")
	}
}
