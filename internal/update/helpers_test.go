package update_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/google/go-containerregistry/pkg/name"
	ggcr "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle/bundletest"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const (
	testArch     = "amd64"
	testOperator = "v1.1.0"
	managerYAML  = "kind: Deployment\n"
	operatorYAML = "kind: Deployment\nmetadata: {name: ilm-operator}\n"
)

var testNotes = []byte("# Release notes\n")

// env is an appliance for the loader and the finder: a registry the bundle images come from, which also serves as
// the online repository, an in-process zot behind TLS, a fake cluster and the data directories.
type env struct {
	t   *testing.T
	src string
	// repo is the env's own online repository.
	repo    string
	key     *ecdsa.PrivateKey
	zotHost string
	zotPool *x509.CertPool
	zot     *registry.Client
	client  client.Client
	dir     string
	uploads string
	audit   *fakeAudit
	logs    *bytes.Buffer
	lock    *fakeLock
	// noSpace makes the free space check refuse.
	noSpace bool
	// spaceChecks records the sizes the free space check was asked about.
	spaceChecks []uint64
	rel         *release.Manifest
	// upstreams are the mirrored registries the loader is given, the release's own when empty.
	upstreams []string
}

// sourceHost names the registry the bundle images and the online bundles come from. Image references with a port
// cannot be pushed under their upstream names, so the in-process registry answers as this host: remote's default
// transport dials it for every address and trusts its certificate, which names example.com.
const sourceHost = "example.com"

// TestMain serves the source registry for every test.
func TestMain(m *testing.M) {
	src := httptest.NewTLSServer(ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0))))
	sourcePool = x509.NewCertPool()
	sourceCert = src.Certificate()
	sourcePool.AddCert(sourceCert)
	sourceAddr = src.Listener.Addr().String()
	remote.DefaultTransport = sourceTransport(nil)
	code := m.Run()
	stopCluster()
	src.Close()
	os.Exit(code)
}

var (
	repoCount  atomic.Int64
	sourcePool *x509.CertPool
	sourceCert *x509.Certificate
	sourceAddr string
)

