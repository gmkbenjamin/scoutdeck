package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPProtocolAndAllTools(t *testing.T) {
	type callRec struct {
		auth, method, path, body string
	}
	var calls []callRec
	c := client{
		base:  "http://127.0.0.1:8765",
		token: "tok",
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			calls = append(calls, callRec{
				auth:   r.Header.Get("Authorization"),
				method: r.Method,
				path:   r.URL.RequestURI(),
				body:   string(b),
			})
			switch r.URL.Path {
			case "/v1/screenshot.jpg":
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
					Body:       io.NopCloser(bytes.NewReader([]byte{0xff, 0xd8, 0xff})),
				}, nil
			case "/v1/microphone.wav":
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"audio/wav"}},
					Body:       io.NopCloser(bytes.NewReader([]byte("RIFF"))),
				}, nil
			case "/v1/say":
				return &http.Response{
					StatusCode: 409,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"speaker busy"}`)),
				}, nil
			default:
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			}
		})},
	}

	in := strings.Join([]string{
		`{invalid-json`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"scout_status"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"scout_look"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"scout_move","arguments":{"distance_m":1.2,"speed":0.2}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"scout_turn","arguments":{"degrees":90}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"scout_stop"}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"scout_dock"}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"scout_say","arguments":{"text":"hello"}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"scout_listen","arguments":{"seconds":2}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"scout_light","arguments":{"setting":"bright"}}}`,
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"scout_light","arguments":{"setting":"bogus"}}}`,
		`{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"scout_events","arguments":{"limit":5}}}`,
		`{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"scout_guard","arguments":{"armed":true,"objects":false}}}`,
		`{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"scout_guard","arguments":{"objects":true}}}`,
		`{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"scout_routes"}}`,
		`{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"scout_patrol","arguments":{"route":"kitchen run"}}}`,
		`{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"scout_patrol","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"scout_record","arguments":{"action":"start"}}}`,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"scout_record","arguments":{"action":"pause"}}}`,
		`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"scout_health"}}`,
		`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":"not-an-object"}`,
		`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":25,"method":"bogus"}`,
	}, "\n")

	var out bytes.Buffer
	if err := serveMCP(context.Background(), c, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) != 26 {
		t.Fatalf("want 26 replies (notification skipped), got %d", len(msgs))
	}

	// id=null parse error (-32700)
	if code := msgs[0]["error"].(map[string]any)["code"]; code != float64(-32700) {
		t.Fatalf("parse error code = %v, want -32700", code)
	}
	// initialize
	if v := msgs[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("version %v", v)
	}
	// ping
	if msgs[2]["result"] == nil || msgs[2]["error"] != nil {
		t.Fatalf("ping failed: %+v", msgs[2])
	}
	// tools/list: verify all 15 tools exist
	toolList := msgs[3]["result"].(map[string]any)["tools"].([]any)
	if len(toolList) != 15 {
		t.Fatalf("tool count = %d, want 15", len(toolList))
	}
	seenTools := map[string]bool{}
	for _, raw := range toolList {
		seenTools[raw.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"scout_status", "scout_look", "scout_move", "scout_turn", "scout_stop",
		"scout_dock", "scout_say", "scout_listen", "scout_light", "scout_events",
		"scout_guard", "scout_routes", "scout_patrol", "scout_record", "scout_health",
	} {
		if !seenTools[want] {
			t.Fatalf("missing tool %s in tools/list", want)
		}
	}

	// Verify content & isError assertions for tool calls.
	assertToolOK := func(idx int, wantType string) {
		t.Helper()
		res := msgs[idx]["result"].(map[string]any)
		if isErr, _ := res["isError"].(bool); isErr {
			t.Fatalf("msg %d unexpectedly returned isError=true: %+v", idx, res)
		}
		content := res["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != wantType {
			t.Fatalf("msg %d content = %+v, want type %s", idx, content, wantType)
		}
	}
	assertToolErr := func(idx int, wantSub string) {
		t.Helper()
		res := msgs[idx]["result"].(map[string]any)
		if isErr, _ := res["isError"].(bool); !isErr {
			t.Fatalf("msg %d expected isError=true, got %+v", idx, res)
		}
		txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(txt, wantSub) {
			t.Fatalf("msg %d error = %q, want %q", idx, txt, wantSub)
		}
	}

	assertToolOK(4, "text")   // scout_status
	assertToolOK(5, "image")  // scout_look
	assertToolOK(6, "text")   // scout_move
	assertToolOK(7, "text")   // scout_turn
	assertToolOK(8, "text")   // scout_stop
	assertToolOK(9, "text")   // scout_dock
	assertToolErr(10, "409")  // scout_say HTTP error propagation
	assertToolOK(11, "audio") // scout_listen
	assertToolOK(12, "text")  // scout_light valid
	assertToolErr(13, "setting")
	assertToolOK(14, "text") // scout_events
	assertToolOK(15, "text") // scout_guard valid
	assertToolErr(16, "armed")
	assertToolOK(17, "text") // scout_routes
	assertToolOK(18, "text") // scout_patrol valid
	assertToolErr(19, "route")
	assertToolOK(20, "text") // scout_record valid
	assertToolErr(21, "action")
	assertToolOK(22, "text") // scout_health

	// Invalid params (-32602), unknown tool (-32602), unknown method (-32601)
	if code := msgs[23]["error"].(map[string]any)["code"]; code != float64(-32602) {
		t.Fatalf("invalid params code = %v, want -32602", code)
	}
	if code := msgs[24]["error"].(map[string]any)["code"]; code != float64(-32602) {
		t.Fatalf("unknown tool code = %v, want -32602", code)
	}
	if code := msgs[25]["error"].(map[string]any)["code"]; code != float64(-32601) {
		t.Fatalf("unknown method code = %v, want -32601", code)
	}

	// Verify exact HTTP calls (invalid local validations made no HTTP call).
	wantCalls := []struct {
		method, path, body string
	}{
		{"GET", "/v1/status", ""},
		{"GET", "/v1/screenshot.jpg", ""},
		{"POST", "/v1/move", `{"distance_m":1.2,"speed":0.2}`},
		{"POST", "/v1/turn", `{"degrees":90}`},
		{"POST", "/v1/stop", ""},
		{"POST", "/v1/dock", `{"timeout_seconds":300}`},
		{"POST", "/v1/say", `{"text":"hello"}`},
		{"GET", "/v1/microphone.wav?seconds=2", ""},
		{"POST", "/v1/light", `{"command":1}`},
		{"GET", "/v1/events?limit=5", ""},
		{"POST", "/v1/guard", `{"armed":true,"objects":false}`},
		{"GET", "/v1/routes", ""},
		{"POST", "/v1/routes/kitchen%20run/run", ""},
		{"POST", "/v1/record/start", ""},
		{"GET", "/v1/health", ""},
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("HTTP calls = %d, want %d: %+v", len(calls), len(wantCalls), calls)
	}
	for i, w := range wantCalls {
		got := calls[i]
		if got.auth != "Bearer tok" || got.method != w.method || got.path != w.path || got.body != w.body {
			t.Fatalf("call %d = %+v, want %+v", i, got, w)
		}
	}
}
