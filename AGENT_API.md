# Agent control API

The Scout app serves this API, and the web app, on `127.0.0.1:8765` by default
(`-api <loopback:port>` to change it, `-api off` to disable):

```sh
go build -o dist/scout .
./dist/scout -host 192.168.1.97              # window + API + web app
./dist/scout -host 192.168.1.97 -headless    # API only
```

`-ssh` (default `root@<host>`) enables speaker playback, live power checks and
verified docking. SSH uses the system's normal authentication; the app does not
read SSH key files. `ffmpeg` must be on PATH for H.264 video and WAV microphone
recordings. JPEG, MJPEG and AAC streaming do not need it.

The listener accepts only literal loopback addresses. Use `tailscale serve` (see
the README) or SSH port forwarding for other devices. Agents authenticate with
`Authorization: Bearer <token>`. The token is `$SCOUT_API_TOKEN` if set, otherwise
a random token the app saves in `~/Library/Application Support/Scout/api-token`
(mode 0600); `scoutctl` reads that file automatically. Do not put it in URLs.
Bearer requests with a browser `Origin` are rejected and the API does not enable
CORS. The paired web app instead uses an HttpOnly, SameSite=Strict session cookie
derived from the token; it can only change state from its own origin and cannot
call `/v1/shutdown`. Rotating the token (delete the file) unpairs all browsers.

While the app is not connected to a robot, endpoints other than `/v1/status`
return 503; `/v1/status` reports `robot.connected`.

## Commands and observations

| Method | Path | Request / response |
|---|---|---|
| GET | `/v1/status` | Control owner and velocity, battery readings with timestamps, camera freshness, capabilities and limits |
| POST | `/v1/drive` | JSON `{"linear_x":0,"linear_y":0.1,"angular_z":0,"ttl_ms":300}` |
| POST | `/v1/stop` | Clear the drive lease, cancel native docking and stop; no body required |
| POST | `/v1/light` | JSON `{"command":0}`: 0 down, 1 up, 3 max, 4 auto |
| POST | `/v1/dock` or `/v1/home` | Optional JSON `{"timeout_seconds":300}` (10–600); HTTP 202 returns a tracked operation |
| GET | `/v1/dock` | Current operation, phase, deadline, power evidence and terminal error |
| POST | `/v1/dock/cancel` | Cancel QR search and reverse docking, then send zero velocity |
| GET | `/v1/autodock` | Auto-dock on low battery: `{"enabled":false,"threshold":20}` |
| POST | `/v1/autodock` | JSON `{"enabled":true,"threshold":20}` (10–50); saved with the app settings |
| GET | `/v1/microphone.pcm` | Continuous 16 kHz mono s16le PCM decoded from the robot microphone (needs `ffmpeg`) |
| GET | `/v1/power` | Fresh SSH read of battery driver status; external power is true for Charging or Full |
| GET | `/v1/screenshot.jpg` | Latest JPEG; HTTP 503 if unavailable or older than three seconds |
| GET | `/v1/video.mjpeg` | Multipart MJPEG, up to 10 frames/sec; reconnect after a camera outage |
| GET | `/v1/microphone.aac` | Live AAC/ADTS audio from `/CoreNode/aac`; close the HTTP request to stop |
| GET | `/v1/microphone.wav?seconds=3` | Record 1–10 seconds from ROS AAC and convert locally to mono 16 kHz PCM WAV |
| POST | `/v1/speaker.wav` | WAV bytes (`Content-Type: audio/wav`); PCM, mono, 16 kHz, 16-bit, at most 30 seconds; requires `-ssh` |
| POST | `/v1/shutdown` | Stop and close the controller |
| POST | `/v1/move` | JSON `{"distance_m":0.5}` (forward +, back −) or `{"strafe_m":-0.2}` (left +), optional `"speed"`; at most 3 m. Blocks until done and returns `{"completed","achieved","stopped_by","feedback"}` |
| POST | `/v1/turn` | JSON `{"degrees":90}` (left +, right −; at most 720), optional `"rate"` in rad/s; gyro closed loop, blocks until done |
| GET | `/v1/pose` | Dead-reckoned pose since the dock (`x`, `y` metres, `yaw` radians) and a breadcrumb trail |
| POST | `/v1/pose/reset` | Make the current position the map origin |
| GET/POST | `/v1/obstacle-guard` | `{"manual":true}`: also limit manual driving near obstacles (moves, patrols and agents are always limited) |
| GET/POST | `/v1/guard` | Guard mode `{"armed":true,"sensitivity":3,"objects":true,"notify":true,"cooldown":30}` |
| GET | `/v1/events?limit=50&since=RFC3339` | Event timeline, newest first: motion, person, cat, dog, patrol, dock, info |
| GET | `/v1/events/{id}.jpg` | Snapshot stored with an event |
| DELETE | `/v1/events` | Delete every event and snapshot |
| POST | `/v1/record/start`, `/v1/record/stop` | Record camera and microphone to an MP4 on the Mac (`~/Movies/Scout`, at most 30 minutes) |
| GET | `/v1/robot-recordings` | Clips and snapshots stored on the robot by its firmware |
| GET/DELETE | `/v1/robot-recordings/{id}` | Download (over SSH) or delete one |
| GET/POST | `/v1/camera` | Video settings; POST `{"name":"image_night_mode","value":1}` (night vision 0 off, 1 on, 2 auto; brightness, WDR, compression) |
| GET | `/v1/routes` | Saved patrol routes and any route being recorded |
| POST | `/v1/routes/record` | `{"name":"Downstairs"}` starts recording a route from manual driving |
| POST | `/v1/routes/record/stop` | Saves it (`{"discard":true}` drops it) |
| POST | `/v1/routes/{name}/run` | Starts a patrol (HTTP 202); `/v1/stop` cancels it |
| POST/DELETE | `/v1/routes/{name}` | `{"every_minutes":120,"dock_after":true}` schedules it; DELETE removes it |
| GET | `/v1/health` | Temperature, Wi-Fi signal, battery voltage and 24-hour history, storage, memory, firmware |
| POST | `/v1/say` | `{"text":"Hello"}` speaks through the robot (macOS voice; needs SSH) |

