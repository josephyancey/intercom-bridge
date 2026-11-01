package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"intercom-bridge/internal/ks"
	"intercom-bridge/internal/t5"
)

const (
	ringFor    = 30 * time.Second // an Echo rings this long; a kiosk caller is told "missed" after it
	socketWait = 10 * time.Second // after an answer, how long the kiosk has to open the audio socket
	requestFor = 4 * time.Second
	wayToEcho  = "kiosk to Echo"
	wayToKiosk = "Echo to kiosk"
)

// session is one call through the bridge, identified by its Kiosk Satellite call id.
type session struct {
	id        string
	way       string
	kioskName string
	echoName  string
	link      *t5.Link

	kioskHost  string // the real kiosk
	kioskAdmin int    // its admin port, for identity
	endpoint   int    // its call port, read from identity
	fromID     string // the stand-in kiosk this call is signed as

	signals  chan string          // the kiosk's words: answer, decline, missed, cancel, hangup
	sockets  chan *websocket.Conn // the audio socket a calling kiosk opens
	started  time.Time
	answered atomic.Bool
}

func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (b *Bridge) addCall(s *session) {
	b.mu.Lock()
	b.calls[s.id] = s
	b.mu.Unlock()
}

func (b *Bridge) dropCall(s *session) {
	b.mu.Lock()
	delete(b.calls, s.id)
	b.mu.Unlock()
}

func (b *Bridge) call(id string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}

// standinCalls is one stand-in kiosk's side of ks.Calls.
type standinCalls struct {
	b *Bridge
	t *echoTwin
}

func (c *standinCalls) Ring(req ks.CallRequest, remote string) string {
	if req.Kind == "broadcast" {
		return c.b.kioskAnnouncesToEcho(c.t, req)
	}
	return c.b.kioskCallsEcho(c.t, req, remote)
}

func (c *standinCalls) Signal(call, action string) bool {
	s := c.b.call(call)
	if s == nil {
		return false
	}
	select {
	case s.signals <- action:
	default:
	}
	return true
}

func (c *standinCalls) HasCall(call string) bool { return c.b.call(call) != nil }

func (c *standinCalls) Audio(call string, ws *websocket.Conn) {
	s := c.b.call(call)
	if s == nil {
		_ = ws.Close()
		return
	}
	select {
	case s.sockets <- ws:
	default:
		_ = ws.Close() // a second socket for one call
	}
}

// signalKiosk tells the kiosk in a call what happened, at its call port.
func (b *Bridge) signalKiosk(s *session, action string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestFor)
	defer cancel()
	if s.endpoint == 0 {
		id, err := b.ksc.Identity(ctx, s.kioskHost, s.kioskAdmin)
		if err != nil {
			b.fail("telling %s %q: %v", s.kioskName, action, err)
			return err
		}
		s.endpoint = id.Endpoint.Port
	}
	if err := b.ksc.Signal(ctx, s.kioskHost, s.endpoint, s.id, s.fromID, action); err != nil {
		b.fail("telling %s %q: %v", s.kioskName, action, err)
		return err
	}
	return nil
}

// kioskCallsEcho is a kiosk calling an Echo's stand-in. It calls the Echo, and its first answer is
// what the kiosk is told.
func (b *Bridge) kioskCallsEcho(t *echoTwin, req ks.CallRequest, remote string) string {
	b.mu.Lock()
	echo := t.e
	b.mu.Unlock()
	caller := req.From.Name
	if caller == "" {
		caller = "Kiosk"
	}
	log := b.log.With("way", wayToEcho, "kiosk", caller, "echo", echo.Name, "call", req.Call)
	if req.Kind != "call" {
		log.Info("call of an unknown kind turned away", "kind", req.Kind)
		return "refused"
	}
	host := req.From.Address
	if host == "" || host == "127.0.0.1" {
		host = remote
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestFor)
	defer cancel()
	link, err := t5.Dial(ctx, echo.Address, echo.Port, b.t5key, caller)
	if err != nil {
		if errors.Is(err, t5.ErrKey) {
			b.fail("%s refused the call: the house word does not match", echo.Name)
		} else {
			b.fail("calling %s: %v", echo.Name, err)
		}
		return "off"
	}
	var first byte
	select {
	case first = <-link.Control:
	case <-link.Gone:
		b.fail("%s hung up before ringing; check the house word", echo.Name)
		return "off"
	case <-ctx.Done():
		link.Close()
		b.fail("%s did not answer the call in time", echo.Name)
		return "off"
	}
	switch first {
	case t5.MsgRinging:
	case t5.MsgBusy:
		link.Close()
		log.Info("Echo busy")
		return "busy"
	case t5.MsgNotNow:
		link.Close()
		log.Info("Echo in do not disturb")
		return "dnd"
	default:
		link.Close()
		log.Info("Echo turned the call away")
		return "refused"
	}
	s := &session{
		id: req.Call, way: wayToEcho, kioskName: caller, echoName: echo.Name, link: link,
		kioskHost: host, kioskAdmin: req.From.Port, fromID: t.id,
		signals: make(chan string, 8), sockets: make(chan *websocket.Conn, 1), started: time.Now(),
	}
	b.addCall(s)
	log.Info("ringing")
	go b.runKioskToEcho(s)
	return "ringing"
}

