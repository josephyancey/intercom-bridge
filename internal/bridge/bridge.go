// Package bridge lets Kiosk Satellite kiosks and TECHO5 Echos call each other. Every Echo gets a
// stand-in kiosk that the kiosks list and call; every kiosk gets a stand-in TECHO5 device that the
// Echos list and call. A call to a stand-in becomes a call to the real device, and the bridge carries
// the audio between the two.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"intercom-bridge/internal/discover"
	"intercom-bridge/internal/ks"
	"intercom-bridge/internal/t5"
)

// Config is how the bridge is set up.
type Config struct {
	IntercomKey string // the fleet's Kiosk Satellite intercom key
	HouseWord   string // the TECHO5 house word
	Advertise   string // the address kiosks and Echos reach the bridge on
	Listen      string // the address stand-ins listen on; empty is all
	PortLow     int    // stand-ins take ports from PortLow to PortHigh
	PortHigh    int
	StateFile   string
	NameSuffix  string // added to an Echo's name in the kiosks' list
	Ifaces      []net.Interface
	BrowseEvery time.Duration
	BrowseFor   time.Duration
	ForgetAfter time.Duration // a device unheard this long loses its stand-in
	MaxCall     time.Duration
	// SilenceHangup ends a call once neither side has sent anything louder than QuietDBFS for this
	// long. Zero leaves calls up however quiet they are.
	SilenceHangup time.Duration
	QuietDBFS     float64
	ToEchoDB      float64
	ToKioskDB     float64
	Version       string
	NoAdvertise   bool // tests: serve stand-ins without announcing them
}

// kioskTwin is a real kiosk and the stand-in TECHO5 device the Echos call it through.
type kioskTwin struct {
	k    discover.Kiosk
	port int
	srv  *http.Server
	adv  *zeroconf.Server
	seen time.Time
}

// echoTwin is a real Echo and the stand-in kiosk the kiosks call it through.
type echoTwin struct {
	e    discover.Echo
	id   string // the stand-in kiosk's id
	name string // the stand-in kiosk's name
	port int
	srv  *http.Server
	adv  *zeroconf.Server
	seen time.Time
}

// Bridge is the running bridge.
type Bridge struct {
	cfg   Config
	auth  *ks.Auth
	ksc   *ks.Client
	t5key []byte
	ann   *t5.Announcements
	log   *slog.Logger

	mu      sync.Mutex
	kiosks  map[string]*kioskTwin // by the real kiosk's id
	echos   map[string]*echoTwin  // by the Echo's name, lower case
	calls   map[string]*session   // by call id
	ports   map[string]int        // "ks:<id>" and "t5:<name>" to port, kept in StateFile
	lastErr string
	lastAt  time.Time
}

func New(cfg Config, log *slog.Logger) *Bridge {
	auth := ks.NewAuth(cfg.IntercomKey)
	return &Bridge{
		cfg: cfg, auth: auth, ksc: ks.NewClient(auth), t5key: t5.Key(cfg.HouseWord), ann: t5.NewAnnouncements(cfg.HouseWord), log: log,
		kiosks: map[string]*kioskTwin{}, echos: map[string]*echoTwin{}, calls: map[string]*session{}, ports: map[string]int{},
	}
}

// StandinID is the stand-in kiosk id for an Echo: 16 hex digits that stay the same across restarts
// and never match a real kiosk's.
func StandinID(echoName string) string {
	sum := sha256.Sum256([]byte("intercom-bridge:" + strings.ToLower(echoName)))
	return hex.EncodeToString(sum[:])[:16]
}

// Run looks for devices every BrowseEvery until ctx ends, then takes every stand-in down.
func (b *Bridge) Run(ctx context.Context) {
	b.loadState()
	for {
		b.scan(ctx)
		select {
		case <-ctx.Done():
			b.shutdown()
			return
		case <-time.After(b.cfg.BrowseEvery):
		}
	}
}

func (b *Bridge) scan(ctx context.Context) {
	var (
		wg         sync.WaitGroup
		kiosks     []discover.Kiosk
		echos      []discover.Echo
		kErr, eErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); kiosks, kErr = discover.Kiosks(ctx, b.cfg.BrowseFor, b.cfg.Ifaces) }()
	go func() { defer wg.Done(); echos, eErr = discover.Echos(ctx, b.cfg.BrowseFor, b.cfg.Ifaces) }()
	wg.Wait()
	if kErr != nil {
		b.fail("looking for kiosks: %v", kErr)
	} else {
		b.SetKiosks(kiosks)
	}
	if eErr != nil {
		b.fail("looking for Echos: %v", eErr)
	} else {
		b.SetEchos(echos)
	}
	b.forget(time.Now().Add(-b.cfg.ForgetAfter))
}

