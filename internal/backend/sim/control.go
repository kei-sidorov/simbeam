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
	"path/filepath"
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

// requiredControlProtocol is the minimum simbeam-control protocol this daemon
// depends on: 3 = typed startup failures (the ready:false envelope) and the
// in-helper wait for the simulator's first framebuffer. Bump it whenever the
// daemon starts requiring a new helper capability — brew cannot pin dependency
// versions, so this preflight is the only thing standing between an upgraded
// daemon and a stale helper silently dropping commands.
//
// 3 is a hard minimum rather than a preference: a helper older than 3 emits no
// failure envelope at all, so every cold-boot failure would arrive as "exited
// before handshake" prose and the whole typed-failure contract this daemon
// promises its clients (issue #4) would be a lie.
const requiredControlProtocol = 3

// controlStartupTimeout is what we pass as --startup-timeout-ms: simbeam-control's
// own bound on waiting for the simulator's first framebuffer IOSurface.
// Protocol 3 does that wait in-process (woken by the CoreSimulator
// surface-change callback), which is why the daemon has no attach-retry loop
// for the ordinary post-boot case: reaching this deadline means the device
// genuinely never produced a surface, and the answer to that is a restart, not
// another attach.
//
// controlStartupTimeout < controlHandshakeTimeout < server.AttachTimeout, and
// the ordering is load-bearing (asserted in the tests). Whoever's clock fires
// first owns the outcome: if ours did, the client gets "we killed a process
// that never spoke"; if the helper's does, it gets display_not_ready and knows
// exactly what to do.
const controlStartupTimeout = 20 * time.Second

// controlHandshakeTimeout is our own backstop for a helper that neither
// handshakes nor reports a typed failure within its own deadline — a wedge, not
// a slow simulator.
const controlHandshakeTimeout = 25 * time.Second

// CheckControlProtocol preflights `simbeam-control --protocol` and fails with
// an actionable error when the helper is older than the daemon requires
// (mirrors companion.CheckToolchain). A helper that predates the flag exits
// non-zero on the unknown argument — that is protocol 1.
func CheckControlProtocol(ctx context.Context, bin string) error {
	out, err := exec.CommandContext(ctx, bin, "--protocol").Output()
	if v := controlProtocol(out, err); v < requiredControlProtocol {
		hint := "simbeamd update"
		if resolved, rerr := filepath.EvalSymlinks(bin); rerr == nil &&
			(strings.Contains(resolved, "/Caskroom/") || strings.Contains(resolved, "/Cellar/")) {
			hint = "brew upgrade simbeam-control"
		}
		return fmt.Errorf("simbeam-control at %s speaks control protocol %d, this simbeamd needs >= %d — update it with `%s`", bin, v, requiredControlProtocol, hint)
	}
	return nil
}

