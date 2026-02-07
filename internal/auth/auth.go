package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
)

const PasskeySize = 32

// GeneratePasskey returns a cryptographically random 32-byte passkey.
func GeneratePasskey() ([]byte, error) {
	key := make([]byte, PasskeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// ComputeAuthToken computes HMAC-SHA256(passkey, exporterMaterial).
// The exporterMaterial should come from TLS.ExportKeyingMaterial
// to bind the auth token to the specific TLS session.
func ComputeAuthToken(passkey, exporterMaterial []byte) [32]byte {
	mac := hmac.New(sha256.New, passkey)
	mac.Write(exporterMaterial)
	var token [32]byte
	copy(token[:], mac.Sum(nil))
	return token
}

// VerifyAuthToken checks that the provided token matches the expected
// HMAC-SHA256(passkey, exporterMaterial).
func VerifyAuthToken(passkey, exporterMaterial []byte, token [32]byte) bool {
	expected := ComputeAuthToken(passkey, exporterMaterial)
	return hmac.Equal(token[:], expected[:])
}