// SetKiosks takes in the kiosks one look found. Stand-ins of our own are left out.
func (b *Bridge) SetKiosks(found []discover.Kiosk) {
	now := time.Now()
	for _, k := range found {
		if b.isOwnStandin(k.ID) {
			continue
		}
		b.mu.Lock()
		t := b.kiosks[k.ID]
		if t != nil {
			renamed := t.k.Name != k.Name
			t.k, t.seen = k, now
			b.mu.Unlock()
			if renamed {
				b.readvertiseKiosk(t)
			}
			continue
		}
		b.mu.Unlock()
		if err := b.addKiosk(k, now); err != nil {
			b.fail("stand-in for kiosk %q: %v", k.Name, err)
		}
	}
}

// SetEchos takes in the Echos one look found. Stand-ins of our own are left out.
func (b *Bridge) SetEchos(found []discover.Echo) {
	now := time.Now()
	for _, e := range found {
		if e.Address == b.cfg.Advertise && e.Port >= b.cfg.PortLow && e.Port <= b.cfg.PortHigh {
			continue // one of ours: the bridge hears its own stand-ins
		}
		key := strings.ToLower(e.Name)
		b.mu.Lock()
		t := b.echos[key]
		if t != nil {
			t.e, t.seen = e, now
			b.mu.Unlock()
			continue
		}
		b.mu.Unlock()
		if err := b.addEcho(e, now); err != nil {
			b.fail("stand-in for Echo %q: %v", e.Name, err)
		}
	}
}

func (b *Bridge) isOwnStandin(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.echos {
		if t.id == id {
			return true
		}
	}
	return false
}

func (b *Bridge) addKiosk(k discover.Kiosk, now time.Time) error {
	t := &kioskTwin{k: k, seen: now}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.serveEchoCall(t, w, r) })
	ln, port, err := b.listen("ks:" + k.ID)
	if err != nil {
		return err
	}
	t.port = port
	t.srv = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = t.srv.Serve(ln) }()
	if !b.cfg.NoAdvertise {
		t.adv, err = discover.AdvertiseEcho(k.Name, port, b.cfg.Advertise, b.cfg.Ifaces)
		if err != nil {
			_ = t.srv.Close()
			return fmt.Errorf("advertising: %w", err)
		}
	}
	b.mu.Lock()
	b.kiosks[k.ID] = t
	b.mu.Unlock()
	b.log.Info("kiosk found; Echos can call it now", "kiosk", k.Name, "address", k.Address, "standin_port", port)
	return nil
}

func (b *Bridge) readvertiseKiosk(t *kioskTwin) {
	b.mu.Lock()
	old, k, port := t.adv, t.k, t.port
	b.mu.Unlock()
	if b.cfg.NoAdvertise {
		return
	}
	if old != nil {
		old.Shutdown()
	}
	adv, err := discover.AdvertiseEcho(k.Name, port, b.cfg.Advertise, b.cfg.Ifaces)
	if err != nil {
		b.fail("advertising kiosk %q: %v", k.Name, err)
		return
	}
	b.mu.Lock()
	t.adv = adv
	b.mu.Unlock()
}

