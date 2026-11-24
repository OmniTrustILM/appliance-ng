package api

import (
	"context"
	"crypto/x509"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// CBOMSecretName holds the access key, the secret key and the CA of the CBOM repository's external store.
const CBOMSecretName = "appliance-cbom"

// cbomInput is a choice of the CBOM repository and the secrets given for an external store.
type cbomInput struct {
	spec    v1alpha1.CBOMSpec
	secrets targetSecrets
}

// parseCBOM validates a choice of the CBOM repository; nil keeps the current one.
func parseCBOM(c *CBOMRepository) (*cbomInput, *fieldError) {
	if c == nil {
		return nil, nil
	}
	in := &cbomInput{spec: v1alpha1.CBOMSpec{Enabled: c.Enabled, Storage: string(c.Storage)}}
	switch {
	case !c.Storage.Valid():
		return nil, &fieldError{"cbom.storage", "Choose the store on the appliance or an external S3 store."}
	case c.Storage == CBOMStorageAppliance:
		return in, nil
	}
	b := c.S3
	switch {
	case b == nil:
		return nil, &fieldError{"cbom.s3.endpoint", "Enter the S3 endpoint."}
	case !validEndpoint(b.Endpoint):
		return nil, &fieldError{"cbom.s3.endpoint", "Enter the endpoint as https://host[:port], without a path."}
	case strings.TrimSpace(b.Bucket) == "":
		return nil, &fieldError{"cbom.s3.bucket", "Enter the bucket."}
	case b.Ca != nil && *b.Ca != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(*b.Ca)):
		return nil, &fieldError{"cbom.s3.ca", "The CA bundle holds no PEM certificate."}
	}
	if ferr := secretLengths("cbom.s3", map[string]*string{"accessKey": b.AccessKey, "secretKey": b.SecretKey, "ca": b.Ca}); ferr != nil {
		return nil, ferr
	}
	in.spec.S3 = &v1alpha1.S3Target{Endpoint: strings.TrimSpace(b.Endpoint), Bucket: strings.TrimSpace(b.Bucket), Region: strings.TrimSpace(deref(b.Region))}
	in.spec.CredentialsRef = CBOMSecretName
	in.secrets = secretsOf(map[string]*string{v1alpha1.CredentialsAccessKey: b.AccessKey, v1alpha1.CredentialsSecretKey: b.SecretKey, v1alpha1.CAKey: b.Ca})
	if b.RemoveCa != nil && *b.RemoveCa {
		if in.secrets.set[v1alpha1.CAKey] != "" {
			return nil, &fieldError{"cbom.s3.ca", "Upload a CA bundle or remove the stored one, not both."}
		}
		in.secrets.drop = []string{v1alpha1.CAKey}
	}
	return in, nil
}

// cbomCredentials is the credentials Secret of the CBOM repository's external store as stored and the data to write,
// nil to delete it.
type cbomCredentials struct {
	stored *corev1.Secret
	data   map[string][]byte
}

// planCBOMCredentials reads the credentials Secret and returns what writing in changes: for an external store the
// stored secrets with the given ones replaced, with in's spec referencing the CA when there is one; for the store on
// the appliance its deletion. The stored keys are kept only for the endpoint of current, the store chosen so far. It
// refuses an external store without both keys.
func (s *Server) planCBOMCredentials(ctx context.Context, in *cbomInput, current *v1alpha1.CBOMSpec) (*cbomCredentials, *fieldError, error) {
	stored, err := s.systemSecret(ctx, CBOMSecretName)
	if err != nil {
		return nil, nil, err
	}
	plan := &cbomCredentials{stored: stored}
	if in.spec.Storage != v1alpha1.CBOMStorageS3 {
		return plan, nil, nil
	}
	plan.data = map[string][]byte{}
	if stored != nil {
		maps.Copy(plan.data, stored.Data)
	}
	if endpointOf(current) != in.spec.S3.Endpoint {
		delete(plan.data, v1alpha1.CredentialsAccessKey)
		delete(plan.data, v1alpha1.CredentialsSecretKey)
	}
	for key, value := range in.secrets.set {
		plan.data[key] = []byte(value)
	}
	for _, key := range in.secrets.drop {
		delete(plan.data, key)
	}
	for _, k := range []struct{ key, field string }{{v1alpha1.CredentialsAccessKey, "accessKey"}, {v1alpha1.CredentialsSecretKey, "secretKey"}} {
		if len(plan.data[k.key]) == 0 {
			return nil, &fieldError{"cbom.s3." + k.field, "Enter the access key and the secret key of the bucket: the endpoint changed, or none are stored."}, nil
		}
	}
	in.spec.S3.CA = nil
	if len(plan.data[v1alpha1.CAKey]) > 0 {
		in.spec.S3.CA = &v1alpha1.SecretKeyRef{Name: CBOMSecretName, Key: v1alpha1.CAKey}
	}
	return plan, nil, nil
}

// endpointOf is the endpoint of the external store of c, empty for none.
func endpointOf(c *v1alpha1.CBOMSpec) string {
	if c == nil || c.Storage != v1alpha1.CBOMStorageS3 || c.S3 == nil {
		return ""
	}
	return c.S3.Endpoint
}

// cbomOf describes the CBOM repository of p, with which secrets of an external store are stored, never their values.
func (s *Server) cbomOf(ctx context.Context, p *v1alpha1.PlatformSpec) (CBOMRepository, error) {
	out := CBOMRepository{Storage: CBOMStorageAppliance}
	if p == nil || p.CBOM == nil {
		return out, nil
	}
	c := p.CBOM
	out.Enabled = c.Enabled
	if c.Storage != v1alpha1.CBOMStorageS3 || c.S3 == nil {
		return out, nil
	}
	stored, err := s.systemSecret(ctx, CBOMSecretName)
	if err != nil {
		return out, err
	}
	set := func(key string) *bool {
		ok := stored != nil && len(stored.Data[key]) > 0
		return &ok
	}
	out.Storage = CBOMStorageS3
	out.S3 = &CBOMS3{
		Endpoint: c.S3.Endpoint, Bucket: c.S3.Bucket, Region: &c.S3.Region, AccessKeySet: set(v1alpha1.CredentialsAccessKey),
		SecretKeySet: set(v1alpha1.CredentialsSecretKey), CaSet: set(v1alpha1.CAKey),
	}
	return out, nil
}
