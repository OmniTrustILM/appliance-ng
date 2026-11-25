package api

import (
	"context"
	"net/url"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
)

const solverILM = "ilm"

// ilmOptionsTimeout bounds the ILM calls of the ILM options, so that a slow ILM cannot hold up the console.
var ilmOptionsTimeout = 15 * time.Second

var (
	errBindingIncomplete = &refusal{code: codeInvalid, message: "The external account binding needs both its key ID and its HMAC key.", field: "eabHmac"}
	errILMNotReady       = ConflictJSONResponse{Code: "ilm_not_ready", Message: "ILM is not ready yet; try again shortly."}
)

// GetCertificateAcmeIlmOptions lists the enabled RA profiles and vault profiles of ILM that its ACME profile can
// use, once the platform is ready and ILM's settings are applied.
func (s *Server) GetCertificateAcmeIlmOptions(ctx context.Context, _ GetCertificateAcmeIlmOptionsRequestObject) (GetCertificateAcmeIlmOptionsResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetCertificateAcmeIlmOptions409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	notReady := GetCertificateAcmeIlmOptions409JSONResponse{ConflictJSONResponse: errILMNotReady}
	if s.deps.ILM == nil || !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionPlatformReady) ||
		!meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionILMSettingsApplied) {
		return notReady, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ilmOptionsTimeout)
	defer cancel()
	options, err := s.ilmOptions(ctx, a)
	if err != nil {
		s.deps.Log.Info("could not list the ILM profiles for the ACME certificate", "reason", ilmclient.Reason(err))
		return notReady, nil
	}
	return GetCertificateAcmeIlmOptions200JSONResponse(*options), nil
}

// ilmOptions reads the enabled RA profiles and vault profiles of ILM.
func (s *Server) ilmOptions(ctx context.Context, a *v1alpha1.Appliance) (*AcmeIlmOptions, error) {
	c, err := s.deps.ILM(ctx, a)
	if err != nil {
		return nil, err
	}
	raProfiles, err := c.ListRAProfiles(ctx)
	if err != nil {
		return nil, err
	}
	vaultProfiles, err := c.ListVaultProfiles(ctx)
	if err != nil {
		return nil, err
	}
	options := &AcmeIlmOptions{RaProfiles: []AcmeIlmRaProfile{}, VaultProfiles: []AcmeIlmVaultProfile{}}
	for _, p := range raProfiles {
		if p.Enabled {
			options.RaProfiles = append(options.RaProfiles, AcmeIlmRaProfile{Uuid: p.UUID, Name: p.Name})
		}
	}
	for _, p := range vaultProfiles {
		options.VaultProfiles = append(options.VaultProfiles, AcmeIlmVaultProfile{Uuid: p.UUID, Name: p.Name, VaultUuid: p.VaultUUID})
	}
	return options, nil
}

// PutCertificateAcme selects the ACME certificate: the settings go into the Appliance, the external account
// binding's HMAC key and the DNS credentials into their Secrets.
func (s *Server) PutCertificateAcme(ctx context.Context, req PutCertificateAcmeRequestObject) (PutCertificateAcmeResponseObject, error) {
	if req.Body == nil {
		return PutCertificateAcme400JSONResponse{BadRequestJSONResponse: badRequestField("body", "Send the ACME settings.")}, nil
	}
	_, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutCertificateAcme409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	in, ref := newACMEInput(req.Body)
	if ref != nil {
		return PutCertificateAcme400JSONResponse{BadRequestJSONResponse: ref.badRequest()}, nil
	}
	if err := s.storeACME(ctx, in); err != nil {
		return nil, err
	}
	details := map[string]string{"solver": in.solverName()}
	if in.dns != nil {
		details["provider"] = in.dns.provider
	}
	recordDetails(ctx, details)
	s.deps.Log.Info("ACME certificate selected", "solver", details["solver"], "provider", details["provider"])
	return PutCertificateAcme202Response{}, nil
}

// acmeInput is a validated request for the ACME certificate.
type acmeInput struct {
	spec *v1alpha1.ACMESpec
	// hmac is the binding's HMAC key; empty without a custom directory or a binding.
	hmac string
	dns  *dnsInput
}

