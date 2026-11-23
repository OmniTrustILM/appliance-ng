package target

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestListOnlyBackups(t *testing.T) {
	t.Run("s3", func(t *testing.T) {
		srv, backend := fakeS3(t, false)
		for _, key := range []string{"p/a.ilmbackup", "p/b.ilmbackup.part", "p/.appliance-test-0123456789abcdef",
			"p/notes.txt", "p/sub/c.ilmbackup", "other/d.ilmbackup", "pa.ilmbackup"} {
			if _, err := backend.PutObject("ilm", key, nil, strings.NewReader("x"), 1, nil); err != nil {
				t.Fatal(err)
			}
		}
		target, err := New(s3Spec(srv.URL, "p"), s3Creds(), Deps{})
		if err != nil {
			t.Fatal(err)
		}
		wantList(t, target, "a.ilmbackup")
	})
	t.Run("sftp", func(t *testing.T) {
		s := newSFTPServer(t)
		for _, name := range []string{"a.ilmbackup", "b.ilmbackup.part", ".appliance-test-0123456789abcdef", "notes.txt"} {
			if err := os.WriteFile(filepath.Join(s.dir, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(s.dir, "c.ilmbackup"), 0o700); err != nil {
			t.Fatal(err)
		}
		target, err := New(s.spec(), map[string][]byte{"password": []byte(testPassword)}, Deps{})
		if err != nil {
			t.Fatal(err)
		}
		wantList(t, target, "a.ilmbackup")
	})
	t.Run("nfs", func(t *testing.T) {
		f := newFakeNFS(t, true)
		f.handler = func(_ []string, _ io.Reader, stdout io.Writer) error {
			_, err := io.WriteString(stdout, "1 1763870400 ./a.ilmbackup\n1 1763870400 ./b.ilmbackup.part\n"+
				"1 1763870400 ./.appliance-test-0123456789abcdef\n1 1763870400 ./notes.txt\n")
			return err
		}
		wantList(t, f.target(t, nfsSpec("", "")), "a.ilmbackup")
	})
}

func wantList(t *testing.T, target Target, want ...string) {
	t.Helper()
	objects, err := target.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := backupNames(objects); !slices.Equal(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

// timeoutError is a net.Error that timed out.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTargetErrorsAreFixedReasons(t *testing.T) {
	fixed := []string{ReasonUnreachable, ReasonAuthFailed, ReasonHostKeyMismatch, ReasonNotFound, ReasonNoSpace, ReasonTimeout, ReasonFailed}
	if want := []string{"unreachable", "authFailed", "hostKeyMismatch", "notFound", "noSpace", "timeout", "failed"}; !slices.Equal(fixed, want) {
		t.Fatalf("reasons %v", fixed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	for err, want := range map[error]string{
		fmt.Errorf("put: %w", ErrHostKeyMismatch):                               ReasonHostKeyMismatch,
		fmt.Errorf("put: %w", ErrAuthFailed):                                    ReasonAuthFailed,
		fmt.Errorf("put: %w", ErrNotFound):                                      ReasonNotFound,
		fmt.Errorf("put: %w", ErrNoSpace):                                       ReasonNoSpace,
		fmt.Errorf("put: %w", ErrUnreachable):                                   ReasonUnreachable,
		fmt.Errorf("put: %w", ctx.Err()):                                        ReasonTimeout,
		&net.OpError{Op: "read", Err: timeoutError{}}:                           ReasonTimeout,
		&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}:                     ReasonUnreachable,
		&net.OpError{Op: "dial", Err: fmt.Errorf("x: %w", syscall.ENETUNREACH)}: ReasonUnreachable,
		fmt.Errorf("x: %w", syscall.EHOSTUNREACH):                               ReasonUnreachable,
		&net.DNSError{Err: "no such host", Name: "s3.example.com"}:              ReasonUnreachable,
		fmt.Errorf("x: %w", syscall.ENOSPC):                                     ReasonNoSpace,
		fmt.Errorf("x: %w", ErrInvalidName):                                     ReasonFailed,
		fmt.Errorf("something else"):                                            ReasonFailed,
		context.Canceled:                                                        ReasonFailed,
	} {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %q, want %q", err, got, want)
		}
	}
	if Reason(nil) != "" {
		t.Error("Reason(nil) is not empty")
	}

	// An S3 server that names the secret key in a URL of its error message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code>`+
			`<Message>Access denied for https://s3.example.com/ilm?X-Amz-Credential=%s&amp;secret=%s</Message>`+
			`<Resource>/ilm</Resource><RequestId>1</RequestId></Error>`, testAccessKey, testSecretKey)
	}))
	t.Cleanup(srv.Close)
	target, err := New(s3Spec(srv.URL, ""), s3Creds(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	errs = append(errs, target.Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1))
	_, err = target.List(context.Background())
	errs = append(errs, err)
	errs = append(errs, Test(context.Background(), target))
	if !strings.Contains(errs[1].Error(), testSecretKey) {
		t.Fatalf("the driver error %q does not carry the key; the test proves nothing", errs[1])
	}
	for i, err := range errs {
		if err == nil {
			t.Fatalf("operation %d succeeded", i)
		}
		reason := Reason(err)
		if reason != ReasonAuthFailed {
			t.Errorf("operation %d: reason %q, want authFailed", i, reason)
		}
		for n := 4; n <= len(testSecretKey); n++ {
			for start := 0; start+n <= len(testSecretKey); start++ {
				if strings.Contains(reason, testSecretKey[start:start+n]) {
					t.Fatalf("the reason %q carries part of the secret key", reason)
				}
			}
		}
	}
}

func TestReasonOfAnUnreachableS3(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	target, err := New(s3Spec("http://"+addr, ""), s3Creds(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonUnreachable {
		t.Errorf("reason %q (%v)", Reason(err), err)
	}
}
