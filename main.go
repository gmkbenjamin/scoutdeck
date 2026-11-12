package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msg"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/shell-company/scout/internal/control"
)

var (
	cameraData         = make(chan []byte, 8)
	ROSHostAddress     = "127.0.0.1:11311"
	localhostAddress   = "127.0.0.1"
	flagWindowX        = flag.Int("windowX", 1920, "window width")
	flagWindowY        = flag.Int("windowY", 1080, "window height")
	flagStream         = flag.String("stream", "jpg", "video stream: jpg or h264")
	flagVideoRes       = flag.String("video-res", "auto", "video resolution preset: auto, 720p, 1080p, 1440p")
	flagDumpH264       = flag.String("dump-h264", "", "optional path to dump normalized h264 bitstream for ffplay/analysis")
	WindowX            int
	WindowY            int
	flagROSHostAddress = flag.String("r", ROSHostAddress, "ROS master endpoint (IP:PORT)")
	flaglocalhost      = flag.String("l", localhostAddress, "localhost address")
	flagControlScheme  = flag.String("c", "auto", "controls: auto (keyboard + gamepad), keyboard, or gamepad (joystick alias)")
	flagVerbose        = flag.Bool("v", false, "verbose")
	n                  *goroslib.Node
	err                error
	lastCameraFeed     *ebiten.Image
	batteryPercent     = -1
	batteryChargeState = -1
	h264Dec            *h264Decoder
	lastH264FrameUnixN int64
	lastH264PacketUnix int64
	h264NoFrameWarned  uint32
	h264ParamMu        sync.Mutex
	h264SPS            []byte
	h264PPS            []byte
	h264DumpFile       *os.File
	media              = &control.Media{}
	batteryMu          sync.RWMutex
	batteryAt          time.Time
	showHelp           bool
)

func init() {
	flag.BoolVar(&showHelp, "h", false, "show help message and exit")
	flag.BoolVar(&showHelp, "help", false, "show help message and exit")
}

const (
	Frame_VIDEO_STREAM_H264 int8 = 0
	Frame_VIDEO_STREAM_JPG  int8 = 1
	Frame_AUDIO_STREAM_AAC  int8 = 2
)

