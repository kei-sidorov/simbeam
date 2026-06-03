# Phase 2 — WebRTC (низкая задержка) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Стримить экран симулятора в браузер по WebRTC с низкой задержкой и без артефактов, кодируя H.264 самостоятельно из idb-скриншотов; тачи — по DataChannel. JPEG/WS-путь Phase 1 остаётся fallback.

**Architecture (пересмотрена после спайка, decisions №34–38):** idb `screenshot` (PNG-поллинг, путь Phase 1) → `ffmpeg` (`h264_videotoolbox`, короткий GOP, low-latency флаги) → H.264 Annex-B → парсинг в access unit'ы → pion `TrackLocalStaticSample` → браузер `<video>`. Мы владеем энкодером, поэтому keyframe'ы под контролем — это устраняет проблему idb-GOP (~10с, без управления), из-за которой сырой H.264-passthrough был отвергнут. Pion-логика в `internal/rtc`; H.264/ffmpeg в `internal/encoder`; WS `/rtc` и оркестрация — в `server` (переиспользует разбор команд Phase 1). Спайк (`cmd/spike-rtc`) уже доказал пайплайн: идеальное качество, ~0.7с задержки @15fps localhost.

**Tech Stack:** Go 1.25/1.26, `github.com/pion/webrtc/v4` (уже в go.mod), внешний `ffmpeg` с `h264_videotoolbox`, `github.com/gorilla/websocket`, gRPC (`idb_companion`), ванильный JS (`RTCPeerConnection` + `<video>`).

**Спайк уже пройден.** Код спайка (`cmd/spike-rtc`) — справочник проверенных решений (фиксы NAL-парсинга, ffmpeg-argv, jitterBuffer). Удаляется в Task 10. Боевой код повторяет проверенные куски.

---

## File Structure

**Создаём:**
- `internal/encoder/nal.go` — `Frame`, Annex-B NAL-сплиттер, сборка access unit'ов (фиксы из спайка).
- `internal/encoder/nal_test.go` — юнит-тесты сплиттера и сборщика.
- `internal/encoder/ffmpeg.go` — `Available`, `ffmpegArgs`, `Encode` (спавн ffmpeg, PNG→H.264).
- `internal/encoder/ffmpeg_test.go` — тест argv + `Available` (skip без ffmpeg).
- `internal/rtc/peer.go` — `Session`: pion peer, H.264-трек, `Answer`, `WriteFrame`, `OnClose`, `Close`.
- `internal/rtc/peer_test.go` — тест обмена SDP.
- `internal/server/rtc.go` — WS-хендлер `/rtc` (оркестрация screenshot→encoder→pion + DataChannel→control).
- `internal/server/rtc_test.go` — быстрый тест 400 без udid.

**Меняем:**
- `internal/server/control.go` — выносим диспетчер команд в `applyControl`.
- `internal/server/session.go` — заменяем инлайн-`switch` на `applyControl`.
- `internal/server/server.go` — регистрируем `/rtc` одной строкой.
- `web/debug/index.html` — переключатель RTC/JPG, `<video>`, DataChannel, `jitterBufferTarget=0`.
- `README.md` — RTC-режим + зависимость `ffmpeg`.

**Без изменений (переиспользуем):** `internal/idb` (`Spawn`, `Describe`, `ScreenshotStream`, `hid`), `internal/server` (`parseControl`, `keymap`, `ScaleTap`). `internal/idb.VideoStream` НЕ создаём — от idb-H.264 отказались.

---

## Task 1: Annex-B NAL-сплиттер (`internal/encoder`)

**Files:**
- Create: `internal/encoder/nal.go`
- Test: `internal/encoder/nal_test.go`

Чистые функции нарезки Annex-B. **Фикс из спайка:** пропускать полную длину start-code (3 или 4 байта), иначе 4-байтный код `00 00 00 01` ловится как 3-байтный + мусорный 1-байтный NAL.

- [ ] **Step 1: Написать падающий тест (включая 4-байтный start-code)**

Create `internal/encoder/nal_test.go`:

