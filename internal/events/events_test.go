package events

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func frame(fill uint8, rect image.Rectangle, rectFill uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, 640, 360))
	for i := range img.Pix {
		img.Pix[i] = fill
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.SetGray(x, y, color.Gray{rectFill})
		}
	}
	return img
}

func TestDetectorFindsMotion(t *testing.T) {
	var d Detector
	now := time.Now()
	empty := image.Rectangle{}
	for i := 0; i < 5; i++ {
		if _, box := d.Feed(frame(100, empty, 0), false, now); box != nil {
			t.Fatal("static scene reported motion")
		}
		now = now.Add(500 * time.Millisecond)
	}
	r := image.Rect(400, 100, 480, 300)
	frac, box := d.Feed(frame(100, r, 220), false, now)
	if box == nil || frac <= 0 {
		t.Fatal("moving object not detected")
	}
	if box.Left < 0.55 || box.Right > 0.8 || box.Top < 0.2 || box.Bottom > 0.9 {
		t.Fatalf("box %+v", box)
	}
}

func TestDetectorIgnoresLightingAndOwnMotion(t *testing.T) {
	var d Detector
	now := time.Now()
	for i := 0; i < 5; i++ {
		d.Feed(frame(100, image.Rectangle{}, 0), false, now)
		now = now.Add(500 * time.Millisecond)
	}
	if _, box := d.Feed(frame(160, image.Rectangle{}, 0), false, now); box != nil {
		t.Fatal("uniform brightness change reported as motion")
	}
	if _, box := d.Feed(frame(100, image.Rect(0, 0, 640, 300), 250), false, now); box != nil {
		t.Fatal("whole-scene change reported as motion")
	}
	d.Reset()
	for i := 0; i < 5; i++ {
		d.Feed(frame(100, image.Rectangle{}, 0), false, now)
		now = now.Add(500 * time.Millisecond)
	}
	if _, box := d.Feed(frame(100, image.Rect(100, 100, 200, 200), 250), true, now); box != nil {
		t.Fatal("motion reported while the robot moves")
	}
}

func TestLogPersistsAndTrims(t *testing.T) {
	dir := t.TempDir()
	l := &Log{Dir: dir, Max: 3}
	ch, unsub := l.Subscribe()
	defer unsub()
	var first Event
	for i := 0; i < 5; i++ {
		e, err := l.Add("motion", "Motion", []byte{0xff, 0xd8, 0xff, 0xd9}, 0.5, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = e
		}
	}
	if got := <-ch; got.Kind != "motion" {
		t.Fatal("subscriber not notified")
	}
	if _, err := os.Stat(filepath.Join(dir, first.ID+".jpg")); !os.IsNotExist(err) {
		t.Fatal("trimmed snapshot not removed")
	}
	again := &Log{Dir: dir, Max: 3}
	list := again.List(0, time.Time{})
	if len(list) != 3 || !list[0].At.After(list[2].At) && list[0].At != list[2].At {
		t.Fatalf("reload: %d events", len(list))
	}
	if data, err := again.Snapshot(list[0].ID); err != nil || len(data) != 4 {
		t.Fatal("snapshot not readable")
	}
	if _, err := again.Snapshot("../../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := again.Clear(); err != nil || len(again.List(0, time.Time{})) != 0 {
		t.Fatal("clear failed")
	}
}
