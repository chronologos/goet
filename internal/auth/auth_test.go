package auth

import (
	"bytes"
	"testing"
)

func TestGeneratePasskey(t *testing.T) {
	key1, err := GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != PasskeySize {
		t.Fatalf("expected %d bytes, got %d", PasskeySize, len(key1))
	}

	key2, err := GeneratePasskey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(key1, key2) {
		t.Fatal("two generated passkeys should not be equal")
	}
}

func TestComputeAndVerify(t *testing.T) {
	passkey := []byte("test-passkey-32-bytes-long-xxxxx")
	material := []byte("tls-exporter-material-for-test")

	token := ComputeAuthToken(passkey, material)
	if !VerifyAuthToken(passkey, material, token) {
		t.Fatal("valid token should verify")
	}
}

func TestVerifyWrongPasskey(t *testing.T) {
	passkey := []byte("correct-passkey-32-bytes-xxxxxxx")
	wrong := []byte("wrong-passkey-32-bytes-xxxxxxxxx")
	material := []byte("tls-exporter-material")

	token := ComputeAuthToken(passkey, material)
	if VerifyAuthToken(wrong, material, token) {
		t.Fatal("wrong passkey should not verify")
	}
}

func TestVerifyWrongMaterial(t *testing.T) {
	passkey := []byte("test-passkey-32-bytes-long-xxxxx")
	material1 := []byte("material-session-1")
	material2 := []byte("material-session-2")

	token := ComputeAuthToken(passkey, material1)
	if VerifyAuthToken(passkey, material2, token) {
		t.Fatal("different TLS session material should not verify")
	}
}

func TestVerifyTamperedToken(t *testing.T) {
	passkey := []byte("test-passkey-32-bytes-long-xxxxx")
	material := []byte("tls-exporter-material")

	token := ComputeAuthToken(passkey, material)
	token[0] ^= 0xFF // flip bits
	if VerifyAuthToken(passkey, material, token) {
		t.Fatal("tampered token should not verify")
	}
}

func TestTokenDeterministic(t *testing.T) {
	passkey := []byte("test-passkey-32-bytes-long-xxxxx")
	material := []byte("same-material")

	token1 := ComputeAuthToken(passkey, material)
	token2 := ComputeAuthToken(passkey, material)
	if token1 != token2 {
		t.Fatal("same inputs should produce same token")
	}
}
