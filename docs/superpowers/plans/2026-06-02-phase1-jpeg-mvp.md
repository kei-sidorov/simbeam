# Phase 1 — JPEG MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** На debug-клиенте в браузере видно живой экран iOS-симулятора (JPEG-кадры по WebSocket), клик попадает как тап; клиент умеет список симуляторов и boot.

**Architecture:** `simcastd serve` поднимает HTTP-сервер. REST `GET /api/simulators` и `POST /api/boot` управляют lifecycle через CLI `idb_companion` (пакет `companion`). На `WS /session?udid=X` сервер спавнит сайдкар `idb_companion --udid X --grpc-port N`, по gRPC зовёт `describe` (размеры), `video_stream(MJPEG)` (кадры → WS binary) и `hid` (тапы из WS). Один WS = одна сессия со своим сайдкаром; teardown убивает сайдкар.

**Tech Stack:** Go 1.26, `google.golang.org/grpc` + `google.golang.org/protobuf` (стабы из `idb.proto`), `github.com/gorilla/websocket`, vanilla HTML/JS debug-клиент, Makefile, protoc 33.4.

---

## File Structure

| Путь | Ответственность | Действие |
|------|-----------------|----------|
| `proto/idb.proto` | Копия idb.proto + `option go_package` | Create |
| `internal/idbpb/*.pb.go` | Сгенерённые protobuf+gRPC стабы (коммитятся, руками не править) | Generate |
| `internal/companion/companion.go` | CLI-обвязка; +`Boot(ctx, udid)` | Modify |
| `internal/companion/companion_test.go` | Тесты Boot через fake-бинарь | Create |
| `internal/idb/freeport.go` | Выбор свободного TCP-порта | Create |
| `internal/idb/freeport_test.go` | Тест freePort | Create |
| `internal/idb/sidecar.go` | `Spawn(ctx, bin, udid)` → готовность; `Close()` | Create |
| `internal/idb/client.go` | `Describe` / `VideoStream` / `Tap` / `Home`; типы `Screen`, `Point` | Create |
| `internal/idb/coords.go` | `ScaleTap(xNorm,yNorm, Screen) Point` | Create |
| `internal/idb/coords_test.go` | Тест ScaleTap | Create |
| `internal/server/framebuf.go` | Single-slot буфер «последний кадр» | Create |
| `internal/server/framebuf_test.go` | Тест FrameBuffer | Create |
| `internal/server/control.go` | Парс control-сообщений WS (`tap`/`home`) | Create |
| `internal/server/control_test.go` | Тест парса control | Create |
| `internal/server/server.go` | Роутер + REST handlers (list/boot) + static | Create |
| `internal/server/server_test.go` | httptest для REST с фейками | Create |
| `internal/server/session.go` | WS-сессия: сайдкар + кадры↓ + ввод↑ + teardown | Create |
| `web/debug/index.html` | Debug-клиент (список/boot/кадр/тап/Home) | Create |
| `cmd/simcastd/main.go` | +подкоманда `serve [--addr] [--web]` | Modify |
| `Makefile` | `proto`/`build`/`run`/`test` | Create |
| `README.md` | Инструкция `serve` | Modify |

**Ключевые типы (определяются в Task 3–5, используются дальше):**

```go
// internal/idb/client.go
type Screen struct { Width, Height, WidthPoints, HeightPoints uint64 }
type Point  struct { X, Y float64 }

type Client struct { /* grpc conn + idbpb.CompanionServiceClient */ }
func (c *Client) Describe(ctx context.Context) (Screen, error)
func (c *Client) VideoStream(ctx context.Context) (<-chan []byte, error) // MJPEG frames; стоп по ctx
func (c *Client) Tap(ctx context.Context, p Point) error
func (c *Client) Home(ctx context.Context) error

// internal/idb/sidecar.go
type Sidecar struct { /* *exec.Cmd, port int, *grpc.ClientConn, *Client */ }
func Spawn(ctx context.Context, bin, udid string) (*Sidecar, error) // спавн + ждёт готовности
func (s *Sidecar) Client() *Client
func (s *Sidecar) Close() error // убивает процесс, закрывает conn
```

---

## Task 1: Proto setup, gRPC stubs, Makefile, deps

**Files:**
- Create: `proto/idb.proto`, `Makefile`, `internal/idbpb/` (generated)
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Скопировать proto и добавить go_package**

```bash
mkdir -p proto
cp ~/Developer/ios-bridge/.venv/proto/idb.proto proto/idb.proto
```

В `proto/idb.proto` сразу после строки `package idb;` добавить:

