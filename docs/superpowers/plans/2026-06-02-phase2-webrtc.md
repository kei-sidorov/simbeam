# Phase 2 — WebRTC (низкая задержка) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Стримить H.264 с симулятора в браузер по WebRTC с низкой задержкой и принимать тачи по DataChannel, не переписывая существующий JPEG/WS-путь.

**Architecture:** H.264 из `idb_companion` `video_stream` нарезается на access unit'ы в `internal/idb` и без перекодирования пишется в pion `TrackLocalStaticSample`. Вся pion-механика изолирована в новом пакете `internal/rtc` (говорит сырым SDP, ничего не знает про idb/HTTP/команды). WS-эндпоинт `/rtc` живёт в пакете `server`, который оркеструет сайдкар + видеопоток + pion-сессию и переиспользует существующий разбор команд (`parseControl` + новый `applyControl`) для входящих сообщений DataChannel. JPEG-путь (`/session`) не трогаем — он остаётся fallback.

**Tech Stack:** Go 1.25, `github.com/pion/webrtc/v4`, `github.com/gorilla/websocket`, gRPC (`idb_companion`), ванильный JS (`RTCPeerConnection` + `<video>`).

**Спайк первым (Task 0):** до любой продакшн-обвязки доказываем, что H.264 от idb непрерывно течёт и рисуется в браузере. Пока спайк не пройдёт визуально — Task 1+ не начинаем.

---

## File Structure

**Создаём:**
- `cmd/spike-rtc/main.go` — одноразовый спайк (Task 0). H.264 → pion → `<video>`, дубовый HTTP-сигналинг. Удаляется в Task 8.
- `internal/idb/video.go` — `Frame`, `VideoStream`, Annex-B NAL-сплиттер, сборка access unit'ов.
- `internal/idb/video_test.go` — юнит-тесты сплиттера и сборщика.
- `internal/rtc/peer.go` — `Session`: pion peer, H.264-трек, `Answer`, `WriteFrame`, `OnClose`, `Close`.
- `internal/rtc/peer_test.go` — тест обмена SDP.
- `internal/server/rtc.go` — WS-хендлер `/rtc` (оркестрация сайдкар+видео+pion+control).

**Меняем:**
- `internal/server/control.go` — выносим диспетчер команд в `applyControl`.
- `internal/server/session.go` — заменяем инлайновый `switch` на `applyControl`.
- `internal/server/server.go` — регистрируем `/rtc` одной строкой.
- `web/debug/index.html` — переключатель RTC/JPG, `<video>`, DataChannel.
- `go.mod` / `go.sum` — добавляем `github.com/pion/webrtc/v4`.
- `README.md` — описываем RTC-режим.
- `docs/decisions.md` — фиксируем находки спайка (fps/SPS-PPS/PTS/профиль), если отличаются от дизайна.

---

## Task 0: Спайк — снять риск H.264 → pion → браузер

**Files:**
- Create: `cmd/spike-rtc/main.go`
- Modify: `go.mod`, `go.sum`

Это **одноразовый exploratory-код**, не TDD. Цель — визуально доказать пайплайн и зафиксировать реальные параметры H.264 (fps, границы AU, SPS/PPS, профиль). Допустимо хардкодить и срезать углы; чистота не важна.

- [ ] **Step 1: Добавить зависимость pion**

Run:
```bash
go get github.com/pion/webrtc/v4@latest
```
Expected: `go.mod`/`go.sum` обновлены, пакет скачан.

- [ ] **Step 2: Написать спайк**

Create `cmd/spike-rtc/main.go`. Логика (можно адаптировать по ходу — это спайк):
1. Флаги: `--udid` (обязательный), `--bin` (default `idb_companion`), `--addr` (default `:8090`).
2. `idb.Spawn(ctx, bin, udid)` → получить `*idb.Sidecar`, `client := sc.Client()`.
3. Открыть `video_stream` напрямую через `client` (на этом этапе можно временно добавить в `internal/idb` экспортируемый сырой доступ или продублировать gRPC-вызов прямо в спайке — НЕ дорабатывать продакшн `VideoStream`, его пишем в Task 1/2):

