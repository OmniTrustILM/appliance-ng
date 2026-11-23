package kube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

type fakeExecutor struct {
	stream func(remotecommand.StreamOptions) error
}

func (f fakeExecutor) Stream(o remotecommand.StreamOptions) error { return f.stream(o) }
func (f fakeExecutor) StreamWithContext(_ context.Context, o remotecommand.StreamOptions) error {
	return f.stream(o)
}

// withExecutor replaces the executor constructor for the test and returns the URL the last request used.
func withExecutor(t *testing.T, stream func(remotecommand.StreamOptions) error) **url.URL {
	t.Helper()
	var got *url.URL
	old := newExecutor
	newExecutor = func(_ *rest.Config, u *url.URL) (remotecommand.Executor, error) {
		got = u
		return fakeExecutor{stream}, nil
	}
	t.Cleanup(func() { newExecutor = old })
	return &got
}

func TestExecStreamsStdinStdout(t *testing.T) {
	got := withExecutor(t, func(o remotecommand.StreamOptions) error {
		if _, err := io.Copy(o.Stdout, o.Stdin); err != nil {
			return err
		}
		_, err := o.Stderr.Write([]byte(strings.Repeat("e", 1<<20)))
		return err
	})
	var out bytes.Buffer
	err := Exec(context.Background(), &rest.Config{Host: "https://k.example"}, Container{Namespace: "ilm", Pod: "db-1", Name: "postgres"},
		[]string{"psql", "-d", "ilm"}, strings.NewReader("select 1;"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "select 1;" {
		t.Errorf("stdout = %q, want the stdin echoed", out.String())
	}
	u := *got
	if u.Path != "/api/v1/namespaces/ilm/pods/db-1/exec" {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("container") != "postgres" || q["command"][0] != "psql" || q["command"][2] != "ilm" || q.Get("stdin") != "true" {
		t.Errorf("query = %v", q)
	}
}

func TestExecWithoutStdinDoesNotRequestIt(t *testing.T) {
	got := withExecutor(t, func(o remotecommand.StreamOptions) error {
		if o.Stdin != nil {
			t.Error("stdin passed to the stream")
		}
		return nil
	})
	if err := Exec(context.Background(), &rest.Config{Host: "https://k.example"}, Container{Namespace: "ilm", Pod: "p", Name: "c"}, []string{"true"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if (*got).Query().Get("stdin") != "" {
		t.Errorf("query = %v", (*got).Query())
	}
}

func TestExecErrorCarriesTheExitCodeOnly(t *testing.T) {
	withExecutor(t, func(o remotecommand.StreamOptions) error {
		_, _ = o.Stderr.Write([]byte("password=hunter2"))
		return utilexec.CodeExitError{Err: errors.New("command terminated: password=hunter2"), Code: 3}
	})
	err := Exec(context.Background(), &rest.Config{Host: "https://k.example"}, Container{Namespace: "ilm", Pod: "p", Name: "c"}, []string{"x"}, nil, io.Discard)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("error = %v, want exit code 3", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q carries command output", err)
	}
}

func TestExecReportsTransportFailure(t *testing.T) {
	withExecutor(t, func(remotecommand.StreamOptions) error { return errors.New("connection reset") })
	err := Exec(context.Background(), &rest.Config{Host: "https://k.example"}, Container{Namespace: "ilm", Pod: "p", Name: "c"}, []string{"x"}, nil, io.Discard)
	var exit *ExitError
	if err == nil || errors.As(err, &exit) {
		t.Fatalf("error = %v, want a transport error", err)
	}
}

func TestExecReportsExecutorFailure(t *testing.T) {
	old := newExecutor
	newExecutor = func(*rest.Config, *url.URL) (remotecommand.Executor, error) { return nil, errors.New("no executor") }
	t.Cleanup(func() { newExecutor = old })
	if err := Exec(context.Background(), &rest.Config{Host: "https://k.example"}, Container{Namespace: "ilm", Pod: "p", Name: "c"}, nil, nil, io.Discard); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDefaultExecutorBuilds(t *testing.T) {
	u, _ := url.Parse("https://k.example/api/v1/namespaces/ilm/pods/p/exec")
	if _, err := newExecutor(&rest.Config{Host: "https://k.example"}, u); err != nil {
		t.Fatal(err)
	}
}
