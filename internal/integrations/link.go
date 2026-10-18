package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Paths of the account links.
const (
	// LinkPath + {token}: GET shows the confirmation page, POST completes it.
	LinkPath = "/integrations/link/"
	// LinkStartPath + {provider}: the signed-in user starts a link from the
	// dashboard with the sign-in of the provider.
	LinkStartPath = "/integrations/link-start/"
	// LinkCallbackPath is the redirect URI of the sign-in of a provider.
	LinkCallbackPath = "/integrations/link-callback"
	// LinksPath + {provider}/{external_id}: DELETE removes a link (admins).
	LinksPath = "/integrations/links/"
)

// Audit actions of account links.
const (
	AuditLink   = "account.link"
	AuditUnlink = "account.unlink"
)

// ErrLinkedToOther means that the external account is linked to another
// user. A link is never replaced; an admin must remove the old link first.
var ErrLinkedToOther = errors.New("the external account is linked to another user")

// AuditRecord is one audit event of an account link or removal.
type AuditRecord struct {
	Action     string // AuditLink or AuditUnlink
	Outcome    string // "success" or "failure"
	ActorID    string // who did it; empty for the server
	UserID     string // the Open Factory user of the link
	TeamID     string
	Provider   string
	ExternalID string
	// Method is how the link was made: "provider_sign_in", "private_message",
	// "email" or "admin" (a removal).
	Method string
	Reason string
}

// Prover proves that the signed-in user controls an external account: the
// OAuth or OpenID Connect sign-in of the provider.
type Prover interface {
	// AuthURL is the authorization URL of the provider.
	AuthURL(state, redirectURI string) string
	// Exchange trades the code for the account ID (and a display handle)
	// that the provider returns.
	Exchange(ctx context.Context, code, redirectURI string) (externalID, handle string, err error)
}

// Linker issues signed account-link URLs and completes a link for the
// signed-in Open Factory user. A link needs a proof from the provider: the
// sign-in of the provider, or, where the provider has no sign-in configured,
// a URL that was sent in a private message to the external account.
type Linker struct {
	Store core.Store
	// Key signs link tokens, sign-in states and CSRF tokens.
	Key []byte
	// PublicURL is OF_PUBLIC_URL.
	PublicURL string
	// CurrentUser returns the signed-in user for a dashboard request (nil/error = not signed in).
	CurrentUser func(r *http.Request) (*core.User, error)
	// LoginURL builds a login redirect for an unauthenticated visitor (optional).
	LoginURL func(next string) string
	// Prover returns the sign-in of a provider for a team. Nil, or a nil
	// result, means that the provider has no sign-in.
	Prover func(ctx context.Context, teamID, provider string) (Prover, error)
	// Audit records each link and each removal. Optional.
	Audit func(ctx context.Context, rec AuditRecord)
	// TTL of link tokens (default 24h).
	TTL    time.Duration
	Logger *slog.Logger
	Now    func() time.Time
}

type linkClaims struct {
	TeamID     string `json:"t"`
	Provider   string `json:"p"`
	ExternalID string `json:"x"`
	Handle     string `json:"h,omitempty"`
	Expires    int64  `json:"e"`
	// Private is true for a URL that was sent in a private message to the
	// external account. Only such a URL completes a link without the
	// sign-in of the provider.
	Private bool `json:"pm,omitempty"`
}

// stateClaims is the state of a sign-in of a provider.
type stateClaims struct {
	Token    string `json:"k,omitempty"` // the link token, if the link started from one
	UserID   string `json:"u"`
	TeamID   string `json:"t"`
	Provider string `json:"p"`
	Expires  int64  `json:"e"`
	Nonce    string `json:"n"`
}

const stateTTL = 15 * time.Minute