```go
package encoder

import (
	"bytes"
	"testing"
)

func TestSplitNALUnits4ByteStartCode(t *testing.T) {
	// NAL A с 4-байтным start code, NAL B с 3-байтным, частичный третий.
	buf := []byte{
		0, 0, 0, 1, 0x67, 0xAA, // NAL A (SPS, type 7) — 4-байтный код
		0, 0, 1, 0x68, 0xBB, // NAL B (PPS, type 8) — 3-байтный код
		0, 0, 0, 1, 0x65, // NAL C начало (неполный → rest)
	}
	nals, rest := splitNALUnits(buf)
	if len(nals) != 2 {
		t.Fatalf("want 2 nals, got %d: %v", len(nals), nals)
	}
	if !bytes.Equal(nals[0], []byte{0, 0, 0, 1, 0x67, 0xAA}) {
		t.Fatalf("nal0 = % x", nals[0])
	}
	if !bytes.Equal(nals[1], []byte{0, 0, 1, 0x68, 0xBB}) {
		t.Fatalf("nal1 = % x", nals[1])
	}
	if !bytes.Equal(rest, []byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("rest = % x", rest)
	}
}

func TestNALTypeAndVCL(t *testing.T) {
	if nalType([]byte{0, 0, 0, 1, 0x65}) != 5 {
		t.Fatal("4-byte IDR should be type 5")
	}
	if nalType([]byte{0, 0, 1, 0x67}) != 7 {
		t.Fatal("3-byte SPS should be type 7")
	}
	if !isVCL(5) || isVCL(7) {
		t.Fatal("VCL classification wrong")
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/encoder/ -run 'TestSplitNALUnits4ByteStartCode|TestNALTypeAndVCL' -v`
Expected: FAIL — `undefined: splitNALUnits` (package doesn't exist yet).

- [ ] **Step 3: Реализовать сплиттер**

Create `internal/encoder/nal.go`:

```go
// Package encoder turns a stream of still images (PNG frames from idb's
// screenshot RPC) into an H.264 access-unit stream via an ffmpeg subprocess
// (hardware h264_videotoolbox). We own the encoder, so keyframe cadence is
// under our control — unlike idb's video_stream (fixed ~10s GOP, decisions
// №34-38). Knows nothing about idb, pion, or HTTP.
package encoder

import "time"

// Frame is one H.264 access unit (NAL units in Annex-B, start codes included)
// plus the duration it occupies, used to advance the RTP timestamp.
type Frame struct {
	Data     []byte
	Duration time.Duration
}

// startCodeLen returns 4 for a 00 00 00 01 prefix, 3 for 00 00 01, else 0.
func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}

// indexStartCode returns the offset of the next start code in b, or -1.
func indexStartCode(b []byte) int {
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				return i
			}
			if i+3 < len(b) && b[i+2] == 0 && b[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// splitNALUnits splits an Annex-B buffer into NAL units, each retaining its
// leading start code. The final (possibly incomplete) unit is returned as rest
// so it can be prepended to the next read — one NAL of latency, but unit
// boundaries stay correct across chunk splits. Fix vs spike: skip the FULL
// current start code (3 or 4 bytes) before scanning, else a 4-byte 00 00 00 01
// is mis-detected as a 3-byte code + a bogus 1-byte NAL.
func splitNALUnits(buf []byte) (nals [][]byte, rest []byte) {
	remaining := buf
	for len(remaining) > 0 {
		scLen := startCodeLen(remaining)
		if scLen == 0 {
			return nals, remaining // not at a start code; carry forward
		}
		next := indexStartCode(remaining[scLen:])
		if next < 0 {
			return nals, remaining // last (maybe incomplete) NAL
		}
		end := scLen + next
		nals = append(nals, remaining[:end])
		remaining = remaining[end:]
	}
	return nals, nil
}

// nalType returns the H.264 NAL unit type (low 5 bits of the header byte after
// the start code), or 0 if malformed.
func nalType(nal []byte) byte {
	sc := startCodeLen(nal)
	if sc == 0 || sc >= len(nal) {
		return 0
	}
	return nal[sc] & 0x1f
}

// isVCL reports whether a NAL type carries coded slice data (types 1..5).
func isVCL(t byte) bool { return t >= 1 && t <= 5 }
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/encoder/ -run 'TestSplitNALUnits4ByteStartCode|TestNALTypeAndVCL' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/encoder/nal.go internal/encoder/nal_test.go
git commit -m "feat(encoder): Annex-B NAL splitter (full start-code skip)"
```

---

## Task 2: Сборка access unit'ов (`internal/encoder`)

**Files:**
- Modify: `internal/encoder/nal.go`
- Test: `internal/encoder/nal_test.go`

**Фикс из спайка:** граница access unit — ПЕРЕД ведущими SEI/SPS/PPS (6/7/8) или AUD(9)/VCL, чтобы SPS+PPS+IDR ехали в одном сэмпле (иначе браузер не декодит keyframe).

- [ ] **Step 1: Написать падающий тест группировки keyframe**

Append to `internal/encoder/nal_test.go`:

```go
func TestAUAssemblerKeyframeGrouping(t *testing.T) {
	p := func(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }
	pSlice := p(0x41, 0x01) // type 1 (VCL, non-IDR)
	sps := p(0x67, 0x02)    // type 7
	pps := p(0x68, 0x03)    // type 8
	sei := p(0x06, 0x04)    // type 6
	idr := p(0x65, 0x05)    // type 5 (VCL, IDR)
	next := p(0x41, 0x06)   // type 1 — opens the AU AFTER the keyframe

	var a auAssembler
	// First P-frame: no flush yet.
	if au := a.push(pSlice); au != nil {
		t.Fatalf("first slice should not complete an AU")
	}
	// SPS arrives → it starts a new AU, so the prior P-frame flushes alone.
	au := a.push(sps)
	if !bytes.Equal(au, pSlice) {
		t.Fatalf("expected prior P-frame flushed, got % x", au)
	}
	// PPS, SEI, IDR accumulate into the keyframe AU (no flush).
	for _, n := range [][]byte{pps, sei, idr} {
		if au := a.push(n); au != nil {
			t.Fatalf("keyframe AU should still accumulate, flushed % x", au)
		}
	}
	// Next picture's slice flushes the keyframe AU = SPS+PPS+SEI+IDR together.
	au = a.push(next)
	want := bytes.Join([][]byte{sps, pps, sei, idr}, nil)
	if !bytes.Equal(au, want) {
		t.Fatalf("keyframe AU = % x\nwant % x", au, want)
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/encoder/ -run TestAUAssemblerKeyframeGrouping -v`
Expected: FAIL — `undefined: auAssembler`.

- [ ] **Step 3: Реализовать сборщик**

Append to `internal/encoder/nal.go`:

```go
// startsNewAU reports whether a NAL of this type begins a new access unit: an
// access-unit delimiter (9), a parameter set / SEI that prefixes a picture
// (7/8/6), or a coded slice (1..5). Placing the boundary BEFORE these keeps an
// IDR together with its SPS/PPS in one sample (decision №38).
func startsNewAU(t byte) bool {
	switch {
	case t == 6 || t == 7 || t == 8 || t == 9:
		return true
	case isVCL(t):
		return true
	default:
		return false
	}
}

// auAssembler groups NAL units into access units. It flushes the current unit
// when a NAL that starts a new access unit arrives while the current unit
// already holds a VCL NAL.
type auAssembler struct {
	cur    [][]byte
	hasVCL bool
}

// push adds a NAL and returns a completed access unit if this NAL starts a new
// one, otherwise nil.
func (a *auAssembler) push(nal []byte) []byte {
	var done []byte
	t := nalType(nal)
	if a.hasVCL && startsNewAU(t) {
		done = flatten(a.cur)
		a.cur = nil
		a.hasVCL = false
	}
	a.cur = append(a.cur, nal)
	if isVCL(t) {
		a.hasVCL = true
	}
	return done
}

func flatten(nals [][]byte) []byte {
	var n int
	for _, b := range nals {
		n += len(b)
	}
	out := make([]byte, 0, n)
	for _, b := range nals {
		out = append(out, b...)
	}
	return out
}
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/encoder/ -v`
Expected: PASS (все тесты encoder).

- [ ] **Step 5: Commit**

```bash
git add internal/encoder/nal.go internal/encoder/nal_test.go
git commit -m "feat(encoder): access-unit assembler keeping SPS/PPS with IDR"
```

---

## Task 3: ffmpeg-энкодер (`internal/encoder`)

**Files:**
- Create: `internal/encoder/ffmpeg.go`
- Test: `internal/encoder/ffmpeg_test.go`

Спавн ffmpeg (`h264_videotoolbox`, low-latency argv из спайка), PNG в stdin → H.264 из stdout → NAL split → AU assemble → канал `Frame`.

- [ ] **Step 1: Написать падающий тест argv + Available**

Create `internal/encoder/ffmpeg_test.go`:

```go
package encoder

import (
	"strings"
	"testing"
)

func TestFFmpegArgs(t *testing.T) {
	got := strings.Join(ffmpegArgs(15), " ")
	for _, want := range []string{
		"-analyzeduration 0", "h264_videotoolbox", "-g 30",
		"-framerate 15", "-f h264", "-flush_packets 1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ffmpegArgs missing %q in: %s", want, got)
		}
	}
}

func TestAvailableSkipIfMissing(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("ffmpeg/h264_videotoolbox not available: %v", err)
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/encoder/ -run 'TestFFmpegArgs|TestAvailableSkipIfMissing' -v`
Expected: FAIL — `undefined: ffmpegArgs` / `undefined: Available`.

- [ ] **Step 3: Реализовать энкодер**

Create `internal/encoder/ffmpeg.go`:

```go
package encoder

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const encoderName = "h264_videotoolbox"

// Available reports whether ffmpeg and the h264_videotoolbox encoder are present.
func Available() error {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH (try: brew install ffmpeg): %w", err)
	}
	out, err := exec.Command(path, "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	if !strings.Contains(string(out), encoderName) {
		return fmt.Errorf("ffmpeg has no %s encoder", encoderName)
	}
	return nil
}

// ffmpegArgs builds the low-latency PNG→H.264 argv (see decision №37). idb's
// screenshot RPC returns PNG, hence image2pipe/png input. -analyzeduration 0 is
// critical: it removes the demuxer's startup backlog that otherwise pins ~3s of
// constant latency.
func ffmpegArgs(fps int) []string {
	return []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "nobuffer", "-flags", "low_delay", "-analyzeduration", "0",
		"-f", "image2pipe", "-vcodec", "png", "-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		"-an",
		"-c:v", encoderName, "-realtime", "1", "-profile:v", "baseline",
		"-g", strconv.Itoa(fps * 2), "-b:v", "8M", "-pix_fmt", "yuv420p",
		"-flush_packets", "1", "-max_delay", "0", "-f", "h264", "pipe:1",
	}
}

// Encode spawns ffmpeg, feeds PNG frames from png to its stdin, and emits one
// Frame per H.264 access unit on the returned channel until ctx is cancelled
// (then the channel closes and ffmpeg is killed via the command context).
func Encode(ctx context.Context, png <-chan []byte, fps int) (<-chan Frame, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(fps)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	cmd.Stderr = nil // discard ffmpeg's own logging
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	// Feed PNG frames into ffmpeg stdin.
	go func() {
		defer stdin.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-png:
				if !ok {
					return
				}
				if _, err := stdin.Write(frame); err != nil {
					return
				}
			}
		}
	}()

	frames := make(chan Frame)
	dur := time.Second / time.Duration(fps)
	go func() {
		defer close(frames)
		defer cmd.Wait()
		var buf []byte
		var asm auAssembler
		readBuf := make([]byte, 1<<16)
		for {
			n, rerr := stdout.Read(readBuf)
			if n > 0 {
				buf = append(buf, readBuf[:n]...)
				var nals [][]byte
				nals, buf = splitNALUnits(buf)
				for _, nal := range nals {
					au := asm.push(nal)
					if au == nil {
						continue
					}
					select {
					case frames <- Frame{Data: au, Duration: dur}:
					case <-ctx.Done():
						return
					}
				}
			}
			if rerr != nil {
				if rerr != io.EOF && ctx.Err() == nil {
					// ffmpeg died unexpectedly; closing the channel tears the session down.
				}
				return
			}
		}
	}()
	return frames, nil
}
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go build ./... && go test ./internal/encoder/ -v`
Expected: компиляция ок; `TestFFmpegArgs` PASS; `TestAvailableSkipIfMissing` PASS или SKIP.

- [ ] **Step 5: Commit**

```bash
git add internal/encoder/ffmpeg.go internal/encoder/ffmpeg_test.go
git commit -m "feat(encoder): ffmpeg h264_videotoolbox encoder (PNG->H264 access units)"
```

---

## Task 4: Вынести `applyControl` (переиспользование ввода)

**Files:**
- Modify: `internal/server/control.go`
- Modify: `internal/server/session.go` (reader loop, строки ~92-117)

Чистый рефактор — поведение `/session` не меняется, покрытие даёт существующий тест-сьют.

- [ ] **Step 1: Добавить `applyControl` в control.go**

Append to `internal/server/control.go` and update its imports:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/kei-sidorov/simcast/internal/idb"
)

