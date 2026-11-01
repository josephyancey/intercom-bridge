package ks

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDigest(t *testing.T) {
	// sha256("test") = 9f86d081...
	if got := Digest("test"); got != "9f86d081" {
		t.Fatalf("Digest = %q", got)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	a, b := NewAuth("secret"), NewAuth("secret")
	tok := a.Token("call1", "dev1")
	if !b.Verify(tok, "call1") {
		t.Fatal("a good token was refused")
	}
	if b.Verify(tok, "call1") {
		t.Fatal("a token was accepted twice")
	}
}

func TestTokenRefusals(t *testing.T) {
	a := NewAuth("secret")
	if NewAuth("other").Verify(a.Token("c", "d"), "c") {
		t.Fatal("a token from another key was accepted")
	}
	if a.Verify(a.Token("c", "d"), "different") {
		t.Fatal("a token for another call was accepted")
	}
	tok := a.Token("c", "d")
	payload, sig, _ := strings.Cut(tok, ".")
	if a.Verify(payload+"x."+sig, "c") {
		t.Fatal("a changed payload was accepted")
	}
	claims, _ := json.Marshal(map[string]any{"intercom": "c", "from": "d", "n": "1", "exp": time.Now().Add(-time.Second).UnixMilli()})
	old := base64.URLEncoding.EncodeToString(claims)
	if a.Verify(old+"."+a.sign(old), "c") {
		t.Fatal("an expired token was accepted")
	}
}

func TestTokenFormat(t *testing.T) {
	// Kiosks sign "payload.sig" with base64url, padding kept, HMAC keyed by "intercom:" + key.
	tok := NewAuth("k").Token("c", "d")
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok || len(sig) != 44 || !strings.HasSuffix(sig, "=") {
		t.Fatalf("token %q is not payload.sig with a padded 32-byte signature", tok)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"intercom", "from", "n", "exp"} {
		if _, ok := claims[k]; !ok {
			t.Fatalf("claims lack %q: %v", k, claims)
		}
	}
}