```proto
option go_package = "github.com/kei-sidorov/simcast/internal/idbpb;idbpb";
```

- [ ] **Step 2: Установить protoc-плагины и gRPC-зависимости**

```bash
export PATH="$PATH:/opt/homebrew/bin:/usr/local/go/bin:$HOME/go/bin"
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go get google.golang.org/grpc@latest
go get google.golang.org/protobuf@latest
```

Expected: `$HOME/go/bin/protoc-gen-go` и `protoc-gen-go-grpc` существуют; `go.mod` содержит `google.golang.org/grpc` и `google.golang.org/protobuf`.

- [ ] **Step 3: Создать Makefile**

```makefile
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: proto build run test

proto:
	mkdir -p internal/idbpb
	protoc \
		--go_out=. --go_opt=module=github.com/kei-sidorov/simcast \
		--go-grpc_out=. --go-grpc_opt=module=github.com/kei-sidorov/simcast \
		proto/idb.proto

build:
	go build ./...

run:
	go run ./cmd/simcastd serve --web ./web/debug

test:
	go test ./...
```

- [ ] **Step 4: Сгенерировать стабы**

Run: `make proto`
Expected: появляются `internal/idbpb/idb.pb.go` и `internal/idbpb/idb_grpc.pb.go` без ошибок.

- [ ] **Step 5: Проверить сборку**

Run: `go build ./... && go vet ./...`
Expected: успех (стабы компилируются).

- [ ] **Step 6: Commit**

```bash
git add proto Makefile internal/idbpb go.mod go.sum
git commit -m "feat(phase1): vendor idb.proto, generate gRPC stubs, add Makefile"
```

---

## Task 2: companion.Boot()

**Files:**
- Modify: `internal/companion/companion.go`
- Test: `internal/companion/companion_test.go`

- [ ] **Step 1: Написать падающий тест (fake-бинарь)**

`internal/companion/companion_test.go`:

```go
package companion

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeBinary пишет исполняемый shell-скрипт, имитирующий idb_companion.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "idb_companion")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBootSuccess(t *testing.T) {
	c := &Client{Binary: fakeBinary(t, "exit 0\n")}
	if err := c.Boot(context.Background(), "UDID-123"); err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
}

func TestBootFailureIncludesStderr(t *testing.T) {
	c := &Client{Binary: fakeBinary(t, "echo 'boot failed: no such device' 1>&2\nexit 1\n")}
	err := c.Boot(context.Background(), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "boot failed") {
		t.Fatalf("error %q does not include stderr", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub { return true }
	}
	return false
})() }
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/companion/ -run TestBoot -v`
Expected: FAIL — `c.Boot` не объявлен (не компилируется).

- [ ] **Step 3: Реализовать Boot**

В `internal/companion/companion.go` добавить:

```go
// Boot boots the simulator with the given UDID via `idb_companion --boot <udid>`.
// idb_companion blocks until the simulator reaches a known-booted state
// (--verify-booted defaults to true). Booting an already-booted simulator is
// effectively a no-op.
func (c *Client) Boot(ctx context.Context, udid string) error {
	if _, err := c.run(ctx, "--boot", udid); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/companion/ -run TestBoot -v`
Expected: PASS (оба теста).

- [ ] **Step 5: Commit**

```bash
git add internal/companion/
git commit -m "feat(phase1): companion.Boot via idb_companion --boot"
```

---

## Task 3: idb sidecar (free port + Spawn/Close + Describe)

**Files:**
- Create: `internal/idb/freeport.go`, `internal/idb/freeport_test.go`, `internal/idb/sidecar.go`, `internal/idb/client.go`

- [ ] **Step 1: Написать падающий тест freePort**

`internal/idb/freeport_test.go`:

```go
package idb

import "testing"

func TestFreePortReturnsUsablePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("port out of range: %d", p)
	}
}

func TestFreePortDistinct(t *testing.T) {
	a, _ := freePort()
	b, _ := freePort()
	// Не гарантия уникальности, но два подряд обычно различны на :0.
	if a == 0 || b == 0 {
		t.Fatal("got zero port")
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/idb/ -run TestFreePort -v`
Expected: FAIL — `freePort` не объявлен.

- [ ] **Step 3: Реализовать freePort**

`internal/idb/freeport.go`:

```go
package idb

import "net"

// freePort asks the OS for an unused TCP port on the loopback interface.
// There is a small race between closing the listener and idb_companion
// binding the port; acceptable for the MVP.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/idb/ -run TestFreePort -v`
Expected: PASS.