// sourceTransport reaches the source registry for every address, through proxy when it is set.
func sourceTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		Proxy:           proxy,
		TLSClientConfig: tlsConfig(sourcePool),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, sourceAddr)
		},
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	zot := httptest.NewTLSServer(ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(zot.Close)
	pool := x509.NewCertPool()
	pool.AddCert(zot.Certificate())
	zotHost := strings.TrimPrefix(zot.URL, "https://")
	root := t.TempDir()
	e := &env{
		t: t, src: sourceHost, repo: fmt.Sprintf("%s/appliance-ng/%d", sourceHost, repoCount.Add(1)), key: bundletest.NewKey(t),
		zotHost: zotHost, zotPool: pool, zot: registry.New(zotHost, pool, registry.PushUser, ""),
		dir: filepath.Join(root, "bundles"), uploads: filepath.Join(root, "updates"),
		audit: &fakeAudit{}, logs: &bytes.Buffer{}, lock: &fakeLock{},
		rel: &release.Manifest{
			Version: "1.0.0", Compatibility: release.Compatibility{Operator: ">=1.0.0 <2.0.0", MinAppliance: "1.0.0"},
			// The release mirrors the registry the bundle images come from.
			Images: []release.Image{{Ref: sourceHost + "/release/tool:1.0"}},
		},
	}
	if err := os.MkdirAll(e.uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	e.client = fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&v1alpha1.Appliance{}).
		WithObjects(&v1alpha1.Appliance{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
			Status: v1alpha1.ApplianceStatus{Versions: v1alpha1.Versions{
				Appliance: "1.0.0", Operator: "1.0.0", ILM: "2.19.0",
			}},
		}).Build()
	return e
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (e *env) space(_ string, expected uint64) error {
	e.spaceChecks = append(e.spaceChecks, expected)
	if e.noSpace {
		return storage.ErrNoSpace
	}
	return nil
}

// online is the repository the env's registry serves the bundles from.
func (e *env) online() update.Online {
	return update.Online{
		Repository: e.repo,
		Options: func(ctx context.Context) ([]remote.Option, error) {
			return []remote.Option{remote.WithContext(ctx)}, nil
		},
	}
}

func (e *env) loader() *update.Loader {
	return e.loaderWith(e.online())
}

// loaderWith is the env's loader reading the online repository online.
func (e *env) loaderWith(online update.Online) *update.Loader {
	return update.NewLoader(update.LoaderDeps{
		Client:    e.client,
		Registry:  func(context.Context) (update.Registry, error) { return e.zot, nil },
		Admission: e.lock,
		Backups:   e.lock,
		Audit:     e.audit,
		Log:       slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Release:   e.rel,
		Upstreams: e.upstreams,
		Dir:       e.dir,
		Uploads:   e.uploads,
		Online:    online,
		Key:       &e.key.PublicKey,
		Arch:      testArch,
		Space:     e.space,
	})
}

// spec describes a bundle to build.
type spec struct {
	kind, arch, version string
	// ilm is the ILM version of a platform bundle; operatorRange the operator range of a system bundle.
	ilm, operatorRange string
	// key signs the bundle; nil leaves it unsigned.
	key *ecdsa.PrivateKey
	// installer adds the installer built with the appliance to a system bundle.
	installer bool
}

func (e *env) system(version string) spec {
	return spec{kind: release.BundleSystem, arch: testArch, version: version, ilm: "2.20.0", operatorRange: ">=1.0.0 <2.0.0", key: e.key}
}

func (e *env) platform(version, ilm string) spec {
	return spec{kind: release.BundlePlatform, arch: testArch, version: version, ilm: ilm, operatorRange: ">=1.0.0 <2.0.0", key: e.key}
}

// build builds the bundle s into a new OCI layout directory from images pushed to the env's registry.
func (e *env) build(s spec) string {
	t := e.t
	t.Helper()
	files := fstest.MapFS{
		"release.yaml":             {Data: []byte("version: " + s.version + "\n")},
		"addons/manager.yaml":      {Data: []byte(managerYAML)},
		"addons/ilm-operator.yaml": {Data: []byte(operatorYAML)},
	}
	m := &release.Manifest{
		Version:       s.version,
		Compatibility: release.Compatibility{Operator: s.operatorRange, MinAppliance: "1.0.0"},
		Talos:         release.TalosPin{Version: "v1.14.5", Kubernetes: "v1.37.1"},
		Platform:      release.PlatformDefaults{ILMVersion: s.ilm},
		Addons: []release.AddonFile{
			{Name: "appliance-manager", File: "manager.yaml", Version: "local", SHA256: sha(managerYAML)},
			{Name: "ilm-operator", File: "ilm-operator.yaml", Version: testOperator, SHA256: sha(operatorYAML)},
		},
		Images: []release.Image{
			e.pushImage(e.src+"/"+s.kind+"/tool:"+s.version, s.kind, s.arch),
			e.pushImage(e.src+"/"+s.kind+"/shared:1.0", s.kind, s.arch),
		},
	}
	if s.installer {
		m.Images = append(m.Images, e.pushImage(e.src+"/installer:"+s.version+"-"+s.arch, s.kind, s.arch))
	}
	dir := filepath.Join(t.TempDir(), "layout")
	if _, err := bundle.Build(t.Context(), m, s.kind, s.arch, bundle.Inputs{Files: files, Notes: testNotes}, dir); err != nil {
		t.Fatal(err)
	}
	if s.key != nil {
		bundletest.Sign(t, dir, s.key)
	}
	return dir
}

// upload writes the tar of the layout dir into the uploads directory, as an upload would, and returns its path.
func (e *env) upload(dir string) string {
	e.t.Helper()
	data, err := os.ReadFile(bundletest.Tar(e.t, dir))
	if err != nil {
		e.t.Fatal(err)
	}
	return e.writeUpload(data)
}

// writeUpload writes data into the uploads directory, as an upload would, and returns its path.
func (e *env) writeUpload(data []byte) string {
	e.t.Helper()
	path := filepath.Join(e.uploads, "upload.tar")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// truncate cuts n bytes off the end of the file at path and returns path.
func (e *env) truncate(path string, n int64) string {
	e.t.Helper()
	info, err := os.Stat(path)
	if err == nil {
		err = os.Truncate(path, info.Size()-n)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return path
}

// sharedImages keeps the image pushed under a reference, so that every bundle lists the same image for it.
var sharedImages sync.Map

// pushImage pushes a single-arch OCI image under ref, once per reference and arch.
func (e *env) pushImage(ref, kind, arch string) release.Image {
	e.t.Helper()
	key := ref + "/" + arch
	img, ok := sharedImages.Load(key)
	if !ok {
		img, _ = sharedImages.LoadOrStore(key, randomImage(e.t, arch))
	}
	tag, err := name.ParseReference(ref)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := remote.Write(tag, img.(v1.Image)); err != nil {
		e.t.Fatal(err)
	}
	d, err := img.(v1.Image).Digest()
	if err != nil {
		e.t.Fatal(err)
	}
	return release.Image{Ref: ref, Digest: d.String(), Bundle: kind, Arches: []string{arch}}
}

func randomImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(512, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf.OS, cf.Architecture = "linux", arch
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return mutate.ConfigMediaType(mutate.MediaType(img, types.OCIManifestSchema1), types.OCIConfigJSON)
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// publish pushes the bundle in the layout dir with its signatures to the online repository as <kind>:<tag>.
func (e *env) publish(dir, kind, tag string) {
	t := e.t
	t.Helper()
	repo, err := name.NewRepository(e.online().Repository + "/" + kind)
	if err != nil {
		t.Fatal(err)
	}
	top, err := layout.Path(dir).ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, err := top.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	// The bundle index goes first, so that its signatures have their subject.
	for _, mt := range []types.MediaType{types.OCIImageIndex, types.OCIManifestSchema1} {
		for _, d := range im.Manifests {
			if d.MediaType == mt {
				e.push(top, d, repo, tag)
			}
		}
	}
}

// push pushes the bundle index d of top as repo:tag, or the signature d by its digest.
func (e *env) push(top v1.ImageIndex, d v1.Descriptor, repo name.Repository, tag string) {
	e.t.Helper()
	var err error
	if d.MediaType == types.OCIImageIndex {
		var idx v1.ImageIndex
		if idx, err = top.ImageIndex(d.Digest); err == nil {
			err = remote.WriteIndex(repo.Tag(tag), idx)
		}
	} else {
		var img v1.Image
		if img, err = top.Image(d.Digest); err == nil {
			err = remote.Write(repo.Digest(d.Digest.String()), img)
		}
	}
	if err != nil {
		e.t.Fatal(err)
	}
}

// zotRepositories lists the repositories in zot.
func (e *env) zotRepositories() []string {
	e.t.Helper()
	reg, err := name.NewRegistry(e.zotHost)
	if err != nil {
		e.t.Fatal(err)
	}
	repos, err := remote.Catalog(e.t.Context(), reg, remote.WithTransport(&http.Transport{TLSClientConfig: tlsConfig(e.zotPool)}))
	if err != nil {
		e.t.Fatal(err)
	}
	return repos
}

// zotBundles lists the bundle tags of kind in zot.
func (e *env) zotBundles(kind string) []string {
	e.t.Helper()
	tags, err := e.zot.Bundles(e.t.Context(), kind)
	if err != nil {
		e.t.Fatal(err)
	}
	slices.Sort(tags)
	return tags
}

// stored lists the bundle directories of kind on the data volume.
func (e *env) stored(kind string) []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.dir, kind))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func (e *env) appliance() *v1alpha1.Appliance {
	e.t.Helper()
	a := &v1alpha1.Appliance{}
	if err := e.client.Get(e.t.Context(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) setVersions(v v1alpha1.Versions) {
	e.t.Helper()
	a := e.appliance()
	a.Status.Versions = v
	if err := e.client.Status().Update(e.t.Context(), a); err != nil {
		e.t.Fatal(err)
	}
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeAudit) Write(e audit.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) last() audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return audit.Entry{}
	}
	return f.entries[len(f.entries)-1]
}

// fakeLock is the shared admission of backup.Runner, and its local backups.
type fakeLock struct {
	mu   sync.Mutex
	held bool
	// backups are the local backups.
	backups []v1alpha1.BackupRecord
}

func (l *fakeLock) Local() ([]v1alpha1.BackupRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.backups), nil
}

func (l *fakeLock) setBackups(records ...v1alpha1.BackupRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.backups = records
}

func (l *fakeLock) Hold() (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil, backup.ErrBusy
	}
	l.held = true
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.held = false
	}, nil
}

func tlsConfig(pool *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}
