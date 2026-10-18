// Package cloudid issues short-lived OpenID Connect tokens for agent runs and
// turns a factory's cloudProviders settings into cloud credentials, so runs can
// federate into AWS (AssumeRoleWithWebIdentity) and Google Cloud (workload
// identity federation) without long-lived keys.
//
// The control plane is the OIDC issuer: its issuer URL is OF_PUBLIC_URL, and it
// serves the discovery document at /.well-known/openid-configuration and the
// signing keys at /.well-known/jwks.json. The ES256 signing key is derived
// deterministically from the signing key (OF_SIGNING_KEY, or OF_SECRET_KEY
// when that is not set), so every replica (and every restart) signs with the
// same key and publishes the same JWKS. During a key rotation the JWKS also
// lists the public keys of the previous signing keys.
package cloudid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// KeyLabel is the derivation label of the signing key (see config.DeriveKey).
const KeyLabel = "openfactory/oidc-signing-key"

// Well-known paths served by Issuer.Register.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	JWKSPath      = "/.well-known/jwks.json"
)

// Audiences and defaults.
const (
	AWSAudience = "sts.amazonaws.com"
	// DefaultTTL is the lifetime of a run token. The SDKs ask the runner for
	// a new token.
	DefaultTTL = time.Hour
	// MaxTTL is the longest life of a run token.
	MaxTTL = time.Hour
	// RefreshInterval is how often a token file is written again during a
	// long turn. It is well below DefaultTTL, so a file never holds an
	// expired token.
	RefreshInterval = 20 * time.Minute
	// Dir is where credential files are written, relative to the sandbox
	// working directory.
	Dir = ".openfactory/cloud"
)

var b64 = base64.RawURLEncoding

// Issuer signs run tokens.
type Issuer struct {
	issuer string
	key    *ecdsa.PrivateKey
	kid    string
	// previous are the keys of earlier seeds. They never sign. The JWKS lists
	// them and Verify accepts them.
	previous []*ecdsa.PrivateKey
	now      func() time.Time
}

// NewIssuer derives the signing key from seed (a sub-key of the signing key)
// and uses issuerURL (OF_PUBLIC_URL) as the iss claim. Each previous seed adds
// a public key to the JWKS. Tokens are always signed with the key of seed.
func NewIssuer(seed []byte, issuerURL string, previousSeeds ...[]byte) (*Issuer, error) {
	issuerURL = strings.TrimRight(strings.TrimSpace(issuerURL), "/")
	if issuerURL == "" {
		return nil, errors.New("cloudid: issuer URL (OF_PUBLIC_URL) is required")
	}
	if len(seed) == 0 {
		return nil, errors.New("cloudid: signing seed is required")
	}
	key, err := deriveKey(seed)
	if err != nil {
		return nil, err
	}
	i := &Issuer{issuer: issuerURL, key: key, kid: keyID(&key.PublicKey), now: time.Now}
	seen := map[string]bool{i.kid: true}
	for _, ps := range previousSeeds {
		if len(ps) == 0 {
			return nil, errors.New("cloudid: previous signing seed is empty")
		}
		pk, err := deriveKey(ps)
		if err != nil {
			return nil, err
		}
		if id := keyID(&pk.PublicKey); !seen[id] {
			seen[id] = true
			i.previous = append(i.previous, pk)
		}
	}
	return i, nil
}

func keyID(pub *ecdsa.PublicKey) string {
	b, _ := pub.Bytes()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// deriveKey maps seed to a valid P-256 scalar (HMAC-SHA256 counter mode;
// retried in the negligible case the candidate is out of range).
func deriveKey(seed []byte) (*ecdsa.PrivateKey, error) {
	for i := uint32(0); i < 64; i++ {
		m := hmac.New(sha256.New, seed)
		m.Write([]byte(KeyLabel))
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], i)
		m.Write(c[:])
		if k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), m.Sum(nil)); err == nil {
			return k, nil
		}
	}
	return nil, errors.New("cloudid: could not derive a signing key")
}

// SetClock overrides the clock (tests).
func (i *Issuer) SetClock(now func() time.Time) { i.now = now }

// IssuerURL is the iss claim.
func (i *Issuer) IssuerURL() string { return i.issuer }

// KeyID is the kid of the signing key.
func (i *Issuer) KeyID() string { return i.kid }

// PublicKey returns the verification key.
func (i *Issuer) PublicKey() *ecdsa.PublicKey { return &i.key.PublicKey }

