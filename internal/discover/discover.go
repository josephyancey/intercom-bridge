// Package discover finds kiosks and TECHO5 devices over mDNS and advertises the bridge's stand-ins.
package discover

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

const (
	kioskService = "_kiosk-satellite._tcp"
	echoService  = "_techo5._tcp"
	domain       = "local."
	labelMax     = 63
)

// Kiosk is a Kiosk Satellite kiosk as it advertises itself.
type Kiosk struct {
	ID        string
	Name      string
	Version   string
	Address   string
	AdminPort int
}

// Echo is a TECHO5 device as it advertises itself.
type Echo struct {
	Name    string
	Address string
	Port    int
}

func txt(e *zeroconf.ServiceEntry, key string) string {
	for _, t := range e.Text {
		if v, ok := strings.CutPrefix(t, key+"="); ok {
			return v
		}
	}
	return ""
}

func address(e *zeroconf.ServiceEntry) string {
	for _, ip := range e.AddrIPv4 {
		if ip.IsGlobalUnicast() {
			return ip.String()
		}
	}
	if h := txt(e, "host"); net.ParseIP(h) != nil {
		return h
	}
	for _, ip := range e.AddrIPv6 {
		if ip.IsGlobalUnicast() {
			return ip.String()
		}
	}
	return ""
}

func browse(ctx context.Context, service string, look time.Duration, ifaces []net.Interface) ([]*zeroconf.ServiceEntry, error) {
	found := make(chan *zeroconf.ServiceEntry, 32)
	var out []*zeroconf.ServiceEntry
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range found {
			out = append(out, e)
		}
	}()
	lctx, cancel := context.WithTimeout(ctx, look)
	defer cancel()
	var opts []zeroconf.ClientOption
	if len(ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(ifaces))
	}
	if err := zeroconf.Browse(lctx, service, domain, found, opts...); err != nil {
		// Browse leaves the channel open when it failed before taking it.
		func() { defer func() { _ = recover() }(); close(found) }()
		<-done
		return nil, err
	}
	<-done
	return out, nil
}

// Kiosks lists the kiosks that answer within look.
func Kiosks(ctx context.Context, look time.Duration, ifaces []net.Interface) ([]Kiosk, error) {
	entries, err := browse(ctx, kioskService, look, ifaces)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Kiosk
	for _, e := range entries {
		id := txt(e, "id")
		addr := address(e)
		if id == "" || addr == "" || seen[id] {
			continue
		}
		port, _ := strconv.Atoi(txt(e, "port"))
		if port == 0 {
			port = e.Port
		}
		seen[id] = true
		out = append(out, Kiosk{ID: id, Name: txt(e, "name"), Version: txt(e, "version"), Address: addr, AdminPort: port})
	}
	return out, nil
}

// Echos lists the TECHO5 devices with microphones that answer within look.
func Echos(ctx context.Context, look time.Duration, ifaces []net.Interface) ([]Echo, error) {
	entries, err := browse(ctx, echoService, look, ifaces)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Echo
	for _, e := range entries {
		name := txt(e, "name")
		if name == "" {
			name = e.Instance
		}
		addr := address(e)
		if name == "" || addr == "" || txt(e, "hears") == "0" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, Echo{Name: name, Address: addr, Port: e.Port})
	}
	return out, nil
}

// Label makes a name one DNS label: letters, digits and dashes, at most 63 characters.
func Label(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > labelMax {
		out = strings.Trim(out[:labelMax], "-")
	}
	if out == "" {
		out = "device"
	}
	return out
}

// AdvertiseKiosk announces a stand-in kiosk the way kiosks announce themselves: instance ks-<id>,
// with id, name, version and admin port in the TXT record.
func AdvertiseKiosk(id, name, version string, port int, ip string, ifaces []net.Interface) (*zeroconf.Server, error) {
	host := "ksb-" + id
	text := []string{"id=" + id, "name=" + name, "version=" + version, "port=" + strconv.Itoa(port), "host=" + ip}
	return zeroconf.RegisterProxy("ks-"+id, kioskService, domain, port, host, []string{ip}, text, ifaces)
}

// AdvertiseEcho announces a stand-in TECHO5 device: the name in full goes in the TXT record, which is
// what devices know each other by.
func AdvertiseEcho(name string, port int, ip string, ifaces []net.Interface) (*zeroconf.Server, error) {
	return zeroconf.RegisterProxy(Label(name), echoService, domain, port, Label("t5b-"+name), []string{ip},
		[]string{"name=" + name}, ifaces)
}
