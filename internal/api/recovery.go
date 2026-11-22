package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	recoveryKitPath     = auth.APIBase + "/recovery-kit"
	recoveryKitFileName = `attachment; filename="ilm-appliance-recovery-kit.tar.age"`

	partKit           = "kit"
	partPassphrase    = "passphrase"
	partNewPassphrase = "newPassphrase"
	fieldConfirm      = "confirm"
)

var (
	refusedKitIssued = &refusal{conflict: true, code: "kit_issued",
		message: "A recovery kit was issued already; issue it again from the current kit, or replace a lost one."}
	refusedKitNotIssued = &refusal{conflict: true, code: "not_issued", message: "No recovery kit was issued yet; issue one first."}
	refusedForeignKit   = &refusal{conflict: true, code: "foreign_kit",
		message: "The kit does not hold this appliance's backup key; upload the latest kit, or replace a lost one."}
	refusedWrongPassphrase = &refusal{code: "wrong_passphrase", message: "The passphrase does not open the kit.", field: partPassphrase}
	refusedInvalidKit      = &refusal{code: "invalid_kit", message: "The file is not a recovery kit.", field: partKit}
	refusedUnconfirmed     = &refusal{code: codeInvalid, field: fieldConfirm,
		message: "Confirm that backups made so far stay readable only with the lost kit."}
	refusedKitUnavailable  = &refusal{code: codeUnavailable, message: messageUnavailable}
	refusedKitNotDelivered = &refusal{conflict: true, code: "kit_not_delivered",
		message: "The latest kit was not handed out; replace it."}
	refusedKitSuperseded = &refusal{conflict: true, code: "kit_superseded",
		message: "A newer kit was issued since this one; download the latest kit and confirm that one."}
	refusedNoIssuance = &refusal{code: codeInvalid, message: "Name the issuance of the kit to confirm."}
)

// GetRecoveryKit describes the latest recovery kit.
func (s *Server) GetRecoveryKit(ctx context.Context, _ GetRecoveryKitRequestObject) (GetRecoveryKitResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetRecoveryKit200JSONResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := s.recoveryKitInfo(ctx, a.Status.RecoveryKit)
	if err != nil {
		return nil, err
	}
	return GetRecoveryKit200JSONResponse(*info), nil
}

// recoveryKitInfo describes the kit of the stored recipient, which decides whether a kit was issued. The details
// and the confirmation in st count only when st records that recipient and issuance: otherwise its kit was not
// handed out.
func (s *Server) recoveryKitInfo(ctx context.Context, st *v1alpha1.RecoveryKitStatus) (*RecoveryKit, error) {
	out := &RecoveryKit{}
	stored, err := recovery.StoredKit(ctx, s.deps.Reader)
	if errors.Is(err, recovery.ErrNoRecipient) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Issued, out.Recipient = true, &stored.Recipient
	if !handedOut(st, stored) {
		return out, nil
	}
	out.Confirmed, out.Issuance = st.Confirmed, &stored.Issuance
	if st.IssuedAt != nil {
		out.IssuedAt = &st.IssuedAt.Time
	}
	if st.TalosconfigNotAfter != nil {
		out.TalosconfigNotAfter = &st.TalosconfigNotAfter.Time
	}
	return out, nil
}

// handedOut tells whether st records the stored kit, which it does once that kit was handed out.
func handedOut(st *v1alpha1.RecoveryKitStatus, stored recovery.Stored) bool {
	return st != nil && st.Recipient == stored.Recipient && st.Issuance == stored.Issuance
}

// PostRecoveryKit issues the first recovery kit with a new backup key.
func (s *Server) PostRecoveryKit(ctx context.Context, req PostRecoveryKitRequestObject) (PostRecoveryKitResponseObject, error) {
	var passphrase string
	if req.Body != nil {
		passphrase = deref(req.Body.Passphrase)
	}
	return s.kitRequest(func() (issuedKit, *refusal, error) { return s.newKit(ctx, passphrase, false) })
}

// PostRecoveryKitReplace issues a kit with a new backup key in place of a lost one.
func (s *Server) PostRecoveryKitReplace(ctx context.Context, req PostRecoveryKitReplaceRequestObject) (PostRecoveryKitReplaceResponseObject, error) {
	if req.Body == nil || !req.Body.Confirm {
		return kitAnswer{ref: refusedUnconfirmed}, nil
	}
	return s.kitRequest(func() (issuedKit, *refusal, error) { return s.newKit(ctx, deref(req.Body.Passphrase), true) })
}

