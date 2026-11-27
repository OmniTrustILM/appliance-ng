package peer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

const (
	// ClientAddressHeader carries the address of the browser from a follower to the leader. Only the peer listener
	// reads it; the console listener drops it.
	ClientAddressHeader = "X-Appliance-Client-Address"
	// codeUnavailable is the error code of an API request no manager can answer at the moment.
	codeUnavailable = "managerUnavailable"
	// idleTimeout closes a connection to the leader that has not been used for a while.
	idleTimeout = 90 * time.Second
)

// apiPrefix is the path of the API, the only thing followers forward.
const apiPrefix = auth.BasePath + "/api/"

// Forwarder forwards requests to the leader's peer listener with the node's peer certificate.
type Forwarder struct {
	log       *slog.Logger
	transport http.RoundTripper
	port      int
}

// NewForwarder returns the forwarder that connects with the peer identity id.
func NewForwarder(log *slog.Logger, id *Identity) *Forwarder {
	return &Forwarder{
		log:       log,
		transport: &http.Transport{DialTLSContext: id.dialTLS, IdleConnTimeout: idleTimeout, MaxIdleConnsPerHost: 4},
		port:      Port,
	}
}

// Forward returns the handler that proxies a request to the peer listener on leaderAddr, passing the browser's
// address in ClientAddressHeader.
func (f *Forwarder) Forward(leaderAddr string) http.Handler {
	target := &url.URL{Scheme: "https", Host: net.JoinHostPort(leaderAddr, strconv.Itoa(f.port))}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del(ClientAddressHeader)
			if host, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set(ClientAddressHeader, host)
			}
		},
		Transport: f.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.Warn("could not forward a request to the leader", "leader", leaderAddr, "method", r.Method, "path", r.URL.Path,
				"error", err)
			unavailable(w)
		},
	}
}

// Router is the console listener's API handler: the leader answers with API itself; any other manager forwards to
// the leader, without the console listener's timeouts. The browser's address cannot be set by the browser.
type Router struct {
	API http.Handler
	// Leading reports whether this manager leads.
	Leading func() bool
	// Leader returns the leader's address, or ErrNoLeader.
	Leader  func(context.Context) (string, error)
	Forward func(leaderAddr string) http.Handler
	// Recheck is how often a forwarded request asks for the leader again; the request is cancelled once the manager it
	// was sent to no longer leads. Zero never asks again.
	Recheck time.Duration
	Log     *slog.Logger
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Del(ClientAddressHeader)
	if rt.Leading() {
		rt.API.ServeHTTP(w, r)
		return
	}
	addr, err := rt.Leader(r.Context())
	if err != nil {
		if !errors.Is(err, ErrNoLeader) {
			rt.Log.Warn("could not find the leader", "error", err)
		}
		unavailable(w)
		return
	}
	// The leader bounds the request with its own timeouts, which an upload, a download or a long operation extends;
	// the console listener's would cut the forwarded request off before them.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	ctx, cancel := context.WithCancel(r.Context())
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		rt.watchLeader(ctx, cancel, addr, r)
	}()
	defer func() {
		cancel()
		<-watched
	}()
	rt.Forward(addr).ServeHTTP(w, r.WithContext(ctx))
}

// watchLeader asks for the leader every Recheck until ctx ends and cancels the request r forwarded to addr once
// another manager leads or none does. Without a timeout on the leader's answer, a request to a paused leader would
// otherwise wait for it forever. A failure to read the leader leaves the request running.
func (rt *Router) watchLeader(ctx context.Context, cancel context.CancelFunc, addr string, r *http.Request) {
	if rt.Recheck <= 0 {
		return
	}
	ticker := time.NewTicker(rt.Recheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now, err := rt.Leader(ctx)
		if err != nil && !errors.Is(err, ErrNoLeader) {
			continue
		}
		if now != addr {
			rt.Log.Warn("the leader changed; cancelling the request forwarded to the previous one", "leader", addr,
				"method", r.Method, "path", r.URL.Path)
			cancel()
			return
		}
	}
}

// Accept returns the peer listener's handler: it serves api under the API's path while leading reports that this
// manager leads, with the browser's address from ClientAddressHeader as the request's remote address.
func Accept(api http.Handler, leading func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(apiPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !leading() {
			unavailable(w)
			return
		}
		if addr, err := netip.ParseAddr(r.Header.Get(ClientAddressHeader)); err == nil {
			r.RemoteAddr = net.JoinHostPort(addr.String(), "0")
		}
		r.Header.Del(ClientAddressHeader)
		api.ServeHTTP(w, r)
	}))
	return mux
}

func unavailable(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusServiceUnavailable, codeUnavailable,
		"The appliance manager is changing its leader. Try again in a few seconds.")
}
