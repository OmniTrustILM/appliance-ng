package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/keycloak"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// AdminPasswordShownAnnotation is "true" once the password in platform.AdminPasswordSecret was shown. Only the console
// writes a new password, marked in the same update; a restored Secret carries no annotations, so its password can be
// shown once more.
const AdminPasswordShownAnnotation = "appliance.otilm.com/admin-password-shown" //nolint:gosec // an annotation name, not a credential

const (
	adminPasswordPath = auth.APIBase + "/platform/admin-password"
	adminPasswordKey  = "password"
)

var errPasswordShown = errors.New("the administrator password was shown already")

// keycloakResetTimeout bounds the calls that set a new password in Keycloak, so the answer that shows it is written
// within the console's write timeout.
var keycloakResetTimeout = 20 * time.Second

var (
	externalIdentityConflict = ConflictJSONResponse{Code: "external_identity",
		Message: "ILM signs users in with an external identity provider, so the appliance keeps no administrator password."}
	platformNotReadyConflict = ConflictJSONResponse{Code: "not_ready",
		Message: "The platform is not ready yet. Try again when it is."}
	backupRunningConflict = ConflictJSONResponse{Code: "backup_running",
		Message: "A backup is running. Set a new password once it has finished."}
	bundleLoadingConflict = ConflictJSONResponse{Code: codeBundleLoading,
		Message: "An update bundle is loading. Set a new password once it has finished."}
	resetRunningConflict = ConflictJSONResponse{Code: "reset_running",
		Message: "A new password is being set. Try again in a moment."}
)

// PostPlatformAdminPassword shows the ILM administrator's password once per password. It fails closed: without its
// audit entry nothing is shown, and the Secret is marked before the answer is sent, so a second request, also a
// concurrent one, gets 410.
func (s *Server) PostPlatformAdminPassword(ctx context.Context, _ PostPlatformAdminPasswordRequestObject) (PostPlatformAdminPasswordResponseObject, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, err
	}
	if externalIdentity(a) {
		return PostPlatformAdminPassword409JSONResponse{ConflictJSONResponse: externalIdentityConflict}, nil
	}
	secret, err := s.adminPasswordSecret(ctx)
	if err == nil {
		if err := s.writeAudit(ctx, Success); err != nil {
			return nil, fmt.Errorf("record that the administrator password was shown in the audit log: %w", err)
		}
		if err = s.markPasswordShown(ctx); err == nil {
			s.deps.Log.Info("administrator password shown")
			return PostPlatformAdminPassword200JSONResponse{User: platform.AdminUser, Password: string(secret.Data[adminPasswordKey])}, nil
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		return PostPlatformAdminPassword404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{
			Code: "not_created", Message: "The platform has not created the administrator password yet.",
		}}, nil
	case errors.Is(err, errPasswordShown):
		return PostPlatformAdminPassword410JSONResponse{GoneJSONResponse: GoneJSONResponse{
			Code: "shown", Message: "The administrator password was shown already; it is shown once.",
		}}, nil
	}
	return nil, fmt.Errorf("show the administrator password: %w", err)
}