// controlProtocol maps the --protocol invocation outcome to a version: the
// printed integer on success, 1 for any failure or unparsable output.
func controlProtocol(out []byte, err error) int {
	if err != nil {
		return 1
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || v < 1 {
		return 1
	}
	return v
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

	// readers counts the goroutines reading the stdout/stderr pipes. Close must
	// wait on them before cmd.Wait(), which closes the pipes: reading a pipe
	// after Wait has closed it is a documented os/exec fd-reuse race.
	readers sync.WaitGroup
	// closed is shut by Close so the frames goroutine unblocks from a channel
	// send even when nothing is draining and ctx was not cancelled first — Close
	// must not depend on the caller's teardown order to reap its readers.
	closed    chan struct{}
	closeOnce sync.Once

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
		// Passed explicitly rather than left to the helper's default, so the
		// daemon's bound and the helper's stay in one place and cannot drift
		// apart across a brew upgrade.
		"--startup-timeout-ms", strconv.Itoa(int(controlStartupTimeout/time.Millisecond)),
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

	c := &control{cmd: cmd, stdin: stdin, w: bufio.NewWriter(stdin), closed: make(chan struct{})}
	go func() {
		<-ctx.Done()
		c.closeStdin()
	}()

	// Read stderr: the ready handshake unblocks us; a ready:false envelope fails
	// us with the helper's own code; later handshakes (rotation / resize rebuild
	// the encoder, README) update the geometry; everything else is logged.
	ready := make(chan controlDims, 1)
	failed := make(chan *server.AttachError, 1)
	done := make(chan struct{})
	var lastLine string
	c.readers.Add(1)
	go func() {
		defer c.readers.Done()
		defer close(done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		firstSent := false
		for sc.Scan() {
			line := sc.Text()
			e, ok := parseControlEvent(line)
			switch {
			case !ok:
				// Plain-text logging. Kept as lastLine: if the helper then dies
				// without an envelope, this is the only clue we can hand up.
				lastLine = line
				log.Printf("simbeam-control: %s", line)
			case e.Ready == nil:
				// The framebuffer-wait notice a cold boot emits before the
				// handshake. It settles nothing, and it must NOT become
				// lastLine — reporting "exited before handshake: {waiting…}"
				// would name the wait as the failure instead of whatever
				// actually killed the process.
				log.Printf("simbeam-control: %s", line)
			case *e.Ready:
				d := e.dims()
				c.setDims(d)
				if !firstSent {
					firstSent = true
					ready <- d
				}
			default:
				log.Printf("simbeam-control %s: startup failed: %s", udid, line)
				select {
				case failed <- e.attachError(udid):
				default: // protocol 3 sends at most one; ignore a repeat
				}
			}
		}
	}()

	select {
	case <-ready:
		c.frames = c.readFrames(ctx, stdout)
		return c, nil
	case err := <-failed:
		c.Close()
		return nil, err
	case <-done:
		c.Close()
		// The envelope and the exit race each other down the same pipe, so an
		// envelope that landed in the same batch as EOF is still the real cause.
		select {
		case err := <-failed:
			return nil, err
		default:
		}
		if ctx.Err() != nil {
			// Cancelled mid-startup: a detach, a newer attach, or the session
			// ending. Protocol 3 stops promptly with a plain-text note and exit
			// 0 — no envelope — precisely so this is not reported as a failure.
			// The intent that superseded us owns the client's reply.
			return nil, ctx.Err()
		}
		if lastLine != "" {
			return nil, fmt.Errorf("simbeam-control %s exited before handshake: %s", udid, lastLine)
		}
		return nil, fmt.Errorf("simbeam-control %s exited before handshake", udid)
	case <-time.After(controlHandshakeTimeout):
		c.Close()
		return nil, fmt.Errorf("simbeam-control %s: no handshake within %s", udid, controlHandshakeTimeout)
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}

// controlEvent is one JSON line on simbeam-control's stderr. Protocol 3 emits
// three kinds before video flows, and `ready` is the discriminator:
//
//	{"waiting":"framebuffer","protocol":3,"timeout_ms":15000}      — cold boot, still waiting
//	{"ready":true,"width":…,"height":…,"scale":…,"protocol":3}     — the handshake
//	{"ready":false,"error":"display_not_ready","message":…,"retryable":true} — one terminal failure
//
// Two things make this a parser rather than a line index. The handshake is NOT
// the first line on a cold boot — the waiting notice precedes it — so anything
// reading stderr line 1 breaks on every cold start. And key order is not stable
// (the helper marshals with JSONSerialization), so the discriminator has to be
// read as JSON, never matched as text.
//
// Ready is a pointer because absent and false mean different things: the
// waiting line has no `ready` at all and settles nothing, while `ready:false`
// is the terminal failure.
type controlEvent struct {
	Ready   *bool  `json:"ready"`
	Waiting string `json:"waiting"`
	// Failure envelope. Note the field names: `error` and `message`, not the
	// `code`/`msg` this daemon uses on the wire to its own clients.
	Error     string `json:"error"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	// Handshake geometry.
	Width         float64 `json:"width"`  // simulator points
	Height        float64 `json:"height"` // simulator points
	Scale         float64 `json:"scale"`  // native display scale
	EncodedWidth  uint64  `json:"encoded_width"`
	EncodedHeight uint64  `json:"encoded_height"`
}

// parseControlEvent decodes one stderr line, reporting false for anything that
// isn't a JSON object (the helper's plain-text logging).
func parseControlEvent(line string) (controlEvent, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return controlEvent{}, false
	}
	var e controlEvent
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return controlEvent{}, false
	}
	return e, true
}

// dims maps a ready handshake to controlDims. Pixel dimensions are the full
// retina resolution (points × scale), matching the screen size the old idb
// Describe reported; the encoded video is a scaled-down slice of it (decision
// №40) with the same aspect.
func (e controlEvent) dims() controlDims {
	d := controlDims{widthPoints: e.Width, heightPoints: e.Height}
	if e.Scale > 0 {
		d.pixelW = uint64(math.Round(e.Width * e.Scale))
		d.pixelH = uint64(math.Round(e.Height * e.Scale))
	} else {
		d.pixelW, d.pixelH = e.EncodedWidth, e.EncodedHeight
	}
	return d
}

// attachError maps a ready:false envelope to the typed failure the session
// layer hands the client. The helper's code travels verbatim — it is stable
// protocol surface over there, never renamed or reused, so remapping it here
// could only lose meaning. A missing code (a helper bug, or a future envelope
// shape) degrades to untyped rather than to a wrong code.
func (e controlEvent) attachError(udid string) *server.AttachError {
	msg := e.Message
	if msg == "" {
		msg = "simbeam-control startup failed"
	}
	return &server.AttachError{
		Code:      e.Error,
		Msg:       fmt.Sprintf("%s: %s", udid, msg),
		Retryable: e.Retryable,
	}
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

// Close stops the process: closing stdin asks it to exit, Kill guarantees it.
// The pipe readers are drained to completion (Kill EOFs their pipes) before
// cmd.Wait(), which closes those pipes — the caller must have cancelled the
// feed's ctx first (Feed contract) so the frames goroutine isn't parked on a
// send with no reader.
func (c *control) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	c.closeStdin()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.readers.Wait()
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

// Touch forwards one phase of a client-driven touch stream (down/move/up) at a
// point scaled like Tap's. Trajectory and pacing belong to the client; the
// stuck-finger protections (re-down releases the previous touch, EOF releases a
// hanging one, tap/swipe suppressed while the stream finger is down) live in
// simbeam-control, so this stays a dumb forward.
func (c *control) Touch(action string, nx, ny float64) {
	d := c.getDims()
	c.writeCmd(struct {
		Type   string  `json:"type"`
		Action string  `json:"action"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
	}{"touch", action, clamp01(nx) * d.widthPoints, clamp01(ny) * d.heightPoints})
}

// AppSwitcher opens the app switcher. The double Home press runs as one
// serialized gesture inside simbeam-control, so the press window is guaranteed
// without the client timing two "home"s itself.
func (c *control) AppSwitcher() {
	c.writeCmd(struct {
		Type string `json:"type"`
	}{"app_switcher"})
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

// readFrames converts simbeam-control's framed H.264 stdout —
// [4B len][1B flags][8B pts_micros][N bytes Annex-B] — into encoder.Frames.
// Duration is the measured pts delta (decision №93), not a fixed 1/fps. The
// channel closes when the process exits or ctx is cancelled; the goroutine is
// tracked in c.readers so Close waits for it before cmd.Wait closes the pipe.
func (c *control) readFrames(ctx context.Context, stdout io.Reader) <-chan encoder.Frame {
	frames := make(chan encoder.Frame)
	c.readers.Add(1)
	go func() {
		defer c.readers.Done()
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
			case <-c.closed:
				return
			}
		}
	}()
	return frames
}
