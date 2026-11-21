package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	accessPath      = auth.APIBase + "/access"
	kubeconfigPath  = accessPath + "/kubeconfig"
	talosconfigPath = accessPath + "/talosconfig"

	// apiServerPort is the port of the cluster's API server on the node address.
	apiServerPort = "6443"
	// neverExpires is the expiry a long-lived kubeconfig has in the audit log.
	neverExpires = "never"

	talosconfigFileName = `attachment; filename="talosconfig.yaml"`
)

// GetAccess lists the issued kubeconfigs and the talosconfigs that have not expired.
func (s *Server) GetAccess(ctx context.Context, _ GetAccessRequestObject) (GetAccessResponseObject, error) {
	creds, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]AccessCredential, len(creds))
	for i, c := range creds {
		items[i] = AccessCredential{Kind: AccessCredentialKind(c.Kind), Role: c.Role, IssuedBy: c.IssuedBy, IssuedAt: c.IssuedAt, Expires: c.Expires}
		if c.Name != "" {
			items[i].Name = &c.Name
		}
		if c.BreakGlass {
			items[i].BreakGlass = &c.BreakGlass
		}
	}
	return GetAccess200JSONResponse{Items: items}, nil
}

// PostAccessKubeconfig issues a kubeconfig. Nothing is handed out without its audit entry.
func (s *Server) PostAccessKubeconfig(ctx context.Context, req PostAccessKubeconfigRequestObject) (PostAccessKubeconfigResponseObject, error) {
	if req.Body == nil {
		return PostAccessKubeconfig400JSONResponse{BadRequestJSONResponse: badRequestField(access.FieldRole, "Choose a role.")}, nil
	}
	body := *req.Body
	var ttl time.Duration
	if body.Hours != nil {
		if *body.Hours < 1 || *body.Hours > int(access.MaxKubeconfigTTL/time.Hour) {
			return PostAccessKubeconfig400JSONResponse{BadRequestJSONResponse: badRequestField(access.FieldHours, "Choose 1 to 24 hours.")}, nil
		}
		ttl = time.Duration(*body.Hours) * time.Hour
	}
	address, conflict, err := s.nodeAddress(ctx)
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return PostAccessKubeconfig409JSONResponse{ConflictJSONResponse: *conflict}, nil
	}
	server := "https://" + net.JoinHostPort(address, apiServerPort)
	data, name, err := access.IssueKubeconfig(ctx, s.deps.Client, s.deps.Kube, server, s.deps.ClusterCA, access.KubeRequest{
		Role: string(body.Role), TTL: ttl, Name: deref(body.Name), IssuedBy: requestFrom(ctx).user,
	})
	var invalid *access.InvalidError
	switch {
	case errors.As(err, &invalid):
		return PostAccessKubeconfig400JSONResponse{BadRequestJSONResponse: badRequestField(invalid.Field, invalid.Message)}, nil
	case errors.Is(err, access.ErrExists):
		return PostAccessKubeconfig409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "exists", Message: "A kubeconfig with this name exists; revoke it first or choose another name.",
		}}, nil
	case err != nil:
		return nil, err
	}
	if err := s.auditCredential(ctx, name); err != nil {
		return nil, errors.Join(err, access.Revoke(ctx, s.deps.Client, name))
	}
	disposition := `attachment; filename="kubeconfig-` + name + `.yaml"`
	return PostAccessKubeconfig200ApplicationyamlResponse{
		Body:          bytes.NewReader(data),
		ContentLength: int64(len(data)),
		Headers:       PostAccessKubeconfig200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

// auditCredential describes the kubeconfig name in the request's audit entry and writes it.
func (s *Server) auditCredential(ctx context.Context, name string) error {
	c, err := s.kubeconfig(ctx, name)
	if err != nil {
		return err
	}
	recordDetails(ctx, details(c))
	if err := s.writeAudit(ctx, Success); err != nil {
		return fmt.Errorf("record the issued kubeconfig in the audit log: %w", err)
	}
	s.deps.Log.Info("kubeconfig issued", "name", c.Name, "role", c.Role, "expires", details(c)["expires"])
	return nil
}

// DeleteAccessKubeconfig revokes a kubeconfig other than the recovery kit's break-glass one.
func (s *Server) DeleteAccessKubeconfig(ctx context.Context, req DeleteAccessKubeconfigRequestObject) (DeleteAccessKubeconfigResponseObject, error) {
	if req.Name == access.BreakGlass {
		return DeleteAccessKubeconfig409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "break_glass", Message: "The break-glass kubeconfig is revoked by issuing the recovery kit again.",
		}}, nil
	}
	c, err := s.kubeconfig(ctx, req.Name)
	if errors.Is(err, access.ErrNotFound) {
		return DeleteAccessKubeconfig404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{
			Code: "not_found", Message: "There is no such kubeconfig; it may have expired.",
		}}, nil
	}
	if err != nil {
		return nil, err
	}
	recordDetails(ctx, details(c))
	if err := access.Revoke(ctx, s.deps.Client, req.Name); err != nil && !errors.Is(err, access.ErrNotFound) {
		return nil, err
	}
	s.deps.Log.Info("kubeconfig revoked", "name", c.Name, "role", c.Role)
	return DeleteAccessKubeconfig204Response{}, nil
}

