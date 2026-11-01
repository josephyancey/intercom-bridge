package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"intercom-bridge/internal/discover"
	"intercom-bridge/internal/ks"
	"intercom-bridge/internal/t5"
)

const (
	testKey  = "fleet-key"
	testWord = "house-word"
	kioskID  = "00112233aabbccdd"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b := New(Config{
		IntercomKey: testKey, HouseWord: testWord, Advertise: "127.0.0.1", Listen: "127.0.0.1",
		PortLow: 47100, PortHigh: 47199, BrowseEvery: time.Minute, BrowseFor: time.Second,
		MaxCall: time.Minute, Version: "test", NoAdvertise: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(b.Close)
	return b
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func frameOf(n int, v int16) []byte {
	b := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

func firstSample(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b)) }

// fakeKiosk is a Kiosk Satellite kiosk: identity on an admin port, calls and audio on an endpoint
// port, every token checked as a kiosk checks it.
type fakeKiosk struct {
	t        *testing.T
	auth     *ks.Auth
	admin    *httptest.Server
	endpoint *httptest.Server

	mu        sync.Mutex
	lastCall  ks.CallRequest
	callStatus string
	actions   chan string
	calls     chan ks.CallRequest
	sockets   chan *websocket.Conn
}

func newFakeKiosk(t *testing.T) *fakeKiosk {
	k := &fakeKiosk{t: t, auth: ks.NewAuth(testKey), callStatus: "ringing",
		actions: make(chan string, 8), calls: make(chan ks.CallRequest, 2), sockets: make(chan *websocket.Conn, 2)}
	k.endpoint = httptest.NewServer(http.HandlerFunc(k.serveEndpoint))
	k.admin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, port := hostPort(t, k.endpoint.Listener.Addr().String())
		_ = json.NewEncoder(w).Encode(map[string]any{"id": kioskID, "name": "Kitchen", "version": "1", "enabled": true,
			"key": ks.Digest(testKey), "dnd": false, "endpoint": map[string]any{"port": port, "tls": false}})
	}))
	t.Cleanup(k.admin.Close)
	t.Cleanup(k.endpoint.Close)
	return k
}

func (k *fakeKiosk) serveEndpoint(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/intercom/call":
		var req ks.CallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !k.auth.Verify(bearer, req.Call) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		k.mu.Lock()
		k.lastCall = req
		status := k.callStatus
		k.mu.Unlock()
		k.calls <- req
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/intercom/call/"):
		call := strings.TrimPrefix(r.URL.Path, "/api/intercom/call/")
		if !k.auth.Verify(bearer, call) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body struct{ Action string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.actions <- body.Action
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case strings.HasPrefix(r.URL.Path, "/api/intercom/audio/"):
		call := strings.TrimPrefix(r.URL.Path, "/api/intercom/audio/")
		if !k.auth.Verify(r.URL.Query().Get("token"), call) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		up := websocket.Upgrader{}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		k.sockets <- ws
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (k *fakeKiosk) discovered() discover.Kiosk {
	host, port := hostPort(k.t, k.admin.Listener.Addr().String())
	return discover.Kiosk{ID: kioskID, Name: "Kitchen", Address: host, AdminPort: port}
}

// post is the kiosk calling out, signed with its key.
func (k *fakeKiosk) post(url, call string, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+k.auth.Token(call, kioskID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// fakeEcho is a TECHO5 device that answers calls on its web port.
type fakeEcho struct {
	srv   *httptest.Server
	links chan *t5.Link
	names chan string
	reply []byte // what it says once called: Ringing, then Answer
}

func newFakeEcho(t *testing.T, reply ...byte) *fakeEcho {
	e := &fakeEcho{links: make(chan *t5.Link, 2), names: make(chan string, 2), reply: reply}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l, name, err := t5.Accept(w, r, t5.Key(testWord))
		if err != nil {
			return
		}
		e.names <- name
		for _, m := range e.reply {
			_ = l.Say(m)
			time.Sleep(50 * time.Millisecond)
		}
		e.links <- l
		<-l.Gone
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeEcho) discovered(t *testing.T, name string) discover.Echo {
	host, port := hostPort(t, e.srv.Listener.Addr().String())
	return discover.Echo{Name: name, Address: host, Port: port}
}

func standinPort(t *testing.T, list []StatusTwin, name string) int {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s.StandinPort
		}
	}
	t.Fatalf("no stand-in for %q", name)
	return 0
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// readAudio reads link frames until one is not silence.
func echoHears(t *testing.T, l *t5.Link) []byte {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b := <-l.Audio:
			if firstSample(b) != 0 {
				return b
			}
		case <-deadline:
			t.Fatal("the Echo heard only silence")
			return nil
		}
	}
}

func kioskHears(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("kiosk socket: %v", err)
		}
		if kind == websocket.BinaryMessage {
			return b
		}
	}
}

