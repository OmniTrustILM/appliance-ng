package manager

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// ILMProviderName is the OAuth2 provider the manager writes into ILM for an external identity provider.
const ILMProviderName = "appliance"

// edgePort is the port the ILM edge serves on the node.
const edgePort = "443"

// defaultScopes are requested from an external identity provider when the spec names none.
var defaultScopes = []string{"openid", "profile", "email"}

// Reason and message of the ILMSettingsApplied condition besides reasonApplied and reasonRestoring. A failure's
// reason is the fixed reason of the ILM client; its message is fixed, the error goes to the manager's log only.
const (
	reasonWaitingForPlatform = "WaitingForPlatform"
	reasonCredentialsPending = "CredentialsPending"
	messageILMFailed         = "applying the ILM settings failed, retrying; the manager log has the details"
)

// ILMIdentity returns a client of the ILM edge authenticated as the manager.
type ILMIdentity interface {
	Ensure(ctx context.Context, edge, ilmHost string, roots *x509.CertPool) (*ilmclient.Client, error)
}

// ILMReconciler keeps ILM's settings that follow the Appliance once the platform is ready: the OAuth2 provider of an
// external identity provider, written after checking the issuer's discovery document and removed with the managed
// Keycloak, and the URL of the CBOM repository.
type ILMReconciler struct {
	Client client.Client
	// APIReader reads past the cache: the restore's progress and the edge certificate in the platform namespace.
	APIReader client.Reader
	ILM       ILMIdentity
}

// SetupWithManager watches the Appliance's spec, platform readiness and restores, and the Secret of the
// external identity provider's credentials.
func (r *ILMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("ilm").
		For(&v1alpha1.Appliance{}, builder.WithPredicates(ilmRelevant)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.identityCredentials), builder.WithPredicates(inSystemNamespace)).
		Complete(r)
}

// ilmRelevant lets through the Appliance changes the ILM settings depend on, not the status updates of others.
var ilmRelevant = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	before, ok := e.ObjectOld.(*v1alpha1.Appliance)
	after, ok2 := e.ObjectNew.(*v1alpha1.Appliance)
	if !ok || !ok2 {
		return true
	}
	return before.Generation != after.Generation ||
		platformIsReady(before) != platformIsReady(after) ||
		before.Status.Restore.Active() != after.Status.Restore.Active()
}}

func platformIsReady(a *v1alpha1.Appliance) bool {
	return meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionPlatformReady)
}

// identityCredentials maps the Secret of the external identity provider's credentials to the Appliance.
func (r *ILMReconciler) identityCredentials(ctx context.Context, o client.Object) []reconcile.Request {
	a := &v1alpha1.Appliance{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil
	}
	if p := a.Spec.Platform; p == nil || p.Identity == nil || p.Identity.CredentialsRef != o.GetName() {
		return nil
	}
	return applianceRequest(ctx, o)
}

// Reconcile applies the ILM settings and records the outcome in ILMSettingsApplied. A failure is returned, so the
// request is retried with back-off while ILM is down.
func (r *ILMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	a := &v1alpha1.Appliance{}
	if err := r.Client.Get(ctx, req.NamespacedName, a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if a.Spec.Platform == nil {
		return ctrl.Result{}, nil
	}
	before := a.Status.DeepCopy()
	result, err := r.reconcile(ctx, a)
	if equality.Semantic.DeepEqual(before, &a.Status) {
		return result, err
	}
	if serr := r.Client.Status().Update(ctx, a, client.FieldOwner(fieldManager)); serr != nil {
		return ctrl.Result{}, errors.Join(err, fmt.Errorf("update the Appliance status: %w", serr))
	}
	return result, err
}

func (r *ILMReconciler) reconcile(ctx context.Context, a *v1alpha1.Appliance) (ctrl.Result, error) {
	// The cached Appliance may not show yet the restore. A restore replaces the manager's identity and ILM's database
	// in separate steps, so nothing is written to ILM until it is over.
	live := &v1alpha1.Appliance{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, live); err != nil {
		return ctrl.Result{}, fmt.Errorf("read the restore status: %w", err)
	}
	if live.Status.Restore.Active() {
		setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionFalse, reasonRestoring, "a restore is running")
		return ctrl.Result{RequeueAfter: addonsRequeue}, nil
	}
	if !platformIsReady(a) {
		setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionFalse, reasonWaitingForPlatform, "waiting for the platform to be ready")
		return ctrl.Result{}, nil
	}
	err := r.apply(ctx, a)
	if errors.Is(err, errCredentialsPending) {
		setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionFalse, reasonCredentialsPending,
			"waiting for the client secret of the identity provider's issuer and client ID")
		return ctrl.Result{RequeueAfter: addonsRequeue}, nil
	}
	if err != nil {
		reason := ilmclient.Reason(err)
		log.FromContext(ctx).Error(err, "could not apply the ILM settings", "reason", reason)
		setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionFalse, ilmConditionReason(reason), messageILMFailed)
		return ctrl.Result{}, err
	}
	setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionTrue, reasonApplied, "the ILM settings are applied")
	return ctrl.Result{RequeueAfter: resync}, nil
}