// credentials lists the issued credentials. Talosconfig records that do not parse are left out and logged, so that
// the kubeconfigs can still be listed and revoked.
func (s *Server) credentials(ctx context.Context) ([]access.Credential, error) {
	creds, err := access.List(ctx, s.deps.Reader)
	if errors.Is(err, access.ErrCorruptRecords) {
		s.deps.Log.Error("the talosconfig records are left out", "error", err)
		return creds, nil
	}
	return creds, err
}

// kubeconfig returns the issued kubeconfig name, or access.ErrNotFound.
func (s *Server) kubeconfig(ctx context.Context, name string) (access.Credential, error) {
	creds, err := s.credentials(ctx)
	if err != nil {
		return access.Credential{}, err
	}
	for _, c := range creds {
		if c.Kind == access.KindKubeconfig && c.Name == name {
			return c, nil
		}
	}
	return access.Credential{}, access.ErrNotFound
}

// details is what the audit log records of a credential.
func details(c access.Credential) map[string]string {
	d := map[string]string{"role": c.Role, "expires": neverExpires}
	if c.Name != "" {
		d["name"] = c.Name
	}
	if c.Expires != nil {
		d["expires"] = c.Expires.UTC().Format(time.RFC3339)
	}
	return d
}

// PostAccessTalosconfig issues a talosconfig and records it. Nothing is handed out without its audit entry.
func (s *Server) PostAccessTalosconfig(ctx context.Context, req PostAccessTalosconfigRequestObject) (PostAccessTalosconfigResponseObject, error) {
	if req.Body == nil {
		return PostAccessTalosconfig400JSONResponse{BadRequestJSONResponse: badRequestField(access.FieldRole, "Choose a role.")}, nil
	}
	if req.Body.Hours < 1 || req.Body.Hours > int(access.MaxTalosconfigTTL/time.Hour) {
		return PostAccessTalosconfig400JSONResponse{BadRequestJSONResponse: badRequestField(access.FieldHours, "Choose 1 to 8 hours.")}, nil
	}
	role, ttl := string(req.Body.Role), time.Duration(req.Body.Hours)*time.Hour
	var invalid *access.InvalidError
	if err := access.ValidateTalosconfig(role, ttl); errors.As(err, &invalid) {
		return PostAccessTalosconfig400JSONResponse{BadRequestJSONResponse: badRequestField(invalid.Field, invalid.Message)}, nil
	}
	address, conflict, err := s.nodeAddress(ctx)
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return PostAccessTalosconfig409JSONResponse{ConflictJSONResponse: *conflict}, nil
	}
	unavailable := PostAccessTalosconfig503JSONResponse{ServiceUnavailableJSONResponse: ServiceUnavailableJSONResponse{Code: codeUnavailable, Message: messageUnavailable}}
	if s.deps.Talos == nil {
		return unavailable, nil
	}
	node, err := s.deps.Talos(ctx)
	if err != nil {
		s.deps.Log.Warn("no access to the Talos API", "action", "talosconfig", "error", err)
		return unavailable, nil
	}
	data, err := access.Talosconfig(ctx, node, address, role, ttl)
	if err != nil {
		// The error names no secret: a talosconfig that does not parse is reported without its content.
		s.deps.Log.Error("the node did not generate a talosconfig", "error", err)
		return unavailable, nil
	}
	user := requestFrom(ctx).user
	// The records are pruned on the cluster's clock, like the kubeconfigs expire.
	notAfter := time.Now().Add(ttl).UTC().Truncate(time.Second)
	if err := access.RecordTalosconfig(ctx, s.deps.Client, role, notAfter, user); err != nil {
		return nil, err
	}
	recordDetails(ctx, details(access.Credential{Role: role, Expires: &notAfter}))
	if err := s.writeAudit(ctx, Success); err != nil {
		return nil, fmt.Errorf("record the issued talosconfig in the audit log: %w", err)
	}
	s.deps.Log.Info("talosconfig issued", "role", role, "hours", req.Body.Hours)
	disposition := talosconfigFileName
	return PostAccessTalosconfig200ApplicationyamlResponse{
		Body:          bytes.NewReader(data),
		ContentLength: int64(len(data)),
		Headers:       PostAccessTalosconfig200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

// nodeAddress returns the node address, or the conflict to answer while the Appliance or its address is missing.
func (s *Server) nodeAddress(ctx context.Context) (string, *ConflictJSONResponse, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		conflict := notSetUp()
		return "", &conflict, nil
	}
	if err != nil {
		return "", nil, err
	}
	addr, ok, err := certs.NodeAddress(ctx, s.deps.Reader, a)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", &ConflictJSONResponse{Code: "no_node_address", Message: "The node address is not known yet; try again shortly."}, nil
	}
	return addr.String(), nil, nil
}
