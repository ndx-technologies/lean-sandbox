package jwt

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustSign(t *testing.T, key *rsa.PrivateKey, sub string, ttl time.Duration) string {
	t.Helper()
	tok, err := Sign(key, sub, ttl)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return tok
}

func TestRoundTrip(t *testing.T) {
	key, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}

	tok, err := Sign(key, "sb-1", time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(tok, pub, "sb-1", time.Now()); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	key, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	other, _, _ := GenerateKey()
	now := time.Now()

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"malformed", "only-two.parts"},
		{"wrong sub", mustSign(t, key, "sb-other", time.Hour)},
		{"expired", mustSign(t, key, "sb-1", -time.Minute)},
		{"bad signature", mustSign(t, other, "sb-1", time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.token, pub, "sb-1", now); err == nil {
				t.Fatalf("expected %q to be rejected", tc.name)
			}
		})
	}

	// Issued in the future: sign now, then verify with a `now` 10 minutes in
	// the past so iat is ahead of the allowed clock skew.
	t.Run("issued in future", func(t *testing.T) {
		tok := mustSign(t, key, "sb-1", time.Hour)
		if err := Verify(tok, pub, "sb-1", time.Now().Add(-10*time.Minute)); err == nil {
			t.Fatal("expected future-iat token to be rejected")
		}
	})

	// alg=none must be rejected: an unsigned token would otherwise pass.
	t.Run("alg none", func(t *testing.T) {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		p := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"sb-1","iat":1,"exp":9999999999}`))
		if err := Verify(h+"."+p+".", pub, "sb-1", now); err == nil {
			t.Fatal("expected alg=none to be rejected")
		}
	})
}

func TestSignClaims(t *testing.T) {
	key, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	before := time.Now()
	tok, err := Sign(key, "sb-1", time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	after := time.Now()

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		t.Fatalf("parse claims: %v", err)
	}

	if c.Sub != "sb-1" {
		t.Fatalf("sub=%q want sb-1", c.Sub)
	}
	if c.Exp-c.Iat != int64(time.Hour.Seconds()) {
		t.Fatalf("ttl=%ds want %ds", c.Exp-c.Iat, int64(time.Hour.Seconds()))
	}
	if before.Unix() > c.Iat || after.Unix() < c.Iat {
		t.Fatalf("iat=%d not within [%d,%d]", c.Iat, before.Unix(), after.Unix())
	}
}

func TestParsePublicKey(t *testing.T) {
	_, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatalf("parse valid key: %v", err)
	}
	if pub == nil {
		t.Fatal("nil public key")
	}

	if _, err := ParsePublicKey("not-valid-base64!!"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
	if _, err := ParsePublicKey(base64.StdEncoding.EncodeToString([]byte("garbage"))); err == nil {
		t.Fatal("expected error for invalid DER")
	}
}
