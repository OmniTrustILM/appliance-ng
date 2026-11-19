package certs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
)

func fingerprint(t *testing.T, issuer *testIssuer) string {
	t.Helper()
	sum := sha256.Sum256(issuer.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func fingerprints(preview []CertSummary) []string {
	out := make([]string, len(preview))
	for i, c := range preview {
		out[i] = c.SHA256
	}
	return out
}

func TestMergeBundleAdditiveAndDeduplicated(t *testing.T) {
	rootA, pemA := newIssuer(t, "Root A", nil)
	rootB, pemB := newIssuer(t, "Root B", nil)
	intermediate, pemI := newIssuer(t, "Intermediate", rootB)

	merged, preview, err := MergeBundle(nil, pemA)
	if err != nil {
		t.Fatalf("MergeBundle into an empty bundle: %v", err)
	}
	if len(preview) != 1 || !preview[0].New || preview[0].SHA256 != fingerprint(t, rootA) {
		t.Fatalf("preview %+v, want Root A as new", preview)
	}
	if got := preview[0]; got.Subject != "CN=Root A" || got.Issuer != "CN=Root A" || !got.NotAfter.Equal(rootA.cert.NotAfter) {
		t.Errorf("summary %+v, want subject, issuer and expiry of Root A", got)
	}

	// Root A again, Root B and an intermediate, with Root B twice: only Root B and the intermediate are new.
	added := slices.Concat(pemA, []byte("\n"), pemB, pemI, pemB)
	merged, preview, err = MergeBundle(merged, added)
	if err != nil {
		t.Fatalf("MergeBundle: %v", err)
	}
	want := []string{fingerprint(t, rootA), fingerprint(t, rootB), fingerprint(t, intermediate)}
	if got := fingerprints(preview); !slices.Equal(got, want) {
		t.Fatalf("preview %v, want %v in bundle order", got, want)
	}
	if preview[0].New || !preview[1].New || !preview[2].New {
		t.Errorf("new flags %v %v %v, want only the added certificates new", preview[0].New, preview[1].New, preview[2].New)
	}
	if !bytes.Equal(merged, slices.Concat(pemA, pemB, pemI)) {
		t.Errorf("merged bundle:\n%s\nwant Root A, Root B and the intermediate once each", merged)
	}

	listed, err := ParseBundle(merged)
	if err != nil || !slices.Equal(fingerprints(listed), want) || slices.ContainsFunc(listed, func(c CertSummary) bool { return c.New }) {
		t.Errorf("ParseBundle = %+v, %v; want the three certificates, none new", listed, err)
	}

	again, preview, err := MergeBundle(merged, pemB)
	if err != nil || !bytes.Equal(again, merged) || slices.ContainsFunc(preview, func(c CertSummary) bool { return c.New }) {
		t.Errorf("adding a trusted certificate again changed the bundle or marked it new: %+v, %v", preview, err)
	}
}

func TestMergeBundleRejectsLeaf(t *testing.T) {
	f := newChain(t)
	_, pemA := newIssuer(t, "Root A", nil)

	for name, added := range map[string][]byte{
		"leaf alone":              f.valid.cert,
		"leaf in a bundle of CAs": slices.Concat(pemA, f.valid.cert, f.rootPEM),
	} {
		merged, preview, err := MergeBundle(pemA, added)
		var rejected *RejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("%s: err = %v, want a rejection", name, err)
		}
		if !strings.Contains(err.Error(), "CN="+uploadHosts[0]) {
			t.Errorf("%s: %q does not name the leaf", name, err)
		}
		if merged != nil || preview != nil {
			t.Errorf("%s: a rejected upload returned a bundle", name)
		}
	}
}

func TestMergeBundleRejectsWhatIsNotACertificate(t *testing.T) {
	_, pemA := newIssuer(t, "Root A", nil)
	for name, tc := range map[string]struct {
		added []byte
		want  string
	}{
		"empty":           {nil, "no certificate"},
		"text":            {[]byte("not a certificate"), "cannot be read"},
		"private key":     {keyPEM(t, ecKey(t)), "PRIVATE KEY"},
		"broken PEM body": {[]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), "cannot be read"},
	} {
		_, _, err := MergeBundle(pemA, tc.added)
		var rejected *RejectedError
		if !errors.As(err, &rejected) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a rejection mentioning %q", name, err, tc.want)
		}
	}
}

func TestMergeBundleKeepsAnyCertificateOfTheCurrentBundle(t *testing.T) {
	f := newChain(t)
	_, pemA := newIssuer(t, "Root A", nil)
	merged, preview, err := MergeBundle(f.valid.cert, pemA)
	if err != nil || len(preview) != 2 || !bytes.Equal(merged, slices.Concat(f.valid.cert, pemA)) {
		t.Errorf("MergeBundle over a current bundle with a leaf = %+v, %v; want it kept", preview, err)
	}
	if _, _, err := MergeBundle([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), pemA); err == nil {
		t.Error("an unreadable current bundle was accepted")
	} else if errors.As(err, new(*RejectedError)) {
		t.Errorf("an unreadable current bundle was blamed on the upload: %v", err)
	}
}

func TestRemoveFromBundle(t *testing.T) {
	rootA, pemA := newIssuer(t, "Root A", nil)
	rootB, pemB := newIssuer(t, "Root B", nil)
	bundle := slices.Concat(pemA, pemB)

	left, found, err := RemoveFromBundle(bundle, strings.ToUpper(fingerprint(t, rootA)))
	if err != nil || !found || !bytes.Equal(left, pemB) {
		t.Fatalf("RemoveFromBundle(Root A) = %q, %v, %v; want Root B left", left, found, err)
	}
	left, found, err = RemoveFromBundle(left, fingerprint(t, rootB))
	if err != nil || !found || len(left) != 0 {
		t.Fatalf("RemoveFromBundle(last) = %q, %v, %v; want an empty bundle", left, found, err)
	}
	if _, found, err := RemoveFromBundle(bundle, "00"); err != nil || found {
		t.Errorf("RemoveFromBundle(unknown) found %v, err %v; want not found", found, err)
	}
	if _, _, err := RemoveFromBundle([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), "00"); err == nil {
		t.Error("an unreadable bundle was accepted")
	}
}
