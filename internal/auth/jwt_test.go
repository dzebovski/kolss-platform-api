package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func TestVerifierES256Claims(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{
		Issuer:    "https://project.supabase.co/auth/v1",
		Audience:  "authenticated",
		keys:      map[string]*ecdsa.PublicKey{"key-1": &privateKey.PublicKey},
		fetchedAt: time.Now(),
	}
	token := signedToken(t, privateKey, "key-1", map[string]any{
		"sub": "cfc9b7a4-ecf1-4e84-b704-ae1d39290cdd",
		"iss": verifier.Issuer,
		"aud": "authenticated",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.Subject != "cfc9b7a4-ecf1-4e84-b704-ae1d39290cdd" {
		t.Fatalf("unexpected subject %q", claims.Subject)
	}
}

func TestVerifierRejectsWrongAudienceAndExpiredToken(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{
		Issuer:    "issuer",
		Audience:  "authenticated",
		keys:      map[string]*ecdsa.PublicKey{"key-1": &privateKey.PublicKey},
		fetchedAt: time.Now(),
	}
	for name, claims := range map[string]map[string]any{
		"audience": {"sub": "user", "iss": "issuer", "aud": "anon", "exp": time.Now().Add(time.Hour).Unix()},
		"expired":  {"sub": "user", "iss": "issuer", "aud": "authenticated", "exp": time.Now().Add(-time.Minute).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), signedToken(t, privateKey, "key-1", claims)); err == nil {
				t.Fatal("Verify() unexpectedly accepted token")
			}
		})
	}
}

func signedToken(t *testing.T, key *ecdsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": keyID, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	headerPart := base64.RawURLEncoding.EncodeToString(header)
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(headerPart + "." + payloadPart))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(paddedInt(r, 32), paddedInt(s, 32)...)
	return headerPart + "." + payloadPart + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func paddedInt(value *big.Int, size int) []byte {
	raw := value.Bytes()
	out := make([]byte, size)
	copy(out[size-len(raw):], raw)
	return out
}
