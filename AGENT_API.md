# Agent control API

Build and start on the computer connected to the Scout network:

```sh
go build -o dist/scout .
export SCOUT_API_TOKEN="$(openssl rand -hex 32)"
# Replace this example hostname with your robot hostname or IP address.
export SCOUT_HOST=robot.example
./dist/scout -r "$SCOUT_HOST:11311" -api 127.0.0.1:8080 -headless -ssh "root@$SCOUT_HOST"
```

Omit `-headless` to keep the window and manual controls. `-ssh` enables speaker
playback, live power checks and verified docking. SSH uses the system's normal authentication and known
hosts; the app does not read SSH key files. `ffmpeg` must be on PATH for H.264
video and WAV microphone recordings. JPEG, MJPEG and AAC streaming do not need it.

The listener accepts only literal loopback addresses. Use SSH port forwarding
when the agent runs elsewhere. Every endpoint requires
`Authorization: Bearer $SCOUT_API_TOKEN`. If the environment variable is absent,
the app generates a token and prints it once at startup. Do not put it in URLs.
Browser-origin requests are rejected; the API does not enable CORS.

## Commands and observations

| Method | Path | Request / response |
|---|---|---|
| GET | `/v1/status` | Control owner and velocity, battery readings with timestamps, camera freshness, capabilities and limits |
| POST | `/v1/drive` | JSON `{"linear_x":0,"linear_y":0.1,"angular_z":0,"ttl_ms":300}` |
| POST | `/v1/stop` | Clear the drive lease, cancel native docking and stop; no body required |
| POST | `/v1/light` | JSON `{"command":0}`: 0 down, 1 up, 3 max, 4 auto |
| POST | `/v1/dock` or `/v1/home` | Optional JSON `{"timeout_seconds":90}` (10–180); HTTP 202 returns a tracked operation |
| GET | `/v1/dock` | Current operation, phase, deadline, power evidence and terminal error |
| POST | `/v1/dock/cancel` | Cancel QR search and reverse docking, then send zero velocity |
| GET | `/v1/power` | Fresh SSH read of battery driver status; external power is true for Charging or Full |
| GET | `/v1/screenshot.jpg` | Latest JPEG; HTTP 503 if unavailable or older than three seconds |
| GET | `/v1/video.mjpeg` | Multipart MJPEG, up to 10 frames/sec; reconnect after a camera outage |
| GET | `/v1/microphone.aac` | Live AAC/ADTS audio from `/CoreNode/aac`; close the HTTP request to stop |
| GET | `/v1/microphone.wav?seconds=3` | Record 1–10 seconds from ROS AAC and convert locally to mono 16 kHz PCM WAV |
| POST | `/v1/speaker.wav` | WAV bytes (`Content-Type: audio/wav`); PCM, mono, 16 kHz, 16-bit, at most 30 seconds; requires `-ssh` |
| POST | `/v1/shutdown` | Stop and close the controller |

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

Manual input takes priority and cancels the agent lease. Both joystick bumpers
or keyboard **X** stop every axis. After a manual stop, release movement controls
before moving again. Stale manual input also stops after 350 ms. Escape,
controller Start, window close, Ctrl-C, SIGTERM and API shutdown all use the
same cleanup path: cancel active native docking, send zero velocity, wait for the transport write, then close
the publisher. No software command can guarantee a stop after a network failure;
robot-side behavior was not changed.

Examples:

```sh
curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8080/v1/status

curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8080/v1/screenshot.jpg -o scout.jpg

curl -fsS -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  'http://127.0.0.1:8080/v1/microphone.wav?seconds=3' -o microphone.wav

curl -fsS -X POST -H "Authorization: Bearer $SCOUT_API_TOKEN" \
  http://127.0.0.1:8080/v1/stop
```

The API deliberately has no arbitrary shell, ROS-topic or ROS-service endpoint.
Use the documented operations so agents cannot bypass movement expiry.

## Agent CLI

```sh
go build -o dist/scoutctl ./cmd/scoutctl
export SCOUT_API_URL=http://127.0.0.1:8080
# Uses the same SCOUT_API_TOKEN as the running app.
./dist/scoutctl status
./dist/scoutctl screenshot -out scout.jpg
./dist/scoutctl drive -y 0.05 -ttl 300
./dist/scoutctl stop
./dist/scoutctl dock -timeout 90
./dist/scoutctl power
./dist/scoutctl record -seconds 3 -out mic.wav
./dist/scoutctl speaker -in speech.wav
./dist/scoutctl video -out -
```

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
While docking is active, normal velocity publishing pauses so it cannot fight
firmware motion. Agent drive commands return 409; cancel docking before driving.
Held manual input or stop prevents docking from starting. Manual movement cancels
docking and requires releasing controls before resuming.

An HTTP 202 means the operation was accepted. Success requires two consecutive
fresh reads of `/sys/class/power_supply/rk-bat/status` showing `Charging` or
`Full`. A firmware success phase alone is insufficient. Starting while already
powered verifies charging without requesting movement. Battery percentages in
`/v1/status` are latched ROS reports; their receipt time is not a fresh power
measurement. Use `/v1/power` to check the current connection.

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
