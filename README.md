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

## Windows and macOS

Both platforms use the maintained root application. Keyboard and supported Xbox
controllers work together by default; no restart or control-mode switch is needed.
Keyboard movement takes priority while keys are held. Use `-c keyboard` to ignore
controllers. `-c gamepad` and the older `-c joystick` spelling also retain keyboard
fallback.

### Windows (PowerShell)

Build from the repository root with Go installed, or use the packaged `scout.exe`:

```powershell
go build -trimpath -o dist/scout.exe .
$ScoutHost = "robot.example" # Replace with your robot hostname or IP.
.\dist\scout.exe -r "${ScoutHost}:11311" -ssh "root@$ScoutHost"
.\dist\scout.exe -input-test
```

Connect the Xbox controller in Windows before using it. Allow Scout through the
Windows firewall on your trusted private network: ROS needs incoming callback
connections as well as access to the robot's master. `-ssh` requires Windows
OpenSSH Client and working normal SSH authentication. Omit it for driving/video
only. `ffmpeg` is optional for JPEG video and required for H.264/WAV recording.

### macOS

Pair a compatible Xbox controller in **System Settings → Bluetooth**. See
[Apple's controller connection guide](https://support.apple.com/en-ie/111099) for
model compatibility and connection options. Allow Scout's incoming ROS connections
in the firewall when prompted.

The macOS package includes `Scout.app` and `scoutctl`. Start the app with your
robot address from Terminal:

```sh
export SCOUT_HOST=robot.example
./Scout.app/Contents/MacOS/scout -r "$SCOUT_HOST:11311" -ssh "root@$SCOUT_HOST"
./Scout.app/Contents/MacOS/scout -input-test
```

Build packages with Python 3 and Go using `scripts/package.py --os darwin --arch
arm64` (Apple silicon), `--arch amd64` (Intel Mac), or `--os windows --arch amd64`
(Windows x64). Run macOS builds on a Mac. Archives are written under `dist/`.
Mac bundles are ad-hoc signed for a stable application identity, not notarized.

### Controls

| Action | Keyboard | Xbox controller |
|---|---|---|
| Forward/back and turn | W/S/A/D or arrow keys | Left stick |
| Strafe | Q/E | Right stick horizontal |
| Stop all movement / cancel docking | X | B or LB + RB |
| Increase/decrease speed | P/O | RB/LB |
| Dock | H | Y |
| Screenshot | Space | A |
| Brighten/dim | 0/9 | D-pad up/down |
| Automatic light | Agent API | X |
| Exit and stop | Escape | Menu/Start |

Manual forward speed is limited to 0.1–0.3 m/s; the initial setting is 0.2 m/s.
Sticks have a 15% dead zone to suppress drift. Center sticks and release buttons
when connecting. Losing focus or disconnecting the selected controller stops
manual motion; release all movement controls before resuming. Agent drive leases
remain governed by the separate [agent control contract](AGENT_API.md).

The HUD shows the selected controller, speed, control owner and stop state. The
app skips controllers without a standard layout and keeps the same selected
controller when another one connects. Controller support uses Ebitengine's
[standard gamepad mapping](https://ebitengine.org/en/documents/2.2.html).

Run `-input-test` to test input without a ROS connection. Movement appears in the
window and console; docking, screenshots and light buttons are logged instead of
sent to the robot. This is useful for checking pairing and mappings before driving.

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
