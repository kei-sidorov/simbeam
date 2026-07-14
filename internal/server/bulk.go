package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// screenshotTimeout bounds a full-resolution capture so the daemon always
// answers — with an image or a text error — before the client's ~20s failsafe
// fires. Without it a wedged gRPC/CDP call would leave the client waiting on
// its timeout instead of a clean error.
const screenshotTimeout = 15 * time.Second

// bulkChunkCeiling caps one binary frame however much the peer allows. A
// full-resolution PNG is several megabytes, so it MUST be split; libwebrtc
// advertises 256 KiB and there is nothing to gain from frames larger than this
// even against a peer that permits them.
const bulkChunkCeiling = 200 * 1024

// bulkChunkFallback is the frame size used when the peer's negotiated cap is
// unknown (no SCTP association — in practice unreachable, since the request we
// are answering arrived over that very association). 16 KiB is below every cap
// SCTP can settle on, including the 65535 pion falls back to for a peer whose
// SDP omits "a=max-message-size".
const bulkChunkFallback = 16 * 1024

// bulkMsg is an inbound request on the reliable ordered "bulk" DataChannel. The
// only request today is {"type":"screenshot"} — a full-resolution capture of
// the currently attached simulator, with no parameters.
type bulkMsg struct {
	Type string `json:"type"` // screenshot
}

// bulkHeader announces an image transfer: the binary chunks that follow
// concatenate to exactly Bytes bytes. The client needs the total because the
// channel gives it no other way to know when the last chunk has landed.
type bulkHeader struct {
	Type  string `json:"type"` // always "screenshot"
	Bytes int    `json:"bytes"`
}

// bulkErr is the text error envelope sent back on "bulk" when a request cannot
// be satisfied.
type bulkErr struct {
	Type string `json:"type"` // always "error"
	Msg  string `json:"msg"`
}

// handleBulk processes one inbound "bulk" message. It runs on pion's per-channel
// read goroutine (separate from control's), and the client keeps a single
// request in flight, so blocking here is fine and needs no id correlation. The
// contract demands the daemon always reply — image or text error — so every
// path below ends in a reply.
func (d *rtcDispatch) handleBulk(data []byte) {
	var m bulkMsg
	if err := json.Unmarshal(data, &m); err != nil {
		d.bulkError("bad bulk json")
		return
	}
	switch m.Type {
	case "screenshot":
		d.doScreenshot()
	default:
		d.bulkError(fmt.Sprintf("unknown bulk type %q", m.Type))
	}
}

// doScreenshot captures the currently attached feed at full resolution and
// streams it back, or replies with a text error if nothing is attached, the
// capture fails, or the transfer breaks partway.
func (d *rtcDispatch) doScreenshot() {
	d.mu.Lock()
	att := d.att
	d.mu.Unlock()
	if att == nil {
		d.bulkError("no simulator attached")
		return
	}
	ctx, cancel := context.WithTimeout(d.baseCtx, screenshotTimeout)
	defer cancel()
	started := time.Now()
	img, err := att.feed.Screenshot(ctx)
	if err != nil {
		log.Printf("screenshot: capture failed after %v: %v", time.Since(started), err)
		d.bulkError(err.Error())
		return
	}
	if len(img) == 0 {
		log.Print("screenshot: capture returned no bytes")
		d.bulkError("capture returned no bytes")
		return
	}
	if err := d.sendImage(img); err != nil {
		log.Printf("screenshot: sending %d bytes failed: %v", len(img), err)
		d.bulkError(fmt.Sprintf("send failed: %v", err))
		return
	}
	log.Printf("screenshot: sent %d bytes in %v", len(img), time.Since(started))
}

// chunkSize is the binary frame size for one transfer: the cap the peer actually
// negotiated, clamped to the ceiling. Reading it per transfer rather than
// hardcoding a guess is what keeps screenshots working against a peer that
// advertises less than we assume (or advertises nothing, leaving pion on its
// 65535 fallback) — every send above the cap is rejected outright.
func (d *rtcDispatch) chunkSize() int {
	if d.bulkMaxMsg == nil {
		return bulkChunkFallback
	}
	negotiated := d.bulkMaxMsg()
	if negotiated <= 0 {
		return bulkChunkFallback
	}
	return min(negotiated, bulkChunkCeiling)
}

// sendImage streams img as a text header announcing the byte count followed by
// binary chunks within the peer's message-size cap. The channel is reliable and
// ordered, so the chunks need no sequence numbers — the client simply appends
// until it holds the announced total.
func (d *rtcDispatch) sendImage(img []byte) error {
	if d.sendBulk == nil || d.sendBulkText == nil {
		return errors.New("bulk channel not wired")
	}
	header, err := json.Marshal(bulkHeader{Type: "screenshot", Bytes: len(img)})
	if err != nil {
		return fmt.Errorf("encode header: %w", err)
	}
	if err := d.sendBulkText(string(header)); err != nil {
		return fmt.Errorf("send header: %w", err)
	}
	size := d.chunkSize()
	for offset := 0; offset < len(img); offset += size {
		end := min(offset+size, len(img))
		if err := d.sendBulk(img[offset:end]); err != nil {
			return fmt.Errorf("send chunk at %d: %w", offset, err)
		}
	}
	return nil
}

func (d *rtcDispatch) bulkError(msg string) {
	b, err := json.Marshal(bulkErr{Type: "error", Msg: msg})
	if err != nil || d.sendBulkText == nil {
		return
	}
	if err := d.sendBulkText(string(b)); err != nil {
		log.Printf("screenshot: sending error envelope %q failed: %v", msg, err)
	}
}