type dnsInput struct {
	provider    string
	credentials map[string]string
}

// solverName is the solver as the audit entry names it.
func (in *acmeInput) solverName() string {
	if in.spec.ILMProfile != nil {
		return solverILM
	}
	return in.spec.Solver.Type
}

// newACMEInput validates body and turns it into the Appliance's ACME settings, or refuses it. The refusals name
// the setting and never quote a secret.
func newACMEInput(body *AcmeSettings) (*acmeInput, *refusal) {
	hasDirectory, hasILM := body.DirectoryUrl != nil && *body.DirectoryUrl != "", body.Ilm != nil
	spec := &v1alpha1.ACMESpec{Email: deref(body.Email), Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01}}
	in := &acmeInput{spec: spec}
	switch {
	case hasDirectory && hasILM:
		return nil, invalid("ilm", "Give either a directory URL or the ILM profile, not both.")
	case hasILM:
		if ref := in.setILM(body.Ilm); ref != nil {
			return nil, ref
		}
		return in, nil
	case !hasDirectory:
		return nil, invalid("directoryUrl", "Give a directory URL or the ILM profile.")
	}
	if u, err := url.Parse(*body.DirectoryUrl); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, invalid("directoryUrl", "The ACME directory must be an https URL.")
	}
	spec.DirectoryURL = *body.DirectoryUrl
	if ref := in.setBinding(body); ref != nil {
		return nil, ref
	}
	if ref := in.setCABundle(deref(body.CaBundle)); ref != nil {
		return nil, ref
	}
	if body.Solver != nil {
		if ref := in.setSolver(body.Solver); ref != nil {
			return nil, ref
		}
	}
	return in, nil
}

func (in *acmeInput) setILM(ilm *AcmeIlm) *refusal {
	if ilm.RaProfileUuid == "" || ilm.VaultUuid == "" || ilm.VaultProfileUuid == "" {
		return invalid("ilm", "The ILM profile needs the RA profile, the vault and the vault profile.")
	}
	in.spec.ILMProfile = &v1alpha1.ILMACMEProfile{RAProfileUUID: ilm.RaProfileUuid, VaultUUID: ilm.VaultUuid,
		VaultProfileUUID: ilm.VaultProfileUuid, ProfileName: certs.ILMACMEProfileName}
	return nil
}

// setBinding takes the external account binding of body, which is both its key ID and its HMAC key or neither.
func (in *acmeInput) setBinding(body *AcmeSettings) *refusal {
	kid, hmac := deref(body.EabKeyId), deref(body.EabHmac)
	if (kid == "") != (hmac == "") {
		return errBindingIncomplete
	}
	if kid != "" {
		in.spec.EABKeyID = kid
		in.spec.EABSecretRef = &corev1.LocalObjectReference{Name: certs.ACMEEABSecretName}
		in.hmac = hmac
	}
	return nil
}

// setCABundle takes the PEM bundle that verifies the ACME server, if one is given.
func (in *acmeInput) setCABundle(bundle string) *refusal {
	if bundle == "" {
		return nil
	}
	if got, err := certs.ParseBundle([]byte(bundle)); err != nil || len(got) == 0 {
		return invalid("caBundle", "The CA bundle holds no certificate.")
	}
	in.spec.CABundle = []byte(bundle)
	return nil
}

func (in *acmeInput) setSolver(solver *AcmeSolverSettings) *refusal {
	switch solver.Type {
	case AcmeSolverSettingsTypeHttp01:
		return nil
	case AcmeSolverSettingsTypeDns01:
	default:
		return invalid("solver", "The solver must be http01 or dns01.")
	}
	if solver.Dns01 == nil || solver.Dns01.Provider == "" {
		return invalid("solver", "Choose a DNS provider.")
	}
	config, credentials := derefMap(solver.Dns01.Config), derefMap(solver.Dns01.Credentials)
	if err := certs.ValidateDNS01(solver.Dns01.Provider, config, credentials); err != nil {
		return invalid("solver", sentence(err.Error()))
	}
	in.spec.Solver = v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverDNS01, DNS01: &v1alpha1.DNS01Solver{
		Provider: solver.Dns01.Provider, Config: config,
	}}
	if len(credentials) > 0 {
		in.spec.Solver.DNS01.CredentialsSecretRef = &corev1.LocalObjectReference{Name: certs.ACMEDNS01SecretName}
	}
	in.dns = &dnsInput{provider: solver.Dns01.Provider, credentials: credentials}
	return nil
}