- [ ] **Step 5: Реализовать Client (Describe) в client.go**

`internal/idb/client.go`:

```go
// Package idb is a gRPC client wrapper around a running idb_companion sidecar
// (idb_companion --udid X --grpc-port N). It exposes the minimal RPC surface
// simcast needs: describe, video_stream (MJPEG), hid.
package idb

import (
	"context"
	"fmt"

	"github.com/kei-sidorov/simcast/internal/idbpb"
	"google.golang.org/grpc"
)

// Screen holds the simulator screen geometry from describe.
type Screen struct {
	Width        uint64
	Height       uint64
	WidthPoints  uint64
	HeightPoints uint64
}

// Point is a coordinate in the device coordinate space expected by hid.
type Point struct {
	X, Y float64
}

// Client wraps the CompanionService gRPC stub.
type Client struct {
	rpc idbpb.CompanionServiceClient
}

// NewClient wraps an established gRPC connection.
func NewClient(conn *grpc.ClientConn) *Client {
	return &Client{rpc: idbpb.NewCompanionServiceClient(conn)}
}

// Describe returns the simulator screen geometry.
func (c *Client) Describe(ctx context.Context) (Screen, error) {
	resp, err := c.rpc.Describe(ctx, &idbpb.TargetDescriptionRequest{})
	if err != nil {
		return Screen{}, fmt.Errorf("describe: %w", err)
	}
	d := resp.GetTargetDescription().GetScreenDimensions()
	if d == nil {
		return Screen{}, fmt.Errorf("describe: no screen dimensions")
	}
	return Screen{
		Width:        d.GetWidth(),
		Height:       d.GetHeight(),
		WidthPoints:  d.GetWidthPoints(),
		HeightPoints: d.GetHeightPoints(),
	}, nil
}
```

- [ ] **Step 6: Реализовать Sidecar (Spawn/Close) в sidecar.go**

`internal/idb/sidecar.go`:

```go
package idb

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Sidecar is a spawned `idb_companion --udid X --grpc-port N` process plus a
// gRPC connection to it.
type Sidecar struct {
	cmd    *exec.Cmd
	port   int
	conn   *grpc.ClientConn
	client *Client
}

// Spawn launches an idb_companion sidecar for udid on a free port and blocks
// until its gRPC server answers a describe call (readiness), or fails.
func Spawn(ctx context.Context, bin, udid string) (*Sidecar, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick port: %w", err)
	}
	cmd := exec.Command(bin, "--udid", udid, "--grpc-port", fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start idb_companion: %w", err)
	}
	sc := &Sidecar{cmd: cmd, port: port}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		sc.Close()
		return nil, fmt.Errorf("grpc client: %w", err)
	}
	sc.conn = conn
	sc.client = NewClient(conn)

	// Readiness: retry describe until it succeeds or we time out.
	ready, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if _, derr := sc.client.Describe(ready); derr == nil {
			return sc, nil
		}
		select {
		case <-ready.Done():
			sc.Close()
			return nil, fmt.Errorf("sidecar for %s not ready within timeout", udid)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Client returns the gRPC client for this sidecar.
func (s *Sidecar) Client() *Client { return s.client }

// Close terminates the gRPC connection and kills the idb_companion process.
func (s *Sidecar) Close() error {
	if s.conn != nil {
		s.conn.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	return nil
}
```

- [ ] **Step 7: Проверить сборку и юнит-тесты**

Run: `go build ./... && go test ./internal/idb/ -run TestFreePort -v`
Expected: сборка ОК; freePort-тесты PASS. (Spawn/Describe проверяются вручную в Task 10 — нужен сим.)

- [ ] **Step 8: Commit**

```bash
git add internal/idb/
git commit -m "feat(phase1): idb sidecar spawn/readiness + Describe"
```

---

## Task 4: idb VideoStream (MJPEG) + server FrameBuffer

**Files:**
- Modify: `internal/idb/client.go`
- Create: `internal/server/framebuf.go`, `internal/server/framebuf_test.go`

- [ ] **Step 1: Реализовать VideoStream в client.go**

Добавить в `internal/idb/client.go` (импорты `io`, `log`):

