# Scout

A Go controller for Moorebot Scout with keyboard/gamepad controls and an
authenticated local API for agents. It supports bounded driving, camera streaming,
screenshots, microphone recording, speaker playback, lights, and QR-guided reverse
docking with live charging verification.

## Build and run

Use Go 1.24 or newer. Install `ffmpeg` for H.264 video or WAV microphone recordings.

```sh
go build -o dist/scout .
go build -o dist/scoutctl ./cmd/scoutctl

# Replace this example hostname with your robot's hostname or IP address.
export SCOUT_HOST=robot.example
./dist/scout -r "$SCOUT_HOST:11311" -ssh "root@$SCOUT_HOST"
```

`-r` selects the ROS master; its default is loopback. `-ssh` uses normal system SSH
authentication and enables speaker playback and live charging verification. The
app does not open SSH key files. Run `./dist/scout -h` for options.

Add `-api 127.0.0.1:8080` for agent access and `-headless` for API-only operation.
The API requires a Bearer token and binds only to loopback. See
[AGENT_API.md](AGENT_API.md) for startup, `scoutctl` commands, media formats,
movement limits, docking behavior and tests.

## Manual controls

Keyboard: W/S forward/back, A/D turn, Q/E strafe, O/P adjust speed, X stop,
H dock, 9/0 dim/brighten, Space screenshot, Esc exit.

Use `-c joystick` for a controller with a standard gamepad mapping. The left stick
moves and turns; the right stick strafes. Bumpers adjust speed, both bumpers stop
all axes, and Start exits. Release movement controls after a stop before resuming.
Action and speed buttons trigger once per press. The HUD shows command and battery
status.

## Local data

Keep tokens and machine-specific configuration in ignored `.env` or `*.local.*`
files. The app reads `SCOUT_API_TOKEN` from its environment; it does not load dotenv
files automatically. Captures, recordings, logs, binaries, private keys and local
recovery bundles are excluded by `.gitignore`. Generated files may still contain
private data; keep them out of source commits.

## Provenance

Based on [Shell-Company/go-scout](https://github.com/Shell-Company/go-scout).
The local [goroslib](third_party/goroslib) copy includes transport and cancellation
fixes; its upstream [MIT license](third_party/goroslib/LICENSE) is retained.
