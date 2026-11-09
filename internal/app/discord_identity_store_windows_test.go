//go:build windows

package app

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestDiscordIdentityDPAPIProtectionRoundTrip(t *testing.T) {
	token := []byte("a-test-worker-signed-identity-token")
	protected, err := protectDiscordIdentity(token)
	if err != nil {
		t.Fatalf("protectDiscordIdentity: %v", err)
	}
	if bytes.Equal(protected, token) {
		t.Fatal("protected identity is plaintext")
	}
	plain, err := unprotectDiscordIdentity(protected)
	if err != nil {
		t.Fatalf("unprotectDiscordIdentity: %v", err)
	}
	if !bytes.Equal(plain, token) {
		t.Fatalf("unprotected token = %q, want original token", plain)
	}
}

func TestCheckRealIdentity(t *testing.T) {
	token, err := loadDiscordIdentity()
	t.Logf("loadDiscordIdentity err: %v, token len: %d", err, len(token))
	if token != "" {
		profile, err := parseDiscordIdentity(token)
		t.Logf("parseDiscordIdentity err: %v, profile: %+v", err, profile)
		parts := strings.Split(token, ".")
		if len(parts) == 3 {
			p, _ := base64.RawURLEncoding.DecodeString(parts[1])
			t.Logf("Claims JSON: %s", string(p))
		}
	}
}
