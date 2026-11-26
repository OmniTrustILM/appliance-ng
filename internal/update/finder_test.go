package update_test

import (
	"bufio"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle/bundletest"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

func (e *env) finder(online update.Online) *update.Finder {
	return update.NewFinder(update.FinderDeps{
		Client:  e.client,
		Online:  online,
		Release: e.rel,
		Dir:     e.dir,
		Uploads: e.uploads,
		Log:     slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Key:     &e.key.PublicKey,
		Arch:    testArch,
		Timeout: 2 * time.Second,
		Space:   e.space,
	})
}

// available lists the bundles the status lists as available, as <kind>:<version>.
func (e *env) available() []string {
	e.t.Helper()
	var out []string
	if u := e.appliance().Status.Updates; u != nil {
		for _, b := range u.Available {
			out = append(out, b.Kind+":"+b.Version)
		}
	}
	slices.Sort(out)
	return out
}

// TestFinderVerifiesBeforeListing lists only the newer bundles for the manager's arch whose signature verifies, and
// reports the others as badSignature.
func TestFinderVerifiesBeforeListing(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	e.publish(e.build(e.system("0.9.0")), release.BundleSystem, "0.9.0-amd64")
	unsigned := e.system("1.2.0")
	unsigned.key = nil
	e.publish(e.build(unsigned), release.BundleSystem, "1.2.0-amd64")
	e.publish(e.build(e.platform("1.1.0", "2.20.0")), release.BundlePlatform, "1.1.0-amd64")
	otherKey := e.platform("1.3.0", "2.21.0")
	otherKey.key = bundletest.NewKey(t)
	e.publish(e.build(otherKey), release.BundlePlatform, "1.3.0-amd64")
	e.publish(e.build(e.platform("1.4.0", "2.22.0")), release.BundlePlatform, "1.2.0-amd64")
	arm := e.platform("1.5.0", "2.23.0")
	arm.arch = "arm64"
	e.publish(e.build(arm), release.BundlePlatform, "1.5.0-arm64")
	e.publish(e.build(e.platform("1.6.0", "2.24.0")), release.BundlePlatform, "1234567-amd64")

	err := e.finder(e.online()).Check(t.Context())
	if update.Reason(err) != update.BadSignature {
		t.Fatalf("Check = %v, want badSignature", err)
	}
	if got, want := e.available(), []string{"platform:1.1.0", "system:1.1.0"}; !slices.Equal(got, want) {
		t.Errorf("available %v, want %v", got, want)
	}
	u := e.appliance().Status.Updates
	if u.LastCheck == nil || u.LastCheckError != update.BadSignature {
		t.Errorf("last check %v, error %q", u.LastCheck, u.LastCheckError)
	}
	for _, b := range u.Available {
		if b.SizeBytes == 0 || b.Blocked != "" {
			t.Errorf("available %+v", b)
		}
	}
	notes, err := e.loader().Notes(release.BundlePlatform, "1.1.0")
	if err != nil || string(notes) != string(testNotes) {
		t.Errorf("cached notes %q, %v", notes, err)
	}
	if _, err := e.loader().Notes(release.BundlePlatform, "1.3.0"); err == nil {
		t.Error("the notes of a bundle with a bad signature are cached")
	}
	if leftovers := filesUnder(t, e.uploads); len(leftovers) != 0 {
		t.Errorf("the uploads directory keeps %v", leftovers)
	}
}

// TestFinderClearsTheErrorOnSuccess lists the bundles and no error once every bundle verifies.
func TestFinderClearsTheErrorOnSuccess(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	if err := e.finder(e.online()).Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.appliance().Status.Updates; u.LastCheckError != "" || len(u.Available) != 1 {
		t.Errorf("updates %+v", u)
	}
}

// TestFinderUsesProxy reaches the repository through the Appliance's proxy, trusting its additional CAs, with the
// credentials of its registries.
func TestFinderUsesProxy(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	proxy := newConnectProxy(t)
	ctx := t.Context()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sourceCert.Raw})
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: "trust"}, Data: map[string][]byte{"ca.pem": caPEM}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: "ghcr"}, Data: map[string][]byte{
			v1alpha1.CredentialsUsernameKey: []byte("puller"), v1alpha1.CredentialsPasswordKey: []byte(pullPassword),
		}},
	} {
		if err := e.client.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	a := e.appliance()
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTPS: proxy.URL}
	a.Spec.Trust = &v1alpha1.SecretKeyRef{Name: "trust", Key: "ca.pem"}
	a.Spec.Registries = []v1alpha1.RegistrySpec{{Host: sourceHost, CredentialsRef: "ghcr"}}
	if err := e.client.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	online := update.Online{Repository: e.online().Repository, Options: update.ApplianceOptions(e.client, sourceHost)}
	if err := e.finder(online).Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.available(); !slices.Equal(got, []string{"system:1.1.0"}) {
		t.Errorf("available %v", got)
	}
	if !slices.Contains(proxy.targets(), sourceHost+":443") {
		t.Errorf("the proxy was asked for %v, want %s:443", proxy.targets(), sourceHost)
	}
	if strings.Contains(e.logs.String(), pullPassword) {
		t.Error("the log holds the registry password")
	}
}