```go
stream, _ := /* idbpb.CompanionServiceClient */ rpc.VideoStream(ctx)
stream.Send(&idbpb.VideoStreamRequest{
    Control: &idbpb.VideoStreamRequest_Start_{Start: &idbpb.VideoStreamRequest_Start{
        Fps: 30, Format: idbpb.VideoStreamRequest_H264, ScaleFactor: 1.0,
    }},
})
for {
    resp, err := stream.Recv()
    if err != nil { log.Fatal(err) }
    data := resp.GetPayload().GetData()
    log.Printf("chunk %d bytes", len(data)) // ПОДТВЕРДИТЬ: чанки идут непрерывно, а не «один и замер»
    // склеить, нарезать по Annex-B start code (0x000001 / 0x00000001),
    // сгруппировать в access unit и track.WriteSample(media.Sample{Data: au, Duration: time.Second/30})
}
```

4. Pion: `webrtc.NewPeerConnection(webrtc.Configuration{})`, `NewTrackLocalStaticSample({MimeType: webrtc.MimeTypeH264}, "video", "spike")`, `AddTrack`.
5. Дубовый сигналинг через HTTP (НЕ WS, это спайк): `POST /offer` принимает SDP offer строкой, делает `SetRemoteDescription`/`CreateAnswer`/`SetLocalDescription`, ждёт `webrtc.GatheringCompletePromise(pc)` и возвращает answer SDP. `GET /` отдаёт инлайн-HTML с `<video>` + JS, который создаёт offer (`addTransceiver video recvonly`), POST'ит на `/offer`, ставит answer.
6. `log.Printf` границы NAL/типы (NAL type = первый байт после start code `& 0x1f`) — собрать данные для Task 2.

- [ ] **Step 3: Запустить спайк и проверить визуально**

Run (нужен booted симулятор; UDID взять из `go run ./cmd/simcastd list`):
```bash
go run ./cmd/spike-rtc --udid <BOOTED_UDID>
```
Открыть `http://localhost:8090/`. **Критерий прохождения:** в `<video>` видно **непрерывное движущееся** видео симулятора (поменяй экран в симуляторе — картинка меняется в реальном времени, не застывает на одном кадре). Латентность НЕ замеряем.

- [ ] **Step 4: Зафиксировать находки**

Записать в `docs/decisions.md` (новой строкой), что реально показал спайк: непрерывен ли H.264; идёт ли один AU на чанк или нужно склеивать; нужно ли руками отделять SPS/PPS; какой profile-level-id принял браузер; есть ли осмысленные PTS. Эти факты уточняют код Task 1/2.

- [ ] **Step 5: Commit**

```bash
git add cmd/spike-rtc/main.go go.mod go.sum docs/decisions.md
git commit -m "spike(phase2): prove idb H264 -> pion -> browser video (throwaway)"
```

> **GATE:** если спайк визуально не прошёл (видео прерывисто/один кадр) — СТОП. Не продолжать Task 1+. Зафиксировать проблему и вернуться к дизайну (транскодинг / video-to-file).

---

## Task 1: Annex-B NAL-сплиттер (`internal/idb`)

**Files:**
- Create: `internal/idb/video.go`
- Test: `internal/idb/video_test.go`

Чистая функция, режущая поток Annex-B на NAL-units. Сборку в кадры добавим в Task 2.

- [ ] **Step 1: Написать падающий тест**

Create `internal/idb/video_test.go`:

