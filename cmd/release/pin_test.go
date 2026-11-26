package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

var update = flag.Bool("update", false, "rewrite the golden files of the pin tests")

// pinnedFiles are the files pin may change, relative to the repository root.
var pinnedFiles = []string{"release.yaml", "addons/ilm-operator.yaml", "addons/platform.yaml", "addons/src/downloads.txt"}

// redirect sends every request to target, whatever host it names.
type redirect struct{ target *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = ""
	return http.DefaultTransport.RoundTrip(req)
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in this test")
}

// upstream serves the operator manifest of release v<operator> at its GitHub download path and, as a registry,
// an image for each of refs, whose digest depends only on its reference.
func upstream(t *testing.T, operator string, manifest []byte, refs []string) redirect {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	mux.HandleFunc("/OmniTrustILM/operator/releases/download/v"+operator+"/ilm-operator.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			t.Fatal(err)
		}
		local, err := name.ParseReference(target.Host + "/" + parsed.Context().RepositoryStr() + ":" + parsed.Identifier())
		if err != nil {
			t.Fatal(err)
		}
		img := mutate.Annotations(empty.Image, map[string]string{"ref": ref}).(v1.Image)
		if err := remote.Write(local, img); err != nil {
			t.Fatal(err)
		}
	}
	return redirect{target}
}

// copyTree copies the files pin may change from dir into a new directory.
func copyTree(t *testing.T, dir string) string {
	t.Helper()
	root := t.TempDir()
	for _, file := range pinnedFiles {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPinRewritesOperatorAndImages(t *testing.T) {
	manifest, err := os.ReadFile("testdata/pin/ilm-operator-v1.1.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := release.BOMImageRefs("2.20.0")
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream(t, "1.1.0", manifest, append(refs, "hub.omnitrustregistry.com/ilm/operator:1.1.0"))
	root := copyTree(t, "testdata/pin/before")

	if err := (pinner{transport: transport}).pin(t.Context(), root, "1.1.0", "2.20.0"); err != nil {
		t.Fatal(err)
	}

	for _, file := range pinnedFiles {
		got, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata/pin/after", file)
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, got, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s:\n%s", file, golden, got)
		}
	}
}

func TestPinRefusesDowngrade(t *testing.T) {
	for _, tc := range []struct{ name, operator, ilm, want string }{
		{"operator", "0.9.0", "2.20.0", "operator from v1.0.0 to 0.9.0"},
		{"ilm", "1.1.0", "2.18.0", "ILM from 2.19.0 to 2.18.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyTree(t, "testdata/pin/before")

			err := (pinner{transport: failing{}}).pin(t.Context(), root, tc.operator, tc.ilm)

			if err == nil || !strings.Contains(err.Error(), "downgrade") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refused downgrade of the %s", err, tc.want)
			}
			assertUnchanged(t, root)
		})
	}
}

// TestPinOlderILM lets an older ILM version past the downgrade check, for a test build, and still refuses an older
// operator.
func TestPinOlderILM(t *testing.T) {
	root := copyTree(t, "testdata/pin/before")
	older := pinner{transport: failing{}, olderILM: true}

	if err := older.pin(t.Context(), root, "1.1.0", "2.18.0"); err == nil || strings.Contains(err.Error(), "downgrade") {
		t.Errorf("err = %v, want the older ILM past the downgrade check and the download to fail", err)
	}
	err := older.pin(t.Context(), root, "0.9.0", "2.18.0")
	if err == nil || !strings.Contains(err.Error(), "operator from v1.0.0 to 0.9.0") {
		t.Errorf("err = %v, want a refused downgrade of the operator", err)
	}
	assertUnchanged(t, root)
}

func assertUnchanged(t *testing.T, root string) {
	t.Helper()
	for _, file := range pinnedFiles {
		got, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		want, err := fs.ReadFile(os.DirFS("testdata/pin/before"), file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed", file)
		}
	}
}