// ilmConditionReason turns a fixed reason of the ILM client into a condition reason: failed becomes Failed.
func ilmConditionReason(reason string) string {
	if reason == "" {
		reason = ilmclient.ReasonFailed
	}
	return strings.ToUpper(reason[:1]) + reason[1:]
}

// errCredentialsPending is returned while the identity credentials Secret holds a client secret given for another
// issuer or client ID than the spec's, as between the writes of a change of both.
var errCredentialsPending = errors.New("the identity credentials are for another issuer or client ID")

// cbomRepositoryURLSetting is ILM's platform setting of the CBOM repository's base URL.
const cbomRepositoryURLSetting = "utils.cbomRepositoryUrl"

// apply writes the OAuth2 provider of the external identity provider, or removes the one the manager wrote, and sets
// or clears the CBOM repository's URL.
func (r *ILMReconciler) apply(ctx context.Context, a *v1alpha1.Appliance) error {
	c, err := ilmClientOf(ctx, r.Client, r.APIReader, r.ILM, a)
	if err != nil {
		return err
	}
	host := ilmHostName(a)
	if err := r.applyIdentity(ctx, a, c, host); err != nil {
		return err
	}
	// Core calls the repository itself and does not trust the edge's certificate for it, so ILM gets the in-cluster
	// URL. Turned off, only that URL is cleared: one an administrator set in ILM stays.
	if p := a.Spec.Platform; p.CBOM != nil && p.CBOM.Enabled {
		return c.SetPlatformSetting(ctx, cbomRepositoryURLSetting, cbom.URL())
	}
	return c.ClearPlatformSetting(ctx, cbomRepositoryURLSetting, cbom.URL())
}

// applyIdentity writes the OAuth2 provider of the external identity provider, or removes the one the manager wrote.
func (r *ILMReconciler) applyIdentity(ctx context.Context, a *v1alpha1.Appliance, c *ilmclient.Client, host string) error {
	id := a.Spec.Platform.Identity
	if id == nil {
		current, err := c.GetOAuth2Provider(ctx, ILMProviderName)
		if err != nil || current == nil {
			return err
		}
		if err := c.DeleteOAuth2Provider(ctx, ILMProviderName); err != nil {
			return err
		}
		log.FromContext(ctx).Info("removed the external identity provider from ILM", "provider", ILMProviderName)
		return nil
	}
	p, err := r.provider(ctx, a, id, host)
	if err != nil {
		return err
	}
	return c.PutOAuth2Provider(ctx, ILMProviderName, p)
}