```go
// VideoStream starts an MJPEG video stream and returns a channel of JPEG
// frames. The stream stops when ctx is cancelled; the channel is then closed.
func (c *Client) VideoStream(ctx context.Context) (<-chan []byte, error) {
	stream, err := c.rpc.VideoStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("video_stream open: %w", err)
	}
	start := &idbpb.VideoStreamRequest{
		Control: &idbpb.VideoStreamRequest_Start_{
			Start: &idbpb.VideoStreamRequest_Start{
				Fps:    30,
				Format: idbpb.VideoStreamRequest_MJPEG,
			},
		},
	}
	if err := stream.Send(start); err != nil {
		return nil, fmt.Errorf("video_stream start: %w", err)
	}

	frames := make(chan []byte)
	go func() {
		defer close(frames)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return // ctx cancelled, EOF, or transport error
			}
			data := resp.GetPayload().GetData()
			if len(data) == 0 {
				continue
			}
			select {
			case frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames, nil
}
```

> Примечание: точные имена сгенерённых типов oneof (`VideoStreamRequest_Start_`, `VideoStreamRequest_MJPEG`) сверить в `internal/idbpb/idb.pb.go` после Task 1 и поправить при расхождении.

- [ ] **Step 2: Написать падающий тест FrameBuffer**

`internal/server/framebuf_test.go`:

```go
package server

import (
	"context"
	"testing"
	"time"
)

func TestFrameBufferReturnsLatest(t *testing.T) {
	b := newFrameBuffer()
	b.set([]byte("A"))
	b.set([]byte("B")) // должен затереть A
	got, err := b.next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "B" {
		t.Fatalf("got %q, want B (latest only)", got)
	}
}

func TestFrameBufferNextBlocksUntilSet(t *testing.T) {
	b := newFrameBuffer()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.next(ctx); err == nil {
		t.Fatal("expected timeout error when no frame set")
	}
}
```

- [ ] **Step 3: Запустить — убедиться, что падает**

Run: `go test ./internal/server/ -run TestFrameBuffer -v`
Expected: FAIL — `newFrameBuffer` не объявлен.

- [ ] **Step 4: Реализовать FrameBuffer**

`internal/server/framebuf.go`:

```go
package server

import (
	"context"
	"sync"
)

// frameBuffer holds only the most recent frame. set() overwrites any
// un-consumed frame, so a slow WebSocket writer never accumulates lag —
// stale frames are dropped, latency stays low.
type frameBuffer struct {
	mu     sync.Mutex
	latest []byte
	notify chan struct{}
}

func newFrameBuffer() *frameBuffer {
	return &frameBuffer{notify: make(chan struct{}, 1)}
}

func (b *frameBuffer) set(frame []byte) {
	b.mu.Lock()
	b.latest = frame
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default: // signal already pending
	}
}

// next blocks until a frame is available or ctx is done.
func (b *frameBuffer) next(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.notify:
		b.mu.Lock()
		f := b.latest
		b.latest = nil
		b.mu.Unlock()
		return f, nil
	}
}
```

- [ ] **Step 5: Запустить — убедиться, что проходит**

Run: `go test ./internal/server/ -run TestFrameBuffer -v`
Expected: PASS (оба теста).

- [ ] **Step 6: Commit**

```bash
git add internal/idb/client.go internal/server/framebuf.go internal/server/framebuf_test.go
git commit -m "feat(phase1): MJPEG VideoStream + single-slot frame buffer"
```

---

## Task 5: idb hid (Tap/Home) + coordinate scaling

**Files:**
- Create: `internal/idb/coords.go`, `internal/idb/coords_test.go`
- Modify: `internal/idb/client.go`

- [ ] **Step 1: Написать падающий тест ScaleTap**

`internal/idb/coords_test.go`:

```go
package idb

import "testing"

func TestScaleTapCenter(t *testing.T) {
	s := Screen{Width: 1170, Height: 2532, WidthPoints: 390, HeightPoints: 844}
	p := ScaleTap(0.5, 0.5, s)
	if p.X != 585 || p.Y != 1266 {
		t.Fatalf("got (%v,%v), want (585,1266)", p.X, p.Y)
	}
}

func TestScaleTapClamps(t *testing.T) {
	s := Screen{Width: 100, Height: 100, WidthPoints: 50, HeightPoints: 50}
	p := ScaleTap(1.5, -0.2, s) // вне диапазона → клампится в [0,1]
	if p.X != 100 || p.Y != 0 {
		t.Fatalf("got (%v,%v), want (100,0)", p.X, p.Y)
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/idb/ -run TestScaleTap -v`
Expected: FAIL — `ScaleTap` не объявлен.

- [ ] **Step 3: Реализовать ScaleTap**

`internal/idb/coords.go`:

