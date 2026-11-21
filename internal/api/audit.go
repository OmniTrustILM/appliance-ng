package api

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"

	"github.com/OmniTrustILM/appliance-ng/internal/audit"
)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

// audited writes one audit entry for a request to next. The answer is held back until the entry is written, so a
// client that sees the answer finds the entry. A failed audit write is logged and the request still answered:
// refusing a sign-out or showing a saved change as failed because the disk is full would not help. A handler that
// must not proceed without its entry writes it itself with writeAudit.
func (s *Server) audited(action AuditEntryAction, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := requestFrom(r.Context())
		req.entry = &audit.Entry{Action: string(action), User: req.user}
		held := &heldResponse{header: http.Header{}}
		next.ServeHTTP(held, r)

		res := result(held.status())
		switch {
		case req.unaudited:
			s.deps.Log.Debug("request refused without an audit entry", "action", action)
		case req.written == res:
		default:
			// Also when the handler's own entry is contradicted by a later failure.
			if err := s.writeEntry(req.entry, res); err != nil {
				s.deps.Log.Error("audit write failed", "action", action, "result", res, "error", err)
			}
		}
		maps.Copy(w.Header(), held.header)
		w.WriteHeader(held.status())
		_, _ = w.Write(held.body.Bytes())
	})
}

// writeAudit writes the request's audit entry with res now, for a handler that must not go on without it.
func (s *Server) writeAudit(ctx context.Context, res AuditEntryResult) error {
	req := requestFrom(ctx)
	if req.entry == nil {
		return nil
	}
	if err := s.writeEntry(req.entry, res); err != nil {
		return err
	}
	req.written = res
	return nil
}

func (s *Server) writeEntry(e *audit.Entry, res AuditEntryResult) error {
	e.Time, e.Result = s.deps.Now().UTC(), string(res)
	return s.deps.Audit.Write(*e)
}

// result is the audit result of an answer with status.
func result(status int) AuditEntryResult {
	switch {
	case status < http.StatusBadRequest:
		return Success
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Denied
	default:
		return Failure
	}
}

// heldResponse keeps an answer in memory. The answers of the audited operations are small.
type heldResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header { return h.header }

func (h *heldResponse) WriteHeader(code int) {
	if h.code == 0 {
		h.code = code
	}
}

func (h *heldResponse) Write(p []byte) (int, error) {
	h.WriteHeader(http.StatusOK)
	return h.body.Write(p)
}

func (h *heldResponse) status() int {
	if h.code == 0 {
		return http.StatusOK
	}
	return h.code
}

// GetAudit returns the newest audit entries, newest first.
func (s *Server) GetAudit(_ context.Context, req GetAuditRequestObject) (GetAuditResponseObject, error) {
	limit := defaultAuditLimit
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 || limit > maxAuditLimit {
		return GetAudit400JSONResponse{BadRequestJSONResponse: badRequestField("limit",
			fmt.Sprintf("The limit must be between 1 and %d.", maxAuditLimit))}, nil
	}
	entries, err := s.deps.Audit.Tail(limit)
	if err != nil {
		return nil, fmt.Errorf("read the audit log: %w", err)
	}
	items := make([]AuditEntry, len(entries))
	for i, e := range entries {
		paths := e.Paths
		if paths == nil {
			paths = []string{}
		}
		items[i] = AuditEntry{Time: e.Time, User: e.User, Action: AuditEntryAction(e.Action), Result: AuditEntryResult(e.Result), Paths: paths}
		if len(e.Details) > 0 {
			items[i].Details = &e.Details
		}
	}
	return GetAudit200JSONResponse{Items: items}, nil
}
