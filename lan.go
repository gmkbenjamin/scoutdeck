package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var flagLAN = flag.Bool("lan", false, "also serve the web app on this Mac's home Wi-Fi addresses (saved; toggle with M > Phone & tablet > L)")

// Phone addresses: the HUD always shows how a phone or tablet can reach the
// web app. Tailscale (HTTPS, any network) is preferred; "home Wi-Fi access"
// optionally also serves the web app on this Mac's private LAN addresses.

type phoneAddresses struct {
	applyMu   sync.Mutex
	mu        sync.Mutex
	tailscale string   // https URL from `tailscale serve`, if configured
	lan       []string // http URLs on private LAN addresses, if enabled
	listeners []net.Listener
	servers   []*http.Server
}

func (p *phoneAddresses) snapshot() (string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tailscale, append([]string(nil), p.lan...)
}

// privateIPv4s lists this Mac's private (RFC 1918) IPv4 addresses on up interfaces.
func isTunnelOrVirtual(name string) bool {
	for _, p := range []string{"utun", "tun", "tap", "wg", "ppp", "ipsec", "bridge", "veth", "docker"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func privateIPv4s() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagPointToPoint != 0 || isTunnelOrVirtual(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip := ipn.IP.To4(); ip != nil && ip.IsPrivate() {
					out = append(out, ip.String())
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// lanOnly serves the web app to browsers on the LAN but keeps the agent API
// (Bearer tokens) on loopback.
func lanOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "the agent API is only available on this Mac", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/shutdown" {
			http.Error(w, "not available", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (p *phoneAddresses) closeLAN() {
	p.mu.Lock()
	servers := p.servers
	listeners := p.listeners
	p.servers, p.listeners, p.lan = nil, nil, nil
	p.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Close()
	}
	for _, l := range listeners {
		_ = l.Close()
	}
}

// applyLANAccess starts or stops the LAN listeners to match the setting.
func (a *application) applyLANAccess() error {
	p := &a.phones
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.closeLAN()
	a.settingsMu.Lock()
	enabled := a.settings.LANAccess
	a.settingsMu.Unlock()
	if !enabled || a.server == nil || a.ctx.Err() != nil {
		return nil
	}
	_, port, err := net.SplitHostPort(*flagAPI)
	if err != nil {
		return err
	}
	handler := lanOnly(a.server.Handler)
	var errs []string
	for _, ip := range privateIPv4s() {
		addr := net.JoinHostPort(ip, port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return a.ctx }}
		go func() {
			if err := srv.Serve(l); err != nil && err != http.ErrServerClosed && a.ctx.Err() == nil {
				log.Printf("home Wi-Fi web app: %v", err)
			}
		}()
		p.mu.Lock()
		p.servers = append(p.servers, srv)
		p.listeners = append(p.listeners, l)
		p.lan = append(p.lan, "http://"+addr+"/")
		p.mu.Unlock()
		log.Printf("web app on home Wi-Fi at http://%s", addr)
	}
	if len(errs) > 0 {
		return fmt.Errorf("home Wi-Fi access: %v", errs)
	}
	return nil
}

func (a *application) setLANAccess(on bool) error {
	a.settingsMu.Lock()
	a.settings.LANAccess = on
	a.settingsMu.Unlock()
	if err := a.saveSettings(); err != nil {
		return err
	}
	return a.applyLANAccess()
}

// watchPhoneAddresses refreshes the Tailscale URL and follows LAN address
// changes (e.g. after joining another Wi-Fi network).
func (a *application) watchPhoneAddresses() {
	var lastIPs string
	for {
		pa := checkPhoneAccess(a.ctx, *flagAPI)
		a.phones.mu.Lock()
		a.phones.tailscale = pa.URL
		a.phones.mu.Unlock()
		if ips := fmt.Sprint(privateIPv4s()); ips != lastIPs {
			lastIPs = ips
			if err := a.applyLANAccess(); err != nil {
				log.Print(err)
			}
		}
		select {
		case <-a.ctx.Done():
			a.phones.applyMu.Lock()
			a.phones.closeLAN()
			a.phones.applyMu.Unlock()
			return
		case <-time.After(30 * time.Second):
		}
	}
}
