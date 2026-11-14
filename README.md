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

./dist/scout                      # asks for the robot's IP in the window
./dist/scout -host 192.168.1.97   # or pass it once on the command line
```

The robot address is saved in `~/Library/Application Support/Scout/settings.json`
(macOS; the OS config directory elsewhere) and reused next time. Defaults derive from
it: ROS master `<host>:11311` and SSH as `root@<host>`. SSH uses your existing keys
or agent (the app never opens key files) and enables speaker playback and verified
docking. On first connect, the robot's host key is added to `known_hosts`; a changed
key is still refused. Override with `-r host:port`, `-ssh user@host` or `-no-ssh`.
Run `./dist/scout -h` for options.

The window shows a HUD over the live video: connection, video rate, SSH, recording,
guard mode, night vision and battery (read over SSH every 5 s, so docking shows
within seconds) across the top; a map of where the robot has been, with the dock
and the forward sensor's view, and the latest events with snapshots on the right;
control owner, speed, distance to the nearest obstacle ahead and motion at the
bottom left; docking, move and patrol progress at the bottom centre; and boxes
around motion, people and pets on the video. Tab hides the map and events, F
toggles full screen. Press ? for the controls.

New in this version:

- **Measured moves and turns.** `/v1/move` and `/v1/turn` (and the voice assistant,
  web app and MCP tools) drive a distance with wheel-odometry feedback and heading
  hold, and turn by an angle with gyro feedback, instead of timing the motors.
- **Obstacle guard.** The forward time-of-flight sensor (up to 2 m, filtered because
  it flickers near its limits) slows forward motion from 0.6 m and stops it at
  0.18 m. Always on for moves, patrols and agents; switchable for manual driving.
- **Guard mode (G).** While the robot is parked, the Mac watches the video for
  motion and the robot's own model reports people, cats and dogs. Each alert is
  logged with a snapshot (Events in the web app and HUD), optionally with a macOS
  notification.
- **Patrol routes.** Record a drive from the dock, replay it as a patrol (turns and
  straight runs closed loop), optionally on a schedule; it docks afterwards.
- **Assisted docking.** Before handing over to the firmware's QR search, the app
  drives to a point in front of the dock from its own map, facing it; if the
  firmware then gives up, it searches once more. Patrols that are blocked or
  stuck go home instead of stopping off the charger.
- **Recording (R).** Camera and microphone to an MP4 in `~/Movies/Scout`.
- **Robot clips, camera settings and health** pages in the menu and web app.
- **Talk and listen** from the phone: hold to talk through the robot's speaker
  (HTTPS/Tailscale address), listen to its microphone, or type text for it to say.
- **MCP server:** `scoutctl mcp` lets assistants use the robot (see AGENT_API.md).

Press M for the Robot menu (Wi-Fi and reset need SSH). It also has **Patrol
routes**, **Guard mode & alerts**, **Camera**, **Recordings on the robot**,
**Robot health** and the **Obstacle guard** switch, described above, plus:

- **Wi-Fi network** scans from the robot and moves it to another network. The
  credentials are saved where the firmware reads them at boot, and an on-robot
  watchdog restores the previous network (or hotspot) and reboots if the robot is
  not connected within three minutes. Afterwards press Tab on the connect screen to
  find the robot's new address.
- **Reset settings** runs the firmware's own reset (`scout_reset2.sh`): it clears
  Wi-Fi, the Moorebot cloud link, timezone, maps, patrols and recordings, then
  reboots into hotspot mode (`robot-scout-xxxxxx`, password `r0123456`, robot at
  `10.42.0.1`). The OS, SSH keys and root password are kept.
- **Phone & tablet** shares the web app over Tailscale and shows a pairing QR code.
- **Voice assistant** (also V) turns on the on-device assistant described below.
- **Auto-dock on low battery** (off by default): Enter toggles it, Left/Right sets
  the threshold (10-50%, default 20%). When the robot is on battery at or below the
  threshold, the app warns for 30 seconds, then starts docking. Driving or pressing
  Stop during the warning postpones it 5 minutes; failed attempts retry up to three
  times per discharge, 5 minutes apart, and it re-arms once the robot charges. It
  needs SSH (to verify charging) and works only while this app is running. The
  same switch is in the web app's ⋯ menu.
- **Factory restore** reinstalls the system from the factory image on the robot's
  backup partition by rebooting into recovery. It first checks that the image and
  restore script are present, no OTA image is pending, the bootloader message is
  free, and the robot is on the charger with at least 50% battery; then you type
  RESTORE. It erases SSH keys and the root password (back to factory), Wi-Fi and
  settings, and returns in hotspot mode. Set or note the root password first: you
  need it to reinstall your SSH key (`ssh-copy-id root@10.42.0.1`). This path has
  not yet been exercised on a robot.

If the robot reboots or leaves the network, the window returns to the connect
screen; Tab scans this Mac's local networks for robots.

### Voice assistant (Apple on-device model)

Press V (or start with `-voice`) and talk to the robot: "Scout, turn around",
"Scout, go forward half a meter", "Scout, look left", "Scout, what's your battery
level?", "Scout, go home". Saying just "Scout" waits eight seconds for the command,
and "stop" works without the wake word while the robot is moving.

`scout-voice` (Swift, built into `Scout.app`; source in `voice/main.swift`) runs
entirely on this Mac: it streams the robot's microphone (`/v1/microphone.pcm`),
transcribes with Apple's SpeechAnalyzer, handles common commands directly, asks
Apple's on-device foundation model (with move, turn, stop, dock, light, status,
patrol, guard and record tools) for everything else, and speaks the reply through
the robot's speaker. It acts only through the agent API, so speed limits, short
drive leases, manual priority and the obstacle guard still apply. Named move and
turn goals use closed-loop gyro and odometry feedback. Requires Apple silicon, macOS 26 or later with Apple Intelligence on,
and SSH for the speaker. Use `--source mac` to listen through the Mac's microphone,
and `--text "…" --dry-run` to test commands without moving the robot.

### iPhone, iPad and browser

The app serves a responsive web HUD: live video, battery, docking progress,
Stop/Dock/Snapshot/light buttons and a speed selector. Phones and tablets get an
on-screen joystick and strafe slider (portrait: video on top, controls under your
thumbs; landscape: full-screen video with controls in the corners). Browsers on the
Mac use the same keyboard keys as the app and Xbox controllers via the Gamepad API.

To use it from an iPhone or iPad, install Tailscale on both devices and sign in to
the same account. In the Mac app press M → **Phone & tablet**, press T once to
share Scout on your tailnet (`tailscale serve --bg --https=8765 …`; existing serve
handlers are left alone), then scan the QR code. It opens Scout already paired; use
Share → Add to Home Screen for a full-screen app. The web app is only reachable
from your own Tailscale devices and still requires pairing. Press B to open it in a
browser on the Mac itself.

Without Tailscale, press L on the same page (or start with `-lan`) to also serve
the web app on this Mac's home Wi-Fi address, e.g. `http://192.168.1.164:8765`.
This is plain HTTP on your network; devices still pair, and the agent API (Bearer
tokens) stays available only on this Mac. Allow incoming connections if macOS asks.
The HUD's bottom-right panel always shows the phone address(es).