```go
package idb

import (
	"bytes"
	"testing"
)

func TestSplitNALUnits(t *testing.T) {
	// NAL A с 4-байтным start code, NAL B с 3-байтным, и частичный третий NAL.
	buf := []byte{
		0, 0, 0, 1, 0x67, 0xAA, // NAL A (SPS, type 7)
		0, 0, 1, 0x68, 0xBB, // NAL B (PPS, type 8)
		0, 0, 0, 1, 0x65, // NAL C начало (неполный — уходит в rest)
	}
	nals, rest := splitNALUnits(buf)
	if len(nals) != 2 {
		t.Fatalf("want 2 nals, got %d", len(nals))
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

func TestSplitNALUnitsNoStartCode(t *testing.T) {
	buf := []byte{0x01, 0x02, 0x03}
	nals, rest := splitNALUnits(buf)
	if len(nals) != 0 {
		t.Fatalf("want 0 nals, got %d", len(nals))
	}
	if !bytes.Equal(rest, buf) {
		t.Fatalf("rest should be whole buf, got % x", rest)
	}
}
```

- [ ] **Step 2: Запустить тест — убедиться, что падает**

Run: `go test ./internal/idb/ -run TestSplitNALUnits -v`
Expected: FAIL — `undefined: splitNALUnits`.

- [ ] **Step 3: Реализовать сплиттер**

Create `internal/idb/video.go`:

```go
package idb

import "time"

// Frame is one H.264 access unit (NAL units in Annex-B framing) plus the
// duration it should occupy, used to advance the RTP timestamp in WriteSample.
type Frame struct {
	Data     []byte
	Duration time.Duration
}

// startCodeLen returns the length (3 or 4) of an Annex-B start code at the head
// of b, or 0 if b does not begin with one.
func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}

// splitNALUnits splits an Annex-B buffer into NAL units, each including its
// leading start code. The final (possibly incomplete) unit is returned as rest
// so it can be prepended to the next read — this costs one NAL of latency but
// keeps unit boundaries correct across chunk splits.
func splitNALUnits(buf []byte) (nals [][]byte, rest []byte) {
	i := 0
	for i < len(buf) && startCodeLen(buf[i:]) == 0 {
		i++ // skip leading junk before the first start code
	}
	if i >= len(buf) {
		return nil, buf
	}
	start := i
	j := i + startCodeLen(buf[i:])
	for j < len(buf) {
		if sc := startCodeLen(buf[j:]); sc != 0 {
			nals = append(nals, buf[start:j])
			start = j
			j += sc
			continue
		}
		j++
	}
	return nals, buf[start:]
}
```

- [ ] **Step 4: Запустить тест — убедиться, что проходит**

Run: `go test ./internal/idb/ -run TestSplitNALUnits -v`
Expected: PASS (оба теста).

- [ ] **Step 5: Commit**

```bash
git add internal/idb/video.go internal/idb/video_test.go
git commit -m "feat(idb): Annex-B NAL splitter for H264 video stream"
```

---

## Task 2: Сборка access unit'ов + `VideoStream` (`internal/idb`)

**Files:**
- Modify: `internal/idb/video.go`
- Test: `internal/idb/video_test.go`

Группируем NAL'ы в кадры и подключаем gRPC `video_stream`.

- [ ] **Step 1: Написать падающий тест сборщика**

Append to `internal/idb/video_test.go`:

```go
func TestAUAssembler(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x01} // type 7
	pps := []byte{0, 0, 0, 1, 0x68, 0x02} // type 8
	idr := []byte{0, 0, 0, 1, 0x65, 0x03} // type 5 (VCL)
	pic := []byte{0, 0, 0, 1, 0x41, 0x04} // type 1 (VCL)

	var a auAssembler
	if au := a.push(sps); au != nil {
		t.Fatalf("sps should not complete an AU, got % x", au)
	}
	if au := a.push(pps); au != nil {
		t.Fatalf("pps should not complete an AU")
	}
	if au := a.push(idr); au != nil {
		t.Fatalf("first VCL should not complete an AU yet")
	}
	// Следующий VCL открывает новый AU -> предыдущий (sps+pps+idr) выдаётся.
	au := a.push(pic)
	want := append(append(append([]byte{}, sps...), pps...), idr...)
	if !bytes.Equal(au, want) {
		t.Fatalf("completed AU = % x, want % x", au, want)
	}
}

func TestIsVCL(t *testing.T) {
	if !isVCL([]byte{0, 0, 0, 1, 0x65}) { // type 5
		t.Fatal("type 5 must be VCL")
	}
	if isVCL([]byte{0, 0, 0, 1, 0x67}) { // type 7
		t.Fatal("type 7 (SPS) must not be VCL")
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/idb/ -run 'TestAUAssembler|TestIsVCL' -v`
Expected: FAIL — `undefined: auAssembler` / `undefined: isVCL`.

