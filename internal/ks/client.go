package ks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// FrameBytes is one Kiosk Satellite audio frame: 80 ms of 16 kHz mono PCM16, little-endian.
const FrameBytes = 2560

// Peer is how a device presents itself in a call ("from").
type Peer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	TLS     bool   `json:"tls"`
}

// Identity is what GET /api/intercom/identity answers.
type Identity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Enabled  bool   `json:"enabled"`
	Key      string `json:"key"`
	DND      bool   `json:"dnd"`
	Endpoint struct {
		Port int  `json:"port"`
		TLS  bool `json:"tls"`
	} `json:"endpoint"`
}

// CallRequest is the body of POST /api/intercom/call.
type CallRequest struct {
	Call string `json:"call"`
	Kind string `json:"kind"` // "call" or "broadcast"
	From Peer   `json:"from"`
}

// Client calls real kiosks.
type Client struct {
	auth *Auth
	http *http.Client

	mu    sync.Mutex
	cache map[string]cachedIdentity
}

type cachedIdentity struct {
	id Identity
	at time.Time
}

func NewClient(auth *Auth) *Client {
	return &Client{auth: auth, http: &http.Client{Timeout: 4 * time.Second}, cache: map[string]cachedIdentity{}}
}

func base(host string, port int) string { return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) }

// Identity reads a kiosk's identity from its admin port. Kiosks answer one request a second per
// client, so a copy younger than that is reused.
func (c *Client) Identity(ctx context.Context, host string, adminPort int) (Identity, error) {
	k := net.JoinHostPort(host, strconv.Itoa(adminPort))
	c.mu.Lock()
	if v, ok := c.cache[k]; ok && time.Since(v.at) < 1100*time.Millisecond {
		c.mu.Unlock()
		return v.id, nil
	}
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base(host, adminPort)+"/api/intercom/identity", nil)
	if err != nil {
		return Identity{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("identity: HTTP %d", resp.StatusCode)
	}
	var id Identity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&id); err != nil {
		return Identity{}, fmt.Errorf("identity: %w", err)
	}
	c.mu.Lock()
	c.cache[k] = cachedIdentity{id: id, at: time.Now()}
	c.mu.Unlock()
	return id, nil
}

// KeyMatches reports whether a kiosk's identity says it has our key.
func (c *Client) KeyMatches(id Identity) bool { return id.Key == c.auth.KeyDigest() }

func (c *Client) post(ctx context.Context, url, call, from string, body any) (int, map[string]any, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.auth.Token(call, from))
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	return resp.StatusCode, out, nil
}

// Call rings a kiosk at its call endpoint. It returns the kiosk's status word: ringing, auto,
// listening, busy, dnd, off, refused, or key / tls when the kiosk refused us.
func (c *Client) Call(ctx context.Context, host string, endpointPort int, req CallRequest) (string, error) {
	code, out, err := c.post(ctx, base(host, endpointPort)+"/api/intercom/call", req.Call, req.From.ID, req)
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusForbidden:
		return "key", nil
	case http.StatusConflict, http.StatusUpgradeRequired:
		return "tls", nil
	case http.StatusOK:
	default:
		return "", fmt.Errorf("call: HTTP %d", code)
	}
	status, _ := out["status"].(string)
	if status == "" {
		return "", fmt.Errorf("call: no status")
	}
	return status, nil
}

// Signal tells a kiosk what happened to a call: answer, decline or missed from the callee, cancel
// or hangup from the caller.
func (c *Client) Signal(ctx context.Context, host string, endpointPort int, call, from, action string) error {
	code, _, err := c.post(ctx, base(host, endpointPort)+"/api/intercom/call/"+url.PathEscape(call), call, from,
		map[string]string{"action": action})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", action, code)
	}
	return nil
}

// DialAudio opens a call's audio socket on a kiosk. The caller opens it, once the call is answered.
func (c *Client) DialAudio(ctx context.Context, host string, endpointPort int, call, from string) (*websocket.Conn, error) {
	u := url.URL{
		Scheme:   "ws",
		Host:     net.JoinHostPort(host, strconv.Itoa(endpointPort)),
		Path:     "/api/intercom/audio/" + call,
		RawQuery: url.Values{"token": {c.auth.Token(call, from)}}.Encode(),
	}
	d := websocket.Dialer{HandshakeTimeout: 4 * time.Second}
	ws, resp, err := d.DialContext(ctx, u.String(), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return ws, err
}
