package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Remote access for phones and tablets goes through `tailscale serve`, which
// gives the loopback-only web app an HTTPS address on the user's tailnet.

type phoneAccess struct {
	Tailscale bool   // CLI found and logged in
	DNSName   string // this Mac's MagicDNS name
	URL       string // https URL serving ScoutDeck, if configured
	Port      string // HTTPS port ScoutDeck uses on the tailnet
	Err       string
}

func tailscaleCLI() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := exec.LookPath(app); err == nil {
		return app, nil
	}
	return "", fmt.Errorf("Tailscale is not installed")
}

func tailscaleJSON(ctx context.Context, out any, args ...string) error {
	cli, err := tailscaleCLI()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, cli, args...).Output()
	if err != nil {
		return fmt.Errorf("tailscale %s: %w", strings.Join(args, " "), err)
	}
	return json.Unmarshal(data, out)
}

// checkPhoneAccess finds this Mac's tailnet name and whether ScoutDeck is served.
func checkPhoneAccess(ctx context.Context, apiAddr string) phoneAccess {
	_, port, _ := net.SplitHostPort(apiAddr)
	pa := phoneAccess{Port: port}
	var st struct {
		BackendState string
		Self         struct{ DNSName string }
	}
	if err := tailscaleJSON(ctx, &st, "status", "--json"); err != nil {
		pa.Err = err.Error()
		return pa
	}
	if st.BackendState != "Running" {
		pa.Err = "Tailscale is " + strings.ToLower(st.BackendState) + "; open Tailscale and sign in"
		return pa
	}
	pa.Tailscale, pa.DNSName = true, strings.TrimSuffix(st.Self.DNSName, ".")
	var serve struct {
		Web map[string]struct {
			Handlers map[string]struct{ Proxy string }
		}
	}
	if err := tailscaleJSON(ctx, &serve, "serve", "status", "--json"); err != nil {
		return pa
	}
	want := "http://" + apiAddr
	for hostPort, web := range serve.Web {
		if h, ok := web.Handlers["/"]; ok && strings.TrimSuffix(h.Proxy, "/") == want {
			host, p, err := net.SplitHostPort(hostPort)
			if err != nil {
				continue
			}
			pa.URL = "https://" + host + "/"
			if p != "443" {
				pa.URL = "https://" + net.JoinHostPort(host, p) + "/"
			}
		}
	}
	return pa
}

// enablePhoneAccess serves the web app on the tailnet at its own HTTPS port,
// leaving any existing `tailscale serve` handlers (e.g. on 443) untouched.
func enablePhoneAccess(ctx context.Context, apiAddr string) error {
	cli, err := tailscaleCLI()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli, "serve", "--bg", "--https="+port, "http://"+apiAddr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale serve failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyToClipboard(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
