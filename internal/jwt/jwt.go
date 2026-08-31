// Package jwt implements the minimal RS256 JWT used by lean-sandbox: the
// control plane signs sandbox tokens with its private key, and agents verify
// them with the control plane's public key. Stdlib-only (crypto/rsa, sha256,
// x509, base64, json) — no external JWT dependency.
package jwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Claims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// Sign mints an RS256 JWT for subject sub, expiring after ttl.
func Sign(key *rsa.PrivateKey, sub string, ttl time.Duration) (string, error) {
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	cl, err := json.Marshal(Claims{Sub: sub, Iat: now.Unix(), Exp: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(cl)
	digest := sha256.Sum256([]byte(header + "." + payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify validates an RS256 JWT signed by the control plane. It accepts only
// RS256 (rejecting alg=none and anything else), requires sub to match
// sandboxID, and rejects expired or future-issued tokens.
func Verify(token string, pub *rsa.PublicKey, sandboxID string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("jwt: malformed")
	}

	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("jwt header: %w", err)
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return fmt.Errorf("jwt header: %w", err)
	}
	if h.Alg != "RS256" {
		return fmt.Errorf("jwt: alg %q not allowed", h.Alg)
	}

	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("jwt claims: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return fmt.Errorf("jwt claims: %w", err)
	}
	if c.Sub != sandboxID {
		return errors.New("jwt: sub mismatch")
	}
	if now.Unix() >= c.Exp {
		return errors.New("jwt: expired")
	}
	if c.Iat > now.Add(5*time.Minute).Unix() {
		return errors.New("jwt: issued in the future")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("jwt signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return errors.New("jwt: bad signature")
	}
	return nil
}

// GenerateKey returns a fresh RSA signing key plus the base64 SPKI public key
// for injection into agent pods. Regenerating on each control plane start
// invalidates all previously minted tokens.
func GenerateKey() (*rsa.PrivateKey, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, "", err
	}
	return key, base64.StdEncoding.EncodeToString(der), nil
}

// ParsePublicKey decodes a base64 SPKI DER RSA public key, as injected into an
// agent via -controlplane-public-key.
func ParsePublicKey(b64 string) (*rsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not RSA")
	}
	return rsaPub, nil
}
