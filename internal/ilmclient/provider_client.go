package ilmclient

import (
	"crypto/x509"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// MaxDiscovery caps a discovery document read into memory.
const MaxDiscovery = 1 << 20

// ProviderClient reaches an identity provider through proxy when it names one, trusting roots. A zero timeout leaves
// the request's context to bound it.
func ProviderClient(roots *x509.CertPool, proxy *v1alpha1.ProxySpec, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != nil && (proxy.HTTP != "" || proxy.HTTPS != "") {
		resolve := (&httpproxy.Config{HTTPProxy: proxy.HTTP, HTTPSProxy: proxy.HTTPS, NoProxy: proxy.NoProxy}).ProxyFunc()
		transport.Proxy = func(r *http.Request) (*url.URL, error) { return resolve(r.URL) }
	}
	transport.TLSClientConfig.RootCAs = roots
	return &http.Client{Timeout: timeout, Transport: transport}
}
