package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/api/setupapi"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

const (
	totpIssuer  = "ILM appliance"
	sessionUser = "setup"
)

var userName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

func conflict(message string) setupapi.ConflictJSONResponse {
	return setupapi.ConflictJSONResponse{Code: "conflict", Message: message}
}

func badRequest(field, message string) setupapi.BadRequestJSONResponse {
	e := setupapi.BadRequestJSONResponse{Code: "invalid", Message: message}
	if field != "" {
		e.Field = &field
	}
	return e
}

var (
	// wrongStep is the conflict answer to a request for a step the wizard is not at.
	wrongStep = conflict("This setup step is not open: complete the previous steps first, or reload the page.")
	// installing is the conflict answer once the answers are applied.
	installing = setupapi.PostSetupCode409JSONResponse{ConflictJSONResponse: conflict("Setup is complete; the appliance is installing.")}
)

// PostSetupCode starts the setup session. The first browser that passes the code owns setup; another browser gets
// 409 while that session lives. Once it expired, the next browser with the code starts over.
func (w *wizard) PostSetupCode(ctx context.Context, req setupapi.PostSetupCodeRequestObject) (setupapi.PostSetupCodeResponseObject, error) {
	if csrf, ok := sessionCSRF(ctx); ok {
		return setupapi.PostSetupCode200JSONResponse{Body: setupapi.CsrfToken{CsrfToken: csrf}}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Code) == "" {
		return setupapi.PostSetupCode400JSONResponse{BadRequestJSONResponse: badRequest("code", "Enter the setup code.")}, nil
	}
	if w.currentStep() == setupapi.Applying {
		return installing, nil
	}
	ok, retry := w.code.Verify(req.Body.Code, w.deps.Now())
	if !ok {
		w.deps.Log.Info("setup code refused", "locked", retry > 0)
		e := setupapi.LockedJSONResponse{Code: "invalid_code", Message: "The setup code is not valid."}
		if retry > 0 {
			secs := int(math.Ceil(retry.Seconds()))
			e.RetryAfter = &secs
			e.Message = "Too many wrong setup codes. Wait before trying again."
		}
		return setupapi.PostSetupCode401JSONResponse{LockedJSONResponse: e}, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.step == setupapi.Applying:
		return installing, nil
	case w.sessions.Active() > 0:
		w.deps.Log.Info("setup code entered in a second browser while setup is in progress")
		return setupapi.PostSetupCode409JSONResponse{ConflictJSONResponse: conflict("Setup is in progress in another browser.")}, nil
	case w.step != setupapi.Code:
		w.deps.Log.Warn("the setup session expired, setup starts over in a new session")
		w.user, w.passwordHash, w.seed, w.node = "", "", "", NodeAnswers{}
	}
	token, csrf := w.sessions.Create(sessionUser)
	w.step = setupapi.Admin
	w.deps.Log.Info("setup session started")
	cookie := auth.SessionCookie(token).String()
	return setupapi.PostSetupCode200JSONResponse{
		Body:    setupapi.CsrfToken{CsrfToken: csrf},
		Headers: setupapi.PostSetupCode200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// GetSetupState returns the step the session resumes from, or code without a session.
func (w *wizard) GetSetupState(ctx context.Context, _ setupapi.GetSetupStateRequestObject) (setupapi.GetSetupStateResponseObject, error) {
	state := setupapi.SetupState{Step: w.currentStep()}
	if csrf, ok := sessionCSRF(ctx); ok {
		state.CsrfToken = &csrf
	} else if state.Step != setupapi.Applying {
		state.Step = setupapi.Code
	}
	return setupapi.GetSetupState200JSONResponse(state), nil
}

// PutSetupAdmin sets the administrator's user name and password.
func (w *wizard) PutSetupAdmin(ctx context.Context, req setupapi.PutSetupAdminRequestObject) (setupapi.PutSetupAdminResponseObject, error) {
	if w.currentStep() != setupapi.Admin {
		return setupapi.PutSetupAdmin409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	if req.Body == nil || !userName.MatchString(req.Body.User) {
		return setupapi.PutSetupAdmin400JSONResponse{BadRequestJSONResponse: badRequest("user",
			"Enter a user name of up to 64 letters, digits and . _ @ - characters.")}, nil
	}
	if req.Body.Password == nil {
		return setupapi.PutSetupAdmin400JSONResponse{BadRequestJSONResponse: badRequest("password", "Enter a password.")}, nil
	}
	if err := auth.ValidatePassword(*req.Body.Password, req.Body.User); err != nil {
		return setupapi.PutSetupAdmin400JSONResponse{BadRequestJSONResponse: badRequest("password", auth.PasswordProblem(err))}, nil
	}
	hash, err := w.hash(ctx, *req.Body.Password)
	if err != nil {
		return nil, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.step != setupapi.Admin {
		return setupapi.PutSetupAdmin409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	w.user, w.passwordHash, w.step = req.Body.User, hash, setupapi.Totp
	w.deps.Log.Info(msgStepDone, "step", "admin")
	return setupapi.PutSetupAdmin204Response{}, nil
}

// msgStepDone is the log message when a wizard step is done.
const msgStepDone = "setup step done"

// hash hashes pw in one of the hash slots, which it frees also when hashing panics.
func (w *wizard) hash(ctx context.Context, pw string) (string, error) {
	if err := w.hashSlots.Acquire(ctx); err != nil {
		return "", fmt.Errorf("wait to hash the password: %w", err)
	}
	defer w.hashSlots.Release()
	return w.hashPassword(pw)
}

// PostSetupTotp issues a new TOTP seed until one is confirmed; an earlier seed is discarded.
func (w *wizard) PostSetupTotp(context.Context, setupapi.PostSetupTotpRequestObject) (setupapi.PostSetupTotpResponseObject, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.step != setupapi.Totp {
		return setupapi.PostSetupTotp409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	seed, url, err := auth.NewTOTP(w.user, totpIssuer)
	if err != nil {
		return nil, err
	}
	w.seed = seed
	return setupapi.PostSetupTotp200JSONResponse{Seed: seed, OtpauthUrl: url}, nil
}

// PostSetupTotpConfirm confirms the issued seed with a code from the authenticator.
func (w *wizard) PostSetupTotpConfirm(_ context.Context, req setupapi.PostSetupTotpConfirmRequestObject) (setupapi.PostSetupTotpConfirmResponseObject, error) {
	now := w.deps.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.step != setupapi.Totp || w.seed == "" {
		return setupapi.PostSetupTotpConfirm409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	code := ""
	if req.Body != nil {
		code = strings.ReplaceAll(strings.TrimSpace(req.Body.Code), " ", "")
	}
	if !w.guard.Check(w.user, w.seed, code, now) {
		serverTime := now.UTC().Truncate(time.Second)
		return setupapi.PostSetupTotpConfirm400JSONResponse{BadRequestWithServerTimeJSONResponse: setupapi.BadRequestWithServerTimeJSONResponse{
			Code:       "invalid_code",
			Message:    "The code does not match. Check that the authenticator's clock agrees with the appliance time.",
			ServerTime: &serverTime,
		}}, nil
	}
	w.step = setupapi.Node
	w.deps.Log.Info(msgStepDone, "step", "totp")
	return setupapi.PostSetupTotpConfirm204Response{}, nil
}

// GetSetupDhcp returns the network setup DHCP gave the node; nothing when the node does not report it.
func (w *wizard) GetSetupDhcp(ctx context.Context, _ setupapi.GetSetupDhcpRequestObject) (setupapi.GetSetupDhcpResponseObject, error) {
	var out setupapi.DhcpSettings
	nw, err := w.deps.Network(ctx)
	if err != nil {
		w.deps.Log.Warn("could not read the DHCP settings", "error", err)
		return setupapi.GetSetupDhcp200JSONResponse(out), nil
	}
	if nw.HostName != "" {
		out.HostName = &nw.HostName
	}
	if nw.Address.IsValid() {
		addr := nw.Address.String()
		out.Address = &addr
	}
	if nw.Gateway.IsValid() {
		gw := nw.Gateway.String()
		out.Gateway = &gw
	}
	if len(nw.DNS) > 0 {
		dns := make([]string, len(nw.DNS))
		for i, a := range nw.DNS {
			dns[i] = a.String()
		}
		out.Dns = &dns
	}
	if len(nw.NTP) > 0 {
		out.Ntp = &nw.NTP
	}
	return setupapi.GetSetupDhcp200JSONResponse(out), nil
}

// PutSetupNode saves the node settings; the review step may send them again.
func (w *wizard) PutSetupNode(_ context.Context, req setupapi.PutSetupNodeRequestObject) (setupapi.PutSetupNodeResponseObject, error) {
	if step := w.currentStep(); step != setupapi.Node && step != setupapi.Apply {
		return setupapi.PutSetupNode409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	if req.Body == nil {
		return setupapi.PutSetupNode400JSONResponse{BadRequestJSONResponse: badRequest("", "Enter the node settings.")}, nil
	}
	answers, input, ferr := parseNode(*req.Body)
	if ferr != nil {
		return setupapi.PutSetupNode400JSONResponse{BadRequestJSONResponse: badRequest(ferr.field, ferr.message)}, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.step != setupapi.Node && w.step != setupapi.Apply {
		return setupapi.PutSetupNode409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	w.node, w.input, w.step = answers, input, setupapi.Apply
	w.deps.Log.Info(msgStepDone, "step", "node", "hostName", answers.HostName, "dhcp", answers.Address == "")
	return setupapi.PutSetupNode204Response{}, nil
}

// PostSetupApply hands the answers over for the machine configuration, once.
func (w *wizard) PostSetupApply(context.Context, setupapi.PostSetupApplyRequestObject) (setupapi.PostSetupApplyResponseObject, error) {
	w.mu.Lock()
	if w.step != setupapi.Apply {
		w.mu.Unlock()
		return setupapi.PostSetupApply409JSONResponse{ConflictJSONResponse: wrongStep}, nil
	}
	file := HandoverFile{Version: HandoverVersion, Node: w.node}
	file.Admin.User, file.Admin.PasswordHash, file.Admin.TOTPSeed = w.user, w.passwordHash, w.seed
	data, err := json.Marshal(file)
	if err != nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("encode the hand-over file: %w", err)
	}
	input := w.input
	input.Handover = data
	w.step = setupapi.Applying
	w.mu.Unlock()

	w.deps.Log.Info("setup answers applied")
	w.deps.Apply(input)
	return setupapi.PostSetupApply202Response{}, nil
}

func (w *wizard) currentStep() setupapi.SetupStep {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.step
}
