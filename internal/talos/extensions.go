package talos

import (
	"context"
	"errors"
	"fmt"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FirstbootExtension is the system extension of the appliance; its version is the appliance's.
const FirstbootExtension = "ilm-firstboot"

// Interrupted tells whether err is a call cut off on its way, as when the node goes down: the node may have carried
// it out.
func Interrupted(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// ExtensionVersion returns the version of the system extension name the node runs, empty when it runs none of that
// name.
func (n *node) ExtensionVersion(ctx context.Context, name string) (string, error) {
	exts, err := safe.StateListAll[*runtime.ExtensionStatus](ctx, n.st)
	if err != nil {
		return "", fmt.Errorf("read the system extensions: %w", err)
	}
	for ext := range exts.All() {
		if md := ext.TypedSpec().Metadata; md.Name == name {
			return md.Version, nil
		}
	}
	return "", nil
}