- [ ] **Step 3: Реализовать сборщик**

Append to `internal/idb/video.go`:

```go
// nalType returns the H.264 NAL unit type (low 5 bits of the header byte that
// follows the start code), or 0 if the slice is malformed.
func nalType(nal []byte) byte {
	sc := startCodeLen(nal)
	if sc == 0 || sc >= len(nal) {
		return 0
	}
	return nal[sc] & 0x1f
}

// isVCL reports whether the NAL carries coded slice data (types 1..5), i.e. it
// is part of a picture rather than a parameter set / delimiter.
func isVCL(nal []byte) bool {
	t := nalType(nal)
	return t >= 1 && t <= 5
}

// auAssembler groups NAL units into access units (frames). A new access unit is
// flushed when a VCL NAL arrives while the current unit already holds one —
// non-VCL NALs (SPS/PPS/SEI/AUD) attach to the picture that follows them. This
// heuristic avoids parsing slice headers; the spike confirmed it matches idb's
// output.
type auAssembler struct {
	cur       [][]byte
	curHasVCL bool
}

// push adds a NAL and returns a completed access unit if this NAL starts a new
// one, otherwise nil.
func (a *auAssembler) push(nal []byte) []byte {
	var done []byte
	if a.curHasVCL && isVCL(nal) {
		done = flatten(a.cur)
		a.cur = nil
		a.curHasVCL = false
	}
	a.cur = append(a.cur, nal)
	if isVCL(nal) {
		a.curHasVCL = true
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

Run: `go test ./internal/idb/ -run 'TestAUAssembler|TestIsVCL' -v`
Expected: PASS.

- [ ] **Step 5: Добавить `VideoStream` (gRPC-обвязка)**

Append to `internal/idb/video.go` (добавь импорты `context`, `fmt`, `log`; `time` уже есть):

```go
// VideoStream opens the companion video_stream RPC in H.264 and emits one Frame
// per access unit until ctx is cancelled (then the channel closes). It mirrors
// ScreenshotStream: transient recv errors after cancellation are silent; an
// unexpected stream error ends the channel and tears the session down.
func (c *Client) VideoStream(ctx context.Context, fps uint64) (<-chan Frame, error) {
	stream, err := c.rpc.VideoStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("video_stream open: %w", err)
	}
	if err := stream.Send(&idbpb.VideoStreamRequest{
		Control: &idbpb.VideoStreamRequest_Start_{Start: &idbpb.VideoStreamRequest_Start{
			Fps:         fps,
			Format:      idbpb.VideoStreamRequest_H264,
			ScaleFactor: 1.0,
		}},
	}); err != nil {
		return nil, fmt.Errorf("video_stream start: %w", err)
	}

	frames := make(chan Frame)
	dur := time.Second / time.Duration(fps)
	go func() {
		defer close(frames)
		var buf []byte
		var asm auAssembler
		for {
			resp, err := stream.Recv()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("video_stream recv: %v", err)
				}
				return
			}
			buf = append(buf, resp.GetPayload().GetData()...)
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
	}()
	return frames, nil
}
```

Add to the import block of `internal/idb/video.go`:
```go
import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/kei-sidorov/simcast/internal/idbpb"
)
```

- [ ] **Step 6: Сборка и весь тест-сьют**

Run: `go build ./... && go test ./internal/idb/ -v`
Expected: компиляция ок; все тесты idb проходят.

- [ ] **Step 7: Commit**

```bash
git add internal/idb/video.go internal/idb/video_test.go
git commit -m "feat(idb): VideoStream emits H264 access units from video_stream"
```

---

## Task 3: Вынести `applyControl` (переиспользование ввода)

**Files:**
- Modify: `internal/server/control.go`
- Modify: `internal/server/session.go:92-117`

Диспетчер команд сейчас инлайном в `handleSession`. Выносим в `applyControl`, чтобы и `/session`, и `/rtc` (Task 5) звали один код. Чистый рефактор — поведение не меняется, покрытие даёт существующий тест-сьют.

- [ ] **Step 1: Добавить `applyControl` в control.go**

Append to `internal/server/control.go` (добавь импорты `context`, `log`, и `github.com/kei-sidorov/simcast/internal/idb`):

```go
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

