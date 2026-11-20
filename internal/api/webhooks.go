package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

const maxWebhooksPerFactory = 20

var (
	webhookNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	urlSafeSecretRe  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	signatureSchemes = []string{"github", "pagerduty", "sentry", "standard_webhooks", "stripe", "vercel"}
	// Providers that issue their signing secret only after receiving the URL.
	lateSecretSchemes = []string{"vercel", "stripe", "pagerduty"}
)

func webhookFile(name string) string { return "webhooks/" + name + ".yaml" }

// managedFiles returns a copy of the active definition files of a managed factory.
func (s *Server) managedFiles(ctx context.Context, f *core.Factory) (map[string]string, error) {
	if f.Source != core.DefinitionManaged {
		return nil, Errf(CodeOperationNotSupported,
			"this factory's definition lives in %s: change webhooks/<name>.yaml there", f.GitRepo)
	}
	if f.ActiveVersionID == "" {
		return nil, Errf(CodeOperationNotSupported, "factory has no applied definition yet")
	}
	v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	return cloneFiles(v.Files), nil
}

func (s *Server) applyFiles(ctx context.Context, p *Principal, f *core.Factory, files map[string]string, msg string) error {
	v, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, files, p.ActorID(), msg, "")
	if err != nil {
		return applyError("definition could not be applied", v, err)
	}
	if v == nil || !v.Valid {
		return validationError("definition could not be applied", versionErrors(v))
	}
	return nil
}

// applyError turns the error of a refused apply into a validation error with
// the errors of the stored invalid version (compiler or preflight errors).
func applyError(detail string, v *core.DefinitionVersion, err error) error {
	if v != nil && !v.Valid && errors.Is(err, core.ErrInvalid) {
		return validationError(detail, v.Errors)
	}
	return err
}

func (s *Server) findWebhookByName(ctx context.Context, factoryID, name string) (*core.Webhook, error) {
	whs, err := s.opts.Store.ListWebhooks(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	for i := range whs {
		if whs[i].Name == name {
			return &whs[i], nil
		}
	}
	return nil, errNotFound("webhook " + name)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	whs, err := s.opts.Store.ListWebhooks(r.Context(), f.ID)
	if err != nil {
		return err
	}
	views := make([]WebhookView, 0, len(whs))
	for i := range whs {
		views = append(views, s.webhookView(&whs[i]))
	}
	writeJSON(w, http.StatusOK, listResponse("webhooks", views, ""))
	return nil
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request, p *Principal) error {
	wh, _, err := s.webhookFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.webhookView(wh))
	return nil
}