// applyControl dispatches one parsed control message to the idb client, scaling
// touch coordinates against the simulator screen. Shared by the JPEG WS session
// and the WebRTC DataChannel so input handling lives in exactly one place.
func applyControl(ctx context.Context, client *idb.Client, screen idb.Screen, m controlMsg) {
	switch m.Type {
	case "tap":
		if err := client.Tap(ctx, idb.ScaleTap(m.X, m.Y, screen)); err != nil {
			log.Printf("tap: %v", err)
		}
	case "home":
		if err := client.Home(ctx); err != nil {
			log.Printf("home: %v", err)
		}
	case "swipe":
		dur := m.Duration
		if dur <= 0 {
			dur = 0.3
		}
		start := idb.ScaleTap(m.X1, m.Y1, screen)
		end := idb.ScaleTap(m.X2, m.Y2, screen)
		if err := client.Swipe(ctx, start, end, dur); err != nil {
			log.Printf("swipe: %v", err)
		}
	case "key":
		if usage, shift, ok := keyUsage(m.Key); ok {
			if err := client.KeyPress(ctx, usage, shift); err != nil {
				log.Printf("key: %v", err)
			}
		}
	}
}
```

- [ ] **Step 2: Заменить инлайн-switch в session.go**

In `internal/server/session.go`, the reader loop body becomes:

```go
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			cancel()
			return
		}
		m, err := parseControl(data)
		if err != nil {
			continue // ignore malformed/unknown
		}
		applyControl(ctx, client, screen, m)
	}