JSON requests require `Content-Type: application/json`. Unknown fields, malformed
JSON and invalid values are rejected. Service calls run independently from
movement and have a three-second deadline, including lookup and handshake.
Concurrent service calls are rejected as busy rather than queued. Audio requests
are bounded and cancel when their HTTP connection closes.

## Driving contract

Velocities use Scout's existing mapping: positive `linear_y` is forward,
`linear_x` is strafe, and `angular_z` is rotation. Limits for agent commands are
`|linear_x| <= 0.2`, `|linear_y| <= 0.3`, and `|angular_z| <= 1.8`.

A drive command lasts 100–500 ms (default 300 ms). Renew approximately every
100 ms while moving; an expired lease produces zero velocity on the next
100 ms control tick. Stop explicitly when finished. An HTTP success means the
command was accepted by this controller, not that the robot physically moved.
The status velocity is the most recent transport submission, not odometry.

Manual input takes priority and cancels the agent lease. Xbox **B**, both bumpers together,
or keyboard **X** stop every axis. After a manual stop, release movement controls
before moving again. Stale manual input also stops after 350 ms. Escape,
controller Start, window close, Ctrl-C, SIGTERM and API shutdown all use the
same cleanup path: cancel active native docking, send zero velocity, wait for the transport write, then close
the publisher. No software command can guarantee a stop after a network failure;
robot-side behavior was not changed.

Examples:

```sh
curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8765/v1/status

curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8765/v1/screenshot.jpg -o scout.jpg

curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  'http://127.0.0.1:8765/v1/microphone.wav?seconds=3' -o microphone.wav

curl -fsS -X POST -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8765/v1/stop
```

The API deliberately has no arbitrary shell, ROS-topic or ROS-service endpoint.
Use the documented operations so agents cannot bypass movement expiry.

## Agent CLI

```sh
go build -o dist/scoutctl ./cmd/scoutctl
export SCOUT_API_URL=http://127.0.0.1:8765
# Uses the same SCOUT_API_TOKEN as the running app.
./dist/scoutctl status
./dist/scoutctl screenshot -out scout.jpg
./dist/scoutctl drive -y 0.05 -ttl 300
./dist/scoutctl stop
./dist/scoutctl dock -timeout 300
./dist/scoutctl power
./dist/scoutctl record -seconds 3 -out mic.wav
./dist/scoutctl speaker -in speech.wav
./dist/scoutctl video -out -
./dist/scoutctl move -m 0.5          # measured: odometry closed loop
./dist/scoutctl turn -deg -90        # measured: gyro closed loop (right)
./dist/scoutctl say -text "Hello"
./dist/scoutctl guard -arm on
./dist/scoutctl events -limit 10
./dist/scoutctl routes
./dist/scoutctl patrol -name Downstairs
./dist/scoutctl camera -name image_night_mode -value 2
./dist/scoutctl health
```

### MCP server

`scoutctl mcp` serves the API as Model Context Protocol tools on stdio, so an
assistant can look, move, turn, speak, listen, dock, patrol and arm guard mode. Register this stdio command with your MCP client:

