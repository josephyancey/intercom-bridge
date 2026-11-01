package bridge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"intercom-bridge/internal/discover"
	"intercom-bridge/internal/ks"
	"intercom-bridge/internal/t5"
)

// Announcements cross over too. A TECHO5 device announces by sending its recorded voice to every
// device it knows, the kiosks' stand-ins among them: each is played on its kiosk as a Kiosk Satellite
// broadcast, with the kiosk's chime. A kiosk's Announce to all reaches each Echo's stand-in as a
// broadcast: the bridge gathers it and sends it on to the Echo as a TECHO5 announcement once the
// kiosk has finished, since a TECHO5 announcement is a recording, not a stream.

const (
	wayAnnounceToKiosk = "Echo announcement"
	wayAnnounceToEcho  = "kiosk announcement"
	announceListen     = 25 * time.Second // the most a kiosk's broadcast is gathered for
)

// echoAnnounces is an announcement on a kiosk's stand-in TECHO5 device.
func (b *Bridge) echoAnnounces(t *kioskTwin, w http.ResponseWriter, r *http.Request) {
	m, ok := b.ann.Receive(w, r)
	b.mu.Lock()
	kiosk := t.k
	b.mu.Unlock()
	if !ok {
		b.fail("an announcement for %s from %s was refused (a different house word, or a clock far off)", kiosk.Name, r.RemoteAddr)
		return
	}
	log := b.log.With("way", wayAnnounceToKiosk, "kiosk", kiosk.Name, "echo", m.From)
	switch {
	case m.Kind != "":
		log.Debug("not carried: a kiosk has no use for it", "kind", m.Kind)
		return
	case len(m.Voice) == 0:
		log.Info("not carried: an announcement of words only has no voice to play", "text", m.Text)
		return
	}
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	go b.playOnKiosk(kiosk, m, remote)
}

// playOnKiosk plays an Echo's announcement on a kiosk as a broadcast.
func (b *Bridge) playOnKiosk(kiosk discover.Kiosk, m t5.Announcement, remote string) {
	log := b.log.With("way", wayAnnounceToKiosk, "kiosk", kiosk.Name, "echo", m.From)
	et := b.echoTwinFor(m.From, remote)
	if et == nil {
		b.fail("%s announced, but the bridge has not found that Echo yet", m.From)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestFor)
	id, err := b.ksc.Identity(ctx, kiosk.Address, kiosk.AdminPort)
	cancel()
	switch {
	case err != nil:
		b.fail("reading %s: %v", kiosk.Name, err)
		return
	case !id.Enabled || id.DND:
		log.Info("not played: the kiosk's intercom is off or in do not disturb")
		return
	case !b.ksc.KeyMatches(id):
		b.fail("%s has a different intercom key", kiosk.Name)
		return
	case id.Endpoint.TLS:
		b.fail("%s has intercom encryption on, which the bridge does not speak yet", kiosk.Name)
		return
	}
	s := &session{
		id: newCallID(), way: wayAnnounceToKiosk, kioskName: kiosk.Name, echoName: et.e.Name,
		kioskHost: kiosk.Address, kioskAdmin: kiosk.AdminPort, endpoint: id.Endpoint.Port, fromID: et.id,
		signals: make(chan string, 8), sockets: make(chan *websocket.Conn, 1), started: time.Now(),
	}
	b.addCall(s)
	defer b.dropCall(s)
	ctx, cancel = context.WithTimeout(context.Background(), requestFor)
	status, err := b.ksc.Call(ctx, kiosk.Address, s.endpoint, ks.CallRequest{
		Call: s.id, Kind: "broadcast",
		From: ks.Peer{ID: et.id, Name: et.name, Address: b.cfg.Advertise, Port: et.port, Version: b.cfg.Version},
	})
	cancel()
	if err != nil {
		b.fail("announcing on %s: %v", kiosk.Name, err)
		return
	}
	if status != "listening" && status != "auto" && status != "ringing" {
		log.Info("not played", "kiosk_said", status)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), requestFor)
	ws, err := b.ksc.DialAudio(ctx, kiosk.Address, s.endpoint, s.id, et.id)
	cancel()
	if err != nil {
		b.fail("opening the audio socket to %s: %v", kiosk.Name, err)
		_ = b.signalKiosk(s, "hangup")
		return
	}
	defer ws.Close()
	gone := make(chan struct{})
	go func() { // the kiosk's side: Dismiss on its card ends the broadcast
		defer close(gone)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	send := func(kind int, p []byte) error {
		_ = ws.SetWriteDeadline(time.Now().Add(wsWriteFor))
		return ws.WriteMessage(kind, p)
	}
	talk, _ := json.Marshal(map[string]any{"type": "talk", "on": true})
	if send(websocket.TextMessage, talk) != nil {
		return
	}
	voice := append([]byte(nil), m.Voice...)
	applyGain(voice, dbToGain(b.cfg.ToKioskDB))
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	dismissed := false
	for i := 0; i < len(voice) && !dismissed; i += ks.FrameBytes {
		frame := make([]byte, ks.FrameBytes)
		copy(frame, voice[i:])
		if send(websocket.BinaryMessage, frame) != nil {
			break
		}
		select {
		case <-tick.C:
		case <-gone:
			dismissed = true
		case a := <-s.signals:
			dismissed = a == "hangup" || a == "cancel"
		}
	}
	if !dismissed {
		end, _ := json.Marshal(map[string]any{"type": "end"})
		_ = send(websocket.TextMessage, end)
		_ = b.signalKiosk(s, "hangup")
	}
	log.Info("announcement played", "seconds", float64(len(m.Voice))/32000, "dismissed", dismissed)
}

