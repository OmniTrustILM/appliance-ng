package api

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// GetAppliance returns the Appliance's configuration. Secrets are reported only as set or not set.
func (s *Server) GetAppliance(ctx context.Context, _ GetApplianceRequestObject) (GetApplianceResponseObject, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, err
	}
	spec := a.Spec
	out := ApplianceSettings{
		Dns:        orEmpty(spec.DNS.Servers),
		Ntp:        orEmpty(spec.NTP.Servers),
		TlsSource:  ApplianceSettingsTlsSourceBuiltin,
		TrustSet:   spec.Trust != nil,
		Registries: make([]Registry, len(spec.Registries)),
		Cluster:    clusterSettings(spec.Cluster),
	}
	if spec.TLS.Source != "" {
		out.TlsSource = ApplianceSettingsTlsSource(spec.TLS.Source)
	}
	if p := spec.Proxy; p != nil {
		out.Proxy = &Proxy{Http: optional(p.HTTP), Https: optional(p.HTTPS), NoProxy: optional(p.NoProxy)}
	}
	for i, r := range spec.Registries {
		out.Registries[i] = Registry{Host: r.Host, CredentialsSet: r.CredentialsRef != ""}
	}
	if out.Mail, err = s.mailSettings(ctx, spec.Mail); err != nil {
		return nil, err
	}
	if p := spec.Platform; p != nil {
		out.Platform = &struct {
			Components []string `json:"components"`
			HostName   *string  `json:"hostName,omitempty"`
			IlmVersion *string  `json:"ilmVersion,omitempty"`
		}{Components: orEmpty(p.ComponentNames()), HostName: optional(p.HostName), IlmVersion: optional(p.ILMVersion)}
	}
	return GetAppliance200JSONResponse(out), nil
}

// clusterSettings describes the address and the nodes of c.
func clusterSettings(c v1alpha1.ClusterSpec) ClusterSettings {
	out := ClusterSettings{Nodes: make([]ClusterNode, len(c.Nodes))}
	if c.Address.Virtual != "" || c.Address.External != "" {
		out.Address = &ClusterAddress{Virtual: optional(c.Address.Virtual), External: optional(c.Address.External)}
	}
	for i, n := range c.Nodes {
		out.Nodes[i] = ClusterNode{
			HostName: n.HostName, Address: optional(n.Address), Gateway: optional(n.Gateway),
			Tags: orEmpty(n.Tags), Maintenance: n.Maintenance,
		}
		if n.Role != "" {
			role := ClusterNodeRole(n.Role)
			out.Nodes[i].Role = &role
		}
	}
	return out
}

// mailSettings describes the mail server m, nil for none, with the user name of its credentials; the password only
// as set or not set.
func (s *Server) mailSettings(ctx context.Context, m *v1alpha1.MailSpec) (*MailSettings, error) {
	if m == nil {
		return nil, nil
	}
	out := &MailSettings{Host: m.Host, Port: int(m.Port), Security: MailSecurity(m.Security), From: m.From}
	if m.CredentialsRef != "" {
		username, password, err := s.mailCredentials(ctx, m.CredentialsRef)
		if err != nil {
			return nil, err
		}
		out.Username, out.PasswordSet = optional(username), password != ""
	}
	return out, nil
}

// mailCredentials reads the user name and password of the mail credentials Secret name; both are empty when it
// does not exist.
func (s *Server) mailCredentials(ctx context.Context, name string) (username, password string, err error) {
	secret := &corev1.Secret{}
	err = s.deps.Client.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("read secret %s/%s: %w", SystemNamespace, name, err)
	}
	return string(secret.Data[MailUsernameKey]), string(secret.Data[MailPasswordKey]), nil
}

// orEmpty is list, or an empty list for nil, so that the JSON has [] rather than null.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