Update the import block of `internal/server/control.go`:
```go
import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/kei-sidorov/simcast/internal/idb"
)
```

- [ ] **Step 2: Заменить инлайн-switch в session.go**

In `internal/server/session.go`, replace the reader loop body (the `switch m.Type { ... }` block, lines ~92-117) so the loop becomes:

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

Remove the now-unused `log` import from `session.go` if `go build` flags it (the dispatch's `log.Printf` calls moved to control.go).

- [ ] **Step 3: Сборка и весь тест-сьют**

Run: `go build ./... && go test ./...`
Expected: компиляция ок; все существующие тесты (`parseControl`, `keymap`, `coords`, `server`, `framebuf`) проходят — поведение `/session` не изменилось.

- [ ] **Step 4: Commit**

```bash
git add internal/server/control.go internal/server/session.go
git commit -m "refactor(server): extract applyControl for reuse by WS and DataChannel"
```

---

## Task 4: pion-сессия (`internal/rtc`)

**Files:**
- Create: `internal/rtc/peer.go`
- Test: `internal/rtc/peer_test.go`

Чистая pion-механика: один H.264-трек, обмен SDP (non-trickle), DataChannel `control` → callback. Никаких idb/HTTP/парсинга команд.

- [ ] **Step 1: Написать падающий тест обмена SDP**

Create `internal/rtc/peer_test.go`:

```go
package rtc

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// makeOffer builds a realistic recvonly-video + control-datachannel offer SDP,
// gathering ICE so the SDP is complete (non-trickle), like the browser will.
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
	// WriteFrame before negotiation must not panic; it may error (no readers),
	// which is fine — we only assert it returns.
	_ = sess.WriteFrame([]byte{0, 0, 0, 1, 0x65, 0x00}, 33)
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/rtc/ -v`
Expected: FAIL — пакет/`New` не существуют.

- [ ] **Step 3: Реализовать Session**

Create `internal/rtc/peer.go`:

```go
// Package rtc holds the WebRTC mechanics for simcast: one peer connection per
// session serving an H.264 video track and receiving control messages over a
// DataChannel. It speaks raw SDP strings and knows nothing about idb, HTTP, or
// the meaning of control messages — the server package wires those in.
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
// DataChannel messages to onControl (nil to ignore). The default MediaEngine
// registers a browser-compatible H.264 codec.
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

// Answer consumes a remote offer SDP and returns the local answer SDP. It blocks
// until ICE gathering completes so all candidates are embedded (non-trickle),
// which is instant on localhost.
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

// OnClose registers a callback fired once when the peer fails, disconnects, or
// closes — used by the server to tear the session down.
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
Expected: PASS (оба теста).

- [ ] **Step 5: Commit**

```bash
git add internal/rtc/peer.go internal/rtc/peer_test.go
git commit -m "feat(rtc): pion Session with H264 track and control DataChannel"
```

---

## Task 5: WS-эндпоинт `/rtc` (оркестрация в `server`)

**Files:**
- Create: `internal/server/rtc.go`
- Modify: `internal/server/server.go:35-44`
- Test: `internal/server/rtc_test.go`

Хендлер спавнит сайдкар, поднимает pion-сессию, качает кадры в трек, а входящие DataChannel-сообщения гонит через `parseControl`+`applyControl`.

- [ ] **Step 1: Написать падающий тест (быстрый, без WS-апгрейда)**

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

	"github.com/kei-sidorov/simcast/internal/idb"
	"github.com/kei-sidorov/simcast/internal/rtc"
)

// rtcFPS is the H.264 frame rate requested from idb for the WebRTC path.
const rtcFPS uint64 = 30

// sdpMsg is the signaling envelope exchanged over the /rtc WebSocket.
type sdpMsg struct {
	Type string `json:"type"` // "offer" | "answer" | "error"
	SDP  string `json:"sdp,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// handleRTC negotiates one WebRTC session: spawn sidecar, exchange SDP (single
// offer→answer, non-trickle), pump H.264 frames into the track, and route
// DataChannel control messages through the shared parse/apply path. The JPEG
// /session path is untouched and remains available as a fallback.
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
		m, err := parseControl(data)
		if err != nil {
			return // ignore malformed/unknown
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

	frames, err := client.VideoStream(ctx, rtcFPS)
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
		cancel() // stream ended → tear down
	}()

	if err := conn.WriteJSON(sdpMsg{Type: "answer", SDP: answerSDP}); err != nil {
		return
	}

	// Block until the client disconnects or teardown cancels ctx. Media and
	// control flow over the peer connection, not this socket.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			cancel()
			return
		}
	}
}
```

- [ ] **Step 4: Зарегистрировать роут**

In `internal/server/server.go`, inside `Handler()`, add after the `/session` line:

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
git commit -m "feat(server): /rtc WebRTC signaling endpoint reusing applyControl"
```

