package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func deploymentWithImage(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "appliance-manager"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sidecar", Image: "quay.io/x/side:1"}, {Name: "manager", Image: image}},
		}}},
	}
}

func TestMirrorUpstreams(t *testing.T) {
	rel := &release.Manifest{Images: []release.Image{{Ref: "ghcr.io/a/b:1"}, {Ref: "docker.io/library/c:1"}}}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	failing := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("forbidden")
	}}
	cases := []struct {
		name  string
		objs  []client.Object
		funcs interceptor.Funcs
		want  []string
	}{
		{"own registry joins the release's", []client.Object{deploymentWithImage("localhost:5005/manager:x")},
			interceptor.Funcs{}, []string{"docker.io", "ghcr.io", "localhost:5005"}},
		{"own registry already listed", []client.Object{deploymentWithImage("ghcr.io/omnitrustilm/manager:1")},
			interceptor.Funcs{}, []string{"docker.io", "ghcr.io"}},
		{"no deployment", nil, interceptor.Funcs{}, []string{"docker.io", "ghcr.io"}},
		{"unreadable", []client.Object{deploymentWithImage("localhost:5005/manager:x")}, failing, []string{"docker.io", "ghcr.io"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objs...).WithInterceptorFuncs(c.funcs).Build()
			got := MirrorUpstreams(t.Context(), reader, rel, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if !slices.Equal(got, c.want) {
				t.Errorf("MirrorUpstreams = %v, want %v", got, c.want)
			}
		})
	}
}