// kitRequest runs issue, one kit change at a time, and answers with the kit or the refusal. Checking the stored
// recipient and writing it happen under the same lock, so concurrent requests cannot hand out kits with different
// keys; the manager runs as one replica.
func (s *Server) kitRequest(issue func() (issuedKit, *refusal, error)) (kitAnswer, error) {
	s.kitMu.Lock()
	defer s.kitMu.Unlock()
	kit, ref, err := issue()
	return kitAnswer{kit: kit, ref: ref}, err
}

// issuedKit is an encrypted kit and its issuance.
type issuedKit struct {
	data     []byte
	issuance string
}

// kitAnswer answers issue, reissue and replace: the kit, or the refusal with its status.
type kitAnswer struct {
	kit issuedKit
	ref *refusal
}

func (a kitAnswer) VisitPostRecoveryKitResponse(w http.ResponseWriter) error {
	switch {
	case a.ref == nil:
		return PostRecoveryKit200ApplicationoctetStreamResponse{kitFile(a.kit)}.VisitPostRecoveryKitResponse(w)
	case a.ref == refusedKitUnavailable:
		return PostRecoveryKit503JSONResponse{ServiceUnavailableJSONResponse(a.ref.badRequest())}.VisitPostRecoveryKitResponse(w)
	case a.ref.conflict:
		return PostRecoveryKit409JSONResponse{ConflictJSONResponse(a.ref.badRequest())}.VisitPostRecoveryKitResponse(w)
	}
	return PostRecoveryKit400JSONResponse{a.ref.badRequest()}.VisitPostRecoveryKitResponse(w)
}

func (a kitAnswer) VisitPostRecoveryKitReplaceResponse(w http.ResponseWriter) error {
	return a.VisitPostRecoveryKitResponse(w)
}

func (a kitAnswer) VisitPostRecoveryKitReissueResponse(w http.ResponseWriter) error {
	return a.VisitPostRecoveryKitResponse(w)
}

// newKit issues a kit with a new backup key and stores its public key: the first kit, or with replace one in place
// of an issued kit.
func (s *Server) newKit(ctx context.Context, passphrase string, replace bool) (issuedKit, *refusal, error) {
	if ref := checkPassphrase(partPassphrase, passphrase); ref != nil {
		return issuedKit{}, ref, nil
	}
	_, err := recovery.StoredRecipient(ctx, s.deps.Reader)
	switch {
	case err == nil && !replace:
		return issuedKit{}, refusedKitIssued, nil
	case errors.Is(err, recovery.ErrNoRecipient) && replace:
		return issuedKit{}, refusedKitNotIssued, nil
	case err != nil && !errors.Is(err, recovery.ErrNoRecipient):
		return issuedKit{}, nil, err
	}
	key, err := recovery.GenerateKey()
	if err != nil {
		return issuedKit{}, nil, err
	}
	return s.issueKit(ctx, key, passphrase, true)
}

// PostRecoveryKitReissue issues the kit again from the uploaded one: the same backup key, a new passphrase and new
// break-glass credentials.
func (s *Server) PostRecoveryKitReissue(ctx context.Context, req PostRecoveryKitReissueRequestObject) (PostRecoveryKitReissueResponseObject, error) {
	parts, ref := readUpload(req.Body, partKit)
	if ref != nil {
		return kitAnswer{ref: ref}, nil
	}
	return s.kitRequest(func() (issuedKit, *refusal, error) { return s.reissueKit(ctx, parts) })
}

// reissueKit issues a kit with the backup key of the uploaded kit, which must be the stored recipient's.
func (s *Server) reissueKit(ctx context.Context, parts map[string][]byte) (issuedKit, *refusal, error) {
	if ref := checkPassphrase(partNewPassphrase, string(parts[partNewPassphrase])); ref != nil {
		return issuedKit{}, ref, nil
	}
	stored, err := recovery.StoredRecipient(ctx, s.deps.Reader)
	if errors.Is(err, recovery.ErrNoRecipient) {
		return issuedKit{}, refusedKitNotIssued, nil
	}
	if err != nil {
		return issuedKit{}, nil, err
	}
	kit, err := s.openKit(ctx, parts[partKit], string(parts[partPassphrase]))
	switch {
	case errors.Is(err, recovery.ErrWrongPassphrase):
		return issuedKit{}, refusedWrongPassphrase, nil
	case errors.Is(err, recovery.ErrInvalidKit):
		return issuedKit{}, refusedInvalidKit, nil
	case err != nil:
		return issuedKit{}, nil, err
	case kit.BackupKey.Recipient().String() != stored:
		return issuedKit{}, refusedForeignKit, nil
	}
	return s.issueKit(ctx, kit.BackupKey, string(parts[partNewPassphrase]), false)
}

