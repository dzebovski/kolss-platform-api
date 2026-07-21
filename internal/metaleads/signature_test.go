package metaleads

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"object":"page"}`)
	mac := hmac.New(sha256.New, []byte("app-secret"))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifySignature("app-secret", body, signature) {
		t.Fatal("valid signature was rejected")
	}
	if VerifySignature("app-secret", []byte(`{"changed":true}`), signature) {
		t.Fatal("signature accepted a changed body")
	}
	if VerifySignature("app-secret", body, "sha1=bad") {
		t.Fatal("legacy or malformed signature was accepted")
	}
}

func TestAppSecretProof(t *testing.T) {
	const want = "e941110e3d2bfe82621f0e3e1434730d7305d106c5f68c87165d0b27a4611a4a"
	if got := AppSecretProof("secret", "token"); got != want {
		t.Fatalf("proof=%q want=%q", got, want)
	}
}