func (b *Bridge) runKioskToEcho(s *session) {
	defer b.dropCall(s)
	log := b.log.With("way", s.way, "kiosk", s.kioskName, "echo", s.echoName, "call", s.id)
	ring := time.NewTimer(ringFor + 5*time.Second)
	defer ring.Stop()
wait:
	for {
		select {
		case m := <-s.link.Control:
			switch m {
			case t5.MsgAnswer:
				break wait
			case t5.MsgDecline, t5.MsgBusy, t5.MsgNotNow:
				action := "decline"
				if time.Since(s.started) >= ringFor-2*time.Second {
					action = "missed"
				}
				s.link.Close()
				log.Info("not answered", "reason", action)
				_ = b.signalKiosk(s, action)
				return
			}
		case <-s.link.Gone:
			log.Info("the Echo dropped the call while ringing")
			_ = b.signalKiosk(s, "decline")
			return
		case a := <-s.signals:
			if a == "cancel" || a == "hangup" {
				log.Info("the kiosk gave up")
				s.link.Close()
				return
			}
		case <-ring.C:
			s.link.Close()
			_ = b.signalKiosk(s, "missed")
			return
		}
	}
	s.answered.Store(true)
	s.link.Talk()
	rl := newRelay(s.link, b.cfg.ToEchoDB, b.cfg.ToKioskDB, b.cfg.QuietDBFS)
	rl.start() // the Echo hears silence until the kiosk's socket is up
	if b.signalKiosk(s, "answer") != nil {
		rl.close()
		return
	}
	log.Info("answered")
	select {
	case ws := <-s.sockets:
		rl.attach(ws)
	case a := <-s.signals:
		if a == "cancel" || a == "hangup" {
			rl.end(endKiosk, "the kiosk hung up")
		}
	case <-rl.done:
	case <-time.After(socketWait):
		rl.end(endBridge, "the kiosk did not open the audio socket")
	}
	b.carry(s, rl, log)
}

// serveEchoCall is a request on a kiosk's stand-in TECHO5 device: an Echo calling that kiosk.
func (b *Bridge) serveEchoCall(t *kioskTwin, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == t5.AnnouncePath {
		b.echoAnnounces(t, w, r)
		return
	}
	if r.URL.Path != t5.Path {
		http.NotFound(w, r)
		return
	}
	link, caller, err := t5.Accept(w, r, b.t5key)
	if errors.Is(err, t5.ErrNotIntercom) {
		http.Error(w, "an intercom call only", http.StatusBadRequest)
		return
	}
	if err != nil {
		if errors.Is(err, t5.ErrKey) {
			b.fail("an Echo at %s called with a different house word", r.RemoteAddr)
		} else {
			b.fail("an Echo's call to %s: %v", t.k.Name, err)
		}
		return
	}
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	b.echoCallsKiosk(t, link, caller, remote)
}

// echoTwinFor is the Echo a call came from: by the name it gave, else by its address.
func (b *Bridge) echoTwinFor(name, address string) *echoTwin {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.echos[strings.ToLower(name)]; t != nil {
		return t
	}
	for _, t := range b.echos {
		if t.e.Address == address {
			return t
		}
	}
	return nil
}