func TestPinFailsWithoutWriting(t *testing.T) {
	manifest, err := os.ReadFile("testdata/pin/ilm-operator-v1.1.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := release.BOMImageRefs("2.20.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, operator, ilm, want string
		images                    []string
	}{
		{"no operator release", "1.2.0", "2.20.0", "404", refs},
		{"no bill of materials", "1.1.0", "9.0.0", "no bill of materials", refs},
		{"image not in a registry", "1.1.0", "2.20.0", "resolve hub.omnitrustregistry.com/ilm/operator:1.1.0", refs},
		{"bad version", "1.1.0", "next", `ILM version "next"`, refs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := upstream(t, "1.1.0", manifest, tc.images)
			root := copyTree(t, "testdata/pin/before")

			err := (pinner{transport: transport}).pin(t.Context(), root, tc.operator, tc.ilm)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			assertUnchanged(t, root)
		})
	}
}

func TestPinNeedsAListedImage(t *testing.T) {
	manifest, err := os.ReadFile("testdata/pin/ilm-operator-v1.1.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := release.BOMImageRefs("2.20.0")
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream(t, "1.1.0", manifest, append(refs, "hub.omnitrustregistry.com/ilm/operator:1.1.0"))
	root := copyTree(t, "testdata/pin/before")
	file := filepath.Join(root, "release.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "ilm/operator:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(file, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	err = (pinner{transport: transport}).pin(t.Context(), root, "1.1.0", "2.20.0")

	if err == nil || !strings.Contains(err.Error(), "no line holds the image hub.omnitrustregistry.com/ilm/operator") {
		t.Fatalf("err = %v, want one naming the unlisted operator image", err)
	}
}

func TestPinRefusesDependencyVersionChange(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"postgresql", `"ghcr.io/cloudnative-pg/postgresql:18"`, `"ghcr.io/cloudnative-pg/postgresql:17"`},
		{"rabbitmq", `"rabbitmq:4.3.1-management"`, `"rabbitmq:4.2.0-management"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyTree(t, "testdata/pin/before")
			file := filepath.Join(root, "release.yaml")
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(tc.from), []byte(tc.to), 1)
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}

			err = (pinner{transport: failing{}}).pin(t.Context(), root, "1.1.0", "2.20.0")

			if err == nil || !strings.Contains(err.Error(), strings.Trim(tc.from, `"`)) || !strings.Contains(err.Error(), "by hand") {
				t.Fatalf("err = %v, want a refused change to %s", err, tc.from)
			}
			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Error("release.yaml changed")
			}
		})
	}
}

func TestCommands(t *testing.T) {
	root := copyTree(t, "testdata/pin/before")
	for _, tc := range []struct {
		name string
		run  func([]string) int
		args []string
		want int
	}{
		{"validate", validate, []string{"-f", "../../release.yaml", "--addons", "../../addons"}, 0},
		{"validate errors", validate, []string{"-f", filepath.Join(root, "release.yaml"), "--addons", filepath.Join(root, "addons")}, 1},
		{"validate missing", validate, []string{"-f", filepath.Join(root, "missing.yaml")}, 1},
		{"images", images, []string{"-f", "../../release.yaml", "--arch", "amd64"}, 0},
		{"images bad arch", images, []string{"--arch", "riscv64"}, 2},
		{"images missing", images, []string{"-f", filepath.Join(root, "missing.yaml"), "--arch", "arm64"}, 1},
		{"pin downgrade", pin, []string{"--root", root, "--operator", "v0.9.0", "--ilm", "2.20.0"}, 1},
		{"pin older ILM", pin, []string{"--root", root, "--operator", "v0.9.0", "--ilm", "2.18.0", "--older-ilm"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run(tc.args); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
	assertUnchanged(t, root)
}

func TestWriteAllLeavesFilesOnFailure(t *testing.T) {
	root := copyTree(t, "testdata/pin/before")
	files := map[string][]byte{"release.yaml": []byte("changed\n"), "missing/platform.yaml": []byte("changed\n")}

	if err := writeAll(root, files); err == nil {
		t.Fatal("writeAll into a missing directory succeeded")
	}

	assertUnchanged(t, root)
	if _, err := os.Stat(filepath.Join(root, "release.yaml.pin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temporary file left behind: %v", err)
	}
}
