package target

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// Reasons are the fixed strings that stand for a target error in status, Events and the API; the error text of a
// driver never leaves the manager.
const (
	ReasonUnreachable     = "unreachable"
	ReasonAuthFailed      = "authFailed"
	ReasonHostKeyMismatch = "hostKeyMismatch"
	ReasonNotFound        = "notFound"
	ReasonNoSpace         = "noSpace"
	ReasonTimeout         = "timeout"
	ReasonFailed          = "failed"
)

// The drivers wrap what they recognise in these errors.
var (
	ErrUnreachable     = errors.New("target unreachable")
	ErrAuthFailed      = errors.New("target authentication failed")
	ErrHostKeyMismatch = errors.New("the SFTP host key does not match")
	ErrNotFound        = errors.New("not found on the target")
	ErrNoSpace         = errors.New("no space left on the target")
	ErrInvalidName     = errors.New("not a backup file name")
)

// Reason maps err to one of the fixed reasons; nil gives "".
func Reason(err error) string {
	var dns *net.DNSError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrHostKeyMismatch):
		return ReasonHostKeyMismatch
	case errors.Is(err, ErrAuthFailed):
		return ReasonAuthFailed
	case errors.Is(err, ErrNotFound):
		return ReasonNotFound
	case errors.Is(err, ErrNoSpace), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ReasonNoSpace
	case errors.Is(err, ErrUnreachable):
		return ReasonUnreachable
	case isTimeout(err):
		return ReasonTimeout
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH),
		errors.As(err, &dns), isDial(err):
		return ReasonUnreachable
	default:
		return ReasonFailed
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func isDial(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
