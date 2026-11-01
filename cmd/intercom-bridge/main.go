// Command intercom-bridge lets Kiosk Satellite kiosks and TECHO5 devices call each other.
// It is set up entirely through environment variables; see README.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"intercom-bridge/internal/bridge"
	"intercom-bridge/internal/discover"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		os.Exit(discoverOnly())
	}
	cfg, statusPort, log, err := configure()
	if err != nil {
		fmt.Fprintln(os.Stderr, "intercom-bridge:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := bridge.NewRunner(cfg, log, bridgeID(filepath.Dir(cfg.StateFile)), statusPort)
	if statusPort > 0 {
		go serveStatus(statusPort, r, log)
	}
	log.Info("intercom bridge starting", "version", version, "advertise", cfg.Advertise,
		"ports", fmt.Sprintf("%d-%d", cfg.PortLow, cfg.PortHigh), "status_port", statusPort)
	r.Run(ctx)
	log.Info("intercom bridge stopped")
}

func configure() (bridge.Config, int, *slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return bridge.Config{}, 0, nil, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(env("LOG_FORMAT", "text"), "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	log := slog.New(handler)

	key, err := secret("KS_INTERCOM_KEY")
	if err != nil {
		return bridge.Config{}, 0, nil, err
	}
	word, err := secret("TECHO5_HOUSE_WORD")
	if err != nil {
		return bridge.Config{}, 0, nil, err
	}

	var ifaces []net.Interface
	ifaceName := env("MDNS_INTERFACE", "")
	if ifaceName != "" {
		ifc, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return bridge.Config{}, 0, nil, fmt.Errorf("MDNS_INTERFACE: %w", err)
		}
		ifaces = []net.Interface{*ifc}
	}
	advertise := env("ADVERTISE_ADDRESS", "")
	if advertise == "" {
		if advertise, err = guessAddress(ifaceName); err != nil {
			return bridge.Config{}, 0, nil, fmt.Errorf("ADVERTISE_ADDRESS not set and none found: %w", err)
		}
	} else if net.ParseIP(advertise) == nil {
		return bridge.Config{}, 0, nil, errors.New("ADVERTISE_ADDRESS must be an IP address")
	}

	low, high, err := portRange(env("PORT_RANGE", "41000-41099"))
	if err != nil {
		return bridge.Config{}, 0, nil, fmt.Errorf("PORT_RANGE: %w", err)
	}
	statusPort, err := strconv.Atoi(env("STATUS_PORT", "8790"))
	if err != nil {
		return bridge.Config{}, 0, nil, fmt.Errorf("STATUS_PORT: %w", err)
	}
	every, err := time.ParseDuration(env("BROWSE_INTERVAL", "30s"))
	if err != nil || every < 5*time.Second {
		return bridge.Config{}, 0, nil, errors.New("BROWSE_INTERVAL must be a duration of at least 5s, like 30s")
	}
	maxCall, err := time.ParseDuration(env("MAX_CALL", "2h"))
	if err != nil || maxCall <= 0 {
		return bridge.Config{}, 0, nil, errors.New("MAX_CALL must be a duration, like 2h")
	}
	forget, err := time.ParseDuration(env("FORGET_AFTER", "10m"))
	if err != nil || forget < 2*every {
		return bridge.Config{}, 0, nil, errors.New("FORGET_AFTER must be a duration of at least two BROWSE_INTERVALs, like 10m")
	}
	silence, err := time.ParseDuration(env("SILENCE_HANGUP", "0s"))
	if err != nil || silence < 0 {
		return bridge.Config{}, 0, nil, errors.New("SILENCE_HANGUP must be a duration, like 15s, or 0 for off")
	}
	quiet, err := strconv.ParseFloat(env("SILENCE_LEVEL_DB", "-55"), 64)
	if err != nil || quiet >= 0 {
		return bridge.Config{}, 0, nil, errors.New("SILENCE_LEVEL_DB must be a negative number of dB, like -55")
	}
	toEcho, err := strconv.ParseFloat(env("GAIN_TO_ECHO_DB", "0"), 64)
	if err != nil {
		return bridge.Config{}, 0, nil, fmt.Errorf("GAIN_TO_ECHO_DB: %w", err)
	}
	toKiosk, err := strconv.ParseFloat(env("GAIN_TO_KIOSK_DB", "0"), 64)
	if err != nil {
		return bridge.Config{}, 0, nil, fmt.Errorf("GAIN_TO_KIOSK_DB: %w", err)
	}

	cfg := bridge.Config{
		IntercomKey: key, HouseWord: word,
		Advertise: advertise, Listen: env("LISTEN_ADDRESS", ""),
		PortLow: low, PortHigh: high,
		StateFile:   filepath.Join(env("STATE_DIR", "/data"), "state.json"),
		NameSuffix:  env("NAME_SUFFIX", ""),
		Ifaces:      ifaces,
		BrowseEvery: every, BrowseFor: 3 * time.Second, ForgetAfter: forget, MaxCall: maxCall,
		SilenceHangup: silence, QuietDBFS: quiet,
		ToEchoDB: toEcho, ToKioskDB: toKiosk,
		Version: version,
	}
	return cfg, statusPort, log, nil
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// secret reads NAME, or the file NAME_FILE names (for Docker secrets).
func secret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, nil
		}
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is required (or %s_FILE)", name, name)
}

func portRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, "-")
	low, err1 := strconv.Atoi(strings.TrimSpace(a))
	high, err2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || err1 != nil || err2 != nil || low < 1 || high > 65535 || low > high {
		return 0, 0, errors.New(`want "low-high", like 41000-41099`)
	}
	return low, high, nil
}

// guessAddress is the interface's first IPv4 address, or the one the default route leaves from.
func guessAddress(ifaceName string) (string, error) {
	if ifaceName != "" {
		ifc, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return "", err
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return "", err
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				return ipn.IP.String(), nil
			}
		}
		return "", fmt.Errorf("%s has no IPv4 address", ifaceName)
	}
	// No packet is sent: connecting a UDP socket only picks the route.
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// discoverOnly lists the kiosks and TECHO5 devices this machine can hear, without advertising
// anything or needing the secrets: a check that the network lets the bridge see both sides.
func discoverOnly() int {
	var ifaces []net.Interface
	if name := env("MDNS_INTERFACE", ""); name != "" {
		ifc, err := net.InterfaceByName(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "MDNS_INTERFACE:", err)
			return 2
		}
		ifaces = []net.Interface{*ifc}
	}
	ctx := context.Background()
	look := 5 * time.Second
	fmt.Println("Looking for 5 seconds...")
	kiosks, kerr := discover.Kiosks(ctx, look, ifaces)
	echos, eerr := discover.Echos(ctx, look, ifaces)
	fmt.Println("\nKiosk Satellite kiosks:")
	if kerr != nil {
		fmt.Println("  error:", kerr)
	}
	for _, k := range kiosks {
		fmt.Printf("  %-28s %s:%d  id %s  version %s\n", k.Name, k.Address, k.AdminPort, k.ID, k.Version)
	}
	if len(kiosks) == 0 && kerr == nil {
		fmt.Println("  none heard")
	}
	fmt.Println("\nTECHO5 devices:")
	if eerr != nil {
		fmt.Println("  error:", eerr)
	}
	for _, e := range echos {
		fmt.Printf("  %-28s %s:%d\n", e.Name, e.Address, e.Port)
	}
	if len(echos) == 0 && eerr == nil {
		fmt.Println("  none heard")
	}
	if len(kiosks) == 0 || len(echos) == 0 {
		return 1
	}
	return 0
}

// bridgeID is this bridge's id, kept in dir so it stays the same across restarts.
func bridgeID(dir string) string {
	path := filepath.Join(dir, "bridge-id")
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); len(id) == 16 {
			return id
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(path, []byte(id+"\n"), 0o644)
	return id
}

func serveStatus(port int, b *bridge.Runner, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b.Status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Warn("status page stopped", "err", err)
	}
}