```go
package idb

// ScaleTap maps a normalized tap (xNorm,yNorm in [0,1] of the displayed frame)
// to a Point in the simulator's pixel coordinate space.
//
// NOTE: whether hid expects pixels (Width/Height) or logical points
// (WidthPoints/HeightPoints) is verified live in Task 10. Default: pixels.
func ScaleTap(xNorm, yNorm float64, s Screen) Point {
	xNorm = clamp01(xNorm)
	yNorm = clamp01(yNorm)
	return Point{
		X: xNorm * float64(s.Width),
		Y: yNorm * float64(s.Height),
	}
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
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/idb/ -run TestScaleTap -v`
Expected: PASS.

- [ ] **Step 5: Реализовать Tap/Home в client.go**

Добавить в `internal/idb/client.go`:

```go
// Tap performs a touch down + up at p (one hid stream per tap).
func (c *Client) Tap(ctx context.Context, p Point) error {
	pt := &idbpb.Point{X: p.X, Y: p.Y}
	return c.sendHID(ctx,
		touchEvent(pt, idbpb.HIDEvent_DOWN),
		touchEvent(pt, idbpb.HIDEvent_UP),
	)
}

// Home presses and releases the Home button.
func (c *Client) Home(ctx context.Context) error {
	return c.sendHID(ctx,
		buttonEvent(idbpb.HIDEvent_HOME, idbpb.HIDEvent_DOWN),
		buttonEvent(idbpb.HIDEvent_HOME, idbpb.HIDEvent_UP),
	)
}

func (c *Client) sendHID(ctx context.Context, events ...*idbpb.HIDEvent) error {
	stream, err := c.rpc.Hid(ctx)
	if err != nil {
		return fmt.Errorf("hid open: %w", err)
	}
	for _, e := range events {
		if err := stream.Send(e); err != nil {
			return fmt.Errorf("hid send: %w", err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("hid close: %w", err)
	}
	return nil
}

func touchEvent(pt *idbpb.Point, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return &idbpb.HIDEvent{
		Event: &idbpb.HIDEvent_Press{Press: &idbpb.HIDEvent_HIDPress{
			Action: &idbpb.HIDEvent_HIDPressAction{
				Action: &idbpb.HIDEvent_HIDPressAction_Touch{
					Touch: &idbpb.HIDEvent_HIDTouch{Point: pt},
				},
			},
			Direction: dir,
		}},
	}
}

func buttonEvent(btn idbpb.HIDEvent_HIDButtonType, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return &idbpb.HIDEvent{
		Event: &idbpb.HIDEvent_Press{Press: &idbpb.HIDEvent_HIDPress{
			Action: &idbpb.HIDEvent_HIDPressAction{
				Action: &idbpb.HIDEvent_HIDPressAction_Button{
					Button: &idbpb.HIDEvent_HIDButton{Button: btn},
				},
			},
			Direction: dir,
		}},
	}
}
```

> Примечание: имена сгенерённых типов (`HIDEvent_Press`, `HIDEvent_HIDPress`, `HIDEvent_HIDPressAction_Touch`, `HIDEvent_HOME`, `Hid` метод) сверить в `internal/idbpb/idb_grpc.pb.go` и `idb.pb.go`; поправить при расхождении.

- [ ] **Step 6: Проверить сборку и тесты**

Run: `go build ./... && go test ./internal/idb/ -v`
Expected: сборка ОК; ScaleTap + freePort PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/idb/
git commit -m "feat(phase1): hid Tap/Home + normalized coordinate scaling"
```

---

## Task 6: server REST handlers (list/boot)

**Files:**
- Create: `internal/server/server.go`, `internal/server/server_test.go`

- [ ] **Step 1: Написать падающий тест REST с фейками**

`internal/server/server_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kei-sidorov/simcast/internal/companion"
)

type fakeCompanion struct {
	sims     []companion.Simulator
	bootErr  error
	bootUDID string
}

func (f *fakeCompanion) List(ctx context.Context) ([]companion.Simulator, error) {
	return f.sims, nil
}
func (f *fakeCompanion) Boot(ctx context.Context, udid string) error {
	f.bootUDID = udid
	return f.bootErr
}

