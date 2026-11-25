package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Masterminds/semver/v3"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const (
	updatesPath       = auth.APIBase + "/updates"
	updatesUploadPath = updatesPath + "/upload"
	// maxBundleUpload caps an uploaded bundle.
	maxBundleUpload = 64 << 30

	codeUpdatesUnavailable = "updates_unavailable"
	codeUpdateBusy         = "busy"
	// codeBundleLoading refuses another operation while an update bundle loads.
	codeBundleLoading = "bundle_loading"
	detailKind        = "kind"
	detailVersion     = "version"
)

var (
	answerUpdatesUnavailable = refused(http.StatusServiceUnavailable, codeUpdatesUnavailable, "Update bundles are not available in this manager.")
	answerBadKind            = refusedField(codeInvalid, detailKind, "Choose a system or a platform bundle.")
	answerBadVersion         = refusedField(codeInvalid, detailVersion, "Enter a version X.Y.Z.")
	answerNoBundleSize       = refused(http.StatusBadRequest, codeInvalid, "Send the bundle's size in Content-Length; a bundle is at most 64 GiB.")
	answerPartialBundle      = refused(http.StatusBadRequest, "incomplete", "The bundle did not arrive completely; upload it again.")
	answerBundleNoSpace      = refused(http.StatusConflict, "no_space",
		"The data volume has too little free space for the bundle: it needs three times the bundle's size.")
	answerUpdateBusy = refused(http.StatusConflict, codeUpdateBusy,
		"A backup, a restore, an update or another bundle load is running; try again once it has finished.")
	answerNoNotes = refused(http.StatusNotFound, codeNotFound, "There are no release notes for that bundle.")
)

// Updater loads bundles, one at a time, into the appliance's registry and data volume.
type Updater interface {
	// Reserve takes the shared admission for a load of kind by user; it refuses with the reason update.Busy.
	Reserve(ctx context.Context, kind, user string) (*update.Reservation, error)
	// Last is the latest load.
	Last() update.LoadState
	// Notes are the release notes of a bundle loaded or found online; update.ErrNoNotes when there are none.
	Notes(kind, version string) ([]byte, error)
}

// UpdateFinder checks the online repository for newer bundles and records them in the status.
type UpdateFinder interface {
	Check(ctx context.Context) error
}

