package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// recorder saves the camera (and robot microphone) to an MP4 on the Mac with
// ffmpeg. Frames arrive as JPEGs; audio as AAC/ADTS packets. Both inputs use
// wall-clock timestamps so they stay in sync without a fixed frame rate.
type recorder struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	video   io.WriteCloser
	audio   io.WriteCloser
	path    string
	started time.Time
	frames  int
	cancel  context.CancelFunc
	done    chan struct{}
	waited  chan struct{} // closed when ffmpeg exits
	err     error
}

// MaxRecording bounds one local recording.
const MaxRecording = 30 * time.Minute

// RecordingStatus is the recording section of /v1/status.
type RecordingStatus struct {
	Active  bool    `json:"active"`
	File    string  `json:"file,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Frames  int     `json:"frames,omitempty"`
}

func (r *recorder) status() RecordingStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil {
		return RecordingStatus{}
	}
	return RecordingStatus{Active: true, File: filepath.Base(r.path), Seconds: time.Since(r.started).Seconds(), Frames: r.frames}
}

func videoEncoder() []string {
	if runtime.GOOS == "darwin" {
		return []string{"-c:v", "h264_videotoolbox", "-b:v", "6M", "-allow_sw", "1"}
	}
	return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "23"}
}

// Start begins recording to dir. It returns the file path.
func (r *recorder) Start(parent context.Context, dir string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("recording needs ffmpeg (brew install ffmpeg)")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		return "", fmt.Errorf("already recording %s", filepath.Base(r.path))
	}
	if f := media.Frame(); len(f.Data) == 0 || time.Since(f.At) > 3*time.Second {
		return "", fmt.Errorf("no live video to record")
	}
	path := filepath.Join(dir, "scout-"+time.Now().Format("2006-01-02-150405")+".mp4")
	ctx, cancel := context.WithTimeout(parent, MaxRecording+15*time.Second)

	// Wait briefly for microphone packets; record video only without them.
	audioCh, unsub := media.SubscribeAudio()
	var firstAudio []byte
	select {
	case firstAudio = <-audioCh:
	case <-time.After(1500 * time.Millisecond):
	}
	if firstAudio == nil {
		unsub()
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-thread_queue_size", "64", "-use_wallclock_as_timestamps", "1", "-f", "mjpeg", "-i", "pipe:0"}
	if firstAudio != nil {
		args = append(args, "-thread_queue_size", "512", "-use_wallclock_as_timestamps", "1", "-f", "aac", "-i", "pipe:3", "-map", "0:v", "-map", "1:a", "-c:a", "copy")
	}
	args = append(args, videoEncoder()...)
	args = append(args, "-pix_fmt", "yuv420p", "-fps_mode", "vfr", "-movflags", "+faststart", path)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	video, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return "", err
	}
	var audioW *os.File
	if firstAudio != nil {
		ar, aw, err := os.Pipe()
		if err != nil {
			cancel()
			return "", err
		}
		cmd.ExtraFiles = []*os.File{ar}
		audioW = aw
		defer ar.Close()
	}
	if err := cmd.Start(); err != nil {
		cancel()
		if audioW != nil {
			audioW.Close()
		}
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	r.cmd, r.video, r.path, r.started, r.frames, r.cancel, r.err = cmd, video, path, time.Now(), 0, cancel, nil
	r.done = make(chan struct{})
	waited := make(chan struct{})
	r.waited = waited
	if audioW != nil {
		r.audio = audioW
		go func() {
			defer unsub()
			_, _ = audioW.Write(firstAudio)
			for {
				select {
				case <-ctx.Done():
					return
				case <-r.done:
					return
				case p := <-audioCh:
					if _, err := audioW.Write(p); err != nil {
						return
					}
				}
			}
		}()
	}
	go r.feed(ctx, video)
	go func() {
		defer close(waited)
		err := cmd.Wait()
		r.mu.Lock()
		if err != nil && !strings.Contains(err.Error(), "signal") {
			r.err = fmt.Errorf("recording failed: %v %s", err, strings.TrimSpace(stderr.String()))
			log.Print(r.err)
		}
		r.mu.Unlock()
	}()
	log.Printf("recording to %s", path)
	return path, nil
}

// feed writes each new camera frame to ffmpeg until stopped.
func (r *recorder) feed(ctx context.Context, w io.Writer) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var last time.Time
	limit := time.NewTimer(MaxRecording)
	defer limit.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-limit.C:
			go func() { _, _ = r.Stop() }()
			return
		case <-t.C:
		}
		f := media.Frame()
		if f.At.Equal(last) || len(f.Data) == 0 {
			continue
		}
		last = f.At
		if _, err := w.Write(f.Data); err != nil {
			return
		}
		r.mu.Lock()
		r.frames++
		r.mu.Unlock()
	}
}

// Stop finishes the file and returns its path.
func (r *recorder) Stop() (string, error) {
	r.mu.Lock()
	cmd, video, audio, path, cancel, done, waited := r.cmd, r.video, r.audio, r.path, r.cancel, r.done, r.waited
	if cmd == nil {
		r.mu.Unlock()
		return "", fmt.Errorf("not recording")
	}
	r.cmd, r.video, r.audio = nil, nil, nil
	r.mu.Unlock()
	close(done)
	_ = video.Close()
	if audio != nil {
		_ = audio.Close()
	}
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		cancel()
		<-waited
	}
	cancel()
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return path, err
	}
	if st, statErr := os.Stat(path); statErr != nil || st.Size() == 0 {
		return path, fmt.Errorf("recording produced no file")
	}
	log.Printf("recording saved: %s", path)
	return path, nil
}

// toggleRecording starts or stops a local recording from the window.
func (a *application) toggleRecording() {
	go func() {
		if a.rec.status().Active {
			path, err := a.rec.Stop()
			if err != nil {
				a.notices.add(err.Error(), noticeBad)
				return
			}
			a.notices.add("Recording saved: "+path, noticeGood)
			return
		}
		path, err := a.rec.Start(a.ctx, mediaDir())
		if err != nil {
			a.notices.add(err.Error(), noticeBad)
			return
		}
		a.notices.add("Recording to "+filepath.Base(path)+" - press R to stop", noticeInfo)
	}()
}