```

Remove the now-unused `log` import from `session.go` if `go build` flags it.

- [ ] **Step 3: Сборка и весь тест-сьют**

Run: `go build ./... && go test ./...`
Expected: всё проходит — поведение `/session` не изменилось.

- [ ] **Step 4: Commit**

```bash
git add internal/server/control.go internal/server/session.go
git commit -m "refactor(server): extract applyControl for reuse by WS and DataChannel"
```

---

## Task 5: pion-сессия (`internal/rtc`)

**Files:**
- Create: `internal/rtc/peer.go`
- Test: `internal/rtc/peer_test.go`

Чистая pion-механика: H.264-трек, обмен SDP (non-trickle), DataChannel `control` → callback. Никаких idb/encoder/HTTP.

- [ ] **Step 1: Написать падающий тест обмена SDP**

Create `internal/rtc/peer_test.go`:

```go
package rtc

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func makeOffer(t *testing.T) string {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.CreateDataChannel("control", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-done
	return pc.LocalDescription().SDP
}

func TestSessionAnswer(t *testing.T) {
	sess, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	answerSDP, err := sess.Answer(makeOffer(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answerSDP, "m=video") {
		t.Fatalf("answer missing video media section:\n%s", answerSDP)
	}
}

func TestSessionWriteFrameNoPanic(t *testing.T) {
	sess, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.WriteFrame([]byte{0, 0, 0, 1, 0x65, 0x00}, 66)
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/rtc/ -v`
Expected: FAIL — пакет/`New` не существуют.

- [ ] **Step 3: Реализовать Session**

Create `internal/rtc/peer.go`:

```go
// Package rtc holds the WebRTC mechanics: one peer connection per session
// serving an H.264 video track and receiving control over a DataChannel. It
// speaks raw SDP strings and knows nothing about idb, the encoder, HTTP, or the
// meaning of control messages — the server package wires those in.
package rtc

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Session is one WebRTC peer connection: H.264 video out, control DataChannel in.
type Session struct {
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticSample
	onClose   func()
	closeOnce sync.Once
}

// New creates a peer with one H.264 video track and routes inbound "control"
// DataChannel messages to onControl (nil to ignore).
func New(onControl func([]byte)) (*Session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "simcast")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, err
	}
	s := &Session{pc: pc, track: track}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "control" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if onControl != nil {
				onControl(msg.Data)
			}
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.fireClose()
		}
	})
	return s, nil
}

