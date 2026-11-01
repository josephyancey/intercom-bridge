package ks

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// Calls is what a stand-in kiosk passes on to the bridge. Methods run on the HTTP request's goroutine.
type Calls interface {
	// Ring is a kiosk calling this stand-in. remote is the address the request came from. It returns
	// the status word the kiosk is told: ringing, busy, dnd, off or refused.
	Ring(req CallRequest, remote string) string
	// Signal is a kiosk's word on a call: answer, decline, missed, cancel or hangup. False when the
	// call is not one the bridge knows.
	Signal(call, action string) bool
	// HasCall reports whether the bridge expects an audio socket for call.
	HasCall(call string) bool
	// Audio hands over a call's audio socket, opened by the kiosk. The bridge owns and closes it.
	Audio(call string, ws *websocket.Conn)
}

// Standin is one stand-in kiosk: what it says about itself.
type Standin struct {
	ID      string
	Name    string
	Version string
	Port    int // identity and calls are both served on it
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
	CheckOrigin:     func(*http.Request) bool { return true }, // kiosks are not browsers; the token is the check
}

// Handler serves a stand-in kiosk's intercom routes.
func Handler(s Standin, auth *Auth, calls Calls) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/intercom/identity", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": s.ID, "name": s.Name, "version": s.Version, "enabled": true, "key": auth.KeyDigest(), "dnd": false,
			"endpoint": map[string]any{"port": s.Port, "tls": false},
		})
	})
	mux.HandleFunc("POST /api/intercom/call", func(w http.ResponseWriter, r *http.Request) {
		var req CallRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil || req.Call == "" || req.From.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "call and from required"})
			return
		}
		if !auth.Verify(bearer(r), req.Call) {
			writeJSON(w, http.StatusForbidden, map[string]any{"status": "key"})
			return
		}
		if req.From.TLS {
			writeJSON(w, http.StatusConflict, map[string]any{"status": "tls"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": calls.Ring(req, remoteHost(r))})
	})
	mux.HandleFunc("POST /api/intercom/call/{call}", func(w http.ResponseWriter, r *http.Request) {
		call := r.PathValue("call")
		if !auth.Verify(bearer(r), call) {
			writeJSON(w, http.StatusForbidden, map[string]any{})
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body) != nil || body.Action == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{})
			return
		}
		if !calls.Signal(call, body.Action) {
			writeJSON(w, http.StatusNotFound, map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/intercom/audio/{call}", func(w http.ResponseWriter, r *http.Request) {
		call := r.PathValue("call")
		if !auth.Verify(r.URL.Query().Get("token"), call) {
			writeJSON(w, http.StatusForbidden, map[string]any{})
			return
		}
		if !calls.HasCall(call) {
			writeJSON(w, http.StatusNotFound, map[string]any{})
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // the upgrader has answered
		}
		calls.Audio(call, ws)
	})
	return mux
}

func bearer(r *http.Request) string {
	v, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return v
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
