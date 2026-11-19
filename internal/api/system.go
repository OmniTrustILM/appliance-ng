package api

import (
	"context"
	"fmt"

	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// powerOutcome is how a reboot or shutdown request ended, when it did not fail.
type powerOutcome int

const (
	powerDone powerOutcome = iota
	powerUnconfirmed
	powerUnavailable
)

const (
	codeUnavailable    = "talos_unavailable"
	messageUnavailable = "The node cannot be reached at the moment; try again shortly."
)

// PostSystemReboot reboots the node once the request confirms it.
func (s *Server) PostSystemReboot(ctx context.Context, req PostSystemRebootRequestObject) (PostSystemRebootResponseObject, error) {
	outcome, err := s.power(ctx, req.Body != nil && req.Body.Confirm, "reboot", talos.Node.Reboot)
	switch {
	case err != nil:
		return nil, err
	case outcome == powerUnconfirmed:
		return PostSystemReboot400JSONResponse{BadRequestJSONResponse: badRequestField("confirm", "Confirm the reboot.")}, nil
	case outcome == powerUnavailable:
		return PostSystemReboot503JSONResponse{ServiceUnavailableJSONResponse: ServiceUnavailableJSONResponse{Code: codeUnavailable, Message: messageUnavailable}}, nil
	}
	return PostSystemReboot202Response{}, nil
}

// PostSystemShutdown shuts the node down once the request confirms it.
func (s *Server) PostSystemShutdown(ctx context.Context, req PostSystemShutdownRequestObject) (PostSystemShutdownResponseObject, error) {
	outcome, err := s.power(ctx, req.Body != nil && req.Body.Confirm, "shutdown", talos.Node.Shutdown)
	switch {
	case err != nil:
		return nil, err
	case outcome == powerUnconfirmed:
		return PostSystemShutdown400JSONResponse{BadRequestJSONResponse: badRequestField("confirm", "Confirm the shutdown.")}, nil
	case outcome == powerUnavailable:
		return PostSystemShutdown503JSONResponse{ServiceUnavailableJSONResponse: ServiceUnavailableJSONResponse{Code: codeUnavailable, Message: messageUnavailable}}, nil
	}
	return PostSystemShutdown202Response{}, nil
}

// power runs act on the node when confirmed. The audit entry is written first: once the node goes down, the
// answer and the entry the audited handler writes after it may never come.
func (s *Server) power(ctx context.Context, confirmed bool, action string, act func(talos.Node, context.Context) error) (powerOutcome, error) {
	if !confirmed {
		return powerUnconfirmed, nil
	}
	if s.deps.Talos == nil {
		return powerUnavailable, nil
	}
	node, err := s.deps.Talos(ctx)
	if err != nil {
		s.deps.Log.Warn("no access to the Talos API", "action", action, "error", err)
		return powerUnavailable, nil
	}
	if err := s.writeAudit(ctx, Success); err != nil {
		return powerDone, fmt.Errorf("write the audit entry of the %s: %w", action, err)
	}
	if err := act(node, ctx); err != nil {
		// The Talos error names no secret: the request carries none.
		s.deps.Log.Error("the node refused the request or could not be reached", "action", action, "error", err)
		return powerUnavailable, nil
	}
	s.deps.Log.Info("node "+action+" requested", "user", requestFrom(ctx).user)
	return powerDone, nil
}