func enqueueCameraFrame(frame []byte) {
	if len(frame) == 0 {
		return
	}
	media.PutFrame(frame)
	select {
	case cameraData <- frame:
	default:
		select {
		case <-cameraData:
		default:
		}
		select {
		case cameraData <- frame:
		default:
		}
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "go-scout — Moorebot Scout controller\n\n")
	fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Options:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Keyboard controls (always available):
  W/S/A/D or arrows  forward/back/turn
  Q/E         strafe left/right
  O/P         decrease/increase speed
  X           stop all movement
  H           return to charging station
  9/0         night vision brightness -/+
  Space       save screenshot
  R           record video to ~/Movies/Scout
  G           guard mode (motion, people and pet alerts)
  Tab / F     clean HUD / full screen
  ?           show/hide controls
  M           Robot menu (patrols, guard, camera, recordings, health,
              obstacle guard, Wi-Fi, reset, phone & tablet)
  Esc         exit

Xbox controller (auto-detected by default):
  Left stick move/turn; right stick strafe; LB/RB speed (0.1-0.3 m/s)
  B or both bumpers stop; Y dock; A screenshot; X auto light
  D-pad up/down brightness; Menu/Start exits
  Center sticks and release buttons after connecting or stopping.

Robot address: pass -host <ip> once, or type it in the window. It is saved
for next time. SSH defaults to root@<host> with your existing SSH keys.

Use -input-test to check controls without connecting to a robot.
`)
}

func main() {
	flag.Usage = printUsage
	flag.Parse()
	if showHelp {
		printUsage()
		return
	}
	prepareBundleEnv()
	applyVideoResolutionPreset(*flagVideoRes)
	if err := runApplication(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func applyVideoResolutionPreset(preset string) {
	switch preset {
	case "720p":
		WindowX = 1280
		WindowY = 720
	case "1080p":
		WindowX = 1920
		WindowY = 1080
	case "1440p":
		WindowX = 2560
		WindowY = 1440
	case "auto":
		WindowX = *flagWindowX
		WindowY = *flagWindowY
	default:
		log.Printf("warning: unknown -video-res %q, using auto", preset)
		WindowX = *flagWindowX
		WindowY = *flagWindowY
	}
}

func streamTopicFromFlag(stream string) string {
	switch stream {
	case "jpg":
		return "/CoreNode/jpg"
	case "h264":
		return "/CoreNode/h264"
	default:
		log.Printf("warning: unknown -stream %q, using jpg", stream)
		return "/CoreNode/jpg"
	}
}

type h264Decoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func newH264Decoder() (*h264Decoder, error) {
	cmd := exec.Command(
		"ffmpeg",
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-f", "h264",
		"-i", "pipe:0",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "3",
		"pipe:1",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 4096), 1024*1024)
		lines := 0
		for sc.Scan() {
			line := sc.Text()
			if *flagVerbose || lines < 4 {
				log.Printf("ffmpeg(h264): %s", line)
			} else if lines == 4 {
				log.Printf("ffmpeg(h264): additional stderr output suppressed (enable -v for full logs)")
			}
			if strings.Contains(line, "non-existing PPS") || strings.Contains(line, "no frame!") {
				enableH264Fallback("ffmpeg reported missing PPS/no frame")
			}
			lines++
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			log.Printf("warning: ffmpeg stderr read failed: %v", err)
		}
	}()

	go decodeMJPEGStream(stdout)

	return &h264Decoder{cmd: cmd, stdin: stdin}, nil
}

func decodeMJPEGStream(r io.Reader) {
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				start := bytes.Index(buf, []byte{0xFF, 0xD8})
				if start < 0 {
					if len(buf) > 2<<20 {
						buf = buf[:0]
					}
					break
				}
				endRel := bytes.Index(buf[start+2:], []byte{0xFF, 0xD9})
				if endRel < 0 {
					if start > 0 {
						buf = append([]byte{}, buf[start:]...)
					}
					break
				}
				end := start + 2 + endRel + 2
				frame := append([]byte(nil), buf[start:end]...)
				atomic.StoreInt64(&lastH264FrameUnixN, time.Now().UnixNano())
				atomic.StoreUint32(&h264NoFrameWarned, 0)
				select {
				default:
				}
				enqueueCameraFrame(frame)
				buf = append([]byte{}, buf[end:]...)
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("warning: ffmpeg mjpeg decode stream ended: %v", err)
			}
			enableH264Fallback("ffmpeg decode stream stopped")
			return
		}
	}
}

func (d *h264Decoder) Write(data []byte) error {
	if d == nil || d.stdin == nil {
		return fmt.Errorf("h264 decoder not initialized")
	}
	_, err := d.stdin.Write(data)
	return err
}

func (d *h264Decoder) Close() {
	if d == nil {
		return
	}
	if d.stdin != nil {
		_ = d.stdin.Close()
	}
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		_, _ = d.cmd.Process.Wait()
	}
}

// onMessageFrame is called when a message is received from the selected camera topic
func onMessageFrame(msg *Frame) {
	if *flagStream == "h264" {
		if msg.Type != Frame_VIDEO_STREAM_H264 {
			if *flagVerbose {
				log.Printf("h264 stream: ignoring non-video packet type=%d size=%d", msg.Type, len(msg.Data))
			}
			return
		}

		atomic.StoreInt64(&lastH264PacketUnix, time.Now().UnixNano())
		atomic.StoreUint32(&h264NoFrameWarned, 0)
		if h264Dec == nil {
			enableH264Fallback("h264 decoder unavailable")
			return
		}

		payload := normalizeH264AnnexB(msg.Data)
		payload = repairH264Bitstream(payload)
		if len(payload) == 0 {
			enableH264Fallback("received empty/unparseable h264 payload")
			return
		}

		if h264DumpFile != nil {
			if _, err := h264DumpFile.Write(payload); err != nil && *flagVerbose {
				log.Printf("warning: failed to write h264 dump: %v", err)
			}
		}

		if err := h264Dec.Write(payload); err != nil {
			log.Printf("warning: h264 decode write error: %v", err)
			enableH264Fallback("h264 decode write failure")
		}
		return
	}
	// write camera data to channel
	enqueueCameraFrame(msg.Data)
}

func enableH264Fallback(reason string) {
	if atomic.CompareAndSwapUint32(&h264NoFrameWarned, 0, 1) {
		log.Printf("error: h264 stream unhealthy: %s", reason)
	}
}

func normalizeH264AnnexB(data []byte) []byte {
	if len(data) < 1 {
		return nil
	}
	if hasAnnexBStartCode(data) {
		return data
	}

	// Some ROS packets carry AVC length-prefixed NAL units. Convert to Annex-B for ffmpeg pipe input.
	if converted, ok := convertAVCCToAnnexB(data, 4); ok {
		return converted
	}
	if converted, ok := convertAVCCToAnnexB(data, 2); ok {
		return converted
	}

	// Last-resort single NAL framing.
	out := make([]byte, 4+len(data))
	copy(out, []byte{0x00, 0x00, 0x00, 0x01})
	copy(out[4:], data)
	return out
}

func hasAnnexBStartCode(data []byte) bool {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x00, 0x00, 0x00, 0x01}) {
		return true
	}
	return len(data) >= 3 && bytes.Equal(data[:3], []byte{0x00, 0x00, 0x01})
}

func convertAVCCToAnnexB(data []byte, naluLengthBytes int) ([]byte, bool) {
	if len(data) <= naluLengthBytes {
		return nil, false
	}

	offset := 0
	out := make([]byte, 0, len(data)+16)
	parsed := 0
	for {
		if offset+naluLengthBytes > len(data) {
			break
		}

		var naluLen int
		switch naluLengthBytes {
		case 4:
			naluLen = int(binary.BigEndian.Uint32(data[offset : offset+4]))
		case 2:
			naluLen = int(binary.BigEndian.Uint16(data[offset : offset+2]))
		default:
			return nil, false
		}
		offset += naluLengthBytes
		if naluLen <= 0 || offset+naluLen > len(data) {
			return nil, false
		}

		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, data[offset:offset+naluLen]...)
		offset += naluLen
		parsed++
		if offset == len(data) {
			break
		}
	}

	if parsed == 0 || offset != len(data) {
		return nil, false
	}
	return out, true
}

func splitAnnexBNALs(data []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(data)-3 {
		start := -1
		for i < len(data)-3 {
			if i+4 <= len(data) && bytes.Equal(data[i:i+4], []byte{0, 0, 0, 1}) {
				start = i + 4
				i += 4
				break
			}
			if i+3 <= len(data) && bytes.Equal(data[i:i+3], []byte{0, 0, 1}) {
				start = i + 3
				i += 3
				break
			}
			i++
		}
		if start < 0 || start >= len(data) {
			break
		}
		end := len(data)
		j := i
		for j < len(data)-3 {
			if j+4 <= len(data) && bytes.Equal(data[j:j+4], []byte{0, 0, 0, 1}) {
				end = j
				break
			}
			if j+3 <= len(data) && bytes.Equal(data[j:j+3], []byte{0, 0, 1}) {
				end = j
				break
			}
			j++
		}
		if end > start {
			nal := append([]byte(nil), data[start:end]...)
			out = append(out, nal)
		}
		i = end
	}
	return out
}

func repairH264Bitstream(annexB []byte) []byte {
	nals := splitAnnexBNALs(annexB)
	if len(nals) == 0 {
		return annexB
	}

	hasSPS := false
	hasPPS := false
	hasIDR := false
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		t := nal[0] & 0x1F
		switch t {
		case 7:
			hasSPS = true
			h264ParamMu.Lock()
			h264SPS = append([]byte(nil), nal...)
			h264ParamMu.Unlock()
		case 8:
			hasPPS = true
			h264ParamMu.Lock()
			h264PPS = append([]byte(nil), nal...)
			h264ParamMu.Unlock()
		case 5:
			hasIDR = true
		}
	}

	if !hasIDR || (hasSPS && hasPPS) {
		return annexB
	}

	h264ParamMu.Lock()
	sps := append([]byte(nil), h264SPS...)
	pps := append([]byte(nil), h264PPS...)
	h264ParamMu.Unlock()
	if len(sps) == 0 || len(pps) == 0 {
		return annexB
	}

	if *flagVerbose {
		log.Printf("h264: prepending cached SPS/PPS before IDR")
	}
	out := make([]byte, 0, len(annexB)+len(sps)+len(pps)+8)
	out = append(out, 0, 0, 0, 1)
	out = append(out, sps...)
	out = append(out, 0, 0, 0, 1)
	out = append(out, pps...)
	out = append(out, annexB...)
	return out
}

func onBatteryStatus(msg *status) {
	batteryMu.Lock()
	defer batteryMu.Unlock()
	batteryAt = time.Now()
	if len(msg.Status) > 0 {
		batteryChargeState = int(msg.Status[0])
	}
	if len(msg.Status) > 1 {
		batteryPercent = int(msg.Status[1])
	}
	if *flagVerbose {
		log.Printf("battery raw status: %v", msg.Status)
	}
}

// squashToFloat converts a signed 16 bit integer to a float between -1 and 1
func squashToFloat(n int) (f float64) {
	if n < -32767 {
		f = -1.0
	} else if n > 32768 {
		f = 1.0
	} else {
		value := float64(n) / 32768.0
		if math.Abs(value) < 0.015 {
			f = 0.0
		} else {
			f = value
		}
	}
	return f * .5
}

func resolveNodeHost(masterAddress, configuredHost string) string {
	if configuredHost != "" && configuredHost != "127.0.0.1" && configuredHost != "localhost" {
		return configuredHost
	}

	masterHost, _, err := net.SplitHostPort(masterAddress)
	if err != nil {
		return configuredHost
	}

	conn, err := net.DialTimeout("udp", net.JoinHostPort(masterHost, "11311"), 3*time.Second)
	if err != nil {
		return configuredHost
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr == nil || localAddr.IP == nil {
		return configuredHost
	}

	if ip := localAddr.IP.To4(); ip != nil {
		return ip.String()
	}
	return localAddr.IP.String()
}

func forceROSHostnameToMasterIP(masterAddress string, verbose bool) error {
	masterHost, _, err := net.SplitHostPort(masterAddress)
	if err != nil {
		return err
	}

	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("http.DefaultTransport is not *http.Transport")
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := baseTransport.Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dialer.DialContext(ctx, network, addr)
		}

		if net.ParseIP(host) == nil && host != "localhost" {
			rewritten := net.JoinHostPort(masterHost, port)
			if verbose {
				log.Printf("rewriting ROS host %q -> %q", addr, rewritten)
			}
			return dialer.DialContext(ctx, network, rewritten)
		}

		return dialer.DialContext(ctx, network, addr)
	}

	http.DefaultTransport = transport
	return nil
}

type status struct {
	msg.Package     `ros:"roller_eye"`
	msg.Definitions `ros:"int8 PROCESS_OK=0,int8 PROCESS_ERROR=-1,int8 OBJ_DETECT_CHARGE=1,int8 RECORD_START=1,int8 RECORD_STOP=2,int8 RECORD_ERROR=3,int8 P2P_AV_PLAYING=1,int8 P2P_AV_STOP=2,int8 P2P_AV_ERROR=3,int8 WIFI_MODE_AP=0,int8 WIFI_MODE_STA=1,int8 WIFI_STATUS_DISCONNECT=0,int8 WIFI_STATUS_CONNECTED=1,int8 WIFI_STATUS_CONNECTING=2,int8 WIFI_STATUS_WRONG_KEY=3,int8 WIFI_STATUS_CONN_FAIL=4,int8 WIFI_STATUS_STOP=5,int8 BACK_UP_DETECT=1,int8 BACK_UP_ALIGN=2,int8 BACK_UP_BACK=3,int8 BACK_UP_SUCCESS=4,int8 BACK_UP_FAIL=5,int8 BACK_UP_INACTIVE=6,int8 BACK_UP_CANCEL=7,int8 BACK_UP_REDETECT=8,int8 BATTERY_CHARGING=0,int8 BATTERY_UNCHARGE=1,int8 BATTERY_FULL=2,int8 BATTERY_UNKOWN=3,int32[] status"`
	Status          []int32 `ros:"status"`
}

// Frame is a struct that holds the image data from the camera topic roller_bot/frame
type Frame struct {
	msg.Package     `ros:"roller_eye"`
	msg.Definitions `ros:"int8 VIDEO_STREAM_H264=0,int8 VIDEO_STREAM_JPG=1,int8 AUDIO_STREAM_AAC=2"`
	Seq             uint32
	Stamp           uint64
	Session         uint32
	Type            int8
	Oseq            uint32
	Par1            int32
	Par2            int32
	Par3            int32
	Par4            int32
	Data            []uint8
}

func saveScreenshot() (string, error) {
	name, err := media.Save(screenshotDir())
	if err != nil {
		return "", err
	}
	log.Printf("Image saved: %s", name)
	return name, nil
}
