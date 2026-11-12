package events

import (
	"image"
	"image/color"
	"math"
	"time"
)

// Detector finds motion between camera frames by comparing a coarse luma
// grid against a slowly updated background. It ignores frames while the robot
// itself moves, and whole-image changes such as lights switching or the
// camera entering night mode.
type Detector struct {
	// Sensitivity 1 (least) to 5 (most); 0 means 3.
	Sensitivity int

	bg      []float64
	w, h    int
	warm    int
	settle  time.Time
	lastBox *Box
}

const (
	gridW      = 64
	gridH      = 36
	cellThresh = 16.0 // luma levels
	globalFrac = 0.55 // more than this changed at once is a lighting change
)

func (d *Detector) minFrac() float64 {
	s := d.Sensitivity
	if s < 1 || s > 5 {
		s = 3
	}
	return []float64{0.05, 0.03, 0.015, 0.008, 0.004}[s-1]
}

// Reset forgets the background, e.g. after the robot moved.
func (d *Detector) Reset() { d.bg, d.warm = nil, 0 }

// Feed compares one frame. moving says the robot is being driven; motion is
// not reported then or for two seconds afterwards. It returns the changed
// fraction of the image and, when it counts as motion, the bounding box.
func (d *Detector) Feed(img image.Image, moving bool, now time.Time) (frac float64, box *Box) {
	grid := lumaGrid(img)
	if grid == nil {
		return 0, nil
	}
	if moving {
		d.settle = now.Add(2 * time.Second)
	}
	if d.bg == nil || len(d.bg) != len(grid) {
		d.bg, d.warm = grid, 0
		return 0, nil
	}
	// Remove overall exposure shifts before comparing.
	var mc, mb float64
	for i := range grid {
		mc += grid[i]
		mb += d.bg[i]
	}
	shift := (mc - mb) / float64(len(grid))
	changed := 0
	minX, minY, maxX, maxY := gridW, gridH, -1, -1
	for y := 0; y < gridH; y++ {
		for x := 0; x < gridW; x++ {
			i := y*gridW + x
			if math.Abs(grid[i]-shift-d.bg[i]) > cellThresh {
				changed++
				minX, minY = min(minX, x), min(minY, y)
				maxX, maxY = max(maxX, x), max(maxY, y)
			}
		}
	}
	frac = float64(changed) / float64(len(grid))
	alpha := 0.15
	report := d.warm >= 3 && !now.Before(d.settle) && frac >= d.minFrac() && frac < globalFrac
	if report {
		alpha = 0.03 // let a new, still object fade into the background slowly
		box = &Box{Left: float64(minX) / gridW, Top: float64(minY) / gridH, Right: float64(maxX+1) / gridW, Bottom: float64(maxY+1) / gridH}
	}
	if frac >= globalFrac || now.Before(d.settle) {
		alpha = 0.6
	}
	for i := range grid {
		d.bg[i] += (grid[i] - d.bg[i]) * alpha
	}
	d.warm++
	d.lastBox = box
	return frac, box
}

// lumaGrid averages the image into a gridW x gridH luma grid, sampling
// every few pixels to stay cheap on 1080p frames.
func lumaGrid(img image.Image) []float64 {
	b := img.Bounds()
	if b.Dx() < gridW || b.Dy() < gridH {
		return nil
	}
	sum := make([]float64, gridW*gridH)
	n := make([]float64, gridW*gridH)
	step := max(1, min(b.Dx()/(gridW*4), b.Dy()/(gridH*4)))
	ycc, isYCC := img.(*image.YCbCr)
	gray, isGray := img.(*image.Gray)
	for y := b.Min.Y; y < b.Max.Y; y += step {
		gy := (y - b.Min.Y) * gridH / b.Dy()
		for x := b.Min.X; x < b.Max.X; x += step {
			gx := (x - b.Min.X) * gridW / b.Dx()
			var l float64
			switch {
			case isYCC:
				l = float64(ycc.Y[ycc.YOffset(x, y)])
			case isGray:
				l = float64(gray.Pix[gray.PixOffset(x, y)])
			default:
				l = float64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			}
			i := gy*gridW + gx
			sum[i] += l
			n[i]++
		}
	}
	for i := range sum {
		if n[i] > 0 {
			sum[i] /= n[i]
		}
	}
	return sum
}