// checkPassphrase refuses a passphrase that a console password could not be.
func checkPassphrase(field, passphrase string) *refusal {
	if err := auth.ValidatePassword(passphrase, ""); err != nil {
		return &refusal{code: codeInvalid, message: auth.PasswordProblem(err), field: field}
	}
	return nil
}

// issueKit issues new break-glass credentials and returns them with key in a kit encrypted with passphrase, under a
// new issuance stored with key's public key, which becomes the backups' recipient; newKey only tells the log. Nothing
// is handed out without its audit entry.
func (s *Server) issueKit(ctx context.Context, key *age.X25519Identity, passphrase string, newKey bool) (issuedKit, *refusal, error) {
	address, conflict, err := s.nodeAddress(ctx)
	if err != nil {
		return issuedKit{}, nil, err
	}
	if conflict != nil {
		return issuedKit{}, &refusal{conflict: true, code: conflict.Code, message: conflict.Message}, nil
	}
	if s.deps.Talos == nil {
		return issuedKit{}, refusedKitUnavailable, nil
	}
	node, err := s.deps.Talos(ctx)
	if err != nil {
		s.deps.Log.Warn("no access to the Talos API", "action", "recovery kit", "error", err)
		return issuedKit{}, refusedKitUnavailable, nil
	}
	talosconfig, notAfter, err := access.BreakGlassTalosconfig(ctx, node, address)
	if err != nil {
		// The error names no secret: a talosconfig that does not parse is reported without its content.
		s.deps.Log.Error("the node did not generate the recovery kit's talosconfig", "error", err)
		return issuedKit{}, refusedKitUnavailable, nil
	}
	user := requestFrom(ctx).user
	if err := access.RecordTalosconfig(ctx, s.deps.Client, access.RoleOSAdmin, notAfter, user); err != nil {
		return issuedKit{}, nil, err
	}
	a, err := s.appliance(ctx)
	if err != nil {
		return issuedKit{}, nil, err
	}
	server := "https://" + net.JoinHostPort(address, apiServerPort)
	kubeconfig, err := access.IssueBreakGlass(ctx, s.deps.Client, s.deps.Kube, server, s.deps.ClusterCA, user)
	if err != nil {
		return issuedKit{}, nil, err
	}
	kit := recovery.Kit{BackupKey: key, Kubeconfig: kubeconfig, Talosconfig: talosconfig, Identity: recovery.Identity{
		HostName: a.Spec.Node.HostName, Address: address, ApplianceVersion: a.Status.Versions.Appliance,
	}}
	if a.Spec.Platform != nil {
		kit.Identity.ILMHostName = a.Spec.Platform.HostName
	}
	data, err := s.buildKit(ctx, kit, passphrase)
	if err != nil {
		return issuedKit{}, nil, err
	}
	issuance, err := recovery.NewIssuance()
	if err != nil {
		return issuedKit{}, nil, err
	}
	recipient := key.Recipient().String()
	err = s.writeSystemSecret(ctx, recovery.RecipientSecret, corev1.SecretTypeOpaque, map[string][]byte{
		recovery.RecipientKey: []byte(recipient), recovery.IssuanceKey: []byte(issuance),
	})
	if err != nil {
		return issuedKit{}, nil, err
	}
	recordDetails(ctx, map[string]string{
		"name": access.BreakGlass, "role": access.RoleClusterAdmin, "expires": notAfter.UTC().Format(time.RFC3339), "recipient": recipient,
	})
	if err := s.writeAudit(ctx, Success); err != nil {
		return issuedKit{}, nil, fmt.Errorf("record the issued recovery kit in the audit log: %w", err)
	}
	// The status names the recipient last: a kit whose status write fails is not handed out and cannot be confirmed.
	issued := metav1.NewTime(s.deps.Now().UTC().Truncate(time.Second))
	expires := metav1.NewTime(notAfter)
	err = s.updateRecoveryKit(ctx, func(*v1alpha1.RecoveryKitStatus) *v1alpha1.RecoveryKitStatus {
		return &v1alpha1.RecoveryKitStatus{IssuedAt: &issued, TalosconfigNotAfter: &expires, Recipient: recipient, Issuance: issuance}
	})
	if err != nil {
		return issuedKit{}, nil, err
	}
	s.deps.Log.Info("recovery kit issued", "recipient", recipient, "issuance", issuance, "newKey", newKey, "talosconfigNotAfter", notAfter)
	return issuedKit{data: data, issuance: issuance}, nil, nil
}

