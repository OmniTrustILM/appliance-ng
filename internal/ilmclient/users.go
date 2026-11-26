package ilmclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// userDetails are the details of a user that an update sets: ILM takes them from the update, so they are sent again.
// The groups are left out: ILM keeps them when an update names none. Roles and the enabled flag are not part of an
// update.
type userDetails struct {
	FirstName   *string `json:"firstName"`
	LastName    *string `json:"lastName"`
	Email       *string `json:"email"`
	Description *string `json:"description"`
}

// requestAttributeFields are the fields of a custom attribute in a request; an answer adds its label and type.
var requestAttributeFields = []string{"uuid", "name", "contentType", "content", "version"}

// hiddenContentTypes are the content types whose data ILM leaves out of an answer: sent back, the attribute would lose
// it.
var hiddenContentTypes = map[string]bool{`"secret"`: true, `"credential"`: true}

// unsupportedAttribute returns the error refusing an update over a custom attribute of a hidden content type, naming
// the attribute only, or nil when there is none.
func unsupportedAttribute(attributes []map[string]json.RawMessage) error {
	for _, attribute := range attributes {
		if hiddenContentTypes[string(attribute["contentType"])] {
			var name string
			_ = json.Unmarshal(attribute["name"], &name)
			return &Error{Op: "keep custom attribute " + strconv.Quote(name), Reason: ReasonUnsupportedAttribute}
		}
	}
	return nil
}

// requestAttributes turns the custom attributes of an answer into those of a request: ILM replaces a user's custom
// attributes with an update's, so they are sent again.
func requestAttributes(answer []map[string]json.RawMessage) []map[string]json.RawMessage {
	out := make([]map[string]json.RawMessage, 0, len(answer))
	for _, attribute := range answer {
		request := map[string]json.RawMessage{}
		for _, field := range requestAttributeFields {
			if v, ok := attribute[field]; ok {
				request[field] = v
			}
		}
		out = append(out, request)
	}
	return out
}

// SetUserCertificate makes der the certificate of the user username, keeping the user's other details, and tells
// whether ILM held another certificate for the user.
func (c *Client) SetUserCertificate(ctx context.Context, username string, der []byte) (bool, error) {
	uuid, err := lookupUser(ctx, c, username)
	if err != nil {
		return false, err
	}
	path := "/api/v1/users/" + url.PathEscape(uuid)
	answer, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	var user struct {
		userDetails
		CustomAttributes []map[string]json.RawMessage `json:"customAttributes"`
		Certificate      *struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"certificate"`
	}
	if err := decode(http.MethodGet+" "+path, answer, &user); err != nil {
		return false, err
	}
	if user.Certificate != nil && strings.EqualFold(user.Certificate.Fingerprint, Fingerprint(der)) {
		return false, nil
	}
	if err := unsupportedAttribute(user.CustomAttributes); err != nil {
		return false, err
	}
	body := struct {
		userDetails
		CustomAttributes []map[string]json.RawMessage `json:"customAttributes"`
		CertificateData  string                       `json:"certificateData"`
	}{user.userDetails, requestAttributes(user.CustomAttributes), base64.StdEncoding.EncodeToString(der)}
	if _, err := c.call(ctx, http.MethodPut, path, body); err != nil {
		return false, err
	}
	return true, nil
}

// CertificateFingerprint returns the Fingerprint of the first certificate in the PEM data, "" when it holds none.
func CertificateFingerprint(data []byte) string {
	der := FirstCertificate(data)
	if der == nil {
		return ""
	}
	return Fingerprint(der)
}

// FirstCertificate returns the DER of the first certificate in the PEM data, nil when it holds none.
func FirstCertificate(data []byte) []byte {
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes
		}
	}
}
