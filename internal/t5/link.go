// Package t5 speaks TECHO5's intercom: an HTTP upgrade to /intercom, a Noise handshake keyed by the
// house word, then one-byte typed messages. It follows echod/internal/feature/phone/intercom.go.
package t5

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Path     = "/intercom"
	Protocol = "techo5-intercom/1" // the Upgrade asked for, and the handshake's prologue
	keyLabel = "techo5-intercom psk"

	// FrameBytes is one audio message: 20 ms of 16 kHz mono PCM16, little-endian.
	FrameBytes = 640
	// payloadMost is the largest message a device accepts (2 * wideFrame in echod).
	payloadMost = FrameBytes
	nameMost    = 40

	handshakeFor = 5 * time.Second
	ringFor      = 30 * time.Second
	waitQuiet    = ringFor + 10*time.Second // before an answer, only a few messages come
	talkQuiet    = 5 * time.Second          // once answered, audio every 20 ms
	writeFor     = 2 * time.Second
	goodbyeFor   = 500 * time.Millisecond
)

// The messages, one byte each.
const (
	MsgHello   = 'H' // the caller's name
	MsgRinging = 'R'
	MsgBusy    = 'B' // already on a call
	MsgAnswer  = 'A'
	MsgDecline = 'D' // declined, or nobody answered
	MsgNotNow  = 'N' // do not disturb
	MsgAudio   = 'S' // 20 ms of 16 kHz
	MsgBye     = 'X' // hung up
)

// Key is the house word as the intercom's handshake wants it.
func Key(houseWord string) []byte { return sealedKey(keyLabel, houseWord) }

func writeMsg(w io.Writer, t byte, payload []byte) error {
	b := make([]byte, 3+len(payload))
	b[0] = t
	binary.BigEndian.PutUint16(b[1:], uint16(len(payload)))
	copy(b[3:], payload)
	_, err := w.Write(b)
	return err
}

func readMsg(r io.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > payloadMost {
		return 0, nil, fmt.Errorf("intercom: a message of %d bytes", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return hdr[0], b, err
}

// ShownName is a caller's name made safe to show and pass on: printable and not too long.
func ShownName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > nameMost {
		s = string([]rune(s)[:nameMost])
	}
	if s == "" {
		return "Another room"
	}
	return s
}

// Link is one call's connection. One goroutine reads it and sorts what arrives: audio to Audio,
// everything else to Control. Gone closes when the other end hangs up or the connection ends.
type Link struct {
	c       *Conn
	Audio   chan []byte
	Control chan byte
	Gone    chan struct{}

	talking   atomic.Bool
	closeOnce sync.Once
}

func newLink(c *Conn) *Link {
	l := &Link{c: c, Audio: make(chan []byte, 25), Control: make(chan byte, 4), Gone: make(chan struct{})}
	go func() {
		defer close(l.Gone)
		for {
			quiet := waitQuiet
			if l.talking.Load() {
				quiet = talkQuiet
			}
			_ = c.SetReadDeadline(time.Now().Add(quiet))
			t, b, err := readMsg(c)
			if err != nil || t == MsgBye {
				return
			}
			if t == MsgAudio {
				select {
				case l.Audio <- b:
				default: // the reader is behind; this frame is lost rather than everything after it late
				}
				continue
			}
			select {
			case l.Control <- t:
			default:
			}
		}
	}()
	return l
}

// Say sends a control message.
func (l *Link) Say(t byte) error {
	_ = l.c.SetWriteDeadline(time.Now().Add(writeFor))
	return writeMsg(l.c, t, nil)
}

// SendAudio sends one 20 ms frame.
func (l *Link) SendAudio(frame []byte) error {
	_ = l.c.SetWriteDeadline(time.Now().Add(writeFor))
	return writeMsg(l.c, MsgAudio, frame)
}

// Talk marks the call answered, which shortens how long the other end may go quiet.
func (l *Link) Talk() { l.talking.Store(true) }

// Close says goodbye if the other end is still listening, and hangs up either way.
func (l *Link) Close() {
	l.closeOnce.Do(func() {
		_ = l.c.SetWriteDeadline(time.Now().Add(goodbyeFor))
		_ = writeMsg(l.c, MsgBye, nil)
		_ = l.c.Close()
	})
}

// bufConn reads through the buffer HTTP left behind, so nothing that arrived with the headers is lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Dial calls a device: the upgrade, the handshake, and Hello with the caller's name. It returns once
// the call is sent; the device's answer (Ringing, Busy, ...) arrives on Control.
func Dial(ctx context.Context, address string, port int, key []byte, callerName string) (*Link, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	_ = raw.SetDeadline(time.Now().Add(handshakeFor))
	if _, err := fmt.Fprintf(raw, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n",
		Path, address, Protocol); err != nil {
		raw.Close()
		return nil, err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		raw.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw.Close()
		return nil, fmt.Errorf("intercom: the device takes no calls (%s)", resp.Status)
	}
	c, err := sealedClient(&bufConn{Conn: raw, r: br}, Protocol, key)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if err := writeMsg(c, MsgHello, []byte(ShownName(callerName))); err != nil {
		raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return newLink(c), nil
}

// ErrNotIntercom is a request on the intercom path that is not an intercom upgrade.
var ErrNotIntercom = errors.New("intercom: not an intercom upgrade")

// Accept takes a device's call on an HTTP request: the upgrade, the handshake and its Hello. It
// returns the link and the caller's name. On ErrNotIntercom nothing has been written yet.
func Accept(w http.ResponseWriter, r *http.Request, key []byte) (*Link, string, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) {
		return nil, "", ErrNotIntercom
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, "", errors.New("intercom: cannot take the connection")
	}
	raw, rw, err := hj.Hijack()
	if err != nil {
		return nil, "", err
	}
	_ = raw.SetDeadline(time.Now().Add(handshakeFor))
	if _, err := fmt.Fprintf(raw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", Protocol); err != nil {
		raw.Close()
		return nil, "", err
	}
	c, err := sealedServer(&bufConn{Conn: raw, r: rw.Reader}, Protocol, key)
	if err != nil {
		raw.Close()
		return nil, "", err
	}
	t, name, err := readMsg(c)
	if err != nil || t != MsgHello {
		raw.Close()
		if err == nil {
			err = errors.New("intercom: the caller did not say who it is")
		}
		return nil, "", err
	}
	_ = raw.SetDeadline(time.Time{})
	return newLink(c), ShownName(string(name)), nil
}
