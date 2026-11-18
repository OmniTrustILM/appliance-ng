package auth

import (
	"crypto/subtle"
	"net/http"
)

// CSRFHeader is the request header that carries the session's CSRF token.
const CSRFHeader = "X-CSRF-Token"

// csrfOK reports whether r may proceed: safe methods always may, the others
// only with the session's CSRF token, compared in constant time.
func csrfOK(r *http.Request, want string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
