package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

// ExitError is returned by Exec when the command ran and exited with a non-zero code.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited with code %d", e.Code)
}

// newExecutor builds the executor for the exec request at u; tests replace it.
var newExecutor = func(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	spdy, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, u)
	if err != nil {
		return nil, err
	}
	ws, err := remotecommand.NewWebSocketExecutor(cfg, http.MethodGet, u.String())
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, httpstream.IsUpgradeFailure)
}

// Exec runs cmd in the container of the pod, streaming stdin into it when stdin is not nil and its output into
// stdout. What the command writes to standard error is discarded: it may carry anything the command saw. A command
// that exits non-zero gives an *ExitError.
func Exec(ctx context.Context, cfg *rest.Config, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("exec client: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, runtime.NewParameterCodec(scheme.Scheme))
	exec, err := newExecutor(cfg, req.URL())
	if err != nil {
		return fmt.Errorf("exec executor: %w", err)
	}
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: io.Discard})
	var exit utilexec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		return &ExitError{Code: exit.ExitStatus()}
	default:
		return fmt.Errorf("exec in %s/%s: %w", ns, pod, err)
	}
}