---

## Task 6: Браузерный клиент — переключатель RTC / JPG

**Files:**
- Modify: `web/debug/index.html`

Добавляем `<video>`, две кнопки RTC/JPG (дефолт RTC), и DataChannel для ввода. JPG-режим — текущий код без изменений в поведении. Ввод шлётся через общий `sendControl`, который выбирает открытый транспорт.

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

let mode = 'rtc';        // 'rtc' | 'jpg'
let currentUDID = null;
let ws = null;           // jpg path
let pc = null, dc = null; // rtc path

// Active pointer surface depends on mode (img for jpg, video for rtc).
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

// ---- JPG path (Phase 1, unchanged behavior) ----
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
    showSurface();
  };
  pc.onconnectionstatechange = () => {
    if (['failed', 'disconnected', 'closed'].includes(pc.connectionState)) {
      console.log('rtc state', pc.connectionState);
    }
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await iceGatheringComplete(pc); // non-trickle: wait for full SDP

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const sig = new WebSocket(`${proto}://${location.host}/rtc?udid=${encodeURIComponent(udid)}`);
  sig.onopen = () => sig.send(JSON.stringify({type: 'offer', sdp: pc.localDescription.sdp}));
  sig.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    if (m.type === 'answer') {
      await pc.setRemoteDescription({type: 'answer', sdp: m.sdp});
    } else if (m.type === 'error') {
      alert('rtc error: ' + m.msg);
    }
  };
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

// ---- Shared input: route to whichever transport is open ----
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

// Convert a pointer event to normalized {x, y} within the active surface.
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

- [ ] **Step 2: Сборка (статика встроена через FileServer — отдельной сборки нет)**

Run: `go build ./...`
Expected: ок (HTML не компилируется, шаг проверяет, что ничего не сломали в Go).

- [ ] **Step 3: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(web): RTC/JPG mode toggle with WebRTC video + DataChannel input"
```

---

## Task 7: Ручной E2E (Definition of Done)

**Files:** нет (проверка вживую).

- [ ] **Step 1: Запустить демон**

Run: `go run ./cmd/simcastd serve --web ./web/debug`
Open: `http://localhost:8080/`