```sh
/Applications/Scout.app/Contents/MacOS/scoutctl mcp
```

It uses the same token and limits as every other agent; a person at the Mac or a
phone always has priority.

JSON responses go to stdout, JSON errors to stderr, and failures exit nonzero.
Media output requires `-out`; existing files are never overwritten. `drive`
issues one short lease. `dock` waits by default and emits newline-delimited JSON
status until charging is verified; use `-wait=false` to return after acceptance.
Interrupting a waiting dock command or losing status polling requests cancellation.
`dock-status` and `dock-cancel` are also available. Use `COMMAND -h` for flags.

## Docking and charging contract

Scout's firmware finds the charging station's QR marker, aligns, turns and
**reverses into the contacts**. Place it near the station with a clear approach.
The app uses this native sequence rather than estimating a reverse distance.

**Assisted docking.** When the map origin is the dock (the app connected while
the robot was charging, or after a verified docking), the app first drives to a
point 0.6 m in front of the dock facing it, using the gyro and wheel odometry,
so the QR marker is in view; then it starts the firmware sequence. The approach
stops for obstacles (then turns to face the dock and hands over) and yields to
manual input like any other move. If the firmware gives up or times out, and
nobody drove or pressed Stop during the attempt, the app starts one more search.

While the firmware docks, the app lets go of the wheel-odometry topic: it is
zeroed when its first subscriber connects and the firmware's alignment depends
on that (with the app subscribed, the firmware's camera node crashed during
alignment in 3 of 7 test dockings; without, 0 of 4). The map position is
therefore unknown until the next verified docking resets it. If the firmware
node does restart mid-docking, the app keeps retrying the cancellation every
5 seconds so the robot is not left locked in `cancel_failed`.
While docking is active, normal velocity publishing pauses so it cannot fight
firmware motion. Agent drive commands return 409; cancel docking before driving.
Held manual input or stop prevents docking from starting. Manual movement cancels
docking and requires releasing controls before resuming.

An HTTP 202 means the operation was accepted. Success requires two consecutive
fresh reads of `/sys/class/power_supply/rk-bat/status` showing `Charging`,
`Full` or `Not charging` (charger connected, battery not charging). A firmware success phase alone is insufficient. Starting while already
powered verifies charging without requesting movement. Battery percentages in
`/v1/status` are latched ROS reports; their receipt time is not a fresh power
measurement. Use `/v1/power` to check the current connection.

Firmware phases are 1 search, 2 align, 3 reverse, 4 success, 5 fail, 6 inactive,
7 cancel and 8 re-detect. A search from across a room can take two minutes, and
the firmware retries missed reversals itself, so allow several minutes. If the
firmware fails, or goes inactive or cancels after starting, without external power
for five seconds, the operation ends as `failed` instead of waiting for the deadline.

The operation deadline triggers cancellation of both the QR search and backing
sequence, followed by a zero velocity command. Terminal states include `charging`,
`failed`, `timed_out` and `cancelled`. If cancellation cannot be confirmed,
`cancel_failed` stays active and blocks driving; retry `/v1/dock/cancel` after
connectivity returns. Inspect `error` and `power.observed_at`. A completed
operation's `charging_verified` is historical evidence, not continuous monitoring.

The operation is owned by the controller, so disconnecting the start HTTP request
does not cancel it. Keep the controller alive; abrupt process termination or
network loss can prevent cancellation of robot-side autonomy. A normal shutdown
attempts cancellation before closing ROS connections. The app does not configure
persistent low-battery return behavior on the robot.

## Offline validation

```sh
go test -race ./...
go vet ./...
(cd third_party/goroslib && go test -race -run '^TestScout' .)
```

These tests use synthetic inputs and loopback mock servers. They do not connect
to the robot. Other upstream goroslib integration tests require Docker/ROS.

## ROS connectivity on macOS

ROS requires the robot to connect back to this app on dynamically assigned TCP
ports. Receiving camera frames does not prove the movement publisher is reachable.
Allow incoming connections for the Scout executable in the macOS firewall; use
`-l YOUR_LAN_IP` only if automatic address selection chooses the wrong interface.

Go's default ad-hoc signing identifier can be `a.out`, which macOS may match to
an unrelated executable's firewall rule. A distinct identity can be assigned
after building, before adding the app-specific firewall permission:

```sh
codesign --force --sign - --identifier com.scout.controller dist/scout
```

Reapply that identity after rebuilding. Keep the firewall enabled and leave
other application rules unchanged. The HTTP agent API still binds only to
loopback; the ROS callback ports are separate.
