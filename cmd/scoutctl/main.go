// scoutctl is a JSON-first client for the local Scout agent API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shell-company/scout/internal/docking"
)

type client struct {
	base, token string
	http        *http.Client
}

func (c client) request(ctx context.Context, method, path string, body []byte, kind string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return res, nil
}
func (c client) json(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	res, err := c.request(ctx, method, path, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}
func main() {
	if err := run(); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: scoutctl status|power|drive|stop|dock|dock-status|dock-cancel|light|screenshot|record|speaker|video|mic-stream|shutdown; use COMMAND -h")
	}
	command := os.Args[1]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	base := fs.String("url", envOr("SCOUT_API_URL", "http://127.0.0.1:8080"), "loopback Scout API URL (or SCOUT_API_URL)")
	x := fs.Float64("x", 0, "strafe velocity, -0.2..0.2")
	y := fs.Float64("y", 0, "forward velocity, -0.3..0.3")
	z := fs.Float64("z", 0, "rotation, -1.8..1.8")
	ttl := fs.Int("ttl", 300, "drive lease milliseconds, 100..500; command does not auto-renew")
	timeout := fs.Int("timeout", 90, "docking timeout seconds, 10..180")
	wait := fs.Bool("wait", true, "wait for docking result; interrupt cancels docking")
	seconds := fs.Int("seconds", 3, "microphone recording duration, 1..10 seconds")
	light := fs.Int("command", 4, "light: 0 down, 1 up, 3 max, 4 auto")
	output := fs.String("out", "", "media output file; '-' writes bytes to stdout")
	input := fs.String("in", "", "speaker WAV input file (mono PCM16 at 16 kHz)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	u, err := url.Parse(*base)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("URL must be plain HTTP on a literal loopback IP; use an SSH tunnel for remote access")
	}
	token := os.Getenv("SCOUT_API_TOKEN")
	if token == "" {
		return errors.New("SCOUT_API_TOKEN is required")
	}
	c := client{strings.TrimRight(*base, "/"), token, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	method, path := "GET", "/v1/"+command
	var body []byte
	switch command {
	case "status", "power":
	case "stop", "shutdown":
		method = "POST"
	case "dock-status":
		path = "/v1/dock"
	case "dock-cancel":
		method, path = "POST", "/v1/dock/cancel"
	case "drive":
		method = "POST"
		body, _ = json.Marshal(map[string]any{"linear_x": *x, "linear_y": *y, "angular_z": *z, "ttl_ms": *ttl})
	case "light":
		method = "POST"
		body, _ = json.Marshal(map[string]int{"command": *light})
	case "dock":
		body, _ = json.Marshal(map[string]int{"timeout_seconds": *timeout})
		call, done := context.WithTimeout(ctx, 15*time.Second)
		b, err := c.json(call, "POST", "/v1/dock", body)
		done()
		if err != nil {
			return err
		}
		fmt.Print(string(b))
		if !*wait {
			return nil
		}
		var initial docking.Operation
		if err = json.Unmarshal(b, &initial); err != nil {
			return err
		}
		// A bounded wait also cancels if status polling fails, rather than leaving native motion running.
		completed := false
		defer func() {
			if !completed {
				cleanup, stop := context.WithTimeout(context.Background(), 12*time.Second)
				defer stop()
				if _, err := c.json(cleanup, "POST", "/v1/dock/cancel", nil); err != nil {
					_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"cancel_error": err.Error()})
				}
			}
		}()
		waitCtx, stop := context.WithTimeout(ctx, time.Duration(*timeout+15)*time.Second)
		defer stop()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-ticker.C:
				call, done := context.WithTimeout(waitCtx, 5*time.Second)
				b, err = c.json(call, "GET", "/v1/dock", nil)
				done()
				if err != nil {
					return err
				}
				var op docking.Operation
				if err = json.Unmarshal(b, &op); err != nil {
					return err
				}
				if op.ID != initial.ID {
					completed = true
					return errors.New("docking operation was replaced; inspect dock-status")
				}
				fmt.Print(string(b))
				if !op.Active {
					completed = true
					if op.State == "charging" && op.ChargingVerified {
						return nil
					}
					return fmt.Errorf("docking ended %s: %s", op.State, op.Error)
				}
				if op.State == "cancel_failed" {
					return fmt.Errorf("docking cancellation failed: %s", op.Error)
				}
			}
		}
	case "screenshot", "record", "video", "mic-stream":
		if *output == "" {
			return errors.New("media commands require -out FILE or -out -")
		}
		switch command {
		case "screenshot":
			path = "/v1/screenshot.jpg"
		case "record":
			path = fmt.Sprintf("/v1/microphone.wav?seconds=%d", *seconds)
		case "video":
			path = "/v1/video.mjpeg"
		case "mic-stream":
			path = "/v1/microphone.aac"
		}
		if command == "screenshot" || command == "record" {
			var stop context.CancelFunc
			ctx, stop = context.WithTimeout(ctx, 15*time.Second)
			defer stop()
		}
		res, err := c.request(ctx, "GET", path, nil, "")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var out io.Writer = os.Stdout
		if *output != "-" {
			f, err := os.OpenFile(*output, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			defer f.Close()
			out = f
		}
		_, err = io.Copy(out, res.Body)
		return err
	case "speaker":
		if *input == "" {
			return errors.New("speaker requires -in FILE")
		}
		f, err := os.Open(*input)
		if err != nil {
			return err
		}
		defer f.Close()
		body, err = io.ReadAll(io.LimitReader(f, 1000001))
		if err != nil {
			return err
		}
		if len(body) > 1000000 {
			return errors.New("WAV exceeds size limit")
		}
		call, stop := context.WithTimeout(ctx, 40*time.Second)
		defer stop()
		res, err := c.request(call, "POST", "/v1/speaker.wav", body, "audio/wav")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, err = io.Copy(os.Stdout, res.Body)
		return err
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	call, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	b, err := c.json(call, method, path, body)
	if err == nil {
		fmt.Print(string(b))
	}
	return err
}
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
