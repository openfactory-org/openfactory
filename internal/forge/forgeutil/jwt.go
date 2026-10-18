package forgeutil

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

// ParseRSAPrivateKey parses a PEM-encoded PKCS#1 or PKCS#8 RSA private key.
func ParseRSAPrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("private key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key: not an RSA key")
	}
	return rk, nil
}

// SignRS256 produces a compact JWS (JWT) signed with RS256.
func SignRS256(key *rsa.PrivateKey, header map[string]any, claims any) (string, error) {
	if header == nil {
		header = map[string]any{}
	}
	header["alg"] = "RS256"
	header["typ"] = "JWT"
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64.EncodeToString(sig), nil
}

// SignHS256 produces a compact JWT signed with HMAC-SHA256.
func SignHS256(secret []byte, claims any) (string, error) {
	hb, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signing))
	return signing + "." + b64.EncodeToString(m.Sum(nil)), nil
}

// JWT is a parsed (not yet verified) compact JWT.
type JWT struct {
	Header    map[string]any
	Claims    map[string]any
	signing   string
	signature []byte
}

// ParseJWT splits and decodes a compact JWT without verifying it.
func ParseJWT(tok string) (*JWT, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: malformed token")
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("jwt header: %w", err)
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("jwt claims: %w", err)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("jwt signature: %w", err)
	}
	j := &JWT{signing: parts[0] + "." + parts[1], signature: sig}
	if err := json.Unmarshal(hb, &j.Header); err != nil {
		return nil, fmt.Errorf("jwt header: %w", err)
	}
	if err := json.Unmarshal(cb, &j.Claims); err != nil {
		return nil, fmt.Errorf("jwt claims: %w", err)
	}
	return j, nil
}

// HeaderString returns a string header value.
func (j *JWT) HeaderString(k string) string { s, _ := j.Header[k].(string); return s }

// ClaimString returns a string claim value.
func (j *JWT) ClaimString(k string) string { s, _ := j.Claims[k].(string); return s }

// VerifyRS256 checks the signature against an RSA public key.
func (j *JWT) VerifyRS256(pub *rsa.PublicKey) error {
	if j.HeaderString("alg") != "RS256" {
		return fmt.Errorf("jwt: unexpected alg %q", j.HeaderString("alg"))
	}
	sum := sha256.Sum256([]byte(j.signing))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], j.signature); err != nil {
		return errors.New("jwt: invalid signature")
	}
	return nil
}

// VerifyHS256 checks an HMAC-SHA256 signature.
func (j *JWT) VerifyHS256(secret []byte) error {
	if j.HeaderString("alg") != "HS256" {
		return fmt.Errorf("jwt: unexpected alg %q", j.HeaderString("alg"))
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(j.signing))
	if !hmac.Equal(m.Sum(nil), j.signature) {
		return errors.New("jwt: invalid signature")
	}
	return nil
}

// VerifyTime checks exp / nbf (and iat not in the future) with the given clock skew.
func (j *JWT) VerifyTime(now time.Time, skew time.Duration) error {
	num := func(k string) (int64, bool) {
		switch v := j.Claims[k].(type) {
		case float64:
			return int64(v), true
		case json.Number:
			n, err := v.Int64()
			return n, err == nil
		}
		return 0, false
	}
	exp, ok := num("exp")
	if !ok {
		return errors.New("jwt: missing exp")
	}
	if now.After(time.Unix(exp, 0).Add(skew)) {
		return errors.New("jwt: token expired")
	}
	if nbf, ok := num("nbf"); ok && now.Add(skew).Before(time.Unix(nbf, 0)) {
		return errors.New("jwt: token not yet valid")
	}
	return nil
}

// HasAudience reports whether the aud claim (string or array) contains aud.
func (j *JWT) HasAudience(aud string) bool {
	switch v := j.Claims["aud"].(type) {
	case string:
		return v == aud
	case []any:
		for _, a := range v {
			if s, _ := a.(string); s == aud {
				return true
			}
		}
	}
	return false
}

// JWK is one RSA key from a JWKS document.
type JWK struct {
	Kty          string   `json:"kty"`
	Kid          string   `json:"kid"`
	Use          string   `json:"use,omitempty"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	X5c          []string `json:"x5c,omitempty"`
	Endorsements []string `json:"endorsements,omitempty"`
}

// RSAPublicKey converts the JWK to an RSA public key.
func (k JWK) RSAPublicKey() (*rsa.PublicKey, error) {
	if k.Kty != "" && k.Kty != "RSA" {
		return nil, fmt.Errorf("jwk %s: unsupported kty %s", k.Kid, k.Kty)
	}
	nb, err := b64.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return nil, fmt.Errorf("jwk %s: n: %w", k.Kid, err)
	}
	eb, err := b64.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return nil, fmt.Errorf("jwk %s: e: %w", k.Kid, err)
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 {
		return nil, fmt.Errorf("jwk %s: invalid exponent", k.Kid)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}

// NewJWK builds a JWK from an RSA public key (used by tests and tooling).
func NewJWK(kid string, pub *rsa.PublicKey) JWK {
	return JWK{
		Kty: "RSA", Kid: kid, Use: "sig",
		N: b64.EncodeToString(pub.N.Bytes()),
		E: b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