func (l *Linker) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Linker) mac(kind, payload string) string {
	m := hmac.New(sha256.New, l.Key)
	m.Write([]byte(kind + ":" + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (l *Linker) seal(kind string, v any) string {
	b, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + l.mac(kind, payload)
}

func (l *Linker) open(kind, tok string, v any) error {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(l.mac(kind, payload))) {
		return errors.New("invalid link")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return errors.New("invalid link")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("invalid link")
	}
	return nil
}

func (l *Linker) configured() bool {
	return l != nil && len(l.Key) > 0 && l.PublicURL != ""
}

// PrivateURL returns a link URL for an external account, to send only in a
// private message to that account (a direct or an ephemeral message). It
// returns "" when the linker is not configured. Never post it where other
// people can read it: whoever opens the URL can link the account.
func (l *Linker) PrivateURL(teamID, provider, externalID, handle string) string {
	if !l.configured() {
		return ""
	}
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	tok := l.seal("link", linkClaims{TeamID: teamID, Provider: provider, ExternalID: externalID, Handle: handle, Expires: l.now().Add(ttl).Unix(), Private: true})
	return strings.TrimRight(l.PublicURL, "/") + LinkPath + tok
}

// DashboardHint is the text of a link prompt in a public place: it has no
// link URL and tells the person to start the link from the dashboard.
func (l *Linker) DashboardHint(provider string) string {
	where := "the Open Factory dashboard"
	if l != nil && l.PublicURL != "" {
		where = strings.TrimRight(l.PublicURL, "/") + "/integrations"
	}
	return fmt.Sprintf("Sign in to %s and link your %s account under \"Link your account\".", where, ProviderLabel(provider))
}

func (l *Linker) parse(tok string) (*linkClaims, error) {
	var c linkClaims
	if err := l.open("link", tok, &c); err != nil {
		return nil, err
	}
	if l.now().Unix() > c.Expires {
		return nil, errors.New("this link has expired; request a new one")
	}
	return &c, nil
}

func (l *Linker) csrf(userID, tok string) string { return l.mac("csrf", userID+":"+tok) }

// Register mounts the account-link endpoints.
func (l *Linker) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LinkPath+"{token}", l.serveConfirm)
	mux.HandleFunc("POST "+LinkPath+"{token}", l.serveComplete)
	mux.HandleFunc("GET "+LinkStartPath+"{provider}", l.serveStart)
	mux.HandleFunc("GET "+LinkCallbackPath, l.serveCallback)
	mux.HandleFunc("DELETE "+LinksPath+"{provider}/{external_id}", l.serveRemove)
}

