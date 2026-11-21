package access

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testClient client.Client
	testKube   kubernetes.Interface
	testConfig *rest.Config
)

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
	testConfig = cfg
	if testClient, err = client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme}); err != nil {
		println("client:", err.Error())
		return 1
	}
	if testKube, err = kubernetes.NewForConfig(cfg); err != nil {
		println("clientset:", err.Error())
		return 1
	}
	if err := testClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}); err != nil {
		println("namespace:", err.Error())
		return 1
	}
	tokenPoll = 20 * time.Millisecond
	return m.Run()
}

// fillTokens plays the token controller, which envtest does not run: it writes a token into every service account
// token Secret in Namespace until the test ends.
func fillTokens(t *testing.T, token string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			secrets := &corev1.SecretList{}
			if err := testClient.List(ctx, secrets, client.InNamespace(Namespace)); err == nil {
				for i := range secrets.Items {
					s := &secrets.Items[i]
					if s.Type == corev1.SecretTypeServiceAccountToken && len(s.Data[corev1.ServiceAccountTokenKey]) == 0 {
						s.Data = map[string][]byte{corev1.ServiceAccountTokenKey: []byte(token)}
						_ = testClient.Update(ctx, s)
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// resetAccess deletes the ServiceAccounts, bindings, Secrets and ConfigMaps earlier tests left behind.
func resetAccess(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &corev1.Secret{}, &corev1.ConfigMap{}} {
		if err := testClient.DeleteAllOf(ctx, obj, client.InNamespace(Namespace)); err != nil {
			t.Fatal(err)
		}
	}
	if err := testClient.DeleteAllOf(ctx, &rbacv1.ClusterRoleBinding{}, client.MatchingLabels{LabelKind: KindKubeconfig}); err != nil {
		t.Fatal(err)
	}
}