func (b *Bridge) addEcho(e discover.Echo, now time.Time) error {
	t := &echoTwin{e: e, id: StandinID(e.Name), name: e.Name + b.cfg.NameSuffix, seen: now}
	ln, port, err := b.listen("t5:" + strings.ToLower(e.Name))
	if err != nil {
		return err
	}
	t.port = port
	standin := ks.Standin{ID: t.id, Name: t.name, Version: b.cfg.Version, Port: port}
	t.srv = &http.Server{Handler: ks.Handler(standin, b.auth, &standinCalls{b: b, t: t}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = t.srv.Serve(ln) }()
	if !b.cfg.NoAdvertise {
		t.adv, err = discover.AdvertiseKiosk(t.id, t.name, b.cfg.Version, port, b.cfg.Advertise, b.cfg.Ifaces)
		if err != nil {
			_ = t.srv.Close()
			return fmt.Errorf("advertising: %w", err)
		}
	}
	b.mu.Lock()
	b.echos[strings.ToLower(e.Name)] = t
	b.mu.Unlock()
	b.log.Info("Echo found; kiosks can call it now", "echo", e.Name, "address", e.Address, "standin_port", port)
	return nil
}

// forget takes down the stand-ins of devices not heard from since before.
func (b *Bridge) forget(before time.Time) {
	stop := func(adv *zeroconf.Server, srv *http.Server) func() {
		return func() {
			if adv != nil {
				adv.Shutdown()
			}
			_ = srv.Close()
		}
	}
	b.mu.Lock()
	var gone []func()
	for id, t := range b.kiosks {
		if t.seen.Before(before) {
			delete(b.kiosks, id)
			b.log.Info("kiosk gone quiet; its stand-in is down", "kiosk", t.k.Name)
			gone = append(gone, stop(t.adv, t.srv))
		}
	}
	for key, t := range b.echos {
		if t.seen.Before(before) {
			delete(b.echos, key)
			b.log.Info("Echo gone quiet; its stand-in is down", "echo", t.e.Name)
			gone = append(gone, stop(t.adv, t.srv))
		}
	}
	b.mu.Unlock()
	for _, f := range gone {
		f()
	}
}

func (b *Bridge) shutdown() {
	b.forget(time.Now().Add(time.Hour))
}

// Close takes every stand-in down.
func (b *Bridge) Close() { b.shutdown() }

// listen opens the port kept for key, or the first free one in the range, and remembers it.
func (b *Bridge) listen(key string) (net.Listener, int, error) {
	b.mu.Lock()
	want, had := b.ports[key]
	taken := map[int]bool{}
	for k, p := range b.ports {
		if k != key {
			taken[p] = true
		}
	}
	b.mu.Unlock()
	try := func(port int) (net.Listener, error) {
		return net.Listen("tcp", net.JoinHostPort(b.cfg.Listen, strconv.Itoa(port)))
	}
	if had {
		if ln, err := try(want); err == nil {
			return ln, want, nil
		}
	}
	for port := b.cfg.PortLow; port <= b.cfg.PortHigh; port++ {
		if taken[port] || (had && port == want) {
			continue
		}
		ln, err := try(port)
		if err != nil {
			continue
		}
		b.mu.Lock()
		b.ports[key] = port
		b.mu.Unlock()
		b.saveState()
		return ln, port, nil
	}
	return nil, 0, errors.New("no free port in the range")
}

type stateFile struct {
	Ports map[string]int `json:"ports"`
}

func (b *Bridge) loadState() {
	if b.cfg.StateFile == "" {
		return
	}
	raw, err := os.ReadFile(b.cfg.StateFile)
	if err != nil {
		return
	}
	var s stateFile
	if json.Unmarshal(raw, &s) == nil && s.Ports != nil {
		b.mu.Lock()
		b.ports = s.Ports
		b.mu.Unlock()
	}
}

func (b *Bridge) saveState() {
	if b.cfg.StateFile == "" {
		return
	}
	b.mu.Lock()
	raw, _ := json.MarshalIndent(stateFile{Ports: b.ports}, "", "  ")
	b.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(b.cfg.StateFile), 0o755)
	tmp := b.cfg.StateFile + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, b.cfg.StateFile)
	}
}

func (b *Bridge) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.log.Warn(msg)
	b.mu.Lock()
	b.lastErr, b.lastAt = msg, time.Now()
	b.mu.Unlock()
}

// Status is what the status page shows.
type Status struct {
	Version   string       `json:"version"`
	Kiosks    []StatusTwin `json:"kiosks"`
	Echos     []StatusTwin `json:"echos"`
	Calls     []StatusCall `json:"calls"`
	LastError string       `json:"last_error,omitempty"`
	LastAt    *time.Time   `json:"last_error_at,omitempty"`
}

type StatusTwin struct {
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	StandinPort int       `json:"standin_port"`
	Seen        time.Time `json:"seen"`
}

type StatusCall struct {
	ID       string    `json:"id"`
	Way      string    `json:"way"`
	Kiosk    string    `json:"kiosk"`
	Echo     string    `json:"echo"`
	Answered bool      `json:"answered"`
	Started  time.Time `json:"started"`
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Status{Version: b.cfg.Version, LastError: b.lastErr}
	if !b.lastAt.IsZero() {
		at := b.lastAt
		s.LastAt = &at
	}
	for _, t := range b.kiosks {
		s.Kiosks = append(s.Kiosks, StatusTwin{Name: t.k.Name, Address: t.k.Address, StandinPort: t.port, Seen: t.seen})
	}
	for _, t := range b.echos {
		s.Echos = append(s.Echos, StatusTwin{Name: t.e.Name, Address: t.e.Address, StandinPort: t.port, Seen: t.seen})
	}
	for _, c := range b.calls {
		s.Calls = append(s.Calls, StatusCall{ID: c.id, Way: c.way, Kiosk: c.kioskName, Echo: c.echoName,
			Answered: c.answered.Load(), Started: c.started})
	}
	sort.Slice(s.Kiosks, func(i, j int) bool { return s.Kiosks[i].Name < s.Kiosks[j].Name })
	sort.Slice(s.Echos, func(i, j int) bool { return s.Echos[i].Name < s.Echos[j].Name })
	return s
}