// Answer consumes a remote offer SDP and returns the local answer SDP, blocking
// until ICE gathering completes (non-trickle; instant on localhost).
func (s *Session) Answer(offerSDP string) (string, error) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	done := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-done
	return s.pc.LocalDescription().SDP, nil
}

// WriteFrame writes one H.264 access unit to the video track.
func (s *Session) WriteFrame(data []byte, dur time.Duration) error {
	return s.track.WriteSample(media.Sample{Data: data, Duration: dur})
}

// OnClose registers a callback fired once when the peer fails/disconnects/closes.
func (s *Session) OnClose(fn func()) { s.onClose = fn }

func (s *Session) fireClose() {
	if s.onClose != nil {
		s.closeOnce.Do(s.onClose)
	}
}

// Close tears down the peer connection.
func (s *Session) Close() error { return s.pc.Close() }
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/rtc/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rtc/peer.go internal/rtc/peer_test.go
git commit -m "feat(rtc): pion Session with H264 track and control DataChannel"
```

---

## Task 6: WS-эндпоинт `/rtc` (оркестрация в `server`)

**Files:**
- Create: `internal/server/rtc.go`
- Modify: `internal/server/server.go` (регистрация роута)
- Test: `internal/server/rtc_test.go`

Хендлер: проверка ffmpeg → spawn сайдкара → `ScreenshotStream` → `encoder.Encode` → pion-сессия; кадры качаются в трек, DataChannel-сообщения идут через `parseControl`+`applyControl`.

- [ ] **Step 1: Написать падающий тест (без WS-апгрейда)**

Create `internal/server/rtc_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleRTCMissingUDID(t *testing.T) {
	s := New(nil, "")
	req := httptest.NewRequest(http.MethodGet, "/rtc", nil) // no ?udid=
	rec := httptest.NewRecorder()
	s.handleRTC(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing udid, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/server/ -run TestHandleRTCMissingUDID -v`
Expected: FAIL — `s.handleRTC` не определён.

- [ ] **Step 3: Реализовать хендлер**

Create `internal/server/rtc.go`:

```go
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/kei-sidorov/simcast/internal/encoder"
	"github.com/kei-sidorov/simcast/internal/idb"
	"github.com/kei-sidorov/simcast/internal/rtc"
)

// rtcFPS is the screenshot/encode frame rate for the WebRTC path.
const rtcFPS = 15

// sdpMsg is the signaling envelope exchanged over the /rtc WebSocket.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// handleRTC negotiates one WebRTC session: verify ffmpeg, spawn the sidecar,
// capture screenshots, encode them to H.264 (ffmpeg/h264_videotoolbox), pump
// access units into the track, and route DataChannel control through the shared
// parse/apply path. The JPEG /session path is untouched (fallback).
func (s *Server) handleRTC(w http.ResponseWriter, r *http.Request) {
	udid := r.URL.Query().Get("udid")
	if udid == "" {
		http.Error(w, "missing udid", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	if err := encoder.Available(); err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sidecar, err := idb.Spawn(ctx, s.binary, udid)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sidecar.Close()
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	sess, err := rtc.New(func(data []byte) {
		m, perr := parseControl(data)
		if perr != nil {
			return
		}
		applyControl(ctx, client, screen, m)
	})
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	defer sess.Close()
	sess.OnClose(cancel)

	var offer sdpMsg
	if err := conn.ReadJSON(&offer); err != nil || offer.Type != "offer" {
		return
	}
	answerSDP, err := sess.Answer(offer.SDP)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}

	png := client.ScreenshotStream(ctx, time.Second/rtcFPS)
	frames, err := encoder.Encode(ctx, png, rtcFPS)
	if err != nil {
		_ = conn.WriteJSON(sdpMsg{Type: "error", Msg: err.Error()})
		return
	}
	go func() {
		for f := range frames {
			if err := sess.WriteFrame(f.Data, f.Duration); err != nil {
				cancel()
				return
			}
		}
		cancel() // encoder/stream ended → tear down
	}()

	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects or teardown cancels ctx.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
```

- [ ] **Step 4: Зарегистрировать роут**

In `internal/server/server.go` inside `Handler()`, after the `/session` line:

```go
	mux.HandleFunc("/session", s.handleSession)
	mux.HandleFunc("/rtc", s.handleRTC)
```

- [ ] **Step 5: Сборка и тесты**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: компиляция ок; `TestHandleRTCMissingUDID` PASS вместе с остальными.

- [ ] **Step 6: Commit**

```bash
git add internal/server/rtc.go internal/server/rtc_test.go internal/server/server.go
git commit -m "feat(server): /rtc endpoint streaming screenshot->ffmpeg->pion"
```

---

## Task 7: Браузерный клиент — переключатель RTC / JPG

**Files:**
- Modify: `web/debug/index.html`

`<video>` + две кнопки RTC/JPG (дефолт RTC) + DataChannel + `jitterBufferTarget=0`. JPG-режим — текущий код Phase 1. Ввод через общий `sendControl`. Образец JS — в `cmd/spike-rtc` (страница спайка) и в коде ниже.

- [ ] **Step 1: Заменить index.html целиком**

Overwrite `web/debug/index.html` with:

```html
<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>simcast debug</title>
  <style>
    body { font-family: -apple-system, sans-serif; margin: 16px; }
    #sims button { display: block; margin: 4px 0; }
    #modes { margin: 8px 0; }
    #modes button.active { font-weight: bold; text-decoration: underline; }
    .surface { border: 1px solid #ccc; max-height: 80vh; cursor: crosshair; display: none; touch-action: none; }
    #home { margin-top: 8px; display: none; }
    #hint { margin-top: 6px; font-size: 12px; color: #666; display: none; }
  </style>
</head>
<body>
  <div id="modes">
    Mode:
    <button id="modeRtc" class="active">RTC</button>
    <button id="modeJpg">JPG</button>
  </div>
  <h3>Simulators</h3>
  <div id="sims">loading…</div>
  <img id="screenImg" class="surface" alt="simulator">
  <video id="screenVid" class="surface" autoplay playsinline muted></video>
  <div id="hint">Click&nbsp;= tap · drag&nbsp;= swipe · keys typed here go to the device</div>
  <div><button id="home">Home</button></div>

<script>
const simsEl = document.getElementById('sims');
const imgEl = document.getElementById('screenImg');
const vidEl = document.getElementById('screenVid');
const homeBtn = document.getElementById('home');
const hintEl = document.getElementById('hint');
const modeRtcBtn = document.getElementById('modeRtc');
const modeJpgBtn = document.getElementById('modeJpg');

let mode = 'rtc';
let currentUDID = null;
let ws = null;           // jpg path
let pc = null, dc = null; // rtc path

function surface() { return mode === 'rtc' ? vidEl : imgEl; }

function teardown() {
  if (ws) { ws.close(); ws = null; }
  if (dc) { try { dc.close(); } catch (e) {} dc = null; }
  if (pc) { try { pc.close(); } catch (e) {} pc = null; }
  imgEl.style.display = 'none';
  vidEl.style.display = 'none';
  vidEl.srcObject = null;
}

function showSurface() {
  surface().style.display = 'block';
  homeBtn.style.display = 'inline-block';
  hintEl.style.display = 'block';
}

modeRtcBtn.onclick = () => setMode('rtc');
modeJpgBtn.onclick = () => setMode('jpg');

function setMode(m) {
  if (m === mode) return;
  mode = m;
  modeRtcBtn.classList.toggle('active', m === 'rtc');
  modeJpgBtn.classList.toggle('active', m === 'jpg');
  teardown();
  if (currentUDID) start(currentUDID);
}

async function loadSims() {
  const r = await fetch('/api/simulators');
  const sims = await r.json();
  simsEl.innerHTML = '';
  sims.forEach(s => {
    const b = document.createElement('button');
    b.textContent = `${s.state === 'Booted' ? '▶' : '○'} ${s.name} (${s.os_version})`;
    b.onclick = () => onPick(s);
    simsEl.appendChild(b);
  });
}

async function onPick(s) {
  if (s.state !== 'Booted') {
    const r = await fetch('/api/boot', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({udid: s.udid}),
    });
    if (!r.ok) { alert('boot failed: ' + (await r.text())); return; }
  }
  currentUDID = s.udid;
  teardown();
  start(s.udid);
}

function start(udid) {
  if (mode === 'rtc') startRTC(udid); else startJPG(udid);
}

// ---- JPG path (Phase 1, unchanged) ----
function startJPG(udid) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/session?udid=${encodeURIComponent(udid)}`);
  ws.binaryType = 'blob';
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') { console.log('status', ev.data); return; }
    const url = URL.createObjectURL(ev.data);
    const old = imgEl.src;
    imgEl.src = url;
    showSurface();
    if (old) URL.revokeObjectURL(old);
  };
  ws.onclose = () => { console.log('ws closed'); };
}

// ---- RTC path (Phase 2) ----
async function startRTC(udid) {
  pc = new RTCPeerConnection();
  pc.addTransceiver('video', {direction: 'recvonly'});
  dc = pc.createDataChannel('control', {ordered: false, maxRetransmits: 0});
  pc.ontrack = (ev) => {
    vidEl.srcObject = ev.streams[0];
    minimizeBuffer(pc);
    showSurface();
  };
  pc.onconnectionstatechange = () => {
    if (['failed', 'disconnected', 'closed'].includes(pc.connectionState)) {
      console.log('rtc state', pc.connectionState);
    }
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await iceGatheringComplete(pc);

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const sig = new WebSocket(`${proto}://${location.host}/rtc?udid=${encodeURIComponent(udid)}`);
  sig.onopen = () => sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'answer') {
      await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
      minimizeBuffer(pc);
    } else if (m.type === 'error') {
      alert('rtc error: ' + m.msg);
    }
  };
}

