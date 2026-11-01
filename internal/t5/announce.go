package t5

// TECHO5 announcements, following echod/internal/feature/announce (wire.go, auth.go, announce.go):
// a POST to /announce whose body is the voice as 16 kHz mono PCM16, little-endian, with who sent it
// and what was said in headers, signed with an HMAC keyed from the house word.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AnnouncePath = "/announce"

	fromHeader   = "X-Techo5-From"
	textHeader   = "X-Techo5-Text"
	kindHeader   = "X-Techo5-Kind"
	idHeader     = "X-Techo5-Id"
	authHeader   = "X-Techo5-Auth"
	timeHeader   = "X-Techo5-Time"
	legacyHeader = "X-Techo5-House" // older devices sent the word itself
	audioType    = "audio/L16; rate=16000; channels=1"
	authLabel    = "techo5-announce hmac"

	// MaxAnnouncement is the longest voice a device takes: twenty seconds.
	MaxAnnouncement = 20 * 16000 * 2

	authSkew   = 5 * time.Minute
	noncesKept = 4096
	sendWait   = 4 * time.Second
)

// Kinds a device sends besides announcements: reminders it shares with the house.
const (
	KindReminder        = "reminder"
	KindReminderStopped = "reminder-stopped"
)

// Announcement is one announcement. Voice is 16 kHz mono PCM16, little-endian; it is empty for an
// announcement made of words only.
type Announcement struct {
	From  string
	Text  string
	Kind  string
	ID    string
	Voice []byte
}

func announceKey(word string) []byte {
	sum := sha256.Sum256([]byte(authLabel + ":" + word))
	return sum[:]
}

// signed is what the signature covers, one field a line.
func signed(ts, nonce, method, path string, get func(string) string, body []byte) []byte {
	sum := sha256.Sum256(body)
	var b bytes.Buffer
	for _, part := range []string{"v1", ts, nonce, method, path,
		get(fromHeader), get(textHeader), get(kindHeader), get(idHeader), hex.EncodeToString(sum[:])} {
		b.WriteString(part)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func signAt(word, method, path string, headers map[string]string, body []byte, at time.Time) string {
	var n [12]byte
	_, _ = rand.Read(n[:])
	ts, nonce := strconv.FormatInt(at.Unix(), 10), hex.EncodeToString(n[:])
	mac := hmac.New(sha256.New, announceKey(word))
	mac.Write(signed(ts, nonce, method, path, func(k string) string { return headers[k] }, body))
	return "v1 " + ts + " " + nonce + " " + hex.EncodeToString(mac.Sum(nil))
}

func unescape(v string) string {
	if s, err := url.QueryUnescape(v); err == nil {
		return s
	}
	return v
}

// Announcements receives and sends announcements for one house word.
type Announcements struct {
	word string
	now  func() time.Time // a test moves it

	mu      sync.Mutex
	nonces  map[string]time.Time
	offsets map[string]time.Duration // how far each device's clock is from ours
}

func NewAnnouncements(houseWord string) *Announcements {
	return &Announcements{word: houseWord, now: time.Now, nonces: map[string]time.Time{}, offsets: map[string]time.Duration{}}
}

func (a *Announcements) fresh(nonce string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.nonces[nonce]; ok {
		return false
	}
	if len(a.nonces) >= noncesKept {
		for n, at := range a.nonces {
			if now.Sub(at) > 2*authSkew {
				delete(a.nonces, n)
			}
		}
		if len(a.nonces) >= noncesKept {
			return false
		}
	}
	a.nonces[nonce] = now
	return true
}

// Receive reads and checks an announcement sent to us, and answers the request as a device would:
// 204 when it was taken, 409 with our clock when the sender's is out, 403 when it was not signed by
// this house. ok is false when the request has been answered with a refusal.
func (a *Announcements) Receive(w http.ResponseWriter, r *http.Request) (Announcement, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "post an announcement", http.StatusMethodNotAllowed)
		return Announcement{}, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxAnnouncement))
	if err != nil {
		http.Error(w, "that was not an announcement", http.StatusBadRequest)
		return Announcement{}, false
	}
	now := a.now()
	switch err := a.verify(r, body, now); {
	case errors.Is(err, errSkew):
		w.Header().Set(timeHeader, strconv.FormatInt(now.Unix(), 10))
		http.Error(w, "our clocks disagree", http.StatusConflict)
		return Announcement{}, false
	case err != nil:
		http.Error(w, "not this house", http.StatusForbidden)
		return Announcement{}, false
	}
	if len(body)%2 != 0 {
		body = body[:len(body)-1]
	}
	m := Announcement{
		From: unescape(r.Header.Get(fromHeader)), Text: unescape(r.Header.Get(textHeader)),
		Kind: unescape(r.Header.Get(kindHeader)), ID: unescape(r.Header.Get(idHeader)), Voice: body,
	}
	if m.Kind == "" && m.Text == "" && len(m.Voice) == 0 {
		http.Error(w, "an announcement says something", http.StatusBadRequest)
		return Announcement{}, false
	}
	w.WriteHeader(http.StatusNoContent)
	return m, true
}

