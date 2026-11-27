package talos

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestExtensionVersion(t *testing.T) {
	st := newState(t)
	for id, name := range map[string]string{"0": "iscsi-tools", "1": FirstbootExtension} {
		ext := runtime.NewExtensionStatus(runtime.NamespaceName, id)
		ext.TypedSpec().Metadata.Name, ext.TypedSpec().Metadata.Version = name, "1.1.0-"+id
		if err := st.Create(context.Background(), ext); err != nil {
			t.Fatal(err)
		}
	}
	n := &node{st: st}
	if v, err := n.ExtensionVersion(context.Background(), FirstbootExtension); err != nil || v != "1.1.0-1" {
		t.Errorf("ilm-firstboot: %q, %v", v, err)
	}
	if v, err := n.ExtensionVersion(context.Background(), "absent"); err != nil || v != "" {
		t.Errorf("absent extension: %q, %v", v, err)
	}
}

func TestInterrupted(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("upgrade: %w", status.Error(codes.Unavailable, "connection reset")): true,
		fmt.Errorf("upgrade: %w", context.DeadlineExceeded):                            true,
		fmt.Errorf("upgrade: %w", status.Error(codes.FailedPrecondition, "bad image")): false,
		errors.New("refused"): false,
	} {
		if got := Interrupted(err); got != want {
			t.Errorf("Interrupted(%v) = %v, want %v", err, got, want)
		}
	}
}