// storeACME writes the Secrets the settings reference, selects the settings and deletes the Secrets they no longer
// reference. The ILM profile's binding has its own Secret, which the manager keeps.
func (s *Server) storeACME(ctx context.Context, in *acmeInput) error {
	if in.dns != nil && len(in.dns.credentials) > 0 {
		if err := s.writeSystemSecret(ctx, certs.ACMEDNS01SecretName, corev1.SecretTypeOpaque, secretData(in.dns.credentials)); err != nil {
			return err
		}
	}
	if in.hmac != "" {
		data := map[string][]byte{certs.ACMEEABHMACKey: []byte(in.hmac)}
		if err := s.writeSystemSecret(ctx, certs.ACMEEABSecretName, corev1.SecretTypeOpaque, data); err != nil {
			return err
		}
	}
	if err := s.selectTLS(ctx, v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: in.spec}); err != nil {
		return err
	}
	var stale []string
	if in.dns == nil || len(in.dns.credentials) == 0 {
		stale = append(stale, certs.ACMEDNS01SecretName)
	}
	if in.hmac == "" {
		stale = append(stale, certs.ACMEEABSecretName)
	}
	for _, name := range stale {
		if err := s.deleteSystemSecret(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// acmeInfo describes the ACME settings of a, with the names of the credentials that are stored and the reason the
// certificate is not issued yet.
func (s *Server) acmeInfo(ctx context.Context, a *v1alpha1.Appliance) (*AcmeInfo, error) {
	spec := a.Spec.TLS.ACME
	info := &AcmeInfo{Solver: AcmeSolverInfo{Type: AcmeSolverInfoType(spec.Solver.Type)}}
	if info.Solver.Type == "" {
		info.Solver.Type = AcmeSolverInfoTypeHttp01
	}
	if spec.DirectoryURL != "" {
		info.DirectoryUrl = &spec.DirectoryURL
	}
	if spec.Email != "" {
		info.Email = &spec.Email
	}
	if spec.EABKeyID != "" {
		hmac, err := s.secretKeys(ctx, certs.ACMEEABSecretName)
		if err != nil {
			return nil, err
		}
		set := slices.Contains(hmac, certs.ACMEEABHMACKey)
		info.EabKeyId, info.EabHmacSet = &spec.EABKeyID, &set
	}
	if len(spec.CABundle) > 0 {
		bundle := string(spec.CABundle)
		info.CaBundle = &bundle
	}
	if p := spec.ILMProfile; p != nil {
		info.Ilm = &AcmeIlm{RaProfileUuid: p.RAProfileUUID, VaultUuid: p.VaultUUID, VaultProfileUuid: p.VaultProfileUUID}
	}
	if d := spec.Solver.DNS01; d != nil && spec.ILMProfile == nil {
		keys, err := s.secretKeys(ctx, certs.ACMEDNS01SecretName)
		if err != nil {
			return nil, err
		}
		info.Solver.Dns01 = &AcmeDns01Info{Provider: d.Provider, Config: orEmptyMap(d.Config), CredentialKeys: keys}
	}
	if c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionCertificateReady); c != nil && c.Status == "False" {
		info.LastError = &c.Message
	}
	return info, nil
}

// secretKeys lists the keys of the Secret name in the system namespace, sorted; none when it does not exist.
func (s *Server) secretKeys(ctx context.Context, name string) ([]string, error) {
	secret, err := s.systemSecret(ctx, name)
	if err != nil || secret == nil {
		return []string{}, err
	}
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

func invalid(field, message string) *refusal {
	return &refusal{code: codeInvalid, message: message, field: field}
}

func derefMap(m *map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return *m
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func secretData(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}