- [ ] **Step 2: Проверить RTC-режим (дефолт)**

- Кнопка RTC активна по умолчанию. Выбери симулятор (забутится, если нужно).
- **Ожидание:** в `<video>` идёт живой H.264-поток симулятора, ощутимо менее лагающий, чем JPG (низкая задержка).
- Клик = тап, drag = свайп, Home = Home, печать с физической клавиатуры уходит в устройство — всё через DataChannel.

- [ ] **Step 3: Проверить JPG-режим (fallback)**

- Нажми JPG. Поток переключается на скриншот-поллинг (как в Phase 1), ввод продолжает работать по WS.
- Переключись обратно на RTC — снова видео.

- [ ] **Step 4: Проверить teardown**

- Закрой вкладку → в логах демона сайдкар (`idb_companion`) убит (нет висящих процессов: `pgrep -fl idb_companion`).

> **DoD достигнут**, если: RTC-режим даёт низколатентный H.264 по WebRTC + тачи по DataChannel, а JPG-режим работает как раньше.

---

## Task 8: Уборка спайка и документация

**Files:**
- Delete: `cmd/spike-rtc/`
- Modify: `README.md`
- Modify: `docs/decisions.md` (если ещё не дополнено в Task 0 Step 4)

- [ ] **Step 1: Удалить спайк**

Run: `git rm -r cmd/spike-rtc`

- [ ] **Step 2: Обновить README**

In `README.md`, under the streaming section, document the RTC/JPG toggle: RTC — дефолт, низколатентный H.264 по WebRTC (тачи по DataChannel); JPG — fallback на скриншот-поллинг (Phase 1). Отметить, что `/rtc?udid=X` — WS-сигналинг (offer→answer, non-trickle), а медиа и control идут по P2P. Добавить `/rtc` в таблицу эндпоинтов API.

- [ ] **Step 3: Финальная проверка сборки и тестов**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: всё зелёное.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "chore(phase2): remove rtc spike, document WebRTC mode in README"
```

---

## Self-Review Notes

- **Spec coverage:** клиент-браузер (Task 6), сосуществование RTC+JPG (Task 5/6, `/session` нетронут), спайк шаг 0 (Task 0), подход A — `internal/rtc` + `/rtc` (Task 4/5), DataChannel-тачи (Task 5/6), non-trickle offer→answer браузер-инициатор (Task 4/5/6), H.264-ингест fps=30 + access unit'ы (Task 1/2), переиспользование `applyControl` без дублирования (Task 3), обработка ошибок/teardown (Task 5: `OnClose`→`cancel`, sidecar kill через defer), тестирование (Task 1/2/4/5 + E2E Task 7) — покрыто.
- **Граница vs спец:** WS-хендлер `/rtc` живёт в пакете `server`, а не в `internal/rtc` — осознанное уточнение спеки: так `rtc` остаётся чистым pion (без idb/HTTP/парсинга команд), `server` владеет роутингом и переиспользует `applyControl`, нет циклов импорта. Дух решения №30 («pion-логика в `internal/rtc`, server подключает одной строкой») соблюдён.
- **Type consistency:** `Frame{Data, Duration}`, `Session.New(onControl func([]byte))`, `Session.Answer(string)→(string,error)`, `Session.WriteFrame([]byte, time.Duration)`, `Session.OnClose(func())`, `applyControl(ctx, *idb.Client, idb.Screen, controlMsg)`, `sdpMsg{Type,SDP,Msg}` — согласованы между задачами.
- **Открыто на спайк (не плейсхолдеры, а явно делегированные на Task 0):** точный fps (старт 30), нужно ли руками отделять SPS/PPS (по умолчанию pion разрулит; код AU-сборки уже корректно цепляет SPS/PPS к кадру), наличие PTS (по умолчанию равномерный шаг от fps), profile-level-id (дефолтный MediaEngine pion). Если спайк покажет иное — правки точечные в Task 2/4.
