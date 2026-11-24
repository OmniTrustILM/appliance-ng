package bundle

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

// testdata/cosign-v3.tar is a platform bundle built by cmd/bundle, signed by the cosign v3 CLI with the throw-away
// key testdata/cosign-v3.pub, and exported with oras; it guards the signature format against drift.
func TestVerifyAcceptsCosignSignature(t *testing.T) {
	pemBytes, err := os.ReadFile("testdata/cosign-v3.pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open("testdata/cosign-v3.tar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	md, err := b.Verify(pub, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if md.Kind != "platform" || len(md.Images) != 1 {
		t.Errorf("metadata = %+v", md)
	}
	if _, err := b.Verify(newKey(t).Public(), "amd64"); err == nil {
		t.Error("Verify with another key: want an error")
	}

	sig := sigstoreBundleOf(t, b.dir)
	var material map[string]json.RawMessage
	if err := json.Unmarshal(sig["verificationMaterial"], &material); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tlogEntries", "timestampVerificationData"} {
		if v, ok := material[field]; ok && string(v) != "[]" && string(v) != "null" {
			t.Errorf("the signature has %s %s, want none", field, v)
		}
	}
}

// sigstoreBundleOf returns the sigstore bundle of the only signature in the layout in dir.
func sigstoreBundleOf(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	ii, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	p := layout.Path(dir)
	for _, d := range im.Manifests {
		data, err := p.Bytes(d.Digest)
		if err != nil {
			t.Fatal(err)
		}
		m, err := v1.ParseManifest(strings.NewReader(string(data)))
		if err != nil || m.ArtifactType != mediaTypeSigstoreBundle {
			continue
		}
		layer, err := p.Bytes(m.Layers[0].Digest)
		if err != nil {
			t.Fatal(err)
		}
		var sig map[string]json.RawMessage
		if err := json.Unmarshal(layer, &sig); err != nil {
			t.Fatal(err)
		}
		return sig
	}
	t.Fatal("no signature in the layout")
	return nil
}
