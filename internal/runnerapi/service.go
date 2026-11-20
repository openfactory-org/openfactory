// Package runnerapi serves the control-plane side of the runner protocol
// (see package runnerproto) over HTTP.
package runnerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

const (
	maxBodyBytes = 8 << 20

	codeBadRequest   = "bad_request"
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeNotFound     = "not_found"
	codeSeqGap       = "seq_gap"
	codeUnsupported  = "unsupported_runner_version"
	codeTooLarge     = "too_large"
	codeNotImpl      = "not_implemented"
	codeInternal     = "internal"

	// sessionUpdateRetries is the number of retries after a version conflict.
	sessionUpdateRetries = 3
)

// Config holds what the service needs.
type Config struct {
	Store   core.Store         // GetRun
	Runner  core.RunnerStore   // sessions, events, commands, inputs
	Keys    runnertoken.KeySet // keys for the session tokens
	Session SessionConfigurer  // fills the start response
	Secrets SecretsProvider    // may be nil: then secrets returns an empty SecretsResponse
	Hooks   Hooks              // may be nil
	Now     func() time.Time   // nil means time.Now
	// Logger gets the log lines of the service. Nil means slog.Default().
	Logger *slog.Logger
	// Redact returns a function that removes the secret values of a run
	// from text. The service uses it for the configuration error that it
	// shows in the run status. Nil, or a nil result, keeps the text.
	Redact func(ctx context.Context, run *core.Run) func(string) string

	// GitCredentials answers the git credential requests. Nil means that the
	// route answers 501.
	GitCredentials GitCredentialProvider
	// CloudTokens makes the cloud identity tokens. Nil means that the route
	// answers 501.
	CloudTokens CloudTokenSource
	// Files presigns the uploads and downloads. Nil means that both routes
	// answer 501.
	Files FileStore
	// SnapshotFiles presigns the snapshot uploads and downloads. Nil means
	// that Files serves snapshots too.
	SnapshotFiles FileStore
	// SnapshotPutHeaders is PutHeaders for SnapshotFiles. Nil means
	// PutHeaders when SnapshotFiles is nil, else the Content-Type header only.
	SnapshotPutHeaders func(contentType string) map[string]string
	// SnapshotMaxBytes is the size limit of one snapshot part. 0 adds no
	// limit to the default limit of the upload kind.
	SnapshotMaxBytes int64
	// MaxUploadBytes sets the size limit per upload kind. A missing kind uses
	// the default limit.
	MaxUploadBytes map[runnerproto.UploadKind]int64
	// UploadTTL is the life of a presigned URL. 0 means 15 minutes.
	UploadTTL time.Duration
	// PutHeaders returns the headers for a presigned upload. Nil means the
	// Content-Type header only.
	PutHeaders func(contentType string) map[string]string

	// Attachments returns the metadata of an attachment of a message. Nil
	// means that the download kind "attachment" answers 501.
	Attachments func(ctx context.Context, id string) (core.Attachment, error)
	// AttachmentFiles presigns the downloads of the attachments (the object
	// key is core.Attachment.Key). Nil means that Files serves them.
	AttachmentFiles FileStore

	// The life of a session token is runnertoken.SessionTTL.

	// PollInterval is how often a long poll re-reads the store. 0 means 1 second.
	PollInterval time.Duration
	// MajorVersion is the supported major runner version. 0 means 1.
	MajorVersion int
}

// SessionConfigurer builds the part of the start response that is not the token.
type SessionConfigurer interface {
	StartConfig(ctx context.Context, run *core.Run, s *core.RunSession) (runnerproto.StartResponse, error)
}

// SecretsProvider returns the secrets of a session. It writes the audit event.
type SecretsProvider interface {
	Secrets(ctx context.Context, run *core.Run, s *core.RunSession) (runnerproto.SecretsResponse, error)
}

// Hooks lets the engine react. Each hook is called after the store write
// succeeded.
type Hooks interface {
	SessionStarted(ctx context.Context, s *core.RunSession)
	EventsStored(ctx context.Context, s *core.RunSession, events []core.RunnerEvent)
	SessionEnded(ctx context.Context, s *core.RunSession, req runnerproto.EndRequest)
}

type noHooks struct{}

func (noHooks) SessionStarted(context.Context, *core.RunSession)                       {}
func (noHooks) EventsStored(context.Context, *core.RunSession, []core.RunnerEvent)     {}
func (noHooks) SessionEnded(context.Context, *core.RunSession, runnerproto.EndRequest) {}

