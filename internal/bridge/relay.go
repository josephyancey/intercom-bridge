package bridge

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"intercom-bridge/internal/ks"
	"intercom-bridge/internal/t5"
)

const (
	// backlogMost is the most kiosk audio held for the Echo, 0.5 s. A kiosk that bursts more than that
	// loses its oldest audio rather than putting the call further behind.
	backlogMost = 16000
	wsWriteFor  = 2 * time.Second
)

// Who ended a call.
const (
	endEcho   = "echo"
	endKiosk  = "kiosk"
	endBridge = "bridge"
)

// relay carries one answered call's audio. The kiosk's 80 ms frames go to the Echo as 20 ms frames on
// a 20 ms clock, with silence whenever the kiosk sends nothing (push to talk, or before its socket is
// up): an Echo hangs up on a call that goes quiet for 5 s. The Echo's frames are gathered into 80 ms
// frames for the kiosk.
type relay struct {
	link        *t5.Link
	toEchoGain  float64
	toKioskGain float64

	mu      sync.Mutex
	backlog []byte // kiosk audio waiting for the Echo
	ws      *websocket.Conn
	wsMu    sync.Mutex // one writer at a time on the socket

	// heardAt is when either side last sent audio above quietLevel, as Unix nanoseconds: what a
	// silence hangup measures from.
	heardAt    atomic.Int64
	quietLevel float64 // RMS, at int16 scale

	done    chan struct{}
	endOnce sync.Once
	by      string // endEcho, endKiosk or endBridge
	why     string
}

func newRelay(link *t5.Link, toEchoDB, toKioskDB, quietDBFS float64) *relay {
	r := &relay{link: link, toEchoGain: dbToGain(toEchoDB), toKioskGain: dbToGain(toKioskDB),
		quietLevel: math.MaxInt16 * dbToGain(quietDBFS), done: make(chan struct{})}
	r.heardAt.Store(time.Now().UnixNano())
	return r
}

// hear notes when a frame was louder than quiet.
func (r *relay) hear(pcm []byte) {
	if rms(pcm) > r.quietLevel {
		r.heardAt.Store(time.Now().UnixNano())
	}
}

// quietFor is how long neither side has sent anything above the quiet level.
func (r *relay) quietFor() time.Duration { return time.Since(time.Unix(0, r.heardAt.Load())) }

func dbToGain(db float64) float64 { return math.Pow(10, db/20) }

// start begins sending to the Echo and reading from it.
func (r *relay) start() {
	go r.pump()
	go r.fromEcho()
}

// attach adds the kiosk's audio socket: our talk state first, then audio both ways.
func (r *relay) attach(ws *websocket.Conn) {
	r.mu.Lock()
	r.ws = ws
	r.mu.Unlock()
	if err := r.writeText(map[string]any{"type": "talk", "on": true}); err != nil {
		r.end(endKiosk, "audio socket: "+err.Error())
		return
	}
	go r.fromKiosk(ws)
}

func (r *relay) end(by, why string) {
	r.endOnce.Do(func() {
		r.by, r.why = by, why
		close(r.done)
	})
}

// close hangs up both sides: the kiosk is told unless it was the one that ended the call.
func (r *relay) close() {
	r.end(endBridge, "closed")
	r.mu.Lock()
	ws := r.ws
	r.mu.Unlock()
	if ws != nil {
		if r.by != endKiosk {
			_ = r.writeText(map[string]any{"type": "end"})
		}
		r.wsMu.Lock()
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(500*time.Millisecond))
		r.wsMu.Unlock()
		_ = ws.Close()
	}
	r.link.Close()
}

func (r *relay) writeText(v any) error {
	b, _ := json.Marshal(v)
	return r.write(websocket.TextMessage, b)
}

func (r *relay) write(kind int, b []byte) error {
	r.mu.Lock()
	ws := r.ws
	r.mu.Unlock()
	if ws == nil {
		return nil
	}
	r.wsMu.Lock()
	defer r.wsMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(wsWriteFor))
	return ws.WriteMessage(kind, b)
}

// pump sends the Echo one 20 ms frame every 20 ms.
func (r *relay) pump() {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	silence := make([]byte, t5.FrameBytes)
	frame := make([]byte, t5.FrameBytes)
	for {
		select {
		case <-r.done:
			return
		case <-r.link.Gone:
			r.end(endEcho, "the Echo hung up")
			return
		case <-tick.C:
		}
		out := silence
		r.mu.Lock()
		if len(r.backlog) >= t5.FrameBytes {
			copy(frame, r.backlog)
			r.backlog = r.backlog[t5.FrameBytes:]
			out = frame
		}
		r.mu.Unlock()
		if err := r.link.SendAudio(out); err != nil {
			r.end(endEcho, "sending to the Echo: "+err.Error())
			return
		}
	}
}

// fromEcho gathers the Echo's frames and sends the kiosk one 80 ms frame per four.
func (r *relay) fromEcho() {
	var gathered []byte
	for {
		select {
		case <-r.done:
			return
		case <-r.link.Gone:
			r.end(endEcho, "the Echo hung up")
			return
		case b := <-r.link.Audio:
			r.hear(b)
			gathered = append(gathered, b...)
			for len(gathered) >= ks.FrameBytes {
				chunk := append([]byte(nil), gathered[:ks.FrameBytes]...)
				gathered = gathered[ks.FrameBytes:]
				applyGain(chunk, r.toKioskGain)
				if err := r.write(websocket.BinaryMessage, chunk); err != nil {
					r.end(endKiosk, "sending to the kiosk: "+err.Error())
					return
				}
			}
		}
	}
}

// fromKiosk reads the kiosk's socket: audio for the Echo, and its word that the call is over.
func (r *relay) fromKiosk(ws *websocket.Conn) {
	ws.SetReadLimit(64 << 10)
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			r.end(endKiosk, "the kiosk closed the audio socket")
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			if len(data)%2 != 0 {
				data = data[:len(data)-1]
			}
			r.hear(data)
			applyGain(data, r.toEchoGain)
			r.mu.Lock()
			r.backlog = append(r.backlog, data...)
			if over := len(r.backlog) - backlogMost; over > 0 {
				over += over % 2
				r.backlog = r.backlog[over:]
			}
			r.mu.Unlock()
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "end" {
				r.end(endKiosk, "the kiosk hung up")
				return
			}
			// {"type":"talk"} is the kiosk's push-to-talk state. Nothing to pass on: when it stops
			// talking it stops sending, and the Echo hears the pump's silence.
		}
	}
}

// rms is the root mean square of 16-bit little-endian samples, at int16 scale.
func rms(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// applyGain scales 16-bit little-endian samples in place, clipping at full scale.
func applyGain(pcm []byte, gain float64) {
	if gain == 1 {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) * gain
		v = math.Max(math.MinInt16, math.Min(math.MaxInt16, math.Round(v)))
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(v)))
	}
}