var (
	errNotThisHouse = errors.New("announce: not signed by this house")
	errSkew         = errors.New("announce: signed at a time too far from our clock")
)

func (a *Announcements) verify(r *http.Request, body []byte, now time.Time) error {
	if a.word == "" {
		return errNotThisHouse
	}
	auth := r.Header.Get(authHeader)
	if auth == "" {
		if old := r.Header.Get(legacyHeader); old != "" && subtle.ConstantTimeCompare([]byte(old), []byte(a.word)) == 1 {
			return nil
		}
		return errNotThisHouse
	}
	parts := strings.Fields(auth)
	if len(parts) != 4 || parts[0] != "v1" {
		return errNotThisHouse
	}
	ts, nonce, got := parts[1], parts[2], parts[3]
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errNotThisHouse
	}
	want := hmac.New(sha256.New, announceKey(a.word))
	want.Write(signed(ts, nonce, r.Method, r.URL.Path, r.Header.Get, body))
	sum, err := hex.DecodeString(got)
	if err != nil || !hmac.Equal(sum, want.Sum(nil)) {
		return errNotThisHouse
	}
	if d := now.Sub(time.Unix(sec, 0)); d > authSkew || d < -authSkew {
		return errSkew
	}
	if !a.fresh(nonce, now) {
		return errors.New("announce: heard this one already")
	}
	return nil
}

// Send delivers an announcement to a device. When the device says our clock is out, it is signed
// again by the device's clock, as devices do with each other.
func (a *Announcements) Send(ctx context.Context, address string, port int, m Announcement) error {
	ctx, cancel := context.WithTimeout(ctx, sendWait)
	defer cancel()
	headers := map[string]string{
		fromHeader:     url.QueryEscape(m.From),
		textHeader:     url.QueryEscape(m.Text),
		"Content-Type": audioType,
	}
	if m.Kind != "" {
		headers[kindHeader] = url.QueryEscape(m.Kind)
		headers[idHeader] = url.QueryEscape(m.ID)
	}
	addr := net.JoinHostPort(address, strconv.Itoa(port))
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+AnnouncePath, bytes.NewReader(m.Voice))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		a.mu.Lock()
		off := a.offsets[addr]
		a.mu.Unlock()
		req.Header.Set(authHeader, signAt(a.word, http.MethodPost, AnnouncePath, headers, m.Voice, a.now().Add(off)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusConflict && try == 0 {
			if theirs, err := strconv.ParseInt(resp.Header.Get(timeHeader), 10, 64); err == nil {
				d := time.Unix(theirs, 0).Sub(a.now())
				if d > 24*time.Hour && a.now().Year() >= 2025 {
					d = 24 * time.Hour // never sign far into the future on someone else's word
				}
				a.mu.Lock()
				a.offsets[addr] = d
				a.mu.Unlock()
				continue
			}
		}
		if resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusForbidden {
				return fmt.Errorf("the device refused it: its house word is different")
			}
			return fmt.Errorf("the device said %s", resp.Status)
		}
		return nil
	}
}