const pullPassword = "ghcr-pull-Pa55word"

// TestFinderErrorsAreFixedReasons records a failed check with a fixed reason, never the error text, and keeps the
// bundles found before.
func TestFinderErrorsAreFixedReasons(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedAddr := closed.Listener.Addr().String()
	closed.Close()
	slow := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	forbidden := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied for "+sourceHost, http.StatusForbidden)
	}))
	t.Cleanup(forbidden.Close)
	for name, c := range map[string]struct {
		addr string
		want string
	}{
		"unreachable": {closedAddr, update.Unreachable},
		"timeout":     {slow.Listener.Addr().String(), update.Timeout},
		"failed":      {forbidden.Listener.Addr().String(), update.Failed},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
			if err := e.finder(e.online()).Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
				return []remote.Option{remote.WithContext(ctx), remote.WithTransport(transportTo(c.addr))}, nil
			}}
			f := e.finder(online)
			err := f.Check(t.Context())
			if update.Reason(err) != c.want {
				t.Fatalf("Check = %v, want %s", err, c.want)
			}
			u := e.appliance().Status.Updates
			if u.LastCheckError != c.want {
				t.Errorf("last check error %q, want %q", u.LastCheckError, c.want)
			}
			if got := e.available(); !slices.Equal(got, []string{"system:1.1.0"}) {
				t.Errorf("available after a failed check %v", got)
			}
		})
	}
}

// TestFinderRunsDaily checks once a day at a fixed minute until its context ends.
func TestFinderRunsDaily(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	for minute, want := range map[int]time.Duration{
		10*60 + 45: 15 * time.Minute,
		10*60 + 30: 24 * time.Hour,
		9 * 60:     22*time.Hour + 30*time.Minute,
	} {
		if got := update.UntilMinute(now, minute); got != want {
			t.Errorf("UntilMinute(10:30, %d) = %v, want %v", minute, got, want)
		}
	}
	e := newEnv(t)
	f := e.finder(e.online())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.Start(ctx); err != nil {
		t.Errorf("Start after its context ended = %v", err)
	}
}

// TestFetchLoadsOnline copies a bundle found online into zot and stores its files.
func TestFetchLoadsOnline(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.platform("1.1.0", "2.20.0")), release.BundlePlatform, "1.1.0-amd64")
	l := e.loader()
	if err := l.Fetch(t.Context(), release.BundlePlatform, "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if got := e.zotBundles(release.BundlePlatform); !slices.Equal(got, []string{"1.1.0-amd64"}) {
		t.Errorf("zot bundles %v", got)
	}
	if got := e.stored(release.BundlePlatform); !slices.Equal(got, []string{"1.1.0-amd64"}) {
		t.Errorf("stored %v", got)
	}
	repos := e.zotRepositories()
	if !slices.Contains(repos, sourceHost+"/platform/tool") {
		t.Errorf("zot repositories %v lack the image under its upstream name", repos)
	}
	if st := l.Last(); st.State != update.StateLoaded || st.Version != "1.1.0" {
		t.Errorf("Last = %+v", st)
	}
	if entry := e.audit.last(); entry.Details["source"] != "online" || entry.Result != "success" {
		t.Errorf("audit entry %+v", entry)
	}
	if leftovers := filesUnder(t, e.uploads); len(leftovers) != 0 {
		t.Errorf("the uploads directory keeps %v", leftovers)
	}
}

