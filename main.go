package main

import (
	"context"
	"image"
	"image/color"
	"log"
	"os"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/pion/mediadevices"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/prop"
)

type motionConfig struct {
	sensitivity     int
	enabled         bool
	smoothingFactor float64 // 0 = без сглаживания, 1 = максимально плавно
	mu              sync.RWMutex
}

func (c *motionConfig) setSensitivity(val int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sensitivity = val
}
func (c *motionConfig) getSensitivity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sensitivity
}
func (c *motionConfig) setEnabled(val bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = val
}
func (c *motionConfig) getEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}
func (c *motionConfig) setSmoothingFactor(val float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.smoothingFactor = val
}
func (c *motionConfig) getSmoothingFactor() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.smoothingFactor
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func differenceScore(c1, c2 color.Color) int64 {
	r1, g1, b1, a1 := c1.RGBA()
	r2, g2, b2, a2 := c2.RGBA()
	return absInt64((int64(r1) + int64(g1) + int64(b1) + int64(a1)) -
		(int64(r2) + int64(g2) + int64(b2) + int64(a2)))
}

func drawCross(img *image.RGBA, centerX, centerY int, clr color.Color) {
	const armLen = 35
	const thick = 3
	bounds := img.Bounds()

	for dx := -armLen; dx <= armLen; dx++ {
		x := centerX + dx
		if x < bounds.Min.X || x >= bounds.Max.X {
			continue
		}
		for dy := -thick / 2; dy <= thick/2; dy++ {
			y := centerY + dy
			if y >= bounds.Min.Y && y < bounds.Max.Y {
				img.Set(x, y, clr)
			}
		}
	}
	for dy := -armLen; dy <= armLen; dy++ {
		y := centerY + dy
		if y < bounds.Min.Y || y >= bounds.Max.Y {
			continue
		}
		for dx := -thick / 2; dx <= thick/2; dx++ {
			x := centerX + dx
			if x >= bounds.Min.X && x < bounds.Max.X {
				img.Set(x, y, clr)
			}
		}
	}
}

type smoothedPosition struct {
	x, y  float64
	first bool
	mu    sync.Mutex
}

func (sp *smoothedPosition) update(rawX, rawY int, smoothness float64) (int, int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if !sp.first {
		sp.x, sp.y = float64(rawX), float64(rawY)
		sp.first = true
	} else {

		sp.x = smoothness*sp.x + (1-smoothness)*float64(rawX)
		sp.y = smoothness*sp.y + (1-smoothness)*float64(rawY)
	}
	return int(sp.x + 0.5), int(sp.y + 0.5)
}

const blur = 0.6

func detectMotionAndDraw(frame image.Image, prevFrame image.Image, cfg *motionConfig, smoother *smoothedPosition) (*image.RGBA, int, int64) {
	bounds := frame.Bounds()
	out := image.NewRGBA(bounds)

	if !cfg.getEnabled() || prevFrame == nil {
		return out, 0, 0
	}

	drawImage(out, frame)

	sensitivity := cfg.getSensitivity()
	points := make([]image.Point, 0, bounds.Dx()*bounds.Dy()/100)
	var totalDiff int64 = 0

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			diff := differenceScore(frame.At(x, y), prevFrame.At(x, y))
			totalDiff += diff
			if diff > int64(sensitivity) {
				points = append(points, image.Point{X: x, Y: y})
			}
		}
	}

	if len(points) > 0 {
		var sumX, sumY int64
		for _, p := range points {
			sumX += int64(p.X)
			sumY += int64(p.Y)
		}
		rawX := int(sumX / int64(len(points)))
		rawY := int(sumY / int64(len(points)))
		centerX, centerY := smoother.update(rawX, rawY, cfg.getSmoothingFactor())
		drawCross(out, centerX, centerY, color.RGBA{255, 255, 0, 255})
	}

	return out, len(points), totalDiff
}

func drawImage(dst *image.RGBA, src image.Image) {
	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
}

func copyImage(img image.Image) image.Image {
	bounds := img.Bounds()
	newImg := image.NewRGBA(bounds)
	drawImage(newImg, img)
	return newImg
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &motionConfig{
		sensitivity:     40000,
		enabled:         true,
		smoothingFactor: 0.8,
	}
	smoother := &smoothedPosition{}

	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(constraint *mediadevices.MediaTrackConstraints) {
			constraint.Width = prop.Int(640)
			constraint.Height = prop.Int(480)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	videoTracks := stream.GetVideoTracks()
	if len(videoTracks) == 0 {
		log.Fatal("no video track")
	}
	videoTrack := videoTracks[0].(*mediadevices.VideoTrack)
	defer videoTrack.Close()

	videoReader := videoTrack.NewReader(false)

	frameCh := make(chan image.Image, 2)

	numOfPixLabel := widget.NewLabel("0")

	var i int

	go func() {
		var prevFrame image.Image
		for {
			select {
			case <-ctx.Done():
				return
			default:
				frame, release, err := videoReader.Read()
				if err != nil {
					log.Println("read error:", err)
					return
				}
				processed, _, totalDiff := detectMotionAndDraw(frame, prevFrame, cfg, smoother)

				if i%4 == 0 {
					fyne.Do(func() {
						averageDiff := totalDiff / int64(640*480)
						if averageDiff > 12000 {
							numOfPixLabel.SetText("Быстро")
						} else if averageDiff > 8000 {
							numOfPixLabel.SetText("Средняя скорость")
						} else {
							numOfPixLabel.SetText("Медленно")
						}
					})
				}

				i++

				prevFrame = copyImage(frame)
				release()
				select {
				case frameCh <- processed:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	myApp := app.New()
	window := myApp.NewWindow("Умная камера")
	window.Resize(fyne.NewSize(1200, 1200))

	videoCanvas := canvas.NewImageFromImage(<-frameCh)
	videoCanvas.FillMode = canvas.ImageFillContain

	sensitivitySlider := widget.NewSlider(0, 50000)
	sensitivitySlider.SetValue(float64(cfg.getSensitivity()))
	sensitivitySlider.Step = 100
	sensitivityLabel := widget.NewLabel("Чувствительность: " + formatInt(cfg.getSensitivity()))
	sensitivitySlider.OnChanged = func(v float64) {
		val := int(v)
		cfg.setSensitivity(val)
		sensitivityLabel.SetText("Чувствительность: " + formatInt(val))
	}

	smoothingSlider := widget.NewSlider(0, 1)
	go smoothingSlider.SetValue(cfg.getSmoothingFactor())
	smoothingSlider.Step = 0.01
	smoothingLabel := widget.NewLabel("Сглаживание: " + formatFloat(cfg.getSmoothingFactor()))
	smoothingSlider.OnChanged = func(v float64) {
		cfg.setSmoothingFactor(v)
		smoothingLabel.SetText("Сглаживание: " + formatFloat(v))
	}

	controls := container.NewVBox(
		sensitivityLabel, sensitivitySlider,
		smoothingLabel, smoothingSlider,
	)

	window.SetContent(container.NewBorder(controls, numOfPixLabel, nil, nil, videoCanvas))

	go func() {
		for img := range frameCh {
			videoCanvas.Image = img
			fyne.Do(videoCanvas.Refresh)
		}
	}()

	window.SetOnClosed(func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
		close(frameCh)
		os.Exit(0)
	})
	window.SetIcon(resourceIconIco)

	window.ShowAndRun()
}

func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func formatFloat(f float64) string {
	return formatInt(int(f*100+0.5)) + "%"
}

// 40000 / 80
