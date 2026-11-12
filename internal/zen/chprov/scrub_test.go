package chprov

import (
	"strings"
	"testing"
)

// Login/token/STS endpoints echo back credentials. short()/statusText() are
// the single formatting choke point for errors that gateway/channels.go
// forwards straight to the dashboard, so the redaction must happen there.
func TestShortRedactsCredentialShapedValues(t *testing.T) {
	raw := []byte(`{"access_token":"eyJhbGciOiJIUzI1NiJ9.payload.signature","refresh_token":"secret-refresh-123","credentials":{"access_key_id":"AKIA123456789","secret_access_key":"sk-live-999"},"msg":"callback https://example.com/cb?token=cf82338abd&state=xyz failed"}`)
	got := short(raw)
	for _, secret := range []string{
		"eyJhbGciOiJIUzI1NiJ9", "secret-refresh-123", "AKIA123456789",
		"sk-live-999", "cf82338abd", "state=xyz",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("leaked %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("no redaction marker in %q", got)
	}
}

func TestStatusTextRedactsBearerAndKeepsStatus(t *testing.T) {
	got := statusText(401, []byte(`Authorization: Bearer abcdef1234567890 {"error":"invalid_token"}`))
	if strings.Contains(got, "abcdef1234567890") {
		t.Fatalf("bearer token leaked: %q", got)
	}
	if !strings.Contains(got, "401") {
		t.Fatalf("status code lost: %q", got)
	}
}

func TestShortPreservesNonSecretDiagnostics(t *testing.T) {
	got := short([]byte(`{"code":0,"msg":"daily quota not found"}`))
	if !strings.Contains(got, "daily quota not found") {
		t.Fatalf("ordinary diagnostics were destroyed: %q", got)
	}
}