// Minimize the receiver playout buffer for low latency (Chrome; try/catch for Safari).
function minimizeBuffer(pc) {
  pc.getReceivers().forEach(r => {
    try { r.jitterBufferTarget = 0; } catch (e) {}
    try { r.playoutDelayHint = 0; } catch (e) {}
  });
}

function iceGatheringComplete(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise(resolve => {
    const check = () => {
      if (pc.iceGatheringState === 'complete') {
        pc.removeEventListener('icegatheringstatechange', check);
        resolve();
      }
    };
    pc.addEventListener('icegatheringstatechange', check);
  });
}

// ---- Shared input ----
function sendControl(obj) {
  const s = JSON.stringify(obj);
  if (mode === 'rtc') {
    if (dc && dc.readyState === 'open') dc.send(s);
  } else {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(s);
  }
}

function controlReady() {
  return mode === 'rtc'
    ? (dc && dc.readyState === 'open')
    : (ws && ws.readyState === WebSocket.OPEN);
}

function normCoords(e) {
  const rect = surface().getBoundingClientRect();
  const x = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
  const y = Math.min(1, Math.max(0, (e.clientY - rect.top) / rect.height));
  return {x, y};
}

const SWIPE_THRESHOLD_PX = 6;
let drag = null;

for (const el of [imgEl, vidEl]) {
  el.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    const c = normCoords(e);
    drag = {x: c.x, y: c.y, clientX: e.clientX, clientY: e.clientY, t: Date.now()};
    el.setPointerCapture(e.pointerId);
  });
  el.addEventListener('pointerup', (e) => {
    if (e.button !== 0 || !drag) return;
    if (!controlReady()) { drag = null; return; }
    const dx = e.clientX - drag.clientX;
    const dy = e.clientY - drag.clientY;
    if (Math.sqrt(dx * dx + dy * dy) < SWIPE_THRESHOLD_PX) {
      sendControl({type: 'tap', x: drag.x, y: drag.y});
    } else {
      const end = normCoords(e);
      const duration = Math.max(0.05, (Date.now() - drag.t) / 1000);
      sendControl({type: 'swipe', x1: drag.x, y1: drag.y, x2: end.x, y2: end.y, duration});
    }
    drag = null;
  });
  el.addEventListener('pointercancel', () => { drag = null; });
}