// PutPlatformAdminPassword sets a new password for the ILM administrator in the managed Keycloak and, only once
// Keycloak took it, stores it marked as shown, since the answer shows it. It fails closed: the audit entry is
// written before Keycloak is called, and a later failure adds a failure entry.
func (s *Server) PutPlatformAdminPassword(ctx context.Context, _ PutPlatformAdminPasswordRequestObject) (PutPlatformAdminPasswordResponseObject, error) {
	// A reset that waited for another one could not show its password within the console's write timeout.
	if !s.adminPasswordMu.TryLock() {
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: resetRunningConflict}, nil
	}
	defer s.adminPasswordMu.Unlock()
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, err
	}
	if externalIdentity(a) {
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: externalIdentityConflict}, nil
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionPlatformReady) {
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: platformNotReadyConflict}, nil
	}
	admin := &corev1.Secret{}
	err = s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.KeycloakAdminSecret}, admin)
	if err == nil {
		err = s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret}, &corev1.Secret{})
	}
	switch {
	case apierrors.IsNotFound(err):
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: platformNotReadyConflict}, nil
	case err != nil:
		return nil, fmt.Errorf("set a new administrator password: %w", err)
	}
	password, err := platform.NewAdminPassword()
	if err != nil {
		return nil, err
	}
	var keycloakErr error
	err = s.withoutBackup(func() error {
		if err := s.writeAudit(ctx, Success); err != nil {
			return fmt.Errorf("record the administrator password reset in the audit log: %w", err)
		}
		kcCtx, cancel := context.WithTimeout(ctx, keycloakResetTimeout)
		defer cancel()
		if keycloakErr = s.setKeycloakPassword(kcCtx, admin, password); keycloakErr != nil {
			return nil
		}
		if err := s.storeAdminPassword(ctx, password); err != nil {
			return fmt.Errorf("store the new administrator password: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, backup.ErrHeld):
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: bundleLoadingConflict}, nil
	case errors.Is(err, backup.ErrBusy):
		return PutPlatformAdminPassword409JSONResponse{ConflictJSONResponse: backupRunningConflict}, nil
	case err != nil:
		return nil, err
	case keycloakErr != nil:
		s.deps.Log.Warn("Keycloak did not take the new administrator password", "error", keycloakErr)
		return PutPlatformAdminPassword502JSONResponse{Code: "keycloak_failed",
			Message: "Keycloak did not confirm the new password. Set a new one again."}, nil
	}
	s.deps.Log.Info("administrator password set")
	return PutPlatformAdminPassword200JSONResponse{User: platform.AdminUser, Password: password}, nil
}

// withoutBackup runs change while no backup runs, so a backup never holds the stored password of one side and
// Keycloak's database of the other.
func (s *Server) withoutBackup(change func() error) error {
	if s.deps.Backups == nil {
		return change()
	}
	return s.deps.Backups.Exclusive(change)
}

// externalIdentity tells whether ILM signs users in with an external identity provider instead of the managed
// Keycloak.
func externalIdentity(a *v1alpha1.Appliance) bool {
	return a.Spec.Platform != nil && a.Spec.Platform.Identity != nil
}

// setKeycloakPassword sets password for the ILM administrator in the managed Keycloak as the Keycloak administrator
// of the Secret admin.
func (s *Server) setKeycloakPassword(ctx context.Context, admin *corev1.Secret, password string) error {
	base, err := keycloak.URL(ctx, s.deps.Reader, s.deps.KeycloakURL)
	if err != nil {
		return err
	}
	kc := keycloak.Admin{Base: base, Client: s.keycloakClient, User: string(admin.Data["username"]), Password: string(admin.Data["password"])}
	return kc.SetPassword(ctx, keycloak.Realm, platform.AdminUser, password)
}

// adminPasswordSecret reads platform.AdminPasswordSecret, failing with errPasswordShown once its password was shown.
func (s *Server) adminPasswordSecret(ctx context.Context) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret}, secret); err != nil {
		return nil, err
	}
	if secret.Annotations[AdminPasswordShownAnnotation] == "true" {
		return nil, errPasswordShown
	}
	return secret, nil
}

// markPasswordShown records in platform.AdminPasswordSecret that its password was shown, failing with
// errPasswordShown when another request did first.
func (s *Server) markPasswordShown(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := s.adminPasswordSecret(ctx)
		if err != nil {
			return err
		}
		return s.patchAdminPassword(ctx, secret, nil)
	})
}

// storeAdminPassword replaces the password in platform.AdminPasswordSecret with password, marked as shown.
func (s *Server) storeAdminPassword(ctx context.Context, password string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret}, secret); err != nil {
			return err
		}
		return s.patchAdminPassword(ctx, secret, []byte(password))
	})
}

// patchAdminPassword replaces the password in the Secret when password is set and marks the Secret's password as
// shown; it fails on a conflict with a concurrent change.
func (s *Server) patchAdminPassword(ctx context.Context, secret *corev1.Secret, password []byte) error {
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if password != nil {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[adminPasswordKey] = password
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[AdminPasswordShownAnnotation] = "true"
	return s.deps.Client.Patch(ctx, secret, patch, client.FieldOwner(FieldManager))
}