type createWebhookRequest struct {
	Name             string `json:"name"`
	AuthMode         string `json:"auth_mode,omitempty"`
	SignatureScheme  string `json:"signature_scheme,omitempty"`
	SecretName       string `json:"secret_name,omitempty"`
	Secret           string `json:"secret,omitempty"` // provider signing secret (signature mode)
	DeliveryIDHeader string `json:"delivery_id_header,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

// WebhookCreated is returned once on creation; Secret is never shown again.
type WebhookCreated struct {
	Webhook    WebhookView `json:"webhook"`
	IngressURL string      `json:"ingress_url"`
	Secret     string      `json:"secret,omitempty"`
	// AwaitingSecret is true when the provider issues its signing secret only
	// after it has the URL; send it with PATCH /webhooks/{id} {"secret": ...}.
	AwaitingSecret bool `json:"awaiting_secret,omitempty"`
	// SecretReused is true when the webhook uses an existing team secret
	// (webhook.secret_name) whose value is not shown.
	SecretReused bool `json:"secret_reused,omitempty"`
}

func defaultWebhookSecretName(name string) string {
	r := strings.NewReplacer("-", "_", ".", "_")
	return strings.ToUpper(r.Replace(name)) + "_WEBHOOK_SECRET"
}

func newWebhookSecret(scheme string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if scheme == "standard_webhooks" {
		return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	var req createWebhookRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if !webhookNameRe.MatchString(req.Name) {
		return errInvalid("name must be 1-63 lowercase letters, digits, '-' or '_' (it becomes webhooks/<name>.yaml)")
	}
	if req.AuthMode == "" {
		req.AuthMode = "token"
	}
	switch req.AuthMode {
	case "token", "url_token":
		if req.SignatureScheme != "" {
			return errInvalid("signature_scheme is only valid with auth_mode signature")
		}
		if req.Secret != "" {
			return errInvalid("secret is generated for %s webhooks; omit it", req.AuthMode)
		}
	case "signature":
		if !slices.Contains(signatureSchemes, req.SignatureScheme) {
			return errInvalid("signature_scheme must be one of %s", strings.Join(signatureSchemes, ", "))
		}
	default:
		return errInvalid("auth_mode must be token, url_token or signature")
	}
	files, err := s.managedFiles(ctx, f)
	if err != nil {
		return err
	}
	if _, exists := files[webhookFile(req.Name)]; exists {
		return Errf(CodeConflict, "webhook %q already exists", req.Name)
	}
	count := 0
	for path := range files {
		if strings.HasPrefix(path, "webhooks/") && strings.HasSuffix(path, ".yaml") {
			count++
		}
	}
	if count >= maxWebhooksPerFactory {
		return Errf(CodeConflict, "a factory can have at most %d webhooks", maxWebhooksPerFactory)
	}
	secretName := strings.TrimSpace(req.SecretName)
	if secretName == "" {
		secretName = defaultWebhookSecretName(req.Name)
	}
	if !secretNameRe.MatchString(secretName) {
		return errInvalid("invalid secret_name %q", secretName)
	}
	enabled := req.Enabled == nil || *req.Enabled

	// Resolve the credential: take the provider's, reuse a team secret, or
	// generate one.
	explicitName := strings.TrimSpace(req.SecretName) != ""
	old, getErr := s.opts.Secrets.Get(ctx, p.TeamID, "", secretName)
	existing := getErr == nil
	if getErr != nil && !isNotFound(getErr) {
		return getErr
	}
	oldType := core.SecretGeneric
	if existing {
		if oldType, err = s.teamSecretType(ctx, p.TeamID, secretName); err != nil {
			return err
		}
	}
	// generated is true when the server makes the credential.
	generated := req.AuthMode != "signature" || req.SignatureScheme == "standard_webhooks"
	reuse := false
	switch {
	case !existing || req.Secret != "":
	case !generated:
		// The secret holds the provider's signing secret.
		reuse = true
	default:
		// A secret that the caller named, or that another webhook of the team
		// uses, is shared: keep it and do not reveal it. A secret with the
		// default name that no webhook uses is left over (for example from an
		// older failed create): replace it so that the caller gets the value.
		used, err := s.webhookSecretInUse(ctx, p.TeamID, secretName)
		if err != nil {
			return err
		}
		reuse = explicitName || used
	}
	var store, reveal string
	awaiting := false
	switch {
	case req.Secret != "":
		store = req.Secret
	case reuse:
		if req.AuthMode == "url_token" && !urlSafeSecretRe.Match(old) {
			return errInvalid("a url_token secret accepts only letters, digits, '-', '.', '_' and '~'")
		}
	case req.AuthMode == "signature" && slices.Contains(lateSecretSchemes, req.SignatureScheme):
		// The provider issues its secret after it has the URL: store a placeholder
		// and keep the webhook disabled until the real secret arrives.
		ph, err := newWebhookSecret("")
		if err != nil {
			return err
		}
		store, enabled, awaiting = ph, false, true
	case req.AuthMode == "signature" && req.SignatureScheme != "standard_webhooks":
		return errInvalid("secret (the provider's signing secret) is required for the %s scheme", req.SignatureScheme)
	default:
		sec, err := newWebhookSecret(req.SignatureScheme)
		if err != nil {
			return err
		}
		store, reveal = sec, sec
	}

	doc := yamlMap()
	yamlSet(doc, "authMode", req.AuthMode)
	if req.SignatureScheme != "" {
		yamlSet(doc, "signatureScheme", req.SignatureScheme)
	}
	yamlSet(doc, "secretName", secretName)
	if req.DeliveryIDHeader != "" {
		yamlSet(doc, "deliveryIdHeader", req.DeliveryIDHeader)
	}
	yamlSet(doc, "enabled", enabled)
	files[webhookFile(req.Name)] = encodeYAMLNode(doc)
	// The apply checks the secret, so write it first and undo the write when
	// the apply fails. Otherwise a retry finds a secret that nobody has seen.
	var written *core.Secret
	if store != "" {
		written, err = s.opts.Secrets.Put(ctx, p.TeamID, "", secretName, core.SecretGeneric, []byte(store))
		if err != nil {
			return err
		}
	}
	if err := s.applyFiles(ctx, p, f, files, "Add webhook "+req.Name); err != nil {
		if store != "" {
			s.restoreSecret(ctx, p.TeamID, secretName, written, existing, oldType, old)
		}
		return err
	}
	wh, err := s.findWebhookByName(ctx, f.ID, req.Name)
	if err != nil {
		return err
	}
	resp := WebhookCreated{
		Webhook: s.webhookView(wh), IngressURL: s.WebhookIngressURL(wh.ID),
		Secret: reveal, AwaitingSecret: awaiting, SecretReused: reuse,
	}
	if req.AuthMode == "url_token" && reveal != "" {
		resp.IngressURL += "/" + reveal
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

// webhookSecretInUse reports whether a webhook of a factory of the team uses
// the secret.
func (s *Server) webhookSecretInUse(ctx context.Context, teamID, name string) (bool, error) {
	fs, err := s.opts.Store.ListFactories(ctx, teamID, "")
	if err != nil {
		return false, err
	}
	for i := range fs {
		whs, err := s.opts.Store.ListWebhooks(ctx, fs[i].ID)
		if err != nil {
			return false, err
		}
		for j := range whs {
			if whs[j].SecretName == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// teamSecretType returns the type of a team secret.
func (s *Server) teamSecretType(ctx context.Context, teamID, name string) (core.SecretType, error) {
	secs, err := s.opts.Secrets.List(ctx, teamID, "")
	if err != nil {
		return "", err
	}
	for i := range secs {
		if secs[i].Name == name && secs[i].FactoryID == "" {
			return secs[i].Type, nil
		}
	}
	return core.SecretGeneric, nil
}

// restoreSecret undoes a secret write after a failed apply: it puts back the
// previous value and type, or deletes a secret that did not exist before.
func (s *Server) restoreSecret(ctx context.Context, teamID, name string, written *core.Secret, existed bool, typ core.SecretType, old []byte) {
	var err error
	switch {
	case existed:
		_, err = s.opts.Secrets.Put(ctx, teamID, "", name, typ, old)
	case written != nil:
		err = s.opts.Secrets.Delete(ctx, written.ID)
	}
	if err != nil {
		s.log.WarnContext(ctx, "restore webhook secret after a failed apply", "secret", name, "err", err)
	}
}

func (s *Server) putSecret(ctx context.Context, teamID, name, value string) error {
	_, err := s.opts.Secrets.Put(ctx, teamID, "", name, core.SecretGeneric, []byte(value))
	return err
}

type updateWebhookRequest struct {
	Name             *string `json:"name"`
	AuthMode         *string `json:"auth_mode"`
	SignatureScheme  *string `json:"signature_scheme"`
	Enabled          *bool   `json:"enabled"`
	DeliveryIDHeader *string `json:"delivery_id_header"`
	Secret           *string `json:"secret"`
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	wh, f, err := s.webhookFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req updateWebhookRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.Name != nil && *req.Name != wh.Name {
		return Errf(CodeOperationNotSupported, "webhooks cannot be renamed; create a new webhook instead")
	}
	if (req.AuthMode != nil && *req.AuthMode != wh.AuthMode) || (req.SignatureScheme != nil && *req.SignatureScheme != wh.SignatureScheme) {
		return Errf(CodeOperationNotSupported, "auth_mode and signature_scheme cannot change; delete and recreate the webhook")
	}
	files, err := s.managedFiles(ctx, f)
	if err != nil {
		return err
	}
	path := webhookFile(wh.Name)
	doc, ok := files[path]
	if !ok {
		return errNotFound("webhook file " + path)
	}
	edits := map[string]any{}
	if req.Secret != nil {
		if s.opts.Secrets == nil {
			return featureUnavailable("secret storage")
		}
		if wh.AuthMode != "signature" {
			return errInvalid("secret can only be supplied for signature webhooks; use rotate-secret instead")
		}
		if strings.TrimSpace(*req.Secret) == "" {
			return errInvalid("secret cannot be empty")
		}
		if err := s.putSecret(ctx, p.TeamID, wh.SecretName, *req.Secret); err != nil {
			return err
		}
		if req.Enabled == nil {
			edits["enabled"] = true
		}
	}
	if req.Enabled != nil {
		edits["enabled"] = *req.Enabled
	}
	if req.DeliveryIDHeader != nil {
		if *req.DeliveryIDHeader == "" {
			edits["deliveryIdHeader"] = nil
		} else {
			edits["deliveryIdHeader"] = *req.DeliveryIDHeader
		}
	}
	nd, err := editYAMLDocument(doc, edits)
	if err != nil {
		return err
	}
	files[path] = nd
	// Re-applying also makes the webhook pick up a new secret value.
	if err := s.applyFiles(ctx, p, f, files, "Update webhook "+wh.Name); err != nil {
		return err
	}
	nwh, err := s.findWebhookByName(ctx, f.ID, wh.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.webhookView(nwh))
	return nil
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	wh, f, err := s.webhookFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	files, err := s.managedFiles(ctx, f)
	if err != nil {
		return err
	}
	delete(files, webhookFile(wh.Name))
	if err := s.applyFiles(ctx, p, f, files, "Delete webhook "+wh.Name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	wh, f, err := s.webhookFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.opts.Secrets == nil {
		return featureUnavailable("secret storage")
	}
	var req struct {
		Secret string `json:"secret"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	files, err := s.managedFiles(ctx, f)
	if err != nil {
		return err
	}
	var reveal string
	switch {
	case req.Secret != "":
		if wh.AuthMode != "signature" {
			return errInvalid("secret can only be supplied for signature webhooks")
		}
		if err := s.putSecret(ctx, p.TeamID, wh.SecretName, req.Secret); err != nil {
			return err
		}
	case wh.AuthMode == "signature" && wh.SignatureScheme != "standard_webhooks":
		return errInvalid("supply the provider's new signing secret in \"secret\"")
	default:
		sec, err := newWebhookSecret(wh.SignatureScheme)
		if err != nil {
			return err
		}
		if err := s.putSecret(ctx, p.TeamID, wh.SecretName, sec); err != nil {
			return err
		}
		reveal = sec
	}
	if err := s.applyFiles(ctx, p, f, files, "Rotate webhook secret "+wh.Name); err != nil {
		return err
	}
	resp := map[string]any{"webhook": s.webhookView(wh), "ingress_url": s.WebhookIngressURL(wh.ID)}
	if reveal != "" {
		resp["secret"] = reveal
		if wh.AuthMode == "url_token" {
			resp["ingress_url"] = s.WebhookIngressURL(wh.ID) + "/" + reveal
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// ---------- deliveries ----------

// DeliveryView is a delivery with a human-readable label.
type DeliveryView struct {
	core.WebhookDelivery
	Label string `json:"label"`
	Size  int    `json:"size"`
}

func deliveryView(d core.WebhookDelivery, withBody bool) DeliveryView {
	v := DeliveryView{WebhookDelivery: d, Size: len(d.Body)}
	switch d.Status {
	case "accepted":
		v.Label = "Accepted"
	case "deduplicated", "duplicate":
		v.Label = "Duplicate"
	case "rejected":
		switch {
		case d.Reason == "rate_limited":
			v.Label = "Rejected (rate limited)"
		case d.Reason == "secret_unavailable":
			v.Label = "Rejected (secret unavailable)"
		case strings.Contains(d.Reason, "json"):
			v.Label = "Rejected (invalid JSON)"
		case strings.Contains(d.Reason, "large") || strings.Contains(d.Reason, "size"):
			v.Label = "Rejected (too large)"
		default:
			v.Label = "Rejected (auth)"
		}
	default:
		v.Label = d.Status
	}
	if !withBody {
		v.Body = nil
	}
	return v
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request, p *Principal) error {
	wh, _, err := s.webhookFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", 50)
	if err != nil {
		return err
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	ds, err := s.opts.Store.ListWebhookDeliveries(r.Context(), wh.ID, limit)
	if err != nil {
		return err
	}
	views := make([]DeliveryView, 0, len(ds))
	for _, d := range ds {
		views = append(views, deliveryView(d, false))
	}
	writeJSON(w, http.StatusOK, listResponse("deliveries", views, ""))
	return nil
}

func (s *Server) handleGetDelivery(w http.ResponseWriter, r *http.Request, p *Principal) error {
	wh, _, err := s.webhookFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	d, err := s.opts.Store.GetWebhookDelivery(r.Context(), r.PathValue("deliveryId"))
	if err != nil || d.WebhookID != wh.ID {
		if err == nil || isNotFound(err) {
			return errNotFound("delivery " + r.PathValue("deliveryId"))
		}
		return err
	}
	writeJSON(w, http.StatusOK, deliveryView(*d, true))
	return nil
}

type testFilterRequest struct {
	// Filter is a webhook trigger filter ({"webhook_ids": [...], "payload": {...}}).
	Filter json.RawMessage `json:"filter,omitempty"`
	// Pattern is a bare payload pattern (alternative to Filter.payload).
	Pattern    json.RawMessage `json:"pattern,omitempty"`
	DeliveryID string          `json:"delivery_id,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

// CheckResult is one check of a filter test.
type CheckResult struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// TestFilterResult is the response of POST /webhooks/{id}/test-filter.
type TestFilterResult struct {
	WouldTrigger bool        `json:"would_trigger"`
	Result       string      `json:"result"` // "Would trigger" | "Would not trigger"
	WebhookCheck CheckResult `json:"webhook_check"`
	PayloadCheck CheckResult `json:"payload_check"`
	DeliveryID   string      `json:"delivery_id,omitempty"`
}

func (s *Server) handleTestFilter(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	wh, _, err := s.webhookFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.opts.TestFilter == nil {
		return featureUnavailable("payload filter evaluation")
	}
	var req testFilterRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	pattern := bytes.TrimSpace(req.Pattern)
	var webhookIDs []string
	hasIDs := false
	if f := bytes.TrimSpace(req.Filter); len(f) > 0 && string(f) != "null" {
		var flt struct {
			WebhookIDs json.RawMessage `json:"webhook_ids"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(f, &flt); err != nil {
			return errInvalid("filter must be an object: %v", err)
		}
		if len(flt.WebhookIDs) > 0 && string(flt.WebhookIDs) != "null" {
			hasIDs = true
			ids, err := parseWebhookIDs(flt.WebhookIDs)
			if err != nil {
				return err
			}
			webhookIDs = ids
		}
		if len(pattern) == 0 {
			pattern = bytes.TrimSpace(flt.Payload)
		}
	}
	res := TestFilterResult{}
	switch {
	case !hasIDs:
		res.WebhookCheck = CheckResult{Passed: true, Detail: "no webhook_ids constraint"}
	case slices.Contains(webhookIDs, wh.ID):
		res.WebhookCheck = CheckResult{Passed: true, Detail: "webhook " + wh.ID + " is selected"}
	default:
		res.WebhookCheck = CheckResult{Passed: false, Detail: "webhook_ids does not include " + wh.ID}
	}

	body := bytes.TrimSpace(req.Body)
	if len(body) == 0 {
		var d *core.WebhookDelivery
		if req.DeliveryID != "" {
			d, err = s.opts.Store.GetWebhookDelivery(ctx, req.DeliveryID)
			if err != nil || d.WebhookID != wh.ID {
				if err == nil || isNotFound(err) {
					return errNotFound("delivery " + req.DeliveryID)
				}
				return err
			}
		} else {
			ds, err := s.opts.Store.ListWebhookDeliveries(ctx, wh.ID, 50)
			if err != nil {
				return err
			}
			for i := range ds {
				if ds[i].Status == "accepted" && len(ds[i].Body) > 0 {
					d = &ds[i]
					break
				}
			}
			if d == nil {
				return errInvalid("no stored accepted delivery to test against; send a test delivery or pass body")
			}
		}
		if len(d.Body) == 0 {
			return errInvalid("delivery %s has no stored payload (only accepted deliveries keep their body)", d.ID)
		}
		body = d.Body
		res.DeliveryID = d.ID
	}
	if !json.Valid(body) {
		return errInvalid("body must be valid JSON")
	}
	if len(pattern) == 0 || string(pattern) == "null" {
		res.PayloadCheck = CheckResult{Passed: true, Detail: "no payload filter: every delivery matches"}
	} else {
		ok, err := s.opts.TestFilter(json.RawMessage(pattern), json.RawMessage(body))
		if err != nil {
			return errInvalid("invalid payload filter: %v", err)
		}
		if ok {
			res.PayloadCheck = CheckResult{Passed: true, Detail: "payload matches the filter"}
		} else {
			res.PayloadCheck = CheckResult{Passed: false, Detail: "payload does not match the filter"}
		}
	}
	res.WouldTrigger = res.WebhookCheck.Passed && res.PayloadCheck.Passed && wh.Enabled
	if !wh.Enabled {
		res.WebhookCheck.Detail += " (webhook is disabled)"
	}
	res.Result = "Would not trigger"
	if res.WouldTrigger {
		res.Result = "Would trigger"
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// parseWebhookIDs accepts ["id"] or {"in": ["id"]}.
func parseWebhookIDs(raw json.RawMessage) ([]string, error) {
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		return ids, nil
	}
	var op struct {
		In []string `json:"in"`
	}
	if err := json.Unmarshal(raw, &op); err == nil && op.In != nil {
		return op.In, nil
	}
	return nil, errInvalid("filter.webhook_ids must be a list of webhook UIDs")
}
