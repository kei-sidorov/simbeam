// control.go drives simbeam-control, our thin native companion: it captures the
// simulator framebuffer over the private CoreSimulator IOSurface and encodes
// H.264 on VideoToolbox with keyframe control we own (replacing the old
// screenshot-poll → ffmpeg path and idb_companion's HID). One process per feed:
// video comes out of stdout as framed access units; input goes in as NDJSON.
package sim

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kei-sidorov/simbeam/internal/encoder"
	"github.com/kei-sidorov/simbeam/internal/server"
)

// controlBinaryName is resolved against PATH; brew installs it to
// /opt/homebrew/bin/simbeam-control.
const controlBinaryName = "simbeam-control"

// controlFPS is the native capture/encode rate. idb's screenshot poll capped the
// old path at 15; the IOSurface tap has no such ceiling, so we run at 30.
const controlFPS = 30

// controlKeyframeMs is the GOP length. A ~1s keyframe interval keeps scene
// changes and packet-loss recovery snappy while we own the encoder (unlike
// idb_companion's fixed ~10s GOP, decision №34).
const controlKeyframeMs = 1000

// ResolveControl returns the absolute path to simbeam-control, or a clear error
// telling the user how to install it.
func ResolveControl() (string, error) {
	path, err := exec.LookPath(controlBinaryName)
	if err != nil {
		return "", fmt.Errorf("simbeam-control not found in PATH (install with `brew install kei-sidorov/simbeam/simbeam-control`): %w", err)
	}
	return path, nil
}

// controlDims is the geometry from simbeam-control's stderr handshake: point
// dimensions (for scaling normalized input) and full-resolution pixel
// dimensions (reported to the client for aspect/coordinate mapping).
type controlDims struct {
	widthPoints, heightPoints float64
	pixelW, pixelH            uint64
}

// control is one running simbeam-control process for a single simulator.
type control struct {
	cmd    *exec.Cmd
	frames <-chan encoder.Frame

	mu    sync.Mutex     // guards w/stdin during writes and shutdown
	w     *bufio.Writer  // NDJSON control writer over stdin; nil once closed
	stdin io.WriteCloser // held open for the feed's lifetime (EOF shuts the process down)

	dmu  sync.RWMutex
	dims controlDims
}

// newControl spawns simbeam-control for udid at the requested quality and blocks
// until its first stderr handshake (so Screen and input scaling have geometry),
// mirroring the old idb Describe-before-return contract. The process is killed
// when ctx is cancelled.
func newControl(ctx context.Context, bin, udid string, q server.QualityOpts) (*control, error) {
	cmd := exec.CommandContext(ctx, bin,
		"--udid", udid,
		"--fps", strconv.Itoa(controlFPS),
		"--keyframe-interval-ms", strconv.Itoa(controlKeyframeMs),
		"--bitrate", strconv.Itoa(q.Bitrate),
		"--scale", strconv.FormatFloat(q.Scale, 'f', -1, 64),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	// Keep stdin OPEN for the feed's lifetime: simbeam-control shuts down on
	// stdin EOF, and an unset Stdin is /dev/null (instant EOF) — which killed it
	// right after the first frame. Closed on ctx cancel (below), by which point
	// CommandContext has also killed the process.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &control{cmd: cmd, stdin: stdin, w: bufio.NewWriter(stdin)}
	go func() {
		<-ctx.Done()
		c.closeStdin()
	}()

	// Read stderr: the first handshake unblocks us; later handshakes (rotation /
	// resize rebuild the encoder, README) update the geometry; everything else is
	// logged.
	ready := make(chan controlDims, 1)
	done := make(chan struct{})
	var lastLine string
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		firstSent := false
		for sc.Scan() {
			line := sc.Text()
			if d, ok := parseHandshake(line); ok {
				c.setDims(d)
				if !firstSent {
					firstSent = true
					ready <- d
				}
				continue
			}
			lastLine = line
			log.Printf("simbeam-control: %s", line)
		}
	}()

	select {
	case <-ready:
		c.frames = readControlFrames(ctx, stdout)
		return c, nil
	case <-done:
		c.Close()
		if lastLine != "" {
			return nil, fmt.Errorf("simbeam-control %s exited before handshake: %s", udid, lastLine)
		}
		return nil, fmt.Errorf("simbeam-control %s exited before handshake", udid)
	case <-time.After(15 * time.Second):
		c.Close()
		return nil, fmt.Errorf("simbeam-control %s: no handshake within 15s", udid)
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}

// handshake mirrors the stderr JSON simbeam-control emits when the encoder is
// ready: point dimensions, native scale, and the even-sized encoded video.
type handshake struct {
	Ready         bool    `json:"ready"`
	Width         float64 `json:"width"`  // simulator points
	Height        float64 `json:"height"` // simulator points
	Scale         float64 `json:"scale"`  // native display scale
	EncodedWidth  uint64  `json:"encoded_width"`
	EncodedHeight uint64  `json:"encoded_height"`
}

// parseHandshake decodes a ready-handshake line into controlDims. Pixel
// dimensions are the full retina resolution (points × scale), matching the
// screen size the old idb Describe reported; the encoded video is a scaled-down
// slice of it (decision №40) with the same aspect.
func parseHandshake(line string) (controlDims, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return controlDims{}, false
	}
	var h handshake
	if err := json.Unmarshal([]byte(line), &h); err != nil || !h.Ready {
		return controlDims{}, false
	}
	d := controlDims{widthPoints: h.Width, heightPoints: h.Height}
	if h.Scale > 0 {
		d.pixelW = uint64(math.Round(h.Width * h.Scale))
		d.pixelH = uint64(math.Round(h.Height * h.Scale))
	} else {
		d.pixelW, d.pixelH = h.EncodedWidth, h.EncodedHeight
	}
	return d, true
}