// provider builds the OAuth2 provider of id from the issuer's discovery document, fetched first.
func (r *ILMReconciler) provider(ctx context.Context, a *v1alpha1.Appliance, id *v1alpha1.ExternalIdentity, host string) (ilmclient.OAuth2Provider, error) {
	hc, err := r.identityHTTPClient(ctx, a)
	if err != nil {
		return ilmclient.OAuth2Provider{}, err
	}
	d, err := ilmclient.Discover(ctx, hc, id.Issuer)
	if err != nil {
		return ilmclient.OAuth2Provider{}, err
	}
	credentials, err := systemSecret(ctx, r.Client, id.CredentialsRef)
	if err != nil {
		return ilmclient.OAuth2Provider{}, err
	}
	secret, err := secretValue(credentials, v1alpha1.CredentialsClientSecretKey)
	if err != nil {
		return ilmclient.OAuth2Provider{}, err
	}
	if string(credentials.Data[v1alpha1.CredentialsIssuerKey]) != id.Issuer || string(credentials.Data[v1alpha1.CredentialsClientIDKey]) != id.ClientID {
		return ilmclient.OAuth2Provider{}, errCredentialsPending
	}
	scopes := id.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes
	}
	return ilmclient.OAuth2Provider{
		IssuerURL:        id.Issuer,
		ClientID:         id.ClientID,
		ClientSecret:     string(secret),
		AuthorizationURL: d.AuthorizationEndpoint,
		TokenURL:         d.TokenEndpoint,
		JWKSetURL:        d.JWKSURI,
		LogoutURL:        d.EndSessionEndpoint,
		UserInfoURL:      d.UserInfoEndpoint,
		PostLogoutURL:    (&url.URL{Scheme: "https", Host: host, Path: "/administrator/"}).String(),
		Scope:            scopes,
		Audiences:        id.Audiences,
	}, nil
}

// identityHTTPClient reaches the identity provider through the appliance's proxy, trusting the system roots and the
// appliance's additional trusted CAs.
func (r *ILMReconciler) identityHTTPClient(ctx context.Context, a *v1alpha1.Appliance) (*http.Client, error) {
	bundle, err := trustBundle(ctx, r.Client, a)
	if err != nil {
		return nil, err
	}
	roots, err := certs.TrustedRoots(bundle)
	if err != nil {
		log.FromContext(ctx).Info("system roots unavailable; only the additional trusted CAs are trusted", "error", err)
	}
	return ilmclient.ProviderClient(roots, a.Spec.Proxy, identityTimeout), nil
}

// identityTimeout bounds the fetch of a discovery document.
const identityTimeout = 30 * time.Second

// ILMClient returns ilmClientOf with c, reader and ilm, for the API.
func ILMClient(c client.Client, reader client.Reader, ilm ILMIdentity) func(context.Context, *v1alpha1.Appliance) (*ilmclient.Client, error) {
	return func(ctx context.Context, a *v1alpha1.Appliance) (*ilmclient.Client, error) {
		return ilmClientOf(ctx, c, reader, ilm, a)
	}
}

// errRestoring refuses an ILM client while a restore runs, which replaces the manager's identity and ILM's database.
var errRestoring = errors.New("a restore is running")

// ilmClientOf returns a client of the ILM edge on the node, authenticated as the manager by ilm. It reads the restore
// status past the cache and refuses while a restore runs, so nothing registers in or writes to the ILM being replaced.
func ilmClientOf(ctx context.Context, c client.Client, reader client.Reader, ilm ILMIdentity, a *v1alpha1.Appliance) (*ilmclient.Client, error) {
	live := &v1alpha1.Appliance{}
	if err := reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, live); err != nil {
		return nil, fmt.Errorf("read the restore status: %w", err)
	}
	if live.Status.Restore.Active() {
		return nil, errRestoring
	}
	ip, ok, err := certs.NodeAddress(ctx, c, a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the node address is not known yet")
	}
	roots, err := edgeRoots(ctx, reader)
	if err != nil {
		return nil, err
	}
	return ilm.Ensure(ctx, net.JoinHostPort(ip.String(), edgePort), ilmHostName(a), roots)
}

// edgeRoots trusts the certificates the edge serves, from the TLS Secret's copy in the platform namespace: the
// built-in chain or an uploaded certificate alike.
func edgeRoots(ctx context.Context, reader client.Reader) (*x509.CertPool, error) {
	s := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: TLSSecretName}, s); err != nil {
		return nil, fmt.Errorf(errReadSecret, platform.Namespace, TLSSecretName, err)
	}
	roots := x509.NewCertPool()
	found := false
	for rest := s.Data[corev1.TLSCertKey]; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if block.Type != "CERTIFICATE" || err != nil {
			continue
		}
		roots.AddCert(cert)
		found = true
	}
	if !found {
		return nil, fmt.Errorf("secret %s/%s holds no certificate", platform.Namespace, TLSSecretName)
	}
	return roots, nil
}

// ilmHostName is the ILM host name: the platform's own, else the node's.
func ilmHostName(a *v1alpha1.Appliance) string {
	if p := a.Spec.Platform; p != nil && p.HostName != "" {
		return p.HostName
	}
	return a.Spec.Node.HostName
}