// Service handles the runner requests of the control plane.
type Service struct {
	store        core.Store
	runner       core.RunnerStore
	keys         runnertoken.KeySet
	session      SessionConfigurer
	secrets      SecretsProvider
	hooks        Hooks
	gitCreds     GitCredentialProvider
	cloudTokens  CloudTokenSource
	files        FileStore
	snapFiles    FileStore // nil means files
	attachments  func(ctx context.Context, id string) (core.Attachment, error)
	attachFiles  FileStore // nil means files
	snapHeaders  func(contentType string) map[string]string
	snapMax      int64
	maxUpload    map[runnerproto.UploadKind]int64
	uploadTTL    time.Duration
	putHeaders   func(contentType string) map[string]string
	now          func() time.Time
	pollInterval time.Duration
	major        string
	wake         *wakeRegistry
	log          *slog.Logger
	redact       func(ctx context.Context, run *core.Run) func(string) string
}

// New checks the config and makes a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("runnerapi: Store is nil")
	case cfg.Runner == nil:
		return nil, errors.New("runnerapi: Runner is nil")
	case cfg.Keys == nil:
		return nil, errors.New("runnerapi: Keys is nil")
	case cfg.Session == nil:
		return nil, errors.New("runnerapi: Session is nil")
	}
	s := &Service{
		store:        cfg.Store,
		runner:       cfg.Runner,
		keys:         cfg.Keys,
		session:      cfg.Session,
		secrets:      cfg.Secrets,
		hooks:        cfg.Hooks,
		gitCreds:     cfg.GitCredentials,
		cloudTokens:  cfg.CloudTokens,
		files:        cfg.Files,
		snapFiles:    cfg.SnapshotFiles,
		attachments:  cfg.Attachments,
		attachFiles:  cfg.AttachmentFiles,
		snapHeaders:  cfg.SnapshotPutHeaders,
		snapMax:      cfg.SnapshotMaxBytes,
		maxUpload:    cfg.MaxUploadBytes,
		uploadTTL:    cfg.UploadTTL,
		putHeaders:   cfg.PutHeaders,
		now:          cfg.Now,
		pollInterval: cfg.PollInterval,
		major:        "1",
		wake:         newWakeRegistry(),
		log:          cfg.Logger,
		redact:       cfg.Redact,
	}
	if s.snapFiles == nil {
		s.snapFiles, s.snapHeaders = s.files, s.putHeaders
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.hooks == nil {
		s.hooks = noHooks{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.pollInterval <= 0 {
		s.pollInterval = time.Second
	}
	if cfg.MajorVersion > 0 {
		s.major = strconv.Itoa(cfg.MajorVersion)
	}
	return s, nil
}

// Handler returns the HTTP handler for the paths under /runner/v1/session/.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runnerproto.PathSessionStart, s.handleStart)
	mux.HandleFunc("GET "+runnerproto.PathSessionSecrets, s.handleSecrets)
	mux.HandleFunc("POST "+runnerproto.PathSessionEvents, s.handleEvents)
	mux.HandleFunc("GET "+runnerproto.PathSessionCommands, s.handleCommands)
	mux.HandleFunc("POST "+runnerproto.PathSessionEnd, s.handleEnd)
	mux.HandleFunc("POST "+runnerproto.PathSessionToken, s.handleToken)
	mux.HandleFunc("POST "+runnerproto.PathGitCredential, s.handleGitCredential)
	mux.HandleFunc("POST "+runnerproto.PathCloudToken, s.handleCloudToken)
	mux.HandleFunc("GET "+runnerproto.PathConversation, s.handleConversation)
	mux.HandleFunc("POST "+runnerproto.PathSessionUploads, s.handleUpload)
	mux.HandleFunc("GET "+runnerproto.PathSessionDownloads, s.handleDownloads)
	mux.HandleFunc(runnerproto.PathPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, &apiError{status: http.StatusNotImplemented, code: codeNotImpl, msg: "not implemented"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.checkVersion(r); err != nil {
			s.writeError(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		mux.ServeHTTP(w, r)
	})
}

// apiError is an error with an HTTP answer.
type apiError struct {
	status int
	code   string
	msg    string
	ack    *int64
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.status, e.code, e.msg) }

func errBad(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: codeBadRequest, msg: fmt.Sprintf(format, args...)}
}

func errUnauthorized(msg string) *apiError {
	return &apiError{status: http.StatusUnauthorized, code: codeUnauthorized, msg: msg}
}

func errGap(ack int64) *apiError {
	return &apiError{status: http.StatusConflict, code: codeSeqGap, msg: "event sequence gap", ack: &ack}
}

// errInternal logs err and hides its text from the runner.
func (s *Service) errInternal(what string, err error) *apiError {
	s.log.Error("runnerapi: "+what, "err", err)
	return &apiError{status: http.StatusInternalServerError, code: codeInternal, msg: "internal error"}
}

func (s *Service) writeError(w http.ResponseWriter, e *apiError) {
	s.writeJSON(w, e.status, runnerproto.ErrorBody{Code: e.code, Message: e.msg, Ack: e.ack})
}

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("runnerapi: write response", "err", err)
	}
}

