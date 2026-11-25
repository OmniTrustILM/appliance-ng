package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const caFileName = `attachment; filename="ilm-appliance-ca.pem"`

// GetStatus returns the setup phase, the conditions, the versions, the node address, whether a reboot is required,
// the backups and, when it can be read, when the certificate in use expires. The condition messages are fixed texts
// the manager sets; they carry no error text and no secret.
func (s *Server) GetStatus(ctx context.Context, _ GetStatusRequestObject) (GetStatusResponseObject, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, err
	}
	st := a.Status
	out := Status{
		Setup:          StatusSetup(st.Setup),
		Conditions:     make([]Condition, len(st.Conditions)),
		RebootRequired: st.RebootRequired,
		NodeAddress:    optional(st.NodeAddress),
		Versions: Versions{
			Appliance:  optional(st.Versions.Appliance),
			Talos:      optional(st.Versions.Talos),
			Kubernetes: optional(st.Versions.Kubernetes),
			Operator:   optional(st.Versions.Operator),
			Ilm:        optional(st.Versions.ILM),
		},
	}
	if out.Setup == "" {
		// The manager has not reconciled the new Appliance yet; part 2 is next.
		out.Setup = StatusSetupPart2
	}
	for i, c := range st.Conditions {
		out.Conditions[i] = condition(c)
	}
	if v := st.DataVolume; v != nil {
		out.DataVolume = &DataVolume{SizeBytes: v.SizeBytes, FreeBytes: v.FreeBytes, LowSpace: v.LowSpace}
	}
	out.AddonStep = addonStep(st.Conditions)
	out.Backup = s.statusBackup(a)
	// Without the kit the rest of the status still helps; the console reads an absent kit as unknown.
	if out.RecoveryKit, err = s.recoveryKitInfo(ctx, st.RecoveryKit); err != nil {
		s.deps.Log.Warn("recovery kit not read for the status", "error", err)
		out.RecoveryKit = nil
	}
	// The cache serves the frequent polls; without the certificate the rest of the status still helps.
	certificate, err := s.certificateInUse(ctx, s.deps.Client, a)
	if err != nil {
		s.deps.Log.Warn("certificate in use not read for the status", "error", err)
	}
	if certificate != nil {
		out.Certificate = &StatusCertificate{NotAfter: certificate.NotAfter}
	}
	return GetStatus200JSONResponse(out), nil
}

func condition(c metav1.Condition) Condition {
	changed := c.LastTransitionTime.UTC()
	return Condition{
		Type:               c.Type,
		Status:             ConditionStatus(c.Status),
		Reason:             optional(c.Reason),
		Message:            optional(c.Message),
		LastTransitionTime: &changed,
	}
}

// addonStep is the add-on step of the AddonsReady condition while it progresses; its message starts with the
// step's name.
func addonStep(conditions []metav1.Condition) *string {
	c := meta.FindStatusCondition(conditions, v1alpha1.ConditionAddonsReady)
	if c == nil || c.Status == metav1.ConditionTrue {
		return nil
	}
	step, _, found := strings.Cut(c.Message, ":")
	if !found || step == "" {
		return nil
	}
	return &step
}

// GetCaPem returns the root certificate of the built-in CA. It is public and needs no session.
func (s *Server) GetCaPem(ctx context.Context, _ GetCaPemRequestObject) (GetCaPemResponseObject, error) {
	pem, err := certs.RootCA(ctx, s.deps.Client)
	if errors.Is(err, certs.ErrNotIssued) {
		return GetCaPem404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{
			Code: "not_issued", Message: "The built-in certificate authority has not issued its certificate yet.",
		}}, nil
	}
	if err != nil {
		return nil, err
	}
	disposition := caFileName
	return GetCaPem200ApplicationxPemFileResponse{
		Body:          bytes.NewReader(pem),
		ContentLength: int64(len(pem)),
		Headers:       GetCaPem200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

// GetReadiness reports whether the console accepts a sign-in: the Appliance exists and the hand-over is complete.
// It is public and exposes nothing else.
func (s *Server) GetReadiness(ctx context.Context, _ GetReadinessRequestObject) (GetReadinessResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetReadiness200JSONResponse{SignIn: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return GetReadiness200JSONResponse{SignIn: meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionHandoverComplete)}, nil
}

// appliance reads the Appliance.
func (s *Server) appliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	if err := s.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, fmt.Errorf("read the Appliance: %w", err)
	}
	return a, nil
}

// optional is nil for an empty string.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
