package ilmclient

import (
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// MaxDiscovery caps a discovery document read into memory.
const MaxDiscovery = 1 << 20

// maxRedirects caps the redirects a request to an identity provider follows.
const maxRedirects = 10

// ProviderClient reaches an identity provider through proxy when it names one, trusting roots. A zero timeout leaves
// the request's context to bound it. It follows only redirects to https URLs.
func ProviderClient(roots *x509.CertPool, proxy *v1alpha1.ProxySpec, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != nil && (proxy.HTTP != "" || proxy.HTTPS != "") {
		resolve := (&httpproxy.Config{HTTPProxy: proxy.HTTP, HTTPSProxy: proxy.HTTPS, NoProxy: proxy.NoProxy}).ProxyFunc()
		transport.Proxy = func(r *http.Request) (*url.URL, error) { return resolve(r.URL) }
	}
	transport.TLSClientConfig.RootCAs = roots
	return &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: httpsRedirect}
}

// httpsRedirect refuses a redirect to a URL without https and stops after maxRedirects.
func httpsRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("refused a redirect to a URL without https")
	}
	if len(via) >= maxRedirects {
		return errors.New("stopped after too many redirects")
	}
	return nil
}
