package registry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// IdleTimeout bounds each read and write of a connection to a registry and the wait for an answer's headers, so that
// a stalled registry or proxy fails an operation instead of holding it.
const IdleTimeout = 2 * time.Minute

// Guard makes t give up a connection that stalls for idle and refuses requests without https, which also
// refuses a redirect away from https.
func Guard(t *http.Transport, idle time.Duration) http.RoundTripper {
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleConn{Conn: c, idle: idle}, nil
	}
	t.ResponseHeaderTimeout = idle
	return httpsOnly{t}
}

type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, errors.New("refused a request without https")
	}
	return h.next.RoundTrip(req)
}

// idleConn fails a read or a write that makes no progress for idle. A write also extends the read deadline: an HTTP/1
// transport waits for the answer while it writes the request body, so the wait counts from the body's last progress.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	n, err := c.Conn.Write(p)
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return n, err
}
