package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

// signInFailedMessage is the message of the answer to every failed sign-in, whichever factor was wrong.
const signInFailedMessage = "The user name, password or code is not correct. If the code is right, check that the authenticator's clock agrees with the appliance time."

// newDummyHash hashes a random password, for sign-ins with an unknown user name.
func newDummyHash() string {
	pw := make([]byte, 32)
	_, _ = rand.Read(pw) // crypto/rand.Read never returns an error.
	hash, err := auth.HashPassword(base64.RawStdEncoding.EncodeToString(pw))
	if err != nil {
		panic(fmt.Sprintf("hash a random password: %v", err)) // argon2 on a fresh random password cannot fail
	}
	return hash
}

// PostSession signs a user in with password and TOTP code. Each request counts once against the limits of the
// user at the peer address, of the peer address and of the user; only a fully successful sign-in takes the user's
// counts back.
func (s *Server) PostSession(ctx context.Context, req PostSessionRequestObject) (PostSessionResponseObject, error) {
	now := s.deps.Now()
	var user, password, code string
	if req.Body != nil {
		user, code = req.Body.User, strings.ReplaceAll(strings.TrimSpace(req.Body.Totp), " ", "")
		if req.Body.Password != nil {
			password = *req.Body.Password
		}
	}
	if ok, retry := s.limiter.Allow(user, requestFrom(ctx).peer, now); !ok {
		// Not audited: an attacker could otherwise fill the audit log while locked out.
		requestFrom(ctx).unaudited = true
		s.deps.Log.Debug("sign-in refused while locked")
		return PostSession401JSONResponse{SignInLockedJSONResponse: SignInLockedJSONResponse{
			Code:       "locked",
			Message:    "Too many failed sign-ins. Wait before trying again.",
			RetryAfter: int(math.Ceil(retry.Seconds())),
		}}, nil
	}

	account, err := s.deps.Users.Get(ctx, user)
	known := err == nil
	if err != nil && !errors.Is(err, auth.ErrUserNotFound) {
		return nil, fmt.Errorf("read the user: %w", err)
	}
	hash := account.PasswordHash
	if !known {
		hash = s.dummyHash
	}
	passwordOK, err := s.passwordMatches(ctx, hash, password)
	if err != nil {
		return nil, err
	}
	ok := passwordOK && known && s.guard.Check(user, account.TOTPSeed, code, now)
	if !ok {
		if known {
			// Only an existing account's name is recorded: an unknown one may be a password typed into the wrong field.
			auditUser(ctx, user)
		}
		s.deps.Log.Info("sign-in failed")
		serverTime := now.UTC().Truncate(time.Second)
		return PostSession400JSONResponse{SignInFailedJSONResponse: SignInFailedJSONResponse{
			Code: "invalid_credentials", Message: signInFailedMessage, ServerTime: serverTime,
		}}, nil
	}

	s.limiter.Succeed(user, requestFrom(ctx).peer)
	token, csrf := s.sessions.Create(user)
	auditUser(ctx, user)
	s.deps.Log.Info("signed in", "user", user)
	cookie := auth.SessionCookie(token).String()
	return PostSession200JSONResponse{Body: CsrfToken{CsrfToken: csrf}, Headers: PostSession200ResponseHeaders{SetCookie: &cookie}}, nil
}

// passwordMatches checks password against hash in one of the hash slots, which it frees also when the check panics.
func (s *Server) passwordMatches(ctx context.Context, hash, password string) (bool, error) {
	if err := s.hashSlots.Acquire(ctx); err != nil {
		return false, fmt.Errorf("wait to check the password: %w", err)
	}
	defer s.hashSlots.Release()
	return s.checkPassword(hash, password), nil
}

// GetSession returns the signed-in user and the session's CSRF token.
func (s *Server) GetSession(ctx context.Context, _ GetSessionRequestObject) (GetSessionResponseObject, error) {
	user, _ := auth.UserFromContext(ctx)
	csrf, _ := auth.CSRFFromContext(ctx)
	return GetSession200JSONResponse{User: user, CsrfToken: csrf}, nil
}

// DeleteSession signs out.
func (s *Server) DeleteSession(ctx context.Context, _ DeleteSessionRequestObject) (DeleteSessionResponseObject, error) {
	s.sessions.Delete(requestFrom(ctx).token)
	user, _ := auth.UserFromContext(ctx)
	s.deps.Log.Info("signed out", "user", user)
	cookie := auth.ClearSessionCookie().String()
	return DeleteSession204Response{Headers: DeleteSession204ResponseHeaders{SetCookie: &cookie}}, nil
}
