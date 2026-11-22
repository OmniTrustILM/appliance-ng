package recovery

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

func recipientSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: RecipientSecret},
		Data:       map[string][]byte{RecipientKey: []byte(value)},
	}
}

func TestStoredRecipient(t *testing.T) {
	ctx := context.Background()
	if _, err := StoredRecipient(ctx, fake.NewClientBuilder().Build()); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("without the Secret: %v", err)
	}
	if _, err := StoredRecipient(ctx, fake.NewClientBuilder().WithObjects(recipientSecret("")).Build()); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("with an empty Secret: %v", err)
	}
	got, err := StoredRecipient(ctx, fake.NewClientBuilder().WithObjects(recipientSecret("age1abc")).Build())
	if err != nil || got != "age1abc" {
		t.Errorf("got %q, %v", got, err)
	}
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, err := StoredRecipient(ctx, failing); err == nil || errors.Is(err, ErrNoRecipient) {
		t.Errorf("with a failing client: %v", err)
	}
}

func TestStoredKitAndIssuance(t *testing.T) {
	first, err := NewIssuance()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewIssuance()
	if err != nil || first == second || len(first) != 32 {
		t.Fatalf("issuances %q and %q, %v", first, second, err)
	}
	secret := recipientSecret("age1abc")
	secret.Data[IssuanceKey] = []byte(first)
	got, err := StoredKit(context.Background(), fake.NewClientBuilder().WithObjects(secret).Build())
	if err != nil || got != (Stored{Recipient: "age1abc", Issuance: first}) {
		t.Errorf("got %+v, %v", got, err)
	}
}