func (b *Bridge) echoCallsKiosk(t *kioskTwin, link *t5.Link, caller, remote string) {
	b.mu.Lock()
	kiosk := t.k
	b.mu.Unlock()
	log := b.log.With("way", wayToKiosk, "kiosk", kiosk.Name, "echo", caller)
	refuse := func(m byte, why string) {
		log.Info("call not placed", "reason", why)
		_ = link.Say(m)
		link.Close()
	}
	et := b.echoTwinFor(caller, remote)
	if et == nil {
		b.fail("%s called %s, but the bridge has not found that Echo yet", caller, kiosk.Name)
		refuse(t5.MsgDecline, "unknown Echo")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestFor)
	defer cancel()
	id, err := b.ksc.Identity(ctx, kiosk.Address, kiosk.AdminPort)
	switch {
	case err != nil:
		b.fail("reading %s: %v", kiosk.Name, err)
		refuse(t5.MsgDecline, "kiosk unreachable")
		return
	case !id.Enabled:
		refuse(t5.MsgNotNow, "intercom off on the kiosk")
		return
	case !b.ksc.KeyMatches(id):
		b.fail("%s has a different intercom key", kiosk.Name)
		refuse(t5.MsgDecline, "intercom key mismatch")
		return
	case id.Endpoint.TLS:
		b.fail("%s has intercom encryption on, which the bridge does not speak yet", kiosk.Name)
		refuse(t5.MsgDecline, "kiosk wants TLS")
		return
	case id.DND:
		refuse(t5.MsgNotNow, "kiosk in do not disturb")
		return
	}
	s := &session{
		id: newCallID(), way: wayToKiosk, kioskName: kiosk.Name, echoName: et.e.Name, link: link,
		kioskHost: kiosk.Address, kioskAdmin: kiosk.AdminPort, endpoint: id.Endpoint.Port, fromID: et.id,
		signals: make(chan string, 8), sockets: make(chan *websocket.Conn, 1), started: time.Now(),
	}
	log = log.With("call", s.id)
	b.addCall(s) // before ringing: a kiosk that answers by itself answers at once
	defer b.dropCall(s)
	status, err := b.ksc.Call(ctx, kiosk.Address, s.endpoint, ks.CallRequest{
		Call: s.id, Kind: "call",
		From: ks.Peer{ID: et.id, Name: et.name, Address: b.cfg.Advertise, Port: et.port, Version: b.cfg.Version},
	})
	if err != nil {
		b.fail("calling %s: %v", kiosk.Name, err)
		refuse(t5.MsgDecline, "call failed")
		return
	}
	switch status {
	case "ringing", "auto", "listening":
	case "busy":
		refuse(t5.MsgBusy, status)
		return
	case "dnd", "off", "refused":
		refuse(t5.MsgNotNow, status)
		return
	default:
		b.fail("%s refused the call: %s", kiosk.Name, status)
		refuse(t5.MsgDecline, status)
		return
	}
	_ = link.Say(t5.MsgRinging)
	log.Info("ringing")
	ring := time.NewTimer(ringFor)
	defer ring.Stop()
wait:
	for {
		select {
		case a := <-s.signals:
			switch a {
			case "answer":
				break wait
			case "decline", "missed", "cancel", "hangup":
				refuse(t5.MsgDecline, a)
				return
			}
		case <-link.Gone:
			log.Info("the Echo gave up")
			_ = b.signalKiosk(s, "cancel")
			return
		case <-link.Control:
		case <-ring.C:
			_ = b.signalKiosk(s, "cancel")
			refuse(t5.MsgDecline, "no answer")
			return
		}
	}
	s.answered.Store(true)
	if err := link.Say(t5.MsgAnswer); err != nil {
		_ = b.signalKiosk(s, "hangup")
		link.Close()
		return
	}
	link.Talk()
	rl := newRelay(link, b.cfg.ToEchoDB, b.cfg.ToKioskDB, b.cfg.QuietDBFS)
	rl.start()
	log.Info("answered")
	actx, acancel := context.WithTimeout(context.Background(), requestFor)
	ws, err := b.ksc.DialAudio(actx, kiosk.Address, s.endpoint, s.id, et.id)
	acancel()
	if err != nil {
		b.fail("opening the audio socket to %s: %v", kiosk.Name, err)
		rl.end(endBridge, "no audio socket")
	} else {
		rl.attach(ws)
	}
	b.carry(s, rl, log)
}

// carry waits out an answered call, then hangs up both sides.
func (b *Bridge) carry(s *session, rl *relay, log interface{ Info(string, ...any) }) {
	limit := time.NewTimer(b.cfg.MaxCall)
	defer limit.Stop()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	for done := false; !done; {
		select {
		case <-rl.done:
			done = true
		case a := <-s.signals:
			if a == "hangup" || a == "cancel" {
				rl.end(endKiosk, "the kiosk hung up")
			}
		case <-limit.C:
			rl.end(endBridge, "maximum call length")
		case <-check.C:
			if b.cfg.SilenceHangup > 0 && rl.quietFor() >= b.cfg.SilenceHangup {
				rl.end(endBridge, "nobody spoke for "+b.cfg.SilenceHangup.String())
			}
		}
	}
	rl.close()
	if rl.by != endKiosk {
		_ = b.signalKiosk(s, "hangup")
	}
	log.Info("call ended", "by", rl.by, "why", rl.why, "length", time.Since(s.started).Round(time.Second))
}
