package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"intercom-bridge/internal/discover"
)

// Runner runs a bridge only while no other bridge runs on the network. A running bridge advertises
// _intercom-bridge._tcp with its id, on its status port; the status page says whether it runs and
// since when. A bridge that finds another one alive stands by and looks again each round; when two
// run at once, the one that started first stays and the other stands down. The Kiosk Satellite
// plugin version of the bridge does the same, so the two take turns.
type Runner struct {
	cfg        Config
	log        *slog.Logger
	id         string
	statusPort int

	mu         sync.Mutex
	b          *Bridge
	since      int64 // Unix ms the running bridge started
	adv        *zeroconf.Server
	standingBy string

	// others finds the other running bridges; a test replaces it.
	others func(ctx context.Context) []other
}

type other struct {
	id, address string
	since       int64
}

func NewRunner(cfg Config, log *slog.Logger, id string, statusPort int) *Runner {
	r := &Runner{cfg: cfg, log: log, id: id, statusPort: statusPort}
	r.others = r.find
	return r
}

// Run takes rounds every BrowseEvery until ctx ends, then takes the bridge down.
func (r *Runner) Run(ctx context.Context) {
	defer r.standDown()
	for {
		r.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.BrowseEvery):
		}
	}
}

// Active reports whether this bridge is the one running.
func (r *Runner) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b != nil
}

func (r *Runner) round(ctx context.Context) {
	others := r.others(ctx)
	r.mu.Lock()
	b, since := r.b, r.since
	r.mu.Unlock()
	if b == nil {
		if len(others) > 0 {
			r.standBy(others[0].address)
			return
		}
		b = r.standUp()
	} else {
		for _, o := range others {
			if o.since < since || o.since == since && o.id < r.id {
				r.log.Info("the bridge at " + o.address + " was running first; this one stands down")
				r.standDown()
				r.standBy(o.address)
				return
			}
		}
	}
	b.scan(ctx)
}

func (r *Runner) standBy(address string) {
	r.mu.Lock()
	changed := r.standingBy != address
	r.standingBy = address
	r.mu.Unlock()
	if changed {
		r.log.Info("another bridge is running; this one stands by", "other", address)
	}
}

// find lists the other bridges whose status pages say they run. mDNS can remember a bridge that
// has gone, so the status page is the word that it still runs.
func (r *Runner) find(ctx context.Context) []other {
	found, err := discover.Bridges(ctx, r.cfg.BrowseFor, r.cfg.Ifaces)
	if err != nil {
		r.log.Warn("looking for other bridges", "err", err)
		return nil
	}
	client := &http.Client{Timeout: 4 * time.Second}
	var out []other
	for _, f := range found {
		if f.ID == r.id {
			continue
		}
		resp, err := client.Get("http://" + net.JoinHostPort(f.Address, strconv.Itoa(f.Port)) + "/status")
		if err != nil {
			continue
		}
		var s struct {
			ID     string `json:"id"`
			Active bool   `json:"active"`
			Since  int64  `json:"since"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s)
		resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && s.Active && s.ID == f.ID {
			out = append(out, other{id: s.ID, address: f.Address, since: s.Since})
		}
	}
	return out
}

func (r *Runner) standUp() *Bridge {
	b := New(r.cfg, r.log)
	since := time.Now().UnixMilli()
	var adv *zeroconf.Server
	if !r.cfg.NoAdvertise && r.statusPort > 0 {
		var err error
		adv, err = discover.AdvertiseBridge(r.id, since, r.cfg.Version, r.statusPort, r.cfg.Advertise, r.cfg.Ifaces)
		if err != nil {
			r.log.Warn("advertising the bridge", "err", err)
		}
	}
	r.mu.Lock()
	r.b, r.since, r.adv, r.standingBy = b, since, adv, ""
	r.mu.Unlock()
	r.log.Info("no other bridge is running; this one is")
	return b
}

func (r *Runner) standDown() {
	r.mu.Lock()
	b, adv := r.b, r.adv
	r.b, r.adv = nil, nil
	r.mu.Unlock()
	if adv != nil {
		adv.Shutdown()
	}
	if b != nil {
		b.Close()
	}
}

// RunnerStatus is what the status page shows: whether this bridge runs, and the bridge's status.
type RunnerStatus struct {
	ID         string `json:"id"`
	Active     bool   `json:"active"`
	Since      int64  `json:"since"`
	StandingBy string `json:"standing_by,omitempty"` // the bridge running instead
	*Status
}

func (r *Runner) Status() RunnerStatus {
	r.mu.Lock()
	b, since, by := r.b, r.since, r.standingBy
	r.mu.Unlock()
	s := RunnerStatus{ID: r.id, StandingBy: by}
	if b != nil {
		st := b.Status()
		s.Active, s.Since, s.Status = true, since, &st
	} else {
		s.Status = &Status{Version: r.cfg.Version}
	}
	return s
}