const MODIFIER_KEYS = new Set(['Shift', 'Control', 'Alt', 'Meta']);
window.addEventListener('keydown', (e) => {
  if (!controlReady()) return;
  if (MODIFIER_KEYS.has(e.key)) return;
  if (e.ctrlKey || e.metaKey) return;
  e.preventDefault();
  sendControl({type: 'key', key: e.key});
});

homeBtn.onclick = () => sendControl({type: 'home'});

loadSims();
</script>
</body>
</html>
```

- [ ] **Step 2: Сборка**

Run: `go build ./...`
Expected: ок (статика встроена через FileServer; шаг проверяет, что Go не сломан).

- [ ] **Step 3: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(web): RTC/JPG toggle with low-latency WebRTC video + DataChannel"
```

---

## Task 8: Ручной E2E (Definition of Done)

**Files:** нет (проверка вживую). Требуется `brew install ffmpeg` и booted-симулятор.

- [ ] **Step 1: Запустить демон**

Run: `go run ./cmd/simcastd serve --web ./web/debug`
Open: `http://localhost:8080/` (Chrome для `jitterBufferTarget`).

- [ ] **Step 2: Проверить RTC-режим (дефолт)**

- RTC активен по умолчанию. Выбрать симулятор (забутится, если нужно).
- **Ожидание:** живое H.264-видео, суб-секундная задержка, **без артефактов** на сменах сцены (свернуть приложение → домой — картинка чистая в пределах ~2с GOP).
- Клик=тап, drag=свайп, Home=Home, физическая клавиатура — всё через DataChannel.