// TestFetchRejects refuses a bundle online before zot or the bundle directory changes.
func TestFetchRejects(t *testing.T) {
	cases := []struct {
		name    string
		version string
		prepare func(e *env)
		want    string
	}{
		{"unsigned", "1.1.0", func(e *env) {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			e.publish(e.build(s), release.BundlePlatform, "1.1.0-amd64")
		}, update.Unsigned},
		{"other key", "1.1.0", func(e *env) {
			s := e.platform("1.1.0", "2.20.0")
			s.key = bundletest.NewKey(e.t)
			e.publish(e.build(s), release.BundlePlatform, "1.1.0-amd64")
		}, update.BadSignature},
		{"another version under the tag", "1.1.0", func(e *env) {
			e.publish(e.build(e.platform("1.4.0", "2.20.0")), release.BundlePlatform, "1.1.0-amd64")
		}, update.BadSignature},
		{"missing", "1.1.0", func(*env) {}, update.NotFound},
		{"platform older than running", "1.1.0", func(e *env) {
			e.publish(e.build(e.platform("1.1.0", "2.18.0")), release.BundlePlatform, "1.1.0-amd64")
		}, update.OlderThanRunning},
		{"no space", "1.1.0", func(e *env) {
			e.noSpace = true
			e.publish(e.build(e.platform("1.1.0", "2.20.0")), release.BundlePlatform, "1.1.0-amd64")
		}, update.NoSpace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.prepare(e)
			err := e.loader().Fetch(t.Context(), release.BundlePlatform, c.version)
			if update.Reason(err) != c.want {
				t.Fatalf("Fetch = %v, want %s", err, c.want)
			}
			if repos := e.zotRepositories(); len(repos) != 0 {
				t.Errorf("zot holds %v after a refused fetch", repos)
			}
			if _, err := os.Stat(e.dir); !os.IsNotExist(err) {
				t.Errorf("the bundle directory exists after a refused fetch: %v", err)
			}
			if leftovers := filesUnder(t, e.uploads); len(leftovers) != 0 {
				t.Errorf("the uploads directory keeps %v", leftovers)
			}
		})
	}
}

// transportTo reaches addr for every address, trusting the test certificate.
func transportTo(addr string) *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		TLSClientConfig: tlsConfig(sourcePool),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}
}

// connectProxy is an HTTPS proxy that tunnels every CONNECT to the source registry and records its targets.
type connectProxy struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

func (p *connectProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

func (p *connectProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, r.Host)
	p.mu.Unlock()
	upstream, err := net.Dial("tcp", sourceAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go pipe(upstream, buf.Reader)
	_, _ = io.Copy(conn, upstream)
	_ = conn.Close()
	_ = upstream.Close()
}

func pipe(dst net.Conn, src *bufio.Reader) {
	_, _ = io.Copy(dst, src)
}

// TestFetchTimesOut ends a fetch from a repository that stalls with the reason timeout and releases the admission.
func TestFetchTimesOut(t *testing.T) {
	stalled := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(stalled.Close)
	e := newEnv(t)
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(transportTo(stalled.Listener.Addr().String()))}, nil
	}}
	l := update.NewLoader(update.LoaderDeps{
		Client: e.client, Admission: e.lock, Backups: e.lock, Audit: e.audit, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Release: e.rel, Dir: e.dir, Uploads: e.uploads, Online: online, Key: &e.key.PublicKey, Arch: testArch,
		Space: e.space, FetchTimeout: 300 * time.Millisecond,
	})
	r, err := l.Reserve(t.Context(), release.BundlePlatform, "admin")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Fetch("1.1.0"):
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch did not end")
	}
	if st := l.Last(); st.State != update.StateRefused || st.Reason != update.Timeout {
		t.Errorf("Last = %+v, want refused with timeout", st)
	}
	free, err := e.lock.Hold()
	if err != nil {
		t.Fatalf("the admission is still held: %v", err)
	}
	free()
}

// TestFinderSkipsUnreadableBundles leaves out a bundle the repository cannot serve and lists the others.
func TestFinderSkipsUnreadableBundles(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	e.publish(e.build(e.system("1.2.0")), release.BundleSystem, "1.2.0-amd64")
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(notFound{"/manifests/1.2.0-amd64"})}, nil
	}}
	err := e.finder(online).Check(t.Context())
	if update.Reason(err) != update.Failed {
		t.Fatalf("Check = %v, want failed", err)
	}
	if got := e.available(); !slices.Equal(got, []string{"system:1.1.0"}) {
		t.Errorf("available %v", got)
	}
}

// notFound answers 404 to the requests whose path ends in suffix and passes the others to the source registry.
type notFound struct{ suffix string }

func (n notFound) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, n.suffix) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	return remote.DefaultTransport.RoundTrip(req)
}

// TestFinderReportsTheEndOfTheCheck reports a repository that becomes unreachable as unreachable, even after a
// bundle was left out, and keeps the bundles found before.
func TestFinderReportsTheEndOfTheCheck(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	e.publish(e.build(e.system("1.2.0")), release.BundleSystem, "1.2.0-amd64")
	if err := e.finder(e.online()).Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(unreachablePlatform{notFound{"/manifests/1.2.0-amd64"}})}, nil
	}}
	if err := e.finder(online).Check(t.Context()); update.Reason(err) != update.Unreachable {
		t.Fatalf("Check = %v, want unreachable", err)
	}
	if u := e.appliance().Status.Updates; u.LastCheckError != update.Unreachable {
		t.Errorf("last check error %q", u.LastCheckError)
	}
	if got := e.available(); !slices.Equal(got, []string{"system:1.1.0", "system:1.2.0"}) {
		t.Errorf("available %v, want the bundles found before", got)
	}
}