To pair without the QR code, open the address and type the **one-time code** shown
in the HUD and on the Phone & tablet page: six digits, valid for 10 minutes, usable
once, revoked after five wrong guesses. Press N for a new code.

The agent API and the web app run on `127.0.0.1:8765` (`-api off` disables them);
add `-headless` for API-only operation.
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
open Scout.app          # enter the robot IP in the window; it is remembered
./Scout.app/Contents/MacOS/scout -host 192.168.1.97
./Scout.app/Contents/MacOS/scout -input-test
```

`python3 scripts/package.py --os darwin --arch arm64` also leaves a ready-to-run
`dist/Scout.app` with its icon (source: `scripts/make_icon.swift`). Double-click it,
or drag it to Applications. The first launch asks for **Local Network** access;
allow it, or the app cannot reach the robot (System Settings → Privacy & Security →
Local Network). Screenshots from the app bundle go to `~/Pictures/Scout`.

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
| Record video on the Mac | R | - |
| Guard mode on/off | G | - |
| Clean HUD / full screen | Tab / F | - |
| Exit and stop | Escape | Menu/Start |

Docking (H or Y) allows up to five minutes: the firmware's QR search can take two
minutes and it retries a missed reverse itself. Charging is confirmed over SSH.

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
files automatically. Events and snapshots, patrol routes and battery history are
kept next to the settings (`events/`, `routes.json`, `battery.jsonl`).
Captures, recordings, logs, binaries, private keys and local
recovery bundles are excluded by `.gitignore`. Generated files may still contain
private data; keep them out of source commits.

## Provenance

Based on [Shell-Company/go-scout](https://github.com/Shell-Company/go-scout).
The local [goroslib](third_party/goroslib) copy includes transport and cancellation
fixes; its upstream [MIT license](third_party/goroslib/LICENSE) is retained.