func (c *control) setDims(d controlDims) {
	c.dmu.Lock()
	c.dims = d
	c.dmu.Unlock()
}

func (c *control) getDims() controlDims {
	c.dmu.RLock()
	defer c.dmu.RUnlock()
	return c.dims
}

// screen reports the full-resolution pixel dimensions for the client's attached
// reply.
func (c *control) screen() (w, h uint64) {
	d := c.getDims()
	return d.pixelW, d.pixelH
}

// closeStdin flushes and closes the control writer, letting simbeam-control shut
// down on EOF. Idempotent.
func (c *control) closeStdin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		return
	}
	_ = c.w.Flush()
	_ = c.stdin.Close()
	c.w = nil
}

// Close stops the process: closing stdin asks it to exit, and CommandContext
// kills it on ctx cancel. Wait reaps it.
func (c *control) Close() error {
	c.closeStdin()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

// writeCmd serializes v as one NDJSON line to stdin. Input is fire-and-forget:
// once stdin is closed (ctx cancelled) writes are silently dropped.
func (c *control) writeCmd(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		return
	}
	if _, err := c.w.Write(b); err != nil {
		log.Printf("simbeam-control: write: %v", err)
		return
	}
	if err := c.w.Flush(); err != nil {
		log.Printf("simbeam-control: flush: %v", err)
	}
}

// Tap taps at a point scaled from normalized [0,1] frame coordinates into the
// simulator's point space (which is what simbeam-control expects).
func (c *control) Tap(nx, ny float64) {
	d := c.getDims()
	c.writeCmd(struct {
		Type string  `json:"type"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
	}{"tap", clamp01(nx) * d.widthPoints, clamp01(ny) * d.heightPoints})
}

// Swipe drags from (nx1,ny1) to (nx2,ny2) (normalized) over duration seconds.
func (c *control) Swipe(nx1, ny1, nx2, ny2, duration float64) {
	d := c.getDims()
	c.writeCmd(struct {
		Type       string  `json:"type"`
		X1         float64 `json:"x1"`
		Y1         float64 `json:"y1"`
		X2         float64 `json:"x2"`
		Y2         float64 `json:"y2"`
		DurationMs int     `json:"duration_ms"`
	}{"swipe",
		clamp01(nx1) * d.widthPoints, clamp01(ny1) * d.heightPoints,
		clamp01(nx2) * d.widthPoints, clamp01(ny2) * d.heightPoints,
		int(math.Round(duration * 1000))})
}

// Home presses the Home button.
func (c *control) Home() {
	c.writeCmd(struct {
		Type string `json:"type"`
	}{"home"})
}

// Key presses a USB HID keyboard usage code with an optional shift modifier.
func (c *control) Key(usage uint64, shift bool) {
	c.writeCmd(struct {
		Type  string `json:"type"`
		Usage uint64 `json:"usage"`
		Shift bool   `json:"shift"`
	}{"key", usage, shift})
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// readControlFrames converts simbeam-control's framed H.264 stdout —
// [4B len][1B flags][8B pts_micros][N bytes Annex-B] — into encoder.Frames.
// Duration is the measured pts delta (decision №93), not a fixed 1/fps. The
// channel closes when the process exits or ctx is cancelled.
func readControlFrames(ctx context.Context, stdout io.Reader) <-chan encoder.Frame {
	frames := make(chan encoder.Frame)
	go func() {
		defer close(frames)
		r := bufio.NewReaderSize(stdout, 1<<20)
		hdr := make([]byte, 13)
		nominal := time.Second / time.Duration(controlFPS)
		var lastPTS uint64
		first := true
		for {
			if _, err := io.ReadFull(r, hdr); err != nil {
				if ctx.Err() == nil && err != io.EOF {
					log.Printf("simbeam-control: read header: %v", err)
				}
				return
			}
			n := binary.BigEndian.Uint32(hdr[0:4])
			pts := binary.BigEndian.Uint64(hdr[5:13])
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				if ctx.Err() == nil {
					log.Printf("simbeam-control: read payload: %v", err)
				}
				return
			}
			dur := nominal
			if !first {
				if d := time.Duration(pts-lastPTS) * time.Microsecond; d > 0 {
					dur = d
				}
			}
			first = false
			lastPTS = pts
			select {
			case frames <- encoder.Frame{Data: buf, Duration: dur}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames
}