func TestSimulatorsEndpoint(t *testing.T) {
	f := &fakeCompanion{sims: []companion.Simulator{{UDID: "U1", Name: "iPhone", OSVersion: "iOS 26.4", State: "Booted"}}}
	srv := New(f, "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/simulators", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	var got []companion.Simulator
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UDID != "U1" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestBootEndpoint(t *testing.T) {
	f := &fakeCompanion{}
	srv := New(f, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/boot", strings.NewReader(`{"udid":"U9"}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if f.bootUDID != "U9" {
		t.Fatalf("Boot got udid %q", f.bootUDID)
	}
}

func TestBootEndpointMissingUDID(t *testing.T) {
	srv := New(&fakeCompanion{}, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/boot", strings.NewReader(`{}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/server/ -run "TestSimulators|TestBoot" -v`
Expected: FAIL — `New`/`Handler` не объявлены.

- [ ] **Step 3: Реализовать server.go (REST + роутер)**

`internal/server/server.go`:

```go
// Package server exposes the simcast daemon HTTP API: REST list/boot plus a
// per-session WebSocket that streams JPEG frames and accepts taps.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/kei-sidorov/simcast/internal/companion"
)

// Companion is the lifecycle surface the server needs (satisfied by *companion.Client).
type Companion interface {
	List(ctx context.Context) ([]companion.Simulator, error)
	Boot(ctx context.Context, udid string) error
}

// Server wires HTTP handlers over a Companion plus the idb_companion binary path.
type Server struct {
	comp   Companion
	binary string // path to idb_companion for sidecars; "" → "idb_companion"
	webDir string // static debug client dir; "" → not served
}

// New creates a Server. webDir is served at / when non-empty.
func New(comp Companion, webDir string) *Server {
	return &Server{comp: comp, webDir: webDir, binary: "idb_companion"}
}

// WithBinary overrides the idb_companion path used for sidecars.
func (s *Server) WithBinary(bin string) *Server { s.binary = bin; return s }

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/simulators", s.handleSimulators)
	mux.HandleFunc("/api/boot", s.handleBoot)
	mux.HandleFunc("/session", s.handleSession)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

func (s *Server) handleSimulators(w http.ResponseWriter, r *http.Request) {
	sims, err := s.comp.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sims)
}

func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		UDID string `json:"udid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UDID == "" {
		writeErr(w, http.StatusBadRequest, "missing udid")
		return
	}
	if err := s.comp.Boot(r.Context(), body.UDID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "Booted"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
```

> `handleSession` реализуется в Task 7 (тот же пакет). Чтобы пакет компилировался сейчас, добавить во `server.go` временную заглушку — она будет заменена в Task 7:
>
> ```go
> func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
> 	http.Error(w, "not implemented", http.StatusNotImplemented)
> }
> ```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/server/ -run "TestSimulators|TestBoot" -v`
Expected: PASS (3 теста).

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "feat(phase1): REST handlers for /api/simulators and /api/boot"
```

---

## Task 7: server WS session (control parse + wiring)

**Files:**
- Create: `internal/server/control.go`, `internal/server/control_test.go`, `internal/server/session.go`
- Modify: `internal/server/server.go` (заменить заглушку handleSession), `go.mod` (gorilla/websocket)

- [ ] **Step 1: Написать падающий тест парса control**

`internal/server/control_test.go`:

```go
package server

import "testing"

func TestParseControlTap(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"tap","x":0.25,"y":0.75}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "tap" || m.X != 0.25 || m.Y != 0.75 {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestParseControlHome(t *testing.T) {
	m, err := parseControl([]byte(`{"type":"home"}`))
	if err != nil || m.Type != "home" {
		t.Fatalf("unexpected: %+v err %v", m, err)
	}
}

func TestParseControlBadJSON(t *testing.T) {
	if _, err := parseControl([]byte(`not json`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseControlUnknownType(t *testing.T) {
	if _, err := parseControl([]byte(`{"type":"wat"}`)); err == nil {
		t.Fatal("expected error for unknown type")
	}
}
```

- [ ] **Step 2: Запустить — убедиться, что падает**

Run: `go test ./internal/server/ -run TestParseControl -v`
Expected: FAIL — `parseControl` не объявлен.

- [ ] **Step 3: Реализовать control.go**

`internal/server/control.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
)

// controlMsg is an upstream WS message from the client.
type controlMsg struct {
	Type string  `json:"type"` // "tap" | "home"
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

func parseControl(data []byte) (controlMsg, error) {
	var m controlMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("bad control json: %w", err)
	}
	switch m.Type {
	case "tap", "home":
		return m, nil
	default:
		return m, fmt.Errorf("unknown control type %q", m.Type)
	}
}
```

- [ ] **Step 4: Запустить — убедиться, что проходит**

Run: `go test ./internal/server/ -run TestParseControl -v`
Expected: PASS (4 теста).

- [ ] **Step 5: Добавить gorilla/websocket**

```bash
export PATH="$PATH:/opt/homebrew/bin:/usr/local/go/bin:$HOME/go/bin"
go get github.com/gorilla/websocket@latest
```

- [ ] **Step 6: Реализовать session.go**

`internal/server/session.go`:

```go
package server

import (
	"context"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/kei-sidorov/simcast/internal/idb"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // debug client; same machine
}

// handleSession upgrades to WS, spawns an idb_companion sidecar for ?udid=,
// streams MJPEG frames down (binary) and applies taps/home from up (JSON).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
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
		_ = conn.WriteJSON(map[string]string{"type": "error", "msg": err.Error()})
		return
	}
	defer sidecar.Close()
	client := sidecar.Client()

	screen, err := client.Describe(ctx)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "msg": err.Error()})
		return
	}
	_ = conn.WriteJSON(map[string]any{"type": "ready", "w": screen.Width, "h": screen.Height})

	frames, err := client.VideoStream(ctx)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "msg": err.Error()})
		return
	}

	// Producer: gRPC frames → single-slot buffer (drop stale).
	buf := newFrameBuffer()
	go func() {
		for f := range frames {
			buf.set(f)
		}
		cancel() // stream ended → tear down session
	}()

	// Writer: buffer → WS binary.
	go func() {
		for {
			f, err := buf.next(ctx)
			if err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
				cancel()
				return
			}
		}
	}()

	// Reader (this goroutine): WS control → hid. Returns on disconnect.
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
		switch m.Type {
		case "tap":
			if err := client.Tap(ctx, idb.ScaleTap(m.X, m.Y, screen)); err != nil {
				log.Printf("tap: %v", err)
			}
		case "home":
			if err := client.Home(ctx); err != nil {
				log.Printf("home: %v", err)
			}
		}
	}
}
```

Удалить временную заглушку `handleSession` из `server.go` (Task 6 Step 3).

- [ ] **Step 7: Проверить сборку и тесты пакета**

Run: `go build ./... && go test ./internal/server/ -v`
Expected: сборка ОК; все server-тесты PASS (REST, framebuf, control).

- [ ] **Step 8: Commit**

```bash
git add internal/server/ go.mod go.sum
git commit -m "feat(phase1): WS session — MJPEG frames down, tap/home up"
```

---

## Task 8: serve subcommand in cmd/simcastd

**Files:**
- Modify: `cmd/simcastd/main.go`

- [ ] **Step 1: Добавить serve в main.go**

В `cmd/simcastd/main.go` добавить ветку в `switch` и функцию `runServe`. Импорты: добавить `flag`, `net/http`, `github.com/kei-sidorov/simcast/internal/server`.

```go
case "serve":
	if err := runServe(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
```

```go
func runServe(argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	webDir := fs.String("web", "", "directory with debug client (served at /) ; empty = API only")
	_ = fs.Parse(argv)

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	srv := server.New(c, *webDir).WithBinary(path)

	fmt.Printf("simcastd serving on %s (idb_companion: %s)\n", *addr, path)
	if *webDir != "" {
		fmt.Printf("debug client: http://localhost%s/\n", *addr)
	}
	return http.ListenAndServe(*addr, srv.Handler())
}
```

Обновить `usage`, добавив строку:

```go
fmt.Fprintln(w, "  simcastd serve   Serve REST API + WebSocket stream (flags: --addr, --web)")
```

- [ ] **Step 2: Проверить сборку и запуск помощи**

Run: `go build ./... && go run ./cmd/simcastd serve --help`
Expected: сборка ОК; печатается usage флагов serve и выход.

- [ ] **Step 3: Commit**

```bash
git add cmd/simcastd/main.go
git commit -m "feat(phase1): simcastd serve subcommand"
```

---

## Task 9: debug client (web/debug/index.html)

**Files:**
- Create: `web/debug/index.html`

- [ ] **Step 1: Создать debug-клиент**

`web/debug/index.html`:

```html
<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>simcast debug</title>
  <style>
    body { font-family: -apple-system, sans-serif; margin: 16px; }
    #sims button { display: block; margin: 4px 0; }
    #screen { border: 1px solid #ccc; max-height: 80vh; cursor: crosshair; display: none; }
    #home { margin-top: 8px; display: none; }
  </style>
</head>
<body>
  <h3>Simulators</h3>
  <div id="sims">loading…</div>
  <img id="screen" alt="simulator">
  <div><button id="home">Home</button></div>

<script>
const simsEl = document.getElementById('sims');
const screenEl = document.getElementById('screen');
const homeBtn = document.getElementById('home');
let ws = null;

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
  startStream(s.udid);
}

