package api

import (
	"bytes"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	cbomAccessKey = "AKIA-cbom-access-never-leaves"
	cbomSecretKey = "cbom-secret-key-never-leaves"
)

// cbomUpdate is a platform update that sets only the CBOM repository.
func cbomUpdate(c CBOMRepository) PlatformUpdate {
	return PlatformUpdate{HostName: "ilm.example.com", Components: []string{}, Cbom: &c}
}

func externalCBOM(accessKey, secretKey, ca *string) CBOMRepository {
	return CBOMRepository{Enabled: true, Storage: CBOMStorageS3, S3: &CBOMS3{
		Endpoint: "https://s3.example.com", Bucket: "cboms", Region: ptr("eu-central-1"), AccessKey: accessKey, SecretKey: secretKey, Ca: ca,
	}}
}

func getCBOM(t *testing.T, h *harness, s *session) CBOMRepository {
	t.Helper()
	rec := h.request(http.MethodGet, pathPlatform, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got PlatformState
	decode(t, rec, &got)
	return got.Cbom
}

func TestCBOMRepositorySettings(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	if got := getCBOM(t, h, s); got.Enabled || got.Storage != CBOMStorageAppliance || got.S3 != nil {
		t.Errorf("cbom before any choice = %+v, want off on the appliance", got)
	}

	wantStatus(t, h.request(http.MethodPut, pathPlatform, cbomUpdate(CBOMRepository{Enabled: true, Storage: CBOMStorageAppliance}), s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Platform.CBOM; !reflect.DeepEqual(got, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}) {
		t.Errorf("cbom spec %+v", got)
	}
	if last := h.entries()[len(h.entries())-1]; !slices.Equal(last.Paths, []string{"spec.platform.cbom"}) {
		t.Errorf("audit paths %v", last.Paths)
	}

	ca := string(newTestCA(t).pem)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, cbomUpdate(externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), &ca)), s), http.StatusAccepted)
	spec := getAppliance(t).Spec.Platform.CBOM
	want := &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageS3, CredentialsRef: CBOMSecretName, S3: &v1alpha1.S3Target{
		Endpoint: "https://s3.example.com", Bucket: "cboms", Region: "eu-central-1", CA: &v1alpha1.SecretKeyRef{Name: CBOMSecretName, Key: v1alpha1.CAKey},
	}}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("cbom spec %+v, want %+v", spec, want)
	}
	stored := systemSecret(t, CBOMSecretName)
	if string(stored.Data[v1alpha1.CredentialsAccessKey]) != cbomAccessKey || string(stored.Data[v1alpha1.CredentialsSecretKey]) != cbomSecretKey || string(stored.Data[v1alpha1.CAKey]) != ca {
		t.Errorf("stored keys %v", slices.Sorted(maps.Keys(stored.Data)))
	}
	got := getCBOM(t, h, s)
	if got.S3 == nil || got.S3.Endpoint != "https://s3.example.com" || !isTrue(got.S3.AccessKeySet) || !isTrue(got.S3.SecretKeySet) || !isTrue(got.S3.CaSet) || got.S3.AccessKey != nil {
		t.Errorf("cbom %+v, want the bucket and which secrets are set", got.S3)
	}

	// Left out, the secrets are kept; the CA can be removed.
	update := externalCBOM(nil, nil, nil)
	update.S3.RemoveCa = ptr(true)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, cbomUpdate(update), s), http.StatusAccepted)
	if spec := getAppliance(t).Spec.Platform.CBOM; spec.S3.CA != nil {
		t.Errorf("CA reference %+v kept after its removal", spec.S3.CA)
	}
	if stored := systemSecret(t, CBOMSecretName); string(stored.Data[v1alpha1.CredentialsSecretKey]) != cbomSecretKey || len(stored.Data[v1alpha1.CAKey]) != 0 {
		t.Errorf("stored keys %v, want the keys kept and the CA removed", slices.Sorted(maps.Keys(stored.Data)))
	}

	// The keys do not follow the store to another endpoint.
	moved := externalCBOM(nil, nil, nil)
	moved.S3.Endpoint = "https://s3.other.example.com"
	rec := h.request(http.MethodPut, pathPlatform, cbomUpdate(moved), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "cbom.s3.accessKey", "endpoint")

	// Turned off, the bucket stays chosen; an update without cbom keeps it.
	off := externalCBOM(nil, nil, nil)
	off.Enabled = false
	wantStatus(t, h.request(http.MethodPut, pathPlatform, cbomUpdate(off), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}}, s), http.StatusAccepted)
	if spec := getAppliance(t).Spec.Platform.CBOM; spec.Enabled || spec.S3 == nil {
		t.Errorf("cbom spec %+v, want off with the bucket kept", spec)
	}

	wantStatus(t, h.request(http.MethodPut, pathPlatform, cbomUpdate(CBOMRepository{Enabled: true, Storage: CBOMStorageAppliance}), s), http.StatusAccepted)
	if !secretGone(t, CBOMSecretName) {
		t.Error("the external store's credentials are kept for the store on the appliance")
	}
}

func TestCBOMRepositoryRefusals(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	noPath := externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), nil)
	noPath.S3.Endpoint = "https://s3.example.com/path"
	noBucket := externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), nil)
	noBucket.S3.Bucket = " "
	both := externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), ptr(string(newTestCA(t).pem)))
	both.S3.RemoveCa = ptr(true)
	long := externalCBOM(ptr(string(bytes.Repeat([]byte("k"), maxSecretLength+1))), ptr(cbomSecretKey), nil)
	for field, c := range map[string]CBOMRepository{
		"cbom.storage":      {Enabled: true, Storage: "nfs"},
		"cbom.s3.endpoint":  {Enabled: true, Storage: CBOMStorageS3},
		"cbom.s3.bucket":    noBucket,
		"cbom.s3.ca":        externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), ptr("not a certificate")),
		"cbom.s3.accessKey": externalCBOM(nil, ptr(cbomSecretKey), nil),
		"cbom.s3.secretKey": externalCBOM(ptr(cbomAccessKey), nil, nil),
	} {
		rec := h.request(http.MethodPut, pathPlatform, cbomUpdate(c), s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), field, "")
	}
	for name, c := range map[string]CBOMRepository{"path": noPath, "CA and removal": both, "long key": long} {
		if rec := h.request(http.MethodPut, pathPlatform, cbomUpdate(c), s); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if getAppliance(t).Spec.Platform.CBOM != nil || !secretGone(t, CBOMSecretName) {
		t.Error("a refused request changed the CBOM repository")
	}
}

func isTrue(b *bool) bool { return b != nil && *b }