// decodeBody decodes the JSON body of r into v.
func decodeBody(r *http.Request, v any) *apiError {
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil {
		return nil
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return &apiError{status: http.StatusRequestEntityTooLarge, code: codeTooLarge, msg: "request body is too large"}
	}
	return errBad("invalid JSON body: %v", err)
}

// checkVersion refuses a major runner version that the service does not support.
func (s *Service) checkVersion(r *http.Request) *apiError {
	v := r.Header.Get(runnerproto.HeaderRunnerVersion)
	if v == "" {
		return nil
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	if major == s.major {
		return nil
	}
	// A development build has no version number, for example "dev".
	// It is accepted, because it is built from the same source.
	if _, err := strconv.Atoi(major); err != nil {
		return nil //nolint:nilerr // a version that is not a number is a development build, which is accepted
	}
	return &apiError{
		status: http.StatusUpgradeRequired,
		code:   codeUnsupported,
		msg:    fmt.Sprintf("runner version %q is not supported, need major version %s", v, s.major),
	}
}

// bearer returns the token of the Authorization header.
func bearer(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	return tok, ok && tok != ""
}

// authSession checks the session token of r. It returns the stored session.
func (s *Service) authSession(r *http.Request) (*core.RunSession, runnertoken.Claims, *apiError) {
	tok, ok := bearer(r)
	if !ok {
		return nil, runnertoken.Claims{}, errUnauthorized("missing session token")
	}
	now := s.now()
	claims, err := runnertoken.Verify(s.keys, tok, now)
	if err != nil {
		return nil, runnertoken.Claims{}, errUnauthorized("invalid session token")
	}
	sess, aerr := s.activeSession(r.Context(), claims.Run, claims.Session)
	if aerr != nil {
		return nil, runnertoken.Claims{}, aerr
	}
	if err := s.runner.TouchRunSession(r.Context(), sess.RunID, sess.Session, now); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, runnertoken.Claims{}, errUnauthorized("unknown session")
		}
		return nil, runnertoken.Claims{}, s.errInternal("touch session", err)
	}
	return sess, claims, nil
}

// activeSession loads a session and checks that it is not ended and not
// superseded by a newer session.
func (s *Service) activeSession(ctx context.Context, runID string, session int) (*core.RunSession, *apiError) {
	sess, err := s.runner.GetRunSession(ctx, runID, session)
	if errors.Is(err, core.ErrNotFound) {
		return nil, errUnauthorized("unknown session")
	}
	if err != nil {
		return nil, s.errInternal("get session", err)
	}
	if sess.State == core.SessionEnded {
		return nil, errUnauthorized("session ended")
	}
	latest, err := s.runner.LatestRunSession(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, errUnauthorized("unknown session")
		}
		return nil, s.errInternal("get latest session", err)
	}
	if latest.Session > session {
		return nil, &apiError{
			status: http.StatusConflict,
			code:   runnerproto.CodeSessionSuperseded,
			msg:    "a newer session exists",
		}
	}
	return sess, nil
}

// issueToken makes a session token for a session of a run. Its subject is
// the run_as principal of the run, or "runner" for a
// run from before that field.
func (s *Service) issueToken(run *core.Run, session int) (string, time.Time, *apiError) {
	sub := run.RunAs.String()
	if sub == "" {
		sub = "runner"
	}
	c := runnertoken.Claims{Version: runnertoken.SessionVersion, Team: run.TeamID, Run: run.ID, Session: session, Subject: sub}
	tok, exp, err := runnertoken.Mint(s.keys, c, s.now())
	if err != nil {
		return "", time.Time{}, s.errInternal("mint token", err)
	}
	return tok, exp, nil
}

// updateSession applies change to sess and stores it. On a version conflict
// it reads the session again and applies the change again. It leaves the
// stored session in sess.
func (s *Service) updateSession(ctx context.Context, sess *core.RunSession, change func(*core.RunSession)) error {
	for attempt := 0; ; attempt++ {
		change(sess)
		err := s.runner.UpdateRunSession(ctx, sess)
		if !errors.Is(err, core.ErrConflict) || attempt >= sessionUpdateRetries {
			return err
		}
		fresh, gerr := s.runner.GetRunSession(ctx, sess.RunID, sess.Session)
		if gerr != nil {
			return gerr
		}
		*sess = *fresh
	}
}

func (s *Service) handleSecrets(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if s.secrets == nil {
		s.writeJSON(w, http.StatusOK, runnerproto.SecretsResponse{})
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	resp, err := s.secrets.Secrets(r.Context(), run, sess)
	if err != nil {
		s.writeError(w, s.errInternal("secrets", err))
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	sess, claims, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil || run.TeamID != claims.Team {
		if err == nil || errors.Is(err, core.ErrNotFound) {
			s.writeError(w, errUnauthorized("unknown run"))
			return
		}
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	tok, exp, aerr := s.issueToken(run, sess.Session)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	s.writeJSON(w, http.StatusOK, runnerproto.TokenResponse{SessionToken: tok, SessionTokenExpiresAt: exp})
}