- [ ] **Step 3: Проверить JPG-режим (fallback)**

- Нажать JPG → скриншот-поллинг (Phase 1), ввод по WS. Вернуться на RTC → снова видео.

- [ ] **Step 4: Проверить teardown**

- Закрыть вкладку → нет висящих процессов: `pgrep -fl idb_companion` и `pgrep -fl ffmpeg` пусты.

> **DoD достигнут**, если: RTC даёт низколатентный (суб-секунда) H.264 по WebRTC без артефактов + тачи по DataChannel, JPG-режим работает как раньше.

---

## Task 9: Тюнинг латентности (best-effort)

**Files:** `internal/server/rtc.go` (fps), `internal/encoder/ffmpeg.go` (флаги/scale).

Спайк дал ~0.7с @15fps. Цель — уронить ближе к ~300мс. Пробовать по одному и мерить вживую:

- [ ] **Step 1: Поднять fps** — `rtcFPS = 30` в `rtc.go` (если screenshot RPC тянет; в спайке `screenshot fps` держался 15 — проверить 30). Меньше интервал кадра и придержки AU.
- [ ] **Step 2: Уменьшить размер кадра/IDR** — добавить `-vf scale=iw/2:ih/2` в `ffmpegArgs` (видео всё равно масштабируется в `<video>`). Меньше энкод/битрейт/размер keyframe → меньше задержка. Оценить читаемость текста.
- [ ] **Step 3: При желании** — `-g` короче (чаще keyframe) ценой битрейта; проверить `-realtime`/`-prio_speed` videotoolbox-флаги.
- [ ] **Step 4:** зафиксировать достигнутую задержку строкой в `docs/decisions.md` (дополнить №37). Закоммитить выбранные значения.

> Это best-effort: суб-секунда уже приемлема. Не застревать — задокументировать достигнутое и идти дальше.

---

## Task 10: Уборка спайка и документация

**Files:**
- Delete: `cmd/spike-rtc/`
- Modify: `README.md`

- [ ] **Step 1: Удалить спайк** — `git rm -r cmd/spike-rtc`
- [ ] **Step 2: README** — описать RTC/JPG: RTC дефолт, низколатентный H.264 по WebRTC (кадры = idb screenshot → ffmpeg `h264_videotoolbox` → pion; тачи по DataChannel); JPG — fallback (Phase 1). Добавить в предпосылки **`brew install ffmpeg`**, в таблицу эндпоинтов — `WS /rtc?udid=X` (сигналинг offer→answer, non-trickle; медиа+control по P2P).
- [ ] **Step 3: Финальная проверка** — `go build ./... && go test ./... && go vet ./...` — всё зелёное.
- [ ] **Step 4: Commit**
```bash
git add -A
git commit -m "chore(phase2): remove rtc spike, document WebRTC mode + ffmpeg dep"
```

---

## Self-Review Notes

- **Spec coverage:** видеопайплайн screenshot→ffmpeg→pion (Task 1-3,6); фиксы NAL/AU из спайка (Task 1-2); ffmpeg-зависимость + Available (Task 3,6,10); applyControl reuse (Task 4); pion Session (Task 5); /rtc + оркестрация + сосуществование с JPG (Task 6,7); браузер RTC/JPG + jitterBuffer (Task 7); DataChannel-тачи (Task 6,7); low-latency флаги (Task 3) + тюнинг (Task 9); DoD E2E (Task 8); уборка+README (Task 10) — покрыто.
- **Границы:** `internal/encoder` = H.264/ffmpeg (не знает про idb/pion/HTTP); `internal/rtc` = pion (raw SDP, onControl); `server` = роутинг+оркестрация+reuse applyControl. Нет циклов импорта.
- **Type consistency:** `encoder.Frame{Data, Duration}`, `encoder.Encode(ctx, <-chan []byte, int) (<-chan Frame, error)`, `encoder.Available() error`, `encoder.ffmpegArgs(int) []string`, `rtc.New(func([]byte))`, `rtc.Session.Answer(string)→(string,error)`, `rtc.Session.WriteFrame([]byte, time.Duration)`, `applyControl(ctx, *idb.Client, idb.Screen, controlMsg)`, `sdpMsg{Type,SDP,Msg}`, `rtcFPS=15` — согласованы.
- **Reuse:** `idb.ScreenshotStream(ctx, interval) <-chan []byte` (Phase 1) — источник кадров и для JPG (`/session`), и для RTC (через encoder). `idb.VideoStream` НЕ добавляется.
- **Спайк-наследие:** проверенные куски (splitNALUnits full-skip, startsNewAU, ffmpegArgs, jitterBuffer) перенесены в боевой код дословно по смыслу.
