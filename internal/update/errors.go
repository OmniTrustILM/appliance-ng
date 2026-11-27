package update

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
)

// Reasons a load is refused or fails, and a check of the online repository fails, besides the reasons of Check.
// They are shown in the status and the API, never an error text.
const (
	Unsigned      = "unsigned"
	BadSignature  = "badSignature"
	Truncated     = "truncated"
	Damaged       = "damaged"
	FormatTooNew  = "formatTooNew"
	InvalidBundle = "invalidBundle"
	WrongKind     = "wrongKind"
	WrongVersion  = "wrongVersion"
	NoSpace       = "noSpace"
	Busy          = "busy"
	NotFound      = "notFound"
	Unreachable   = "unreachable"
	Timeout       = "timeout"
	Failed        = "failed"

	// UnmirroredRegistry is a bundle with an image the node pulls from a registry the running release does not
	// mirror, which it could not pull offline: any image of a platform bundle, the installer, manager or etcd image
	// of a system bundle.
	UnmirroredRegistry = "unmirroredRegistry"
)

// Error is a load or check refused or failed for a fixed Reason. Its text holds the cause for the manager alone.
type Error struct {
	Reason string
	// step names what failed, for the log.
	step string
	err  error
}

func (e *Error) Error() string {
	if e.err == nil {
		return "bundle: " + e.Reason
	}
	return "bundle: " + e.Reason + ": " + e.err.Error()
}

func (e *Error) Unwrap() error { return e.err }

// Reason is the fixed reason of err: that of an *Error, else Failed.
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return Failed
}

func stepOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.step
	}
	return ""
}

// fail is a failure of step, with the reason the cause suggests.
func fail(step string, err error) error {
	return &Error{Reason: reasonOf(err), step: step, err: err}
}

// reasonOf is the reason of a failure to reach a registry: Timeout, Unreachable, NotFound or Failed.
func reasonOf(err error) string {
	var terr *transport.Error
	var nerr net.Error
	var uerr *url.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &nerr) && nerr.Timeout():
		return Timeout
	case errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound:
		return NotFound
	case errors.As(err, &terr) && (terr.StatusCode >= http.StatusInternalServerError || terr.StatusCode == http.StatusTooManyRequests):
		return Unreachable
	case errors.As(err, &terr):
		return Failed
	case errors.As(err, &uerr), errors.As(err, &nerr):
		return Unreachable
	}
	return Failed
}

// verifyErrors map the refusals of opening and verifying a bundle to their reasons.
var verifyErrors = []struct {
	err    error
	reason string
}{
	{bundle.ErrUnsigned, Unsigned},
	{bundle.ErrBadSignature, BadSignature},
	{bundle.ErrWrongArch, WrongArch},
	{bundle.ErrTruncated, Truncated},
	{bundle.ErrMissingImage, Damaged},
	{bundle.ErrFormatTooNew, FormatTooNew},
}

// verifyError is a refusal of opening or verifying a bundle; a bundle that is not one is InvalidBundle.
func verifyError(err error) error {
	for _, v := range verifyErrors {
		if errors.Is(err, v.err) {
			return &Error{Reason: v.reason, step: "verify", err: err}
		}
	}
	return &Error{Reason: InvalidBundle, step: "verify", err: err}
}
