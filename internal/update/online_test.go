package update_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

// rawManifest is a manifest pushed as it is written, without the checks of a v1.Image.
type rawManifest struct {
	mt  types.MediaType
	raw []byte
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.raw, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

// putRaw pushes body into repo as tag, or by its digest without a tag, and returns its descriptor.
func putRaw(t *testing.T, repo name.Repository, tag string, mt types.MediaType, body any) v1.Descriptor {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	h, size, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var ref name.Reference = repo.Digest(h.String())
	if tag != "" {
		ref = repo.Tag(tag)
	}
	if err := remote.Put(ref, rawManifest{mt: mt, raw: raw}); err != nil {
		t.Fatal(err)
	}
	return v1.Descriptor{MediaType: mt, Digest: h, Size: size}
}

// recorder records the paths of the requests it passes to the source registry.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return remote.DefaultTransport.RoundTrip(req)
}

func (r *recorder) asked(suffix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// TestFetchRefusesUnknownSizesBeforeReading refuses a bundle whose metadata declares a negative size, which would
// leave the read of that blob unbounded, before the blob is read.
func TestFetchRefusesUnknownSizesBeforeReading(t *testing.T) {
	for caseName, sizes := range map[string][2]int64{
		"unknown config": {-1, 8},
		"unknown layer":  {16, -1},
		"overflow":       {1 << 62, 1 << 62},
	} {
		t.Run(caseName, func(t *testing.T) {
			e := newEnv(t)
			blobs := e.publishSized(release.BundlePlatform, "1.1.0-amd64", sizes[0], sizes[1])
			rec := &recorder{}
			l := e.loaderWith(update.Online{Repository: e.online().Repository, Options: func(ctx context.Context) ([]remote.Option, error) {
				return []remote.Option{remote.WithContext(ctx), remote.WithTransport(rec)}, nil
			}})
			err := l.Fetch(t.Context(), release.BundlePlatform, "1.1.0")
			if update.Reason(err) != update.BadSignature {
				t.Fatalf("Fetch = %v, want badSignature", err)
			}
			for _, d := range blobs {
				if rec.asked("/blobs/" + d.String()) {
					t.Errorf("the blob %s was read before its size was checked", d)
				}
			}
		})
	}
}

// publishSized publishes as kind:tag a bundle index whose metadata manifest declares its config and its layer of
// configSize and layerSize bytes, and returns the digests of the two blobs.
func (e *env) publishSized(kind, tag string, configSize, layerSize int64) []v1.Hash {
	t := e.t
	t.Helper()
	repo, err := name.NewRepository(e.online().Repository + "/" + kind)
	if err != nil {
		t.Fatal(err)
	}
	config := static.NewLayer([]byte(`{"big":"config"}`), types.OCIConfigJSON)
	layer := static.NewLayer([]byte("metadata"), types.OCILayer)
	var digests []v1.Hash
	for _, l := range []v1.Layer{config, layer} {
		if err := remote.WriteLayer(repo, l); err != nil {
			t.Fatal(err)
		}
		d, _ := l.Digest()
		digests = append(digests, d)
	}
	md := putRaw(t, repo, "", types.OCIManifestSchema1, v1.Manifest{
		SchemaVersion: 2, MediaType: types.OCIManifestSchema1,
		Config: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: digests[0], Size: configSize},
		Layers: []v1.Descriptor{{MediaType: types.OCILayer, Digest: digests[1], Size: layerSize}},
	})
	putRaw(t, repo, tag, types.OCIImageIndex, v1.IndexManifest{
		SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{md},
	})
	return digests
}