var linkPage = template.Must(template.New("link").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Open Factory</title>
<meta name="referrer" content="no-referrer">
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem}button{font:inherit;padding:.5rem 1rem}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Message}}</p>
{{if .Action}}<form method="post" action="{{.Action}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">{{.Button}}</button></form>{{end}}
</body></html>`))

type pageData struct {
	Title, Message       string
	Action, CSRF, Button string
}

func (l *Linker) render(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(status)
	_ = linkPage.Execute(w, d)
}

func (l *Linker) page(w http.ResponseWriter, status int, title, msg string) {
	l.render(w, status, pageData{Title: title, Message: msg})
}

// signedIn returns the signed-in user, or writes a login redirect or page.
func (l *Linker) signedIn(w http.ResponseWriter, r *http.Request) *core.User {
	var user *core.User
	var err error
	if l.CurrentUser != nil {
		user, err = l.CurrentUser(r)
	}
	if user != nil && err == nil {
		return user
	}
	if l.LoginURL != nil && r.Method == http.MethodGet {
		http.Redirect(w, r, l.LoginURL(r.URL.RequestURI()), http.StatusFound)
		return nil
	}
	l.page(w, http.StatusUnauthorized, "Sign in required", "Sign in to Open Factory, then open this link again.")
	return nil
}

// linkRequest checks the token and the signed-in user of a link request.
func (l *Linker) linkRequest(w http.ResponseWriter, r *http.Request) (*linkClaims, *core.User, string) {
	tok := r.PathValue("token")
	claims, err := l.parse(tok)
	if err != nil {
		l.page(w, http.StatusBadRequest, "Link failed", err.Error())
		return nil, nil, ""
	}
	user := l.signedIn(w, r)
	if user == nil {
		return nil, nil, ""
	}
	if user.TeamID != claims.TeamID {
		l.page(w, http.StatusForbidden, "Wrong team", "Your Open Factory account is not a member of the team this request belongs to.")
		return nil, nil, ""
	}
	return claims, user, tok
}

func (l *Linker) prover(ctx context.Context, teamID, provider string) Prover {
	if l.Prover == nil {
		return nil
	}
	p, err := l.Prover(ctx, teamID, provider)
	if err != nil {
		forgeutil.Logger(l.Logger).Warn("account link: provider sign-in", "provider", provider, "err", err)
		return nil
	}
	return p
}

// serveConfirm shows the confirmation page. A GET request never links.
func (l *Linker) serveConfirm(w http.ResponseWriter, r *http.Request) {
	claims, user, tok := l.linkRequest(w, r)
	if claims == nil {
		return
	}
	if msg, status := l.ownerConflict(r.Context(), user, claims.Provider, claims.ExternalID); status != 0 {
		l.page(w, status, "Link not possible", msg)
		return
	}
	label := ProviderLabel(claims.Provider)
	d := pageData{
		Title:  "Link your " + label + " account",
		Action: LinkPath + tok,
		CSRF:   l.csrf(user.ID, tok),
	}
	who := claims.Handle
	if who == "" {
		who = claims.ExternalID
	}
	switch {
	case l.prover(r.Context(), claims.TeamID, claims.Provider) != nil:
		d.Message = fmt.Sprintf("Link the %s account %s to %s? You sign in to %s next, to show that you control the account.", label, who, user.Email, label)
		d.Button = "Continue with " + label
	case claims.Private:
		d.Message = fmt.Sprintf("Link the %s account %s to %s? Continue only if you asked for this link in %s.", label, who, user.Email, label)
		d.Button = "Link account"
	default:
		l.page(w, http.StatusForbidden, "Link not possible", fmt.Sprintf("This link cannot be proved: %s sign-in is not configured for your team. Ask a team admin.", label))
		return
	}
	l.render(w, http.StatusOK, d)
}

// serveComplete completes a link after the confirmation (POST with a CSRF
// token): through the sign-in of the provider, or directly for a private URL
// of a provider without a sign-in.
func (l *Linker) serveComplete(w http.ResponseWriter, r *http.Request) {
	claims, user, tok := l.linkRequest(w, r)
	if claims == nil {
		return
	}
	if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(l.csrf(user.ID, tok))) {
		l.page(w, http.StatusForbidden, "Link failed", "The request is not valid. Open the link again.")
		return
	}
	if p := l.prover(r.Context(), claims.TeamID, claims.Provider); p != nil {
		l.redirectToProvider(w, r, p, stateClaims{Token: tok, UserID: user.ID, TeamID: claims.TeamID, Provider: claims.Provider})
		return
	}
	if !claims.Private {
		l.page(w, http.StatusForbidden, "Link not possible", "This link cannot be proved. Ask a team admin.")
		return
	}
	l.finish(w, r, user, claims.Provider, claims.ExternalID, claims.Handle, "private_message")
}

// serveStart starts a link from the dashboard with the sign-in of a provider.
func (l *Linker) serveStart(w http.ResponseWriter, r *http.Request) {
	user := l.signedIn(w, r)
	if user == nil {
		return
	}
	provider := r.PathValue("provider")
	p := l.prover(r.Context(), user.TeamID, provider)
	if p == nil {
		l.page(w, http.StatusNotFound, "Link not possible", fmt.Sprintf("%s sign-in is not configured for your team. Ask a team admin.", ProviderLabel(provider)))
		return
	}
	l.redirectToProvider(w, r, p, stateClaims{UserID: user.ID, TeamID: user.TeamID, Provider: provider})
}

func (l *Linker) redirectURI() string {
	return strings.TrimRight(l.PublicURL, "/") + LinkCallbackPath
}

func (l *Linker) redirectToProvider(w http.ResponseWriter, r *http.Request, p Prover, st stateClaims) {
	var n [16]byte
	_, _ = rand.Read(n[:])
	st.Nonce = base64.RawURLEncoding.EncodeToString(n[:])
	st.Expires = l.now().Add(stateTTL).Unix()
	http.Redirect(w, r, p.AuthURL(l.seal("state", st), l.redirectURI()), http.StatusSeeOther) //nolint:gosec // G710: the sign-in URL of the provider that a team admin configured
}

// serveCallback completes a link after the sign-in of the provider. The
// account ID that the provider returns must be the account ID of the link
// token, if the link started from one.
func (l *Linker) serveCallback(w http.ResponseWriter, r *http.Request) {
	var st stateClaims
	if err := l.open("state", r.URL.Query().Get("state"), &st); err != nil || l.now().Unix() > st.Expires {
		l.page(w, http.StatusBadRequest, "Link failed", "The sign-in is not valid or has expired. Start the link again.")
		return
	}
	user := l.signedIn(w, r)
	if user == nil {
		return
	}
	if user.ID != st.UserID || user.TeamID != st.TeamID {
		l.page(w, http.StatusForbidden, "Link failed", "The sign-in was started by another Open Factory account.")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		l.page(w, http.StatusBadRequest, "Link failed", "The sign-in was not completed.")
		return
	}
	p := l.prover(r.Context(), st.TeamID, st.Provider)
	if p == nil {
		l.page(w, http.StatusBadRequest, "Link failed", "The sign-in of this provider is no longer configured.")
		return
	}
	id, handle, err := p.Exchange(r.Context(), r.URL.Query().Get("code"), l.redirectURI())
	if err != nil || id == "" {
		forgeutil.Logger(l.Logger).Warn("account link: sign-in exchange", "provider", st.Provider, "err", err)
		l.page(w, http.StatusBadGateway, "Link failed", "The sign-in could not be checked. Try again later.")
		return
	}
	if st.Token != "" {
		claims, err := l.parse(st.Token)
		if err != nil {
			l.page(w, http.StatusBadRequest, "Link failed", err.Error())
			return
		}
		if claims.ExternalID != id || claims.Provider != st.Provider {
			l.audit(r.Context(), AuditRecord{Action: AuditLink, Outcome: "failure", ActorID: user.ID, UserID: user.ID, TeamID: user.TeamID,
				Provider: st.Provider, ExternalID: claims.ExternalID, Method: "provider_sign_in", Reason: "account mismatch"})
			l.page(w, http.StatusForbidden, "Link failed", fmt.Sprintf("You signed in to %s with a different account than the one in the link.", ProviderLabel(st.Provider)))
			return
		}
		if handle == "" {
			handle = claims.Handle
		}
	}
	l.finish(w, r, user, st.Provider, id, handle, "provider_sign_in")
}

// finish writes the link and the page.
func (l *Linker) finish(w http.ResponseWriter, r *http.Request, user *core.User, provider, externalID, handle, method string) {
	err := l.link(r.Context(), user, provider, externalID, handle, method)
	switch {
	case errors.Is(err, ErrLinkedToOther):
		msg, status := l.ownerConflict(r.Context(), user, provider, externalID)
		if status == 0 {
			msg, status = "The account is linked to another user.", http.StatusConflict
		}
		l.page(w, status, "Link not possible", msg)
		return
	case err != nil:
		forgeutil.Logger(l.Logger).Error("link account", "provider", provider, "err", err)
		l.page(w, http.StatusInternalServerError, "Link failed", "The account could not be linked. Try again later.")
		return
	}
	who := handle
	if who == "" {
		who = externalID
	}
	l.page(w, http.StatusOK, "Account linked", fmt.Sprintf("Your %s account %s is now linked to %s. Send your request again.", ProviderLabel(provider), who, user.Email))
}

// ownerConflict returns a message and a status when the external account is
// linked to another user. It names the owner when the owner is in the team
// of the user. It returns status 0 when there is no conflict.
func (l *Linker) ownerConflict(ctx context.Context, user *core.User, provider, externalID string) (string, int) {
	x, err := l.Store.FindExternalIdentity(ctx, provider, externalID)
	if err != nil || x.UserID == "" || x.UserID == user.ID {
		return "", 0
	}
	owner := "another Open Factory account"
	if u, err := l.Store.GetUser(ctx, x.UserID); err == nil && u.TeamID == user.TeamID {
		owner = u.Email
	}
	return fmt.Sprintf("This %s account is already linked to %s. A link is never replaced: ask a team admin to remove the old link first.", ProviderLabel(provider), owner), http.StatusConflict
}

// link binds an external account to user. It fails with ErrLinkedToOther
// when the account is linked to another user, and writes an audit event.
func (l *Linker) link(ctx context.Context, user *core.User, provider, externalID, handle, method string) error {
	rec := AuditRecord{Action: AuditLink, Outcome: "success", ActorID: user.ID, UserID: user.ID, TeamID: user.TeamID, Provider: provider, ExternalID: externalID, Method: method}
	err := createLink(ctx, l.Store, &core.ExternalIdentity{UserID: user.ID, Provider: provider, ExternalID: externalID, Handle: handle})
	if err != nil {
		rec.Outcome, rec.Reason = "failure", err.Error()
	}
	l.audit(ctx, rec)
	return err
}

// createLink writes a link unless the external account is linked to another
// user. A link of the same user is kept as it is.
//
// With a store that has core.IdentityLinker (the memory, SQLite and PostgreSQL
// stores) the check and the write are one atomic step, so two links of one
// external account at the same moment give one owner. Another store gets a
// read and then a write, which can race.
func createLink(ctx context.Context, store core.Store, x *core.ExternalIdentity) error {
	if l, ok := store.(core.IdentityLinker); ok {
		_, linked, err := l.LinkExternalIdentity(ctx, x)
		if err != nil {
			return err
		}
		if !linked {
			return ErrLinkedToOther
		}
		return nil
	}
	cur, err := store.FindExternalIdentity(ctx, x.Provider, x.ExternalID)
	switch {
	case err == nil && cur.UserID != "" && cur.UserID != x.UserID:
		return ErrLinkedToOther
	case err == nil && cur.UserID == x.UserID && (x.Handle == "" || cur.Handle == x.Handle):
		return nil
	case err != nil && !errors.Is(err, core.ErrNotFound):
		return err
	}
	return store.UpsertExternalIdentity(ctx, x)
}

func (l *Linker) audit(ctx context.Context, rec AuditRecord) {
	if l == nil {
		return
	}
	if l.Audit != nil {
		l.Audit(ctx, rec)
		return
	}
	forgeutil.Logger(l.Logger).Info("audit", "action", rec.Action, "outcome", rec.Outcome, "actor_id", rec.ActorID, "user_id", rec.UserID,
		"team_id", rec.TeamID, "provider", rec.Provider, "external_id", rec.ExternalID, "method", rec.Method, "reason", rec.Reason)
}

// serveRemove removes a link. Only an admin of the team of the linked user
// can do it.
func (l *Linker) serveRemove(w http.ResponseWriter, r *http.Request) {
	user := l.signedIn(w, r)
	if user == nil {
		return
	}
	provider, externalID := r.PathValue("provider"), r.PathValue("external_id")
	x, err := l.Store.FindExternalIdentity(r.Context(), provider, externalID)
	if errors.Is(err, core.ErrNotFound) || (err == nil && x.UserID == "") {
		forgeutil.WriteError(w, http.StatusNotFound, "not_found", "no link for this account")
		return
	}
	if err != nil {
		forgeutil.WriteError(w, http.StatusInternalServerError, "internal", "look up the link")
		return
	}
	owner, err := l.Store.GetUser(r.Context(), x.UserID)
	sameTeam := err == nil && owner.TeamID == user.TeamID
	if !sameTeam || !strings.EqualFold(string(user.Role), "admin") {
		forgeutil.WriteError(w, http.StatusForbidden, "not_authorized", "only an admin of the team can remove a link")
		return
	}
	if err := l.Unlink(r.Context(), l.Store, provider, externalID, user.ID); err != nil {
		forgeutil.WriteError(w, http.StatusInternalServerError, "internal", "remove the link")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Unlink removes the link of an external account (the identity is kept with
// no user) and writes an audit event. actorID is who removed it; empty means
// the account itself (for example an unlink command in the provider). A nil
// Linker writes no audit event.
func (l *Linker) Unlink(ctx context.Context, store core.Store, provider, externalID, actorID string) error {
	x, err := store.FindExternalIdentity(ctx, provider, externalID)
	if errors.Is(err, core.ErrNotFound) || (err == nil && x.UserID == "") {
		return nil
	}
	if err != nil {
		return err
	}
	rec := AuditRecord{Action: AuditUnlink, Outcome: "success", ActorID: actorID, UserID: x.UserID, Provider: provider, ExternalID: externalID, Method: "admin"}
	if actorID == "" {
		rec.Method = "self"
	}
	if u, err := store.GetUser(ctx, x.UserID); err == nil {
		rec.TeamID = u.TeamID
	}
	x.UserID = ""
	if err := store.UpsertExternalIdentity(ctx, x); err != nil {
		return err
	}
	l.audit(ctx, rec)
	return nil
}

// Unlink removes the link of an external account without an audit event.
// Prefer Linker.Unlink.
func Unlink(ctx context.Context, store core.Store, provider, externalID string) error {
	return (*Linker)(nil).Unlink(ctx, store, provider, externalID, "")
}

// EmailCandidate is an external account that may be linked by its email
// address. The provider handler fills it from its user directory.
type EmailCandidate struct {
	Provider, ExternalID, Handle, Email string
	// Guest is true for a guest, a restricted or a single-channel user.
	Guest bool
	// Foreign is true for a user of another workspace or organization.
	Foreign bool
	// Bot and Deleted accounts are never linked.
	Bot, Deleted bool
}

// LinkByEmail links an external account to the team member with the same
// email address. The caller checks the opt-in setting of the integration.
// It never links a guest, a user of another
// workspace, a bot or a deleted account, and it never replaces a link. It
// returns the member, or nil when no link was made.
func (l *Linker) LinkByEmail(ctx context.Context, store core.Store, teamID string, c EmailCandidate) (*core.User, error) {
	if store == nil || c.Email == "" || c.Guest || c.Foreign || c.Bot || c.Deleted {
		return nil, nil
	}
	u, err := store.GetUserByEmail(ctx, c.Email)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.TeamID != teamID {
		return nil, nil
	}
	rec := AuditRecord{Action: AuditLink, Outcome: "success", UserID: u.ID, TeamID: teamID, Provider: c.Provider, ExternalID: c.ExternalID, Method: "email"}
	err = createLink(ctx, store, &core.ExternalIdentity{UserID: u.ID, Provider: c.Provider, ExternalID: c.ExternalID, Handle: c.Handle})
	if err != nil {
		rec.Outcome, rec.Reason = "failure", err.Error()
		l.audit(ctx, rec)
		if errors.Is(err, ErrLinkedToOther) {
			return nil, nil
		}
		return nil, err
	}
	l.audit(ctx, rec)
	return u, nil
}

// LinkByEmail is Linker.LinkByEmail without an audit event and without the
// account checks of an EmailCandidate. Prefer Linker.LinkByEmail.
func LinkByEmail(ctx context.Context, store core.Store, teamID, provider, externalID, handle, email string) (*core.User, error) {
	return (*Linker)(nil).LinkByEmail(ctx, store, teamID, EmailCandidate{Provider: provider, ExternalID: externalID, Handle: handle, Email: email})
}
