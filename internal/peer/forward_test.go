package peer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/console"
)

// follower returns the console routes of a follower whose leader is on loopback, reached over the peer listener on
// port with the follower's own peer certificate.
func follower(t *testing.T, ca *testCA, port int, leader func(context.Context) (string, error)) http.Handler {
	t.Helper()
	id := identity(t, followerNode, ca.secret(t, followerNode, CommonName, "127.0.0.1"))
	return console.Routes(&Router{
		API:     http.NotFoundHandler(),
		Leading: never,
		Leader:  leader,
		Forward: forwarder(id, port).Forward,
		Log:     quiet,
	})
}

func onLoopback(context.Context) (string, error) { return "127.0.0.1", nil }

func TestFollowerForwardsAPI(t *testing.T) {
	ca := newCA(t)
	seen := make(chan recorded, 4)
	_, port := leaderServer(t, ca, recordingAPI(seen), always)
	routes := follower(t, ca, port, onLoopback)

	req := httptest.NewRequest(http.MethodPost, "/appliance/api/v1/session?next=1", strings.NewReader(`{"user":"admin"}`))
	req.AddCookie(&http.Cookie{Name: "session", Value: "old"})
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("forwarded answer %d %q", rec.Code, rec.Body.String())
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Value != "new" {
		t.Errorf("the leader's cookie is not passed back: %v", c)
	}
	got := <-seen
	if got.method != http.MethodPost || got.path != "/appliance/api/v1/session" || got.cookie != "old" || string(got.body) != `{"user":"admin"}` {
		t.Errorf("the leader saw %+v", got)
	}

	// The health check and the console's files are answered by the follower itself.
	for _, path := range []string{"/healthz", "/appliance/", "/appliance/signin"} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
	select {
	case r := <-seen:
		t.Errorf("the follower forwarded %s %s", r.method, r.path)
	default:
	}

	t.Run("no leader", func(t *testing.T) {
		routes := follower(t, ca, port, func(context.Context) (string, error) { return "", ErrNoLeader })
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
		assertUnavailable(t, rec)
	})
	t.Run("leader unreachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		routes := follower(t, ca, closed, onLoopback)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
		assertUnavailable(t, rec)
	})
	t.Run("leader stepped down", func(t *testing.T) {
		_, port := leaderServer(t, ca, recordingAPI(seen), never)
		routes := follower(t, ca, port, onLoopback)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
		assertUnavailable(t, rec)
	})
	t.Run("leader serves itself", func(t *testing.T) {
		local := make(chan recorded, 1)
		routes := console.Routes(&Router{
			API: recordingAPI(local), Leading: always, Log: quiet,
			Leader:  func(context.Context) (string, error) { return "", errors.New("not asked") },
			Forward: func(string) http.Handler { return http.NotFoundHandler() },
		})
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
		if rec.Code != http.StatusCreated || len(local) != 1 {
			t.Errorf("the leader answered %d, its API saw %d requests", rec.Code, len(local))
		}
	})
}

func assertUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct{ Code, Message string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Code != codeUnavailable || body.Message == "" {
		t.Errorf("answer %d %+v, want 503 %s", rec.Code, body, codeUnavailable)
	}
}

func TestForwardedClientAddressOnlyFromPeers(t *testing.T) {
	ca := newCA(t)
	seen := make(chan recorded, 4)
	srv, port := leaderServer(t, ca, recordingAPI(seen), always)

	// A browser on a follower: the leader sees the browser's address, not the header the browser sent.
	routes := follower(t, ca, port, onLoopback)
	req := httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil)
	req.RemoteAddr = "192.0.2.7:5555"
	req.Header.Set(ClientAddressHeader, "203.0.113.9")
	routes.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got.remote != "192.0.2.7:0" || got.header != "" {
		t.Errorf("forwarded from a follower: the leader saw %q, header %q", got.remote, got.header)
	}

	// A browser on the leader's console listener cannot set its address.
	local := make(chan recorded, 1)
	leaderRoutes := console.Routes(&Router{API: recordingAPI(local), Leading: always, Log: quiet})
	req = httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil)
	req.RemoteAddr = "192.0.2.8:4444"
	req.Header.Set(ClientAddressHeader, "203.0.113.9")
	leaderRoutes.ServeHTTP(httptest.NewRecorder(), req)
	if got := <-local; got.remote != "192.0.2.8:4444" || got.header != "" {
		t.Errorf("on the console listener: the API saw %q, header %q", got.remote, got.header)
	}

	// A peer that sends no usable address is seen at its own address.
	id := identity(t, followerNode, ca.secret(t, followerNode, CommonName, "127.0.0.1"))
	peerClient := &http.Client{Transport: forwarder(id, port).transport}
	for _, header := range []string{"", "not-an-address"} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/appliance/api/v1/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(ClientAddressHeader, header)
		}
		resp, err := peerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := <-seen; !strings.HasPrefix(got.remote, "127.0.0.1:") || got.header != "" {
			t.Errorf("header %q: the leader saw %q, header %q", header, got.remote, got.header)
		}
	}
}

