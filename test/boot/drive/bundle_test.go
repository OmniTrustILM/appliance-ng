package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testImage = "127.0.0.1:5443/hub.example/curl:8"

// testBundleEntries are the entries of a small bundle tar; the layer is its largest blob.
var testBundleEntries = []struct{ name, content string }{
	{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`},
	{"index.json", `{"schemaVersion":2}`},
	{"blobs/sha256/aa", "manifest"},
	{"blobs/sha256/bb", strings.Repeat("layer-", 200)},
	{"blobs/sha256/cc", "config"},
}

func writeTestBundle(t *testing.T) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range testBundleEntries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "platform.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, buf.Bytes()
}

// updatesConsole models the updates API: the bundle file is loaded, any other upload is refused as damaged, each
// after one poll that shows the load running.
type updatesConsole struct {
	bundle    []byte
	uploads   [][]byte
	polls     int
	load      map[string]any
	loaded    []map[string]any
	refuseOK  bool
	acceptAll bool
}

func (uc *updatesConsole) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /appliance/api/v1/updates/upload", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("kind") != "platform" || r.ContentLength != int64(len(data)) {
			t.Errorf("upload kind %q, Content-Length %d of %d bytes", r.URL.Query().Get("kind"), r.ContentLength, len(data))
		}
		uc.uploads = append(uc.uploads, data)
		uc.polls = 0
		switch {
		case bytes.Equal(data, uc.bundle) && !uc.refuseOK, uc.acceptAll:
			uc.load = map[string]any{"state": "loaded", "kind": "platform", "version": "0.1.0"}
			uc.loaded = []map[string]any{{"kind": "platform", "version": "0.1.0", "arch": "arm64", "ilm": "2.20.1"}}
		default:
			uc.load = map[string]any{"state": "refused", "kind": "platform", "reason": "damaged"}
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /appliance/api/v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		uc.polls++
		load := uc.load
		if uc.polls == 1 {
			load = map[string]any{"state": "running", "kind": "platform"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"available": []any{}, "loaded": uc.loaded, "load": load})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type fakePods struct {
	kubeconfigs, images []string
	err                 error
}

func (f *fakePods) runPod(_ context.Context, kubeconfig []byte, image string) error {
	f.kubeconfigs = append(f.kubeconfigs, string(kubeconfig))
	f.images = append(f.images, image)
	return f.err
}

func TestDriveBundleLoadNeverPrintsSecrets(t *testing.T) {
	fastPolls(t)
	path, data := writeTestBundle(t)
	uc := &updatesConsole{bundle: data}
	srv := uc.serve(t)
	pods := &fakePods{}
	o := options{bundleFile: path, image: testImage, password: testPassphrase}
	var err error
	out := captureStdout(t, func() {
		err = newClient(srv.URL, nil, "").loadBundle(context.Background(), o, []byte("token: "+testKubeSecret), pods)
	})
	if err != nil {
		t.Fatalf("loadBundle = %v\n%s", err, out)
	}
	for _, secret := range []string{testPassphrase, testKubeSecret} {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed a secret:\n%s", out)
		}
	}
	for _, want := range []string{"load loaded platform 0.1.0", "platform bundle 0.1.0-arm64 with ILM 2.20.1", "refused as damaged"} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
	if len(pods.images) != 1 || pods.images[0] != testImage || !strings.Contains(pods.kubeconfigs[0], testKubeSecret) {
		t.Errorf("ran pods of %v, want one of %s with the kit's kubeconfig", pods.images, testImage)
	}
	if len(uc.uploads) != 2 || !bytes.Equal(uc.uploads[0], data) {
		t.Fatalf("uploaded %d files, want the bundle and then its changed copy", len(uc.uploads))
	}
	if diff := differing(uc.uploads[1], data); len(diff) != 1 || !inEntry(data, diff[0], "blobs/sha256/bb") {
		t.Errorf("the copy differs at %v, want one byte of the largest blob", diff)
	}
}

func TestDriveBundleLoadFails(t *testing.T) {
	fastPolls(t)
	for name, tc := range map[string]struct {
		console updatesConsole
		podErr  error
		want    string
	}{
		"a refused bundle":         {console: updatesConsole{refuseOK: true}, want: "refused (damaged), want loaded"},
		"a pod that fails":         {podErr: errors.New("pod zot-only failed"), want: "pod zot-only failed"},
		"an accepted changed copy": {console: updatesConsole{acceptAll: true}, want: "want refused as damaged"},
	} {
		t.Run(name, func(t *testing.T) {
			path, data := writeTestBundle(t)
			uc := tc.console
			uc.bundle = data
			srv := uc.serve(t)
			o := options{bundleFile: path, image: testImage}
			var err error
			_ = captureStdout(t, func() {
				err = newClient(srv.URL, nil, "").loadBundle(context.Background(), o, nil, &fakePods{err: tc.podErr})
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadBundle = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// differing returns the offsets at which a and b differ.
func differing(a, b []byte) []int {
	var out []int
	for i := range a {
		if a[i] != b[i] {
			out = append(out, i)
		}
	}
	return out
}

// inEntry reports whether offset lies in the content of the tar entry name, found by its bytes.
func inEntry(data []byte, offset int, name string) bool {
	for _, e := range testBundleEntries {
		if e.name == name {
			start := bytes.Index(data, []byte(e.content))
			return start >= 0 && offset >= start && offset < start+len(e.content)
		}
	}
	return false
}

func TestLargestBlobRefusesAFileWithoutBlobs(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "index.json", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	if _, err := largestBlob(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("largestBlob found a blob in a file without blobs")
	}
	if _, err := largestBlob(bytes.NewReader([]byte("not a tar file, but long enough to hold a header block?"))); err == nil {
		t.Error("largestBlob read a file that is not a tar")
	}
}

func TestRunPodWaitsForTheCurlPod(t *testing.T) {
	fastPolls(t)
	for name, tc := range map[string]struct {
		phase   corev1.PodPhase
		wantErr string
	}{
		"succeeded": {phase: corev1.PodSucceeded},
		"failed":    {phase: corev1.PodFailed, wantErr: "pod zot-only failed: Error, exit code 1"},
	} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewClientset()
			cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				p := testPod(testImage)
				p.Status.Phase = tc.phase
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
				}}}
				return true, p, nil
			})
			var err error
			_ = captureStdout(t, func() { err = runPod(context.Background(), cs, testImage) })
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("runPod = %v, want %q", err, tc.wantErr)
			}
			if _, err := cs.CoreV1().Namespaces().Get(context.Background(), podNamespace, metav1.GetOptions{}); err == nil {
				t.Error("the test namespace was not deleted")
			}
		})
	}
}

func TestTestPodIsRestricted(t *testing.T) {
	p := testPod(testImage)
	c := p.Spec.Containers[0]
	if c.Image != testImage || p.Spec.RestartPolicy != corev1.RestartPolicyNever || !*c.SecurityContext.RunAsNonRoot ||
		*c.SecurityContext.AllowPrivilegeEscalation || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("test pod = %+v, want a one-off non-root pod of %s without privileges", p.Spec, testImage)
	}
	pending := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
	}}}}
	if got := waiting(pending); got != "ErrImagePull" {
		t.Errorf("waiting = %q, want ErrImagePull", got)
	}
	if got := waiting(&corev1.Pod{}); got != "no container status" {
		t.Errorf("waiting = %q for a pod without status", got)
	}
}

func TestValidateRequiresTheBundleLoadFlags(t *testing.T) {
	o := options{user: "u", password: "p", seedFile: "s", hostName: "h"}
	if err := o.validate(cmdBundleLoad); err == nil {
		t.Error("bundle-load without its flags passed validation")
	}
	o.bundleFile, o.image, o.kitFile, o.passphraseFile = "f", testImage, "k", "p"
	if err := o.validate(cmdBundleLoad); err != nil {
		t.Errorf("bundle-load with its flags = %v", err)
	}
}