// Claims are the claims of a run token.
type Claims struct {
	Issuer     string `json:"iss"`
	Subject    string `json:"sub"`
	Audience   string `json:"aud"`
	IssuedAt   int64  `json:"iat"`
	NotBefore  int64  `json:"nbf"`
	Expiry     int64  `json:"exp"`
	JTI        string `json:"jti"`
	TeamID     string `json:"team_id,omitempty"`
	FactoryID  string `json:"factory_id,omitempty"`
	Agent      string `json:"agent,omitempty"`
	AgentType  string `json:"agent_type,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	WorkItemID string `json:"work_item_id,omitempty"`
	// RunAs is the principal of the run, for example user:usr_1 or
	// factory:fac_1.
	RunAs string `json:"run_as,omitempty"`
	// Creator is the user who caused the run, if one is known.
	Creator string `json:"creator,omitempty"`
	// WorkerHost is the worker or the backend of the session.
	WorkerHost string `json:"worker_host,omitempty"`
}

// TokenOptions are the claims of a run token that do not come from the run.
type TokenOptions struct {
	// Creator is the user ID of the creator of the run. Empty means none.
	Creator string
	// RunAs is the principal of the run. Empty means the run_as principal
	// of the run (Run.RunAs); for a run from before that field,
	// user:<Creator> when Creator is set, else factory:<factory ID>.
	RunAs string
	// WorkerHost is the worker ID or the backend of the session.
	WorkerHost string
}

// Subject is the sub claim of a run: team/<team>/factory/<factory>/agent/<agent>.
// Cloud trust policies can match it exactly (one agent) or by prefix.
func Subject(r *core.Run) string {
	return "team/" + r.TeamID + "/factory/" + r.FactoryID + "/agent/" + r.AgentName
}

// RunToken signs a token for run r with audience aud.
func (i *Issuer) RunToken(r *core.Run, aud string, ttl time.Duration) (string, error) {
	tok, _, err := i.Token(r, aud, ttl, TokenOptions{})
	return tok, err
}

// Token signs a token for run r with audience aud and returns it with its
// expiry. A ttl of 0 means DefaultTTL. A ttl over MaxTTL is cut to MaxTTL.
func (i *Issuer) Token(r *core.Run, aud string, ttl time.Duration, o TokenOptions) (string, time.Time, error) {
	if strings.TrimSpace(aud) == "" {
		return "", time.Time{}, errors.New("cloudid: audience is required")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	runAs := o.RunAs
	switch {
	case runAs != "":
	case r.RunAs != nil && r.RunAs.ID != "":
		runAs = r.RunAs.String()
	case o.Creator != "":
		runAs = "user:" + o.Creator
	default:
		runAs = "factory:" + r.FactoryID
	}
	now := i.now().UTC()
	exp := now.Add(ttl)
	c := Claims{
		Issuer: i.issuer, Subject: Subject(r), Audience: aud,
		IssuedAt: now.Unix(), NotBefore: now.Add(-30 * time.Second).Unix(), Expiry: exp.Unix(),
		JTI: core.NewID("jti"), TeamID: r.TeamID, FactoryID: r.FactoryID, Agent: r.AgentName,
		AgentType: string(r.Config.Type), RunID: r.ID, WorkItemID: r.WorkItemID,
		RunAs: runAs, Creator: o.Creator, WorkerHost: o.WorkerHost,
	}
	tok, err := i.sign(c)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, time.Unix(exp.Unix(), 0).UTC(), nil
}

func (i *Issuer) sign(claims any) (string, error) {
	hb, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": i.kid})
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, i.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Discovery is the OpenID provider configuration document.
func (i *Issuer) Discovery() map[string]any {
	return map[string]any{
		"issuer":                                i.issuer,
		"jwks_uri":                              i.issuer + JWKSPath,
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                      []string{"openid"},
		"claims_supported":                      []string{"iss", "sub", "aud", "iat", "nbf", "exp", "jti", "team_id", "factory_id", "agent", "agent_type", "run_id", "work_item_id", "run_as", "creator", "worker_host"},
	}
}

// JWKS is the JSON Web Key Set. It lists the public key of the signing key
// first, then the public keys of the previous signing keys.
func (i *Issuer) JWKS() map[string]any {
	jwk := func(k *ecdsa.PrivateKey) map[string]string {
		x := make([]byte, 32)
		y := make([]byte, 32)
		pub, _ := k.PublicKey.Bytes() // 0x04 || X || Y
		if len(pub) == 65 {
			copy(x, pub[1:33])
			copy(y, pub[33:])
		}
		return map[string]string{
			"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "kid": keyID(&k.PublicKey),
			"x": b64.EncodeToString(x), "y": b64.EncodeToString(y),
		}
	}
	keys := []map[string]string{jwk(i.key)}
	for _, k := range i.previous {
		keys = append(keys, jwk(k))
	}
	return map[string]any{"keys": keys}
}

// Register mounts the discovery document and the JWKS.
func (i *Issuer) Register(mux *http.ServeMux) {
	serve := func(doc func() map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_ = json.NewEncoder(w).Encode(doc())
		}
	}
	mux.HandleFunc("GET "+DiscoveryPath, serve(i.Discovery))
	mux.HandleFunc("GET "+JWKSPath, serve(i.JWKS))
}

// Credentials are the environment variables and files that give a run its
// cloud identity. File paths are absolute sandbox paths.
type Credentials struct {
	Env   map[string]string
	Files map[string][]byte
}

// GCPAudience is the workload identity provider resource used as the token
// audience and in the external-account credentials.
func GCPAudience(projectNumber, pool, provider string) string {
	return "//iam.googleapis.com/projects/" + projectNumber + "/locations/global/workloadIdentityPools/" + pool + "/providers/" + provider
}

// RunCredentials builds the credentials for run r from the factory's
// cloudProviders. dir is the absolute sandbox directory for credential files
// (normally <workdir>/.openfactory/cloud). It returns nil when cp declares no
// provider.
//
//   - AWS: AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE (a token with audience
//     sts.amazonaws.com) and AWS_ROLE_SESSION_NAME; AWS SDKs and the CLI then
//     call sts:AssumeRoleWithWebIdentity.
//   - GCP: an external_account credentials file (GOOGLE_APPLICATION_CREDENTIALS)
//     whose credential_source reads a token with the workload identity provider
//     as audience; with serviceAccountEmail it impersonates that account.
func (i *Issuer) RunCredentials(r *core.Run, cp *core.CloudProviders, dir string) (*Credentials, error) {
	return i.RunCredentialsWith(r, cp, dir, TokenOptions{})
}

// RunCredentialsWith is RunCredentials with the claims of o.
func (i *Issuer) RunCredentialsWith(r *core.Run, cp *core.CloudProviders, dir string, o TokenOptions) (*Credentials, error) {
	s := SetupFor(cp)
	if s == nil {
		return nil, nil
	}
	out := &Credentials{Env: map[string]string{}, Files: map[string][]byte{}}
	if a := s.AWS; a != nil {
		tok, _, err := i.Token(r, a.Audience, DefaultTTL, o)
		if err != nil {
			return nil, err
		}
		file := path.Join(dir, "aws-web-identity-token")
		out.Files[file] = []byte(tok)
		out.Env["AWS_ROLE_ARN"] = a.RoleARN
		out.Env["AWS_WEB_IDENTITY_TOKEN_FILE"] = file
		out.Env["AWS_ROLE_SESSION_NAME"] = sessionName(r.ID)
	}
	if g := s.GCP; g != nil {
		tok, _, err := i.Token(r, g.Audience, DefaultTTL, o)
		if err != nil {
			return nil, err
		}
		tokFile := path.Join(dir, "gcp-subject-token")
		credFile := path.Join(dir, "gcp-credentials.json")
		b, err := GCPCredentialFile(g, "", tokFile)
		if err != nil {
			return nil, err
		}
		out.Files[tokFile] = []byte(tok)
		out.Files[credFile] = b
		out.Env["GOOGLE_APPLICATION_CREDENTIALS"] = credFile
		out.Env["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"] = credFile
		out.Env["GOOGLE_CLOUD_PROJECT_NUMBER"] = g.ProjectNumber
	}
	return out, nil
}

// sessionName is a valid AWS role session name (2-64 chars of [\w+=,.@-]).
func sessionName(runID string) string {
	n := "openfactory-" + runID
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// Verify checks a token signed by this issuer and returns its claims (tests,
// diagnostics).
func (i *Issuer) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("cloudid: malformed token")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("cloudid: malformed signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	ok := ecdsa.Verify(&i.key.PublicKey, sum[:], bigInt(sig[:32]), bigInt(sig[32:]))
	for _, k := range i.previous {
		ok = ok || ecdsa.Verify(&k.PublicKey, sum[:], bigInt(sig[:32]), bigInt(sig[32:]))
	}
	if !ok {
		return nil, errors.New("cloudid: bad signature")
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, fmt.Errorf("cloudid: claims: %w", err)
	}
	if c.Issuer != i.issuer {
		return nil, errors.New("cloudid: wrong issuer")
	}
	if now := i.now().Unix(); now >= c.Expiry || now < c.NotBefore {
		return nil, errors.New("cloudid: token expired or not yet valid")
	}
	return &c, nil
}

func bigInt(b []byte) *big.Int { return new(big.Int).SetBytes(b) }