// TestForwardOutlastsConsoleTimeouts: an upload and a download through a follower take longer than the console
// listener's timeouts; the leader's own timeouts bound them.
func TestForwardOutlastsConsoleTimeouts(t *testing.T) {
	const timeout = 200 * time.Millisecond
	ca := newCA(t)
	received := make(chan int, 1)
	_, port := leaderServer(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- len(body)
		for range 4 {
			_, _ = w.Write([]byte("chunk"))
			_ = http.NewResponseController(w).Flush()
			time.Sleep(timeout)
		}
	}), always)
	srv := httptest.NewUnstartedServer(follower(t, ca, port, onLoopback))
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = timeout, timeout
	srv.StartTLS()
	t.Cleanup(srv.Close)

	upload, send := io.Pipe()
	go func() {
		for range 4 {
			_, _ = send.Write([]byte("part"))
			time.Sleep(timeout)
		}
		_ = send.Close()
	}()
	resp, err := srv.Client().Post(srv.URL+"/appliance/api/v1/updates/upload", "application/octet-stream", upload)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != strings.Repeat("chunk", 4) {
		t.Errorf("download %q, %v", body, err)
	}
	if n := <-received; n != len("part")*4 {
		t.Errorf("the leader received %d bytes", n)
	}
}

// TestForwardToPausedLeader: a paused leader accepts connections but never completes the handshake; the follower
// answers 503 once the dial timeout passes.
func TestForwardToPausedLeader(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	ca := newCA(t)
	id := identity(t, followerNode, ca.secret(t, followerNode, CommonName, "127.0.0.1"))
	id.dialTimeout = 100 * time.Millisecond
	routes := console.Routes(&Router{
		API: http.NotFoundHandler(), Leading: never, Leader: onLoopback, Log: quiet,
		Forward: forwarder(id, ln.Addr().(*net.TCPAddr).Port).Forward,
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
		done <- rec
	}()
	select {
	case rec := <-done:
		assertUnavailable(t, rec)
	case <-time.After(5 * time.Second):
		t.Fatal("the follower waits for a paused leader")
	}
}

// TestForwardCancelledWhenLeaderChanges: a leader that completes the handshake and then pauses never answers; the
// follower ends the forwarded request once another manager leads or none does.
func TestForwardCancelledWhenLeaderChanges(t *testing.T) {
	for name, after := range map[string]func(context.Context) (string, error){
		"another leader": func(context.Context) (string, error) { return "127.0.0.2", nil },
		"no leader":      func(context.Context) (string, error) { return "", ErrNoLeader },
	} {
		t.Run(name, func(t *testing.T) {
			ca := newCA(t)
			arrived := make(chan struct{})
			_, port := leaderServer(t, ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(arrived)
				<-r.Context().Done()
			}), always)
			var changed atomic.Bool
			leader := func(ctx context.Context) (string, error) {
				if changed.Load() {
					return after(ctx)
				}
				return onLoopback(ctx)
			}
			routes := watchingFollower(t, ca, port, leader)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
				done <- rec
			}()
			<-arrived
			select {
			case <-done:
				t.Fatal("the request ended while the leader still led")
			case <-time.After(5 * recheck):
			}
			changed.Store(true)
			select {
			case rec := <-done:
				assertUnavailable(t, rec)
			case <-time.After(5 * time.Second):
				t.Fatal("the follower waits for a leader that no longer leads")
			}
		})
	}
}

// TestForwardToSlowLeader: a leader that keeps leading answers late, even while the follower cannot read the leader
// for a moment, and its answer reaches the browser intact.
func TestForwardToSlowLeader(t *testing.T) {
	ca := newCA(t)
	_, port := leaderServer(t, ca, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * recheck)
		_, _ = w.Write([]byte("slow"))
		_ = http.NewResponseController(w).Flush()
		time.Sleep(5 * recheck)
		_, _ = w.Write([]byte(" answer"))
	}), always)
	var calls atomic.Int32
	leader := func(ctx context.Context) (string, error) {
		if calls.Add(1)%2 == 0 {
			return "", errors.New("the API server is not reachable")
		}
		return onLoopback(ctx)
	}
	rec := httptest.NewRecorder()
	watchingFollower(t, ca, port, leader).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/status", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "slow answer" {
		t.Errorf("answer %d %q", rec.Code, rec.Body.String())
	}
	if calls.Load() < 3 {
		t.Errorf("the follower asked for the leader %d times", calls.Load())
	}
}

const recheck = 20 * time.Millisecond

// watchingFollower returns the console routes of a follower that asks for the leader every recheck while it
// forwards a request.
func watchingFollower(t *testing.T, ca *testCA, port int, leader func(context.Context) (string, error)) http.Handler {
	t.Helper()
	id := identity(t, followerNode, ca.secret(t, followerNode, CommonName, "127.0.0.1"))
	return console.Routes(&Router{
		API: http.NotFoundHandler(), Leading: never, Leader: leader, Recheck: recheck, Log: quiet,
		Forward: forwarder(id, port).Forward,
	})
}