func (a restoreAnswer) VisitGetUpdatesResponse(w http.ResponseWriter) error       { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesCheckResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPutUpdatesUploadResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesFetchResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitGetUpdateNotesResponse(w http.ResponseWriter) error   { return a.visit(w) }

func (s *Server) updatesAvailable() bool {
	return s.deps.Updates != nil && s.deps.Finder != nil
}

// GetUpdates lists the bundles available online and loaded, and the latest load.
func (s *Server) GetUpdates(ctx context.Context, _ GetUpdatesRequestObject) (GetUpdatesResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	u, err := s.updates(ctx)
	if err != nil {
		return nil, err
	}
	return restoreAnswer{status: http.StatusOK, body: u}, nil
}

// PostUpdatesCheck checks the online repository now; a failed check is answered with its fixed reason.
func (s *Server) PostUpdatesCheck(ctx context.Context, _ PostUpdatesCheckRequestObject) (PostUpdatesCheckResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	if err := s.deps.Finder.Check(ctx); err != nil {
		requestFrom(ctx).result = Failure
		recordDetails(ctx, map[string]string{detailRefused: update.Reason(err)})
	}
	u, err := s.updates(ctx)
	if err != nil {
		return nil, err
	}
	return restoreAnswer{status: http.StatusOK, body: u}, nil
}

// PutUpdatesUpload stores the uploaded bundle and loads it in the background. The admission and the free space are
// checked before the body is read.
func (s *Server) PutUpdatesUpload(ctx context.Context, req PutUpdatesUploadRequestObject) (PutUpdatesUploadResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	kind := req.Params.Kind
	details := map[string]string{detailKind: string(kind)}
	size := requestFrom(ctx).contentLength
	switch {
	case !kind.Valid():
		return answered(ctx, details, answerBadKind), nil
	case size < 0 || size > maxBundleUpload:
		return answered(ctx, details, answerNoBundleSize), nil
	}
	r, answer, err := s.reserve(ctx, string(kind))
	if answer != nil || err != nil {
		return answered(ctx, details, orFailure(answer)), err
	}
	path, err := s.storeBundle(size, req.Body)
	switch {
	case errors.Is(err, errSource):
		r.Cancel()
		return answered(ctx, details, answerPartialBundle), nil
	case errors.Is(err, storage.ErrNoSpace):
		r.Cancel()
		return answered(ctx, details, answerBundleNoSpace), nil
	case err != nil:
		r.Cancel()
		return nil, err
	}
	r.Load(path)
	return answered(ctx, details, restoreAnswer{status: http.StatusAccepted}), nil
}

// PostUpdatesFetch loads a bundle from the online repository in the background.
func (s *Server) PostUpdatesFetch(ctx context.Context, req PostUpdatesFetchRequestObject) (PostUpdatesFetchResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	if req.Body == nil || !req.Body.Kind.Valid() {
		return answered(ctx, map[string]string{}, answerBadKind), nil
	}
	details := map[string]string{detailKind: string(req.Body.Kind)}
	if _, err := semver.StrictNewVersion(req.Body.Version); err != nil {
		return answered(ctx, details, answerBadVersion), nil
	}
	details[detailVersion] = req.Body.Version
	r, answer, err := s.reserve(ctx, string(req.Body.Kind))
	if answer != nil || err != nil {
		return answered(ctx, details, orFailure(answer)), err
	}
	r.Fetch(req.Body.Version)
	return answered(ctx, details, restoreAnswer{status: http.StatusAccepted}), nil
}

// GetUpdateNotes serves the release notes of a bundle as Markdown.
func (s *Server) GetUpdateNotes(_ context.Context, req GetUpdateNotesRequestObject) (GetUpdateNotesResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	notes, err := s.deps.Updates.Notes(string(req.Kind), req.Version)
	if errors.Is(err, update.ErrNoNotes) {
		return answerNoNotes, nil
	}
	if err != nil {
		return nil, err
	}
	return GetUpdateNotes200TextmarkdownResponse{Body: bytes.NewReader(notes), ContentLength: int64(len(notes))}, nil
}

// reserve takes the shared admission for a load of kind by the request's user, or answers why not.
func (s *Server) reserve(ctx context.Context, kind string) (*update.Reservation, *restoreAnswer, error) {
	r, err := s.deps.Updates.Reserve(ctx, kind, requestFrom(ctx).user)
	if update.Reason(err) == update.Busy {
		return nil, &answerUpdateBusy, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return r, nil, nil
}

// storeBundle writes size bytes of the upload to a new file in the uploads directory, once three times that is free
// for the file and the load that follows, and returns its path. A failure to read the upload wraps errSource, a full
// volume storage.ErrNoSpace; nothing is kept then.
func (s *Server) storeBundle(size int64, body interface{ Read([]byte) (int, error) }) (string, error) {
	dir := s.deps.UpdatesDir
	if err := os.MkdirAll(dir, restoreDirMode); err != nil {
		return "", fmt.Errorf("create the uploads directory: %w", err)
	}
	// checkSpace wants twice what it is given free: three times the size is twice one and a half times it.
	need := uint64(size) + uint64(size+1)/2 //nolint:gosec // checked to be positive by the caller
	if err := s.checkSpace(dir, need); err != nil {
		return "", err
	}
	name := make([]byte, 16)
	_, _ = rand.Read(name) // crypto/rand.Read never returns an error.
	path := filepath.Join(dir, hex.EncodeToString(name)+".tar")
	if err := writeFile(path, size, body); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	s.deps.Log.Info("bundle upload stored", "sizeBytes", size)
	return path, nil
}

// updates are the update bundles in the status and the latest load.
func (s *Server) updates(ctx context.Context) (Updates, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return Updates{}, err
	}
	out := Updates{Available: []UpdateBundle{}, Loaded: []UpdateBundle{}}
	if u := a.Status.Updates; u != nil {
		if u.LastCheck != nil {
			out.LastCheck = &u.LastCheck.Time
		}
		if u.LastCheckError != "" {
			e := UpdatesLastCheckError(u.LastCheckError)
			out.LastCheckError = &e
		}
		out.Available, out.Loaded = updateBundles(u.Available), updateBundles(u.Loaded)
	}
	if last := s.deps.Updates.Last(); last.State != "" {
		out.Load = &BundleLoad{
			State: BundleLoadState(last.State), Kind: BundleKind(last.Kind), StartedAt: last.StartedAt,
			Version: optional(last.Version), Reason: optional(last.Reason),
		}
	}
	return out, nil
}

func updateBundles(infos []v1alpha1.BundleInfo) []UpdateBundle {
	out := make([]UpdateBundle, 0, len(infos))
	for _, b := range infos {
		u := UpdateBundle{
			Kind: BundleKind(b.Kind), Version: b.Version, Arch: b.Arch, SizeBytes: b.SizeBytes,
			Ilm: optional(b.ILM), Operator: optional(b.Operator), MinAppliance: optional(b.MinAppliance),
			OperatorRange: optional(b.OperatorRange), Needs: optional(b.Needs),
			Notes: updatesPath + "/notes/" + url.PathEscape(b.Kind) + "/" + url.PathEscape(b.Version),
		}
		if b.LoadedAt != nil {
			u.LoadedAt = &b.LoadedAt.Time
		}
		if b.Blocked != "" {
			blocked := UpdateBundleBlocked(b.Blocked)
			u.Blocked = &blocked
		}
		out = append(out, u)
	}
	return out
}