function startStream(udid) {
  if (ws) ws.close();
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/session?udid=${encodeURIComponent(udid)}`);
  ws.binaryType = 'blob';
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') { console.log('status', ev.data); return; }
    const url = URL.createObjectURL(ev.data);
    const old = screenEl.src;
    screenEl.src = url;
    screenEl.style.display = 'block';
    homeBtn.style.display = 'inline-block';
    if (old) URL.revokeObjectURL(old);
  };
  ws.onclose = () => { console.log('ws closed'); };
}

screenEl.onclick = (e) => {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  const rect = screenEl.getBoundingClientRect();
  const x = (e.clientX - rect.left) / rect.width;
  const y = (e.clientY - rect.top) / rect.height;
  ws.send(JSON.stringify({type: 'tap', x, y}));
};

homeBtn.onclick = () => {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type: 'home'}));
};

loadSims();
</script>
</body>
</html>
```

- [ ] **Step 2: Проверить, что сервер отдаёт страницу**

Run: `go run ./cmd/simcastd serve --web ./web/debug &` затем `curl -s localhost:8080/ | grep -o '<title>simcast debug</title>'` ; затем убить процесс.
Expected: выводит `<title>simcast debug</title>`.

- [ ] **Step 3: Commit**

```bash
git add web/debug/index.html
git commit -m "feat(phase1): debug web client (list/boot/stream/tap/home)"
```

---

## Task 10: Manual DoD verification + README + resolve coords

**Files:**
- Modify: `internal/idb/coords.go` (если потребуется по результату проверки), `README.md`, `docs/decisions.md`

- [ ] **Step 1: Полный прогон тестов и сборки**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: всё зелёное.

- [ ] **Step 2: Ручная проверка DoD (нужен реальный сим)**

```bash
go run ./cmd/simcastd serve --web ./web/debug
```
Открыть `http://localhost:8080/`, выбрать симулятор (если не booted — кнопка забутит), дождаться кадров.

Expected:
- виден живой экран симулятора (кадры обновляются);
- клик по экрану → тап попадает в соответствующую точку симулятора;
- кнопка Home → симулятор уходит на домашний экран.

- [ ] **Step 3: Решить вопрос координат (px vs points)**

Кликнуть по углу заметного элемента (напр. иконка в углу). Если тап попадает мимо со сдвигом в ~2–3× — `hid` ждёт логические точки, а не пиксели. Тогда в `internal/idb/coords.go` заменить `s.Width`/`s.Height` на `s.WidthPoints`/`s.HeightPoints`, поправить ожидаемые значения в `coords_test.go`, перезапустить `go test ./internal/idb/` и повторить проверку. Зафиксировать выбор строкой в `docs/decisions.md`.

- [ ] **Step 4: Обновить README**

В `README.md` добавить раздел про `serve`:

```markdown
## Запуск стрима (Phase 1)

```bash
go run ./cmd/simcastd serve --web ./web/debug
```

Открой `http://localhost:8080/`, выбери симулятор (кнопка забутит, если он не запущен),
смотри живые JPEG-кадры по WebSocket. Клик по экрану = тап в симулятор, кнопка Home = Home.

Без `--web` демон отдаёт только API (`GET /api/simulators`, `POST /api/boot`,
`WS /session?udid=X`) — для подключения собственных клиентов.
```

- [ ] **Step 5: Финальный commit**

```bash
git add README.md docs/decisions.md internal/idb/
git commit -m "docs(phase1): README serve instructions; resolve hid coordinate space"
```

---

## Self-Review

**Spec coverage:** describe/video_stream(MJPEG)/hid → Tasks 3–5; REST list/boot → Tasks 2,6; WS session + sidecar lifecycle + teardown → Tasks 3,7; нормализованные координаты → Task 5; debug-клиент отдельным файлом + `--web` → Tasks 6,8,9; single-slot backpressure → Task 4; коммит сгенерённого кода + Makefile → Task 1; px-vs-points research-item → Task 10. Все пункты спека покрыты.

**Placeholder scan:** реальный код во всех шагах; «примечания» о сверке имён сгенерённых oneof-типов — не placeholder'ы, а указание сверить с фактическим выводом protoc (имена детерминированы, но зависят от версии плагина). Заглушка `handleSession` в Task 6 явно временная и удаляется в Task 7.

**Type consistency:** `Screen`/`Point`/`Client`/`Sidecar` (Task 3) используются в Tasks 4,5,7; `frameBuffer.set/next` (Task 4) — в Task 7; `parseControl`/`controlMsg` (Task 7) — там же; `Server.New/Handler/WithBinary` (Task 6) — в Task 8; `ScaleTap` (Task 5) — в Task 7. Сигнатуры согласованы.