// unreachablePlatform fails every request for the platform repository as if the network were down.
type unreachablePlatform struct{ next http.RoundTripper }

func (u unreachablePlatform) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/platform/") {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return u.next.RoundTrip(req)
}

// TestFinderChecksOneAtATime lets a check asked for while another runs wait for that one and share its result. The
// shared check does not end with the request that started it, and a caller whose context ends stops waiting.
func TestFinderChecksOneAtATime(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	gate := &gatedList{entered: make(chan struct{}), release: make(chan struct{})}
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(gate)}, nil
	}}
	f := e.finder(online)
	leaderCtx, leave := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { leader <- f.Check(leaderCtx) }()
	<-gate.entered
	waiterCtx := &waiting{Context: context.Background(), waits: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- f.Check(waiterCtx) }()
	<-waiterCtx.waits
	leave()
	if err := <-leader; err == nil {
		t.Error("the check of a caller that left succeeded")
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.Check(gone); err == nil {
		t.Error("a check whose context ended while waiting succeeded")
	}
	close(gate.release)
	if err := <-waiter; err != nil {
		t.Fatalf("the waiting check = %v", err)
	}
	if got := gate.lists.Load(); got != 2 {
		t.Errorf("the repository was listed %d times, want once per kind", got)
	}
	if got := e.available(); !slices.Equal(got, []string{"system:1.1.0"}) {
		t.Errorf("available %v", got)
	}
}

// waiting is a context that closes waits once a caller waits on it.
type waiting struct {
	context.Context
	once  sync.Once
	waits chan struct{}
}

func (w *waiting) Done() <-chan struct{} {
	w.once.Do(func() { close(w.waits) })
	return w.Context.Done()
}

// gatedList holds the first listing of a repository until release is closed and counts the listings.
type gatedList struct {
	entered, release chan struct{}
	once             sync.Once
	lists            atomic.Int32
}

func (g *gatedList) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/tags/list") {
		if g.lists.Add(1) == 1 {
			g.once.Do(func() { close(g.entered) })
			<-g.release
		}
	}
	return remote.DefaultTransport.RoundTrip(req)
}

// TestFinderChecksSpaceBeforeEachBundle ends a check without reading a bundle while the data volume lacks the space
// for its metadata.
func TestFinderChecksSpaceBeforeEachBundle(t *testing.T) {
	e := newEnv(t)
	e.publish(e.build(e.system("1.1.0")), release.BundleSystem, "1.1.0-amd64")
	e.noSpace = true
	rec := &recorder{}
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(rec)}, nil
	}}
	err := e.finder(online).Check(t.Context())
	if update.Reason(err) != update.Failed {
		t.Fatalf("Check = %v, want failed", err)
	}
	if u := e.appliance().Status.Updates; u.LastCheckError != update.Failed {
		t.Errorf("last check error %q, want failed", u.LastCheckError)
	}
	if len(e.spaceChecks) != 1 || e.spaceChecks[0] < 128<<20 {
		t.Errorf("space checks %v, want one for the metadata and signatures", e.spaceChecks)
	}
	if rec.asked("/manifests/1.1.0-amd64") {
		t.Error("the bundle was read without the space for it")
	}
}

// TestFinderInspectsTheNewestBundles reads only the ten newest bundles of a kind newer than what runs.
func TestFinderInspectsTheNewestBundles(t *testing.T) {
	e := newEnv(t)
	dir := e.build(e.system("1.1.0"))
	var want []string
	for minor := 1; minor <= 12; minor++ {
		v := fmt.Sprintf("1.%d.0", minor)
		e.publish(dir, release.BundleSystem, v+"-amd64")
		if minor > 2 {
			want = append(want, v+"-amd64")
		}
	}
	rec := &recorder{}
	online := update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
		return []remote.Option{remote.WithContext(ctx), remote.WithTransport(rec)}, nil
	}}
	_ = e.finder(online).Check(t.Context())
	for minor := 1; minor <= 12; minor++ {
		tag := fmt.Sprintf("1.%d.0-amd64", minor)
		if got := rec.asked("/manifests/" + tag); got != slices.Contains(want, tag) {
			t.Errorf("%s read %v, want %v", tag, got, !got)
		}
	}
}
