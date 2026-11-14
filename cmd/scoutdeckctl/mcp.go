package main

// `scoutdeckctl mcp` is a Model Context Protocol server on stdio, so assistants
// can drive, look, listen and speak through the same bounded
// agent API (speed limits, short leases, manual priority, obstacle guard).
// Register this stdio command with your MCP client:
//
//	/path/to/scoutdeckctl mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(ctx context.Context, c client, args map[string]any) ([]map[string]any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func num(desc string) map[string]any  { return map[string]any{"type": "number", "description": desc} }
func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func boolean(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }

func textContent(s string) []map[string]any {
	return []map[string]any{{"type": "text", "text": s}}
}

// jsonCall performs one API call and returns its JSON as text content.
func jsonCall(ctx context.Context, c client, method, path string, body any, timeout time.Duration) ([]map[string]any, error) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := c.json(call, method, path, data)
	if err != nil {
		return nil, err
	}
	return textContent(strings.TrimSpace(string(b))), nil
}

func pick(args map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := args[k]; ok {
			out[k] = v
		}
	}
	return out
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{Name: "scoutdeck_status", Description: "Robot status: connection, battery, what is controlling it, obstacle distance ahead, heading and position since the dock, docking, guard mode, recording.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "GET", "/v1/status", nil, 10*time.Second)
			}},
		{Name: "scoutdeck_look", Description: "Take a photo with the robot's forward camera and return it as an image.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				call, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				res, err := c.request(call, "GET", "/v1/screenshot.jpg", nil, "")
				if err != nil {
					return nil, err
				}
				defer res.Body.Close()
				img, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
				if err != nil {
					return nil, err
				}
				return []map[string]any{{"type": "image", "data": base64.StdEncoding.EncodeToString(img), "mimeType": "image/jpeg"}}, nil
			}},
		{Name: "scoutdeck_move", Description: "Drive a measured distance and stop (closed loop on wheel odometry). Give distance_m (forward positive, backward negative) or strafe_m (left positive). Forward moves stop early for obstacles. Max 3 m.",
			InputSchema: obj(map[string]any{"distance_m": num("metres forward, negative backward"), "strafe_m": num("metres sideways, positive left"), "speed": num("m/s, optional (default 0.15)")}),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "POST", "/v1/move", pick(a, "distance_m", "strafe_m", "speed"), 165*time.Second)
			}},
		{Name: "scoutdeck_turn", Description: "Turn in place by an angle using the gyro. Positive degrees turn left (counter-clockwise), negative right. Max 720.",
			InputSchema: obj(map[string]any{"degrees": num("degrees, positive left"), "rate": num("rad/s, optional (default 1.0)")}, "degrees"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "POST", "/v1/turn", pick(a, "degrees", "rate"), 165*time.Second)
			}},
		{Name: "scoutdeck_stop", Description: "Stop all movement, cancel any move, patrol or docking.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "POST", "/v1/stop", nil, 15*time.Second)
			}},
		{Name: "scoutdeck_dock", Description: "Return to the charging dock (the robot searches for the dock's QR code and reverses onto it). Returns immediately; poll scoutdeck_status for progress.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "POST", "/v1/dock", map[string]int{"timeout_seconds": 300}, 15*time.Second)
			}},
		{Name: "scoutdeck_say", Description: "Speak text out loud through the robot's speaker.",
			InputSchema: obj(map[string]any{"text": str("what to say, up to 500 characters")}, "text"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "POST", "/v1/say", pick(a, "text"), 60*time.Second)
			}},
		{Name: "scoutdeck_listen", Description: "Record the robot's microphone for 1-10 seconds and return the audio (WAV).",
			InputSchema: obj(map[string]any{"seconds": num("1..10, default 3")}),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				secs := 3
				if v, ok := a["seconds"].(float64); ok {
					secs = int(v)
				}
				call, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				res, err := c.request(call, "GET", fmt.Sprintf("/v1/microphone.wav?seconds=%d", secs), nil, "")
				if err != nil {
					return nil, err
				}
				defer res.Body.Close()
				wav, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
				if err != nil {
					return nil, err
				}
				return []map[string]any{{"type": "audio", "data": base64.StdEncoding.EncodeToString(wav), "mimeType": "audio/wav"}}, nil
			}},
		{Name: "scoutdeck_light", Description: "Set the robot's light: dim, bright, max or auto.",
			InputSchema: obj(map[string]any{"setting": map[string]any{"type": "string", "enum": []string{"dim", "bright", "max", "auto"}}}, "setting"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				setting, _ := a["setting"].(string)
				cmd, ok := map[string]int{"dim": 0, "bright": 1, "max": 3, "auto": 4}[setting]
				if !ok {
					return nil, fmt.Errorf("setting must be dim, bright, max or auto")
				}
				return jsonCall(ctx, c, "POST", "/v1/light", map[string]int{"command": cmd}, 10*time.Second)
			}},
		{Name: "scoutdeck_events", Description: "Recent events: motion, people and pets seen while guard mode was armed, patrols and docking.",
			InputSchema: obj(map[string]any{"limit": num("1..300, default 20")}),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				limit := 20
				if v, ok := a["limit"].(float64); ok {
					limit = int(v)
				}
				return jsonCall(ctx, c, "GET", fmt.Sprintf("/v1/events?limit=%d", limit), nil, 10*time.Second)
			}},
		{Name: "scoutdeck_guard", Description: "Arm or disarm guard mode (alerts for motion, and people and pets if objects is true).",
			InputSchema: obj(map[string]any{"armed": boolean("arm (true) or disarm (false)"), "objects": boolean("also detect people and pets with the robot's model"), "notify": boolean("macOS notifications")}, "armed"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				if _, ok := a["armed"].(bool); !ok {
					return nil, fmt.Errorf("armed must be a boolean")
				}
				return jsonCall(ctx, c, "POST", "/v1/guard", pick(a, "armed", "objects", "notify"), 10*time.Second)
			}},
		{Name: "scoutdeck_routes", Description: "List saved patrol routes.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "GET", "/v1/routes", nil, 10*time.Second)
			}},
		{Name: "scoutdeck_patrol", Description: "Start a saved patrol route (it runs in the background, then docks if the route says so).",
			InputSchema: obj(map[string]any{"route": str("route name from scoutdeck_routes")}, "route"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				route, _ := a["route"].(string)
				if route == "" {
					return nil, fmt.Errorf("route must be a non-empty string")
				}
				return jsonCall(ctx, c, "POST", "/v1/routes/"+url.PathEscape(route)+"/run", nil, 15*time.Second)
			}},
		{Name: "scoutdeck_record", Description: "Start or stop recording video and sound to the Mac (Movies/ScoutDeck).",
			InputSchema: obj(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"start", "stop"}}}, "action"),
			call: func(ctx context.Context, c client, a map[string]any) ([]map[string]any, error) {
				action := fmt.Sprint(a["action"])
				if action != "start" && action != "stop" {
					return nil, fmt.Errorf("action must be start or stop")
				}
				return jsonCall(ctx, c, "POST", "/v1/record/"+action, nil, 20*time.Second)
			}},
		{Name: "scoutdeck_health", Description: "Robot vital signs: temperature, Wi-Fi signal, battery voltage and 24-hour history, storage, memory, firmware.",
			InputSchema: obj(map[string]any{}),
			call: func(ctx context.Context, c client, _ map[string]any) ([]map[string]any, error) {
				return jsonCall(ctx, c, "GET", "/v1/health", nil, 20*time.Second)
			}},
	}
}

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// serveMCP answers newline-delimited JSON-RPC 2.0 messages until stdin closes.
func serveMCP(ctx context.Context, c client, in io.Reader, out io.Writer) error {
	tools := mcpTools()
	byName := map[string]mcpTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, rerr *rpcError) error {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		return enc.Encode(msg)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := reply(json.RawMessage("null"), nil, &rpcError{-32700, "parse error"}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		var err error
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			version := mcpVersions[0]
			for _, v := range mcpVersions {
				if v == p.ProtocolVersion {
					version = v
				}
			}
			err = reply(req.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "scoutdeck", "version": "1.0"},
				"instructions": "Tools for a Moorebot Scout robot through the ScoutDeck Mac app. Use scoutdeck_look to see, scoutdeck_move and scoutdeck_turn " +
					"for measured movement, scoutdeck_stop to stop. A person at the Mac or phone always has priority over your commands.",
			}, nil)
		case "ping":
			err = reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			err = reply(req.ID, map[string]any{"tools": tools}, nil)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				err = reply(req.ID, nil, &rpcError{-32602, "invalid params"})
				break
			}
			t, ok := byName[p.Name]
			if !ok {
				err = reply(req.ID, nil, &rpcError{-32602, "unknown tool " + p.Name})
				break
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			content, cerr := t.call(ctx, c, p.Arguments)
			if cerr != nil {
				err = reply(req.ID, map[string]any{"content": textContent(cerr.Error()), "isError": true}, nil)
			} else {
				err = reply(req.ID, map[string]any{"content": content}, nil)
			}
		default:
			err = reply(req.ID, nil, &rpcError{-32601, "method not found: " + req.Method})
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}