// kioskAnnouncesToEcho is a kiosk's Announce to all reaching an Echo's stand-in.
func (b *Bridge) kioskAnnouncesToEcho(t *echoTwin, req ks.CallRequest) string {
	b.mu.Lock()
	echo := t.e
	b.mu.Unlock()
	caller := req.From.Name
	if caller == "" {
		caller = "Kiosk"
	}
	s := &session{
		id: req.Call, way: wayAnnounceToEcho, kioskName: caller, echoName: echo.Name,
		signals: make(chan string, 8), sockets: make(chan *websocket.Conn, 1), started: time.Now(),
	}
	b.addCall(s)
	go b.gatherForEcho(s, echo)
	return "listening"
}

// gatherForEcho takes a kiosk's broadcast off its audio socket and, once it ends, sends it to the
// Echo as an announcement.
func (b *Bridge) gatherForEcho(s *session, echo discover.Echo) {
	defer b.dropCall(s)
	log := b.log.With("way", wayAnnounceToEcho, "kiosk", s.kioskName, "echo", echo.Name, "call", s.id)
	var ws *websocket.Conn
	select {
	case ws = <-s.sockets:
	case <-s.signals:
		return
	case <-time.After(socketWait):
		log.Info("the kiosk did not open the audio socket")
		return
	}
	var voice []byte
	_ = ws.SetReadDeadline(time.Now().Add(announceListen))
	ws.SetReadLimit(64 << 10)
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.BinaryMessage {
			if room := t5.MaxAnnouncement - len(voice); room > 0 {
				voice = append(voice, data[:min(len(data), room)]...)
			}
			continue
		}
		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Type == "end" {
			break
		}
	}
	_ = ws.Close()
	if len(voice)%2 != 0 {
		voice = voice[:len(voice)-1]
	}
	if len(voice) == 0 {
		log.Info("nothing was said")
		return
	}
	applyGain(voice, dbToGain(b.cfg.ToEchoDB))
	if err := b.ann.Send(context.Background(), echo.Address, echo.Port, t5.Announcement{From: s.kioskName, Voice: voice}); err != nil {
		b.fail("announcing on %s: %v", echo.Name, err)
		return
	}
	log.Info("announcement passed on", "seconds", float64(len(voice))/32000)
}