// buildKit encrypts the kit; it takes a password slot, as its scrypt takes as much memory as a password hash.
func (s *Server) buildKit(ctx context.Context, kit recovery.Kit, passphrase string) ([]byte, error) {
	if err := s.hashSlots.Acquire(ctx); err != nil {
		return nil, err
	}
	defer s.hashSlots.Release()
	return recovery.Build(kit, passphrase)
}

// openKit decrypts an uploaded kit in a password slot.
func (s *Server) openKit(ctx context.Context, data []byte, passphrase string) (recovery.Kit, error) {
	if err := s.hashSlots.Acquire(ctx); err != nil {
		return recovery.Kit{}, err
	}
	defer s.hashSlots.Release()
	return recovery.Open(data, passphrase)
}

// PostRecoveryKitConfirm records that the administrator stored the latest kit safely: the kit of the stored
// recipient and issuance, which the status names once it was handed out. A kit superseded since it was downloaded
// is refused, so that only a kit the administrator holds counts as confirmed.
func (s *Server) PostRecoveryKitConfirm(ctx context.Context, req PostRecoveryKitConfirmRequestObject) (PostRecoveryKitConfirmResponseObject, error) {
	if req.Body == nil {
		return PostRecoveryKitConfirm400JSONResponse{BadRequestJSONResponse(refusedNoIssuance.badRequest())}, nil
	}
	s.kitMu.Lock()
	defer s.kitMu.Unlock()
	stored, err := recovery.StoredKit(ctx, s.deps.Reader)
	if errors.Is(err, recovery.ErrNoRecipient) {
		return confirmRefused(refusedKitNotIssued), nil
	}
	if err != nil {
		return nil, err
	}
	a, err := s.appliance(ctx)
	switch {
	case apierrors.IsNotFound(err):
		return PostRecoveryKitConfirm409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case err != nil:
		return nil, err
	case !handedOut(a.Status.RecoveryKit, stored):
		return confirmRefused(refusedKitNotDelivered), nil
	case req.Body.Issuance != stored.Issuance:
		return confirmRefused(refusedKitSuperseded), nil
	}
	err = s.updateRecoveryKit(ctx, func(st *v1alpha1.RecoveryKitStatus) *v1alpha1.RecoveryKitStatus {
		if !handedOut(st, stored) {
			return nil
		}
		st.Confirmed = true
		return st
	})
	if errors.Is(err, recovery.ErrNoRecipient) {
		return confirmRefused(refusedKitNotDelivered), nil
	}
	if err != nil {
		return nil, err
	}
	s.deps.Log.Info("recovery kit confirmed", "issuance", stored.Issuance)
	return PostRecoveryKitConfirm204Response{}, nil
}

func confirmRefused(ref *refusal) PostRecoveryKitConfirm409JSONResponse {
	return PostRecoveryKitConfirm409JSONResponse{ConflictJSONResponse: ConflictJSONResponse(ref.badRequest())}
}

// updateRecoveryKit replaces the recovery kit status with what change makes of it; recovery.ErrNoRecipient when it
// makes nil.
func (s *Server) updateRecoveryKit(ctx context.Context, change func(*v1alpha1.RecoveryKitStatus) *v1alpha1.RecoveryKitStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		if a.Status.RecoveryKit = change(a.Status.RecoveryKit); a.Status.RecoveryKit == nil {
			return recovery.ErrNoRecipient
		}
		return s.deps.Client.Status().Update(ctx, a, client.FieldOwner(FieldManager))
	})
}

// kitFile is the answer that hands out a kit, naming its issuance.
func kitFile(kit issuedKit) RecoveryKitFileApplicationoctetStreamResponse {
	disposition := recoveryKitFileName
	return RecoveryKitFileApplicationoctetStreamResponse{
		Body:          bytes.NewReader(kit.data),
		ContentLength: int64(len(kit.data)),
		Headers:       RecoveryKitFileResponseHeaders{ContentDisposition: &disposition, XRecoveryKitIssuance: &kit.issuance},
	}
}
