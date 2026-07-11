package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("expired token")
)

type Claims struct {
	Subject  string
	Email    string
	Issuer   string
	Audience []string
	Expires  time.Time
}

type Verifier struct {
	JWKSURL  string
	Issuer   string
	Audience string
	HTTP     *http.Client
	TTL      time.Duration

	mu        sync.RWMutex
	keys      map[string]*ecdsa.PublicKey
	fetchedAt time.Time
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

type rawClaims struct {
	Subject   string          `json:"sub"`
	Email     string          `json:"email"`
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Expires   int64           `json:"exp"`
	NotBefore int64           `json:"nbf"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Algorithm string `json:"alg"`
	Curve     string `json:"crv"`
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	X         string `json:"x"`
	Y         string `json:"y"`
}

func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}

	var header jwtHeader
	if err := decodeSegment(parts[0], &header); err != nil || header.Algorithm != "ES256" || header.KeyID == "" {
		return Claims{}, ErrInvalidToken
	}

	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return Claims{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return Claims{}, ErrInvalidToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return Claims{}, ErrInvalidToken
	}

	var raw rawClaims
	if err := decodeSegment(parts[1], &raw); err != nil {
		return Claims{}, ErrInvalidToken
	}
	audience, err := parseAudience(raw.Audience)
	if err != nil || raw.Subject == "" || raw.Issuer != v.Issuer || !contains(audience, v.Audience) {
		return Claims{}, ErrInvalidToken
	}
	now := time.Now().UTC()
	if raw.Expires <= 0 || !now.Before(time.Unix(raw.Expires, 0)) {
		return Claims{}, ErrExpiredToken
	}
	if raw.NotBefore > 0 && now.Before(time.Unix(raw.NotBefore, 0)) {
		return Claims{}, ErrInvalidToken
	}
	return Claims{
		Subject:  raw.Subject,
		Email:    raw.Email,
		Issuer:   raw.Issuer,
		Audience: audience,
		Expires:  time.Unix(raw.Expires, 0).UTC(),
	}, nil
}

func (v *Verifier) key(ctx context.Context, keyID string) (*ecdsa.PublicKey, error) {
	if key := v.cachedKey(keyID); key != nil {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	if key := v.cachedKey(keyID); key != nil {
		return key, nil
	}
	return nil, ErrInvalidToken
}

func (v *Verifier) cachedKey(keyID string) *ecdsa.PublicKey {
	v.mu.RLock()
	defer v.mu.RUnlock()
	ttl := v.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if time.Since(v.fetchedAt) > ttl {
		return nil
	}
	return v.keys[keyID]
}

func (v *Verifier) refresh(ctx context.Context) error {
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: status %d", res.StatusCode)
	}
	var doc jwksDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey, len(doc.Keys))
	for _, item := range doc.Keys {
		if item.KeyType != "EC" || item.Curve != "P-256" || item.Algorithm != "ES256" || item.KeyID == "" {
			continue
		}
		xBytes, xErr := base64.RawURLEncoding.DecodeString(item.X)
		yBytes, yErr := base64.RawURLEncoding.DecodeString(item.Y)
		if xErr != nil || yErr != nil {
			continue
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
		if key.Curve.IsOnCurve(key.X, key.Y) {
			keys[item.KeyID] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("jwks contains no supported keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func decodeSegment(segment string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func parseAudience(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