func TestKioskCallsEcho(t *testing.T) {
	b := newTestBridge(t)
	kiosk := newFakeKiosk(t)
	echo := newFakeEcho(t, t5.MsgRinging, t5.MsgAnswer)
	b.SetEchos([]discover.Echo{echo.discovered(t, "Garage Echo")})
	port := standinPort(t, b.Status().Echos, "Garage Echo")
	standin := "http://127.0.0.1:" + strconv.Itoa(port)

	// The stand-in introduces itself as a kiosk with our key.
	resp, err := http.Get(standin + "/api/intercom/identity")
	if err != nil {
		t.Fatal(err)
	}
	var id ks.Identity
	_ = json.NewDecoder(resp.Body).Decode(&id)
	resp.Body.Close()
	if id.ID != StandinID("Garage Echo") || id.Name != "Garage Echo" || id.Key != ks.Digest(testKey) || id.Endpoint.Port != port {
		t.Fatalf("identity %+v", id)
	}

	_, adminPort := hostPort(t, kiosk.admin.Listener.Addr().String())
	call := "call-1"
	code, out := kiosk.post(standin+"/api/intercom/call", call, map[string]any{
		"call": call, "kind": "call",
		"from": map[string]any{"id": kioskID, "name": "Kitchen", "address": "127.0.0.1", "port": adminPort, "version": "1"},
	})
	if code != http.StatusOK || out["status"] != "ringing" {
		t.Fatalf("call answered %d %v", code, out)
	}
	if n := waitFor(t, echo.names, "the Echo to ring"); n != "Kitchen" {
		t.Fatalf("the Echo saw the caller as %q", n)
	}
	l := waitFor(t, echo.links, "the Echo to answer")
	if a := waitFor(t, kiosk.actions, "the answer"); a != "answer" {
		t.Fatalf("the kiosk was told %q", a)
	}

	// The kiosk opens the audio socket on the stand-in, as a calling kiosk does.
	wsURL := url.URL{Scheme: "ws", Host: "127.0.0.1:" + strconv.Itoa(port), Path: "/api/intercom/audio/" + call,
		RawQuery: url.Values{"token": {kiosk.auth.Token(call, kioskID)}}.Encode()}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	_ = ws.WriteMessage(websocket.BinaryMessage, frameOf(ks.FrameBytes, 1000))
	if got := echoHears(t, l); len(got) != t5.FrameBytes || firstSample(got) != 1000 {
		t.Fatalf("the Echo heard %d bytes of %d", len(got), firstSample(got))
	}
	for i := 0; i < 4; i++ {
		_ = l.SendAudio(frameOf(t5.FrameBytes, 2000))
	}
	if got := kioskHears(t, ws); len(got) != ks.FrameBytes || firstSample(got) != 2000 {
		t.Fatalf("the kiosk heard %d bytes of %d", len(got), firstSample(got))
	}

	// The kiosk hangs up: the Echo is told.
	_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"end"}`))
	waitFor(t, l.Gone, "the Echo to be hung up on")
}

func TestKioskCallsBusyEcho(t *testing.T) {
	b := newTestBridge(t)
	kiosk := newFakeKiosk(t)
	echo := newFakeEcho(t, t5.MsgBusy)
	b.SetEchos([]discover.Echo{echo.discovered(t, "Garage Echo")})
	port := standinPort(t, b.Status().Echos, "Garage Echo")
	_, adminPort := hostPort(t, kiosk.admin.Listener.Addr().String())
	code, out := kiosk.post("http://127.0.0.1:"+strconv.Itoa(port)+"/api/intercom/call", "c2", map[string]any{
		"call": "c2", "kind": "call", "from": map[string]any{"id": kioskID, "name": "Kitchen", "address": "127.0.0.1", "port": adminPort},
	})
	if code != http.StatusOK || out["status"] != "busy" {
		t.Fatalf("call answered %d %v", code, out)
	}
}

func TestBadTokenRefused(t *testing.T) {
	b := newTestBridge(t)
	echo := newFakeEcho(t, t5.MsgRinging)
	b.SetEchos([]discover.Echo{echo.discovered(t, "Garage Echo")})
	port := standinPort(t, b.Status().Echos, "Garage Echo")
	stranger := ks.NewAuth("not-the-key")
	raw, _ := json.Marshal(map[string]any{"call": "c3", "kind": "call", "from": map[string]any{"id": "x"}})
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/intercom/call", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+stranger.Token("c3", "x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a call with another key got HTTP %d", resp.StatusCode)
	}
}

func TestEchoCallsKiosk(t *testing.T) {
	b := newTestBridge(t)
	kiosk := newFakeKiosk(t)
	echo := newFakeEcho(t)
	b.SetEchos([]discover.Echo{echo.discovered(t, "Garage Echo")})
	b.SetKiosks([]discover.Kiosk{kiosk.discovered()})
	port := standinPort(t, b.Status().Kiosks, "Kitchen")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := t5.Dial(ctx, "127.0.0.1", port, t5.Key(testWord), "Garage Echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	req := waitFor(t, kiosk.calls, "the kiosk to ring")
	if req.Kind != "call" || req.From.ID != StandinID("Garage Echo") || req.From.Name != "Garage Echo" {
		t.Fatalf("the kiosk was called as %+v", req)
	}
	if m := waitFor(t, l.Control, "ringing"); m != t5.MsgRinging {
		t.Fatalf("the Echo heard %q", m)
	}

	// The person at the kiosk answers: the kiosk tells the caller at from.address:from.port.
	code, _ := kiosk.post("http://"+net.JoinHostPort(req.From.Address, strconv.Itoa(req.From.Port))+"/api/intercom/call/"+req.Call,
		req.Call, map[string]string{"action": "answer"})
	if code != http.StatusOK {
		t.Fatalf("the answer got HTTP %d", code)
	}
	if m := waitFor(t, l.Control, "the answer"); m != t5.MsgAnswer {
		t.Fatalf("the Echo heard %q", m)
	}
	l.Talk()

	// The bridge opens the audio socket on the kiosk, talk on first.
	ws := waitFor(t, kiosk.sockets, "the audio socket")
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, first, err := ws.ReadMessage()
	if err != nil || kind != websocket.TextMessage || !strings.Contains(string(first), `"talk"`) {
		t.Fatalf("the socket opened with %d %q %v", kind, first, err)
	}
	_ = ws.WriteMessage(websocket.BinaryMessage, frameOf(ks.FrameBytes, 1500))
	if got := echoHears(t, l); firstSample(got) != 1500 {
		t.Fatalf("the Echo heard %d", firstSample(got))
	}
	for i := 0; i < 4; i++ {
		_ = l.SendAudio(frameOf(t5.FrameBytes, -900))
	}
	if got := kioskHears(t, ws); firstSample(got) != -900 {
		t.Fatalf("the kiosk heard %d", firstSample(got))
	}

	// The Echo hangs up: the kiosk hears "end" and is told hangup.
	l.Close()
	if a := waitFor(t, kiosk.actions, "hangup"); a != "hangup" {
		t.Fatalf("the kiosk was told %q", a)
	}
}

func TestEchoCallsDecliningKiosk(t *testing.T) {
	b := newTestBridge(t)
	kiosk := newFakeKiosk(t)
	echo := newFakeEcho(t)
	b.SetEchos([]discover.Echo{echo.discovered(t, "Garage Echo")})
	b.SetKiosks([]discover.Kiosk{kiosk.discovered()})
	port := standinPort(t, b.Status().Kiosks, "Kitchen")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := t5.Dial(ctx, "127.0.0.1", port, t5.Key(testWord), "Garage Echo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	req := waitFor(t, kiosk.calls, "the kiosk to ring")
	waitFor(t, l.Control, "ringing")
	kiosk.post("http://"+net.JoinHostPort(req.From.Address, strconv.Itoa(req.From.Port))+"/api/intercom/call/"+req.Call,
		req.Call, map[string]string{"action": "decline"})
	if m := waitFor(t, l.Control, "the decline"); m != t5.MsgDecline {
		t.Fatalf("the Echo heard %q", m)
	}
}

func TestOwnStandinsIgnored(t *testing.T) {
	b := newTestBridge(t)
	b.SetEchos([]discover.Echo{{Name: "Kitchen", Address: "127.0.0.1", Port: 47100}})
	if len(b.Status().Echos) != 0 {
		t.Fatal("the bridge took its own stand-in for an Echo")
	}
	b.SetEchos([]discover.Echo{{Name: "x", Address: "10.0.0.9", Port: 8181}})
	b.SetKiosks([]discover.Kiosk{{ID: StandinID("x"), Name: "x", Address: "127.0.0.1", AdminPort: 1}})
	for _, k := range b.Status().Kiosks {
		if k.Name == "x" {
			t.Fatal("the bridge took its own stand-in for a kiosk")
		}
	}
}
