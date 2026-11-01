// Package ks speaks the Kiosk Satellite intercom, as Kiosk Phone (github.com/mpjanofsky/kiosk-phone,
// docs/protocol.md) verified it against real kiosks. No Kiosk Satellite code is used.
package ks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// tokenLife is how long a token is good for. Kiosks accept one use within 60 s.
const tokenLife = 60 * time.Second

// Digest is the first 8 hex digits of sha256(key): what identity reports as "key", so two ends can
// tell they share the key without showing it.
func Digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

// Auth signs and checks call tokens with the fleet's intercom key.
type Auth struct {
	key  string
	mu   sync.Mutex
	seen map[string]int64 // tokens used, by expiry, so one cannot be replayed
}

func NewAuth(key string) *Auth { return &Auth{key: key, seen: map[string]int64{}} }

// KeyDigest is this key's Digest.
func (a *Auth) KeyDigest() string { return Digest(a.key) }

func (a *Auth) sign(payload string) string {
	m := hmac.New(sha256.New, []byte("intercom:"+a.key))
	m.Write([]byte(payload))
	return base64.URLEncoding.EncodeToString(m.Sum(nil))
}

// Token is a one-use token for call, from the device with id from.
func (a *Auth) Token(call, from string) string {
	n := make([]byte, 12)
	_, _ = rand.Read(n)
	claims, _ := json.Marshal(map[string]any{
		"intercom": call,
		"from":     from,
		"n":        hex.EncodeToString(n),
		"exp":      time.Now().Add(tokenLife).UnixMilli(),
	})
	payload := base64.URLEncoding.EncodeToString(claims)
	return payload + "." + a.sign(payload)
}

// Verify reports whether token was signed with the key, is for call, has not expired and has not
// been used before. A token that passes is spent.
func (a *Auth) Verify(token, call string) bool {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return false
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(payload); err != nil {
			return false
		}
	}
	var claims struct {
		Intercom string `json:"intercom"`
		Exp      int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Intercom != call {
		return false
	}
	now := time.Now().UnixMilli()
	if claims.Exp < now {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for t, exp := range a.seen {
		if exp < now {
			delete(a.seen, t)
		}
	}
	if _, used := a.seen[token]; used {
		return false
	}
	a.seen[token] = claims.Exp
	return true
}
