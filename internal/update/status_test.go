package update

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// TestCheckGivesUpAHungStatusWrite ends a check whose status write the API server never answers, so that the next
// check can run.
func TestCheckGivesUpAHungStatusWrite(t *testing.T) {
	defer func(d time.Duration) { recordTimeout = d }(recordTimeout)
	recordTimeout = 200 * time.Millisecond
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.Appliance{}).
		WithObjects(&v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}}).Build()
	hung := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	f := NewFinder(FinderDeps{
		Client: hung,
		Online: Online{Repository: "127.0.0.1:1/appliance-ng", Options: func(ctx context.Context) ([]remote.Option, error) {
			return []remote.Option{remote.WithContext(ctx)}, nil
		}},
		Release: &release.Manifest{Version: "1.0.0"},
		Dir:     t.TempDir(), Uploads: t.TempDir(),
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Key:  struct{}{},
		Arch: "amd64",
	})
	for range 2 {
		done := make(chan error, 1)
		go func() { done <- f.Check(context.Background()) }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("a check whose status was not written succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the check waits for the status write without end")
		}
	}
}
