package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kei-sidorov/simbeam/internal/companion"
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

// bulkMsg is an inbound request on the reliable ordered "bulk" DataChannel:
// {"type":"list"} — the simulator list — {"type":"screenshot"}, a
// full-resolution capture of the currently attached simulator (no parameters) —
// or {"type":"quality","scale":…,"bitrate":…}, which re-encodes the live feed at
// a new quality.
//
// list rides "bulk" rather than "control" because control is unreliable
// (maxRetransmits: 0), and the sims reply is the largest, most critical control
// message: on a cellular/relay path it was dropped with no retransmission,
// hanging the list screen forever (issue #2). Quality rides bulk for the same
// reason — dropped on exactly the bad network that motivates lowering it
// (decision №88).
type bulkMsg struct {
	Type string `json:"type"` // list|screenshot|quality
	// QualityOpts carries quality's "scale"/"bitrate" (embedded → top-level
	// fields). Ignored by list and screenshot, which take no parameters.
	QualityOpts
}

// bulkSims is the simulator-list reply on "bulk", the reliable-channel home of
// what used to be control's "sims" reply. The JSON shape is unchanged — only the
// channel moved (issue #2).
type bulkSims struct {
	Type string                `json:"type"` // always "sims"
	Sims []companion.Simulator `json:"sims"`
}

// bulkQuality echoes the quality that actually took effect, after unset fields
// were defaulted and out-of-range ones clamped — otherwise a client whose
// request was clamped would render a preset the daemon never applied.
type bulkQuality struct {
	Type string `json:"type"` // always "quality"
	QualityOpts
}

// bulkHeader announces an image transfer: the binary chunks that follow
// concatenate to exactly Bytes bytes. The client needs the total because the
// channel gives it no other way to know when the last chunk has landed.
type bulkHeader struct {
	Type  string `json:"type"` // always "screenshot"
	Bytes int    `json:"bytes"`
}

// Error codes carried by bulkErr.Code alongside the human text, so a client can
// branch on a stable machine value instead of grepping the message (decision
// №80, same contract as signal.Msg).
//
// CodeUnknownType is load-bearing beyond mere hygiene: it is how a client
// detects a daemon too old to support a request. Probing with {"type":"quality"}
// BEFORE attaching costs nothing (no feed exists to rebuild) and separates
// "unsupported" from "unattached" — whereas probing with attach cannot work at
// all, since an old daemon silently ignores unknown JSON fields.
const (
	CodeUnknownType   = "unknown_type"   // this daemon has no such bulk request (i.e. it predates it)
	CodeBadRequest    = "bad_request"    // the request was not valid JSON
	CodeNoAttachment  = "no_attachment"  // nothing is attached to act on
	CodeCaptureFailed = "capture_failed" // the capture itself failed; the request was fine
	CodeListFailed    = "list_failed"    // enumerating the simulators failed; the request was fine
)

// bulkErr is the text error envelope sent back on "bulk" when a request cannot
// be satisfied.
type bulkErr struct {
	Type string `json:"type"` // always "error"
	Msg  string `json:"msg"`
	Code string `json:"code,omitempty"`
}

// handleBulk processes one inbound "bulk" message. It runs on pion's per-channel
// read goroutine (separate from control's), and the client keeps a single
// request in flight, so blocking here is fine and needs no id correlation. The
// contract demands the daemon always reply — image or text error — so every
// path below ends in a reply.
func (d *rtcDispatch) handleBulk(data []byte) {
	var m bulkMsg
	if err := json.Unmarshal(data, &m); err != nil {
		d.bulkError(CodeBadRequest, "bad bulk json")
		return
	}
	switch m.Type {
	case "list":
		d.doList()
	case "screenshot":
		d.doScreenshot()
	case "quality":
		d.doQuality(m.QualityOpts)
	default:
		d.bulkError(CodeUnknownType, fmt.Sprintf("unknown bulk type %q", m.Type))
	}
}

// doList answers a "list" request with the current simulator list. The client
// re-sends list until a sims reply lands, so this is idempotent — it just
// enumerates and replies every time. It rides "bulk" (reliable, ordered) so the
// reply cannot be silently dropped on a cellular/relay path (issue #2).
func (d *rtcDispatch) doList() {
	sims, err := d.backend.List(d.baseCtx)
	if err != nil {
		d.bulkError(CodeListFailed, err.Error())
		return
	}
	b, err := json.Marshal(bulkSims{Type: "sims", Sims: sims})
	if err != nil || d.sendBulkText == nil {
		return
	}
	if err := d.sendBulkText(string(b)); err != nil {
		log.Printf("list: sending sims reply failed: %v", err)
	}
}

// doQuality re-encodes the live feed at quality q and replies with what actually
// took effect. Nothing attached → error: the starting quality is attach's job,
// and there is no feed here to remember a setting for.
//
// Only the backend spawn runs on its own goroutine; the teardown and the
// generation claim happen here, synchronously. That split is deliberate:
// handleBulk's goroutine must stay free (doScreenshot can hold it for up to
// screenshotTimeout), but claiming the generation inside the goroutine would
// race a detach that lands before the scheduler runs it, and the rebuild would
// then install a feed the client already dismissed.
//
// The reply reports the requested-and-clamped quality rather than a completed
// attach; failures surface on control as attachAs's "error", the same as any
// other attach.
func (d *rtcDispatch) doQuality(q QualityOpts) {
	udid, gen, ok := d.restartAttachment()
	if !ok {
		d.bulkError(CodeNoAttachment, "no simulator attached")
		return
	}
	q = q.Resolve(d.backend.DefaultScale())
	go d.attachAs(udid, q, gen)

	b, err := json.Marshal(bulkQuality{Type: "quality", QualityOpts: q})
	if err != nil || d.sendBulkText == nil {
		return
	}
	if err := d.sendBulkText(string(b)); err != nil {
		log.Printf("quality: sending reply failed: %v", err)
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
		d.bulkError(CodeNoAttachment, "no simulator attached")
		return
	}
	ctx, cancel := context.WithTimeout(d.baseCtx, screenshotTimeout)
	defer cancel()
	started := time.Now()
	img, err := att.feed.Screenshot(ctx)
	if err != nil {
		log.Printf("screenshot: capture failed after %v: %v", time.Since(started), err)
		d.bulkError(CodeCaptureFailed, err.Error())
		return
	}
	if len(img) == 0 {
		log.Print("screenshot: capture returned no bytes")
		d.bulkError(CodeCaptureFailed, "capture returned no bytes")
		return
	}
	if err := d.sendImage(img); err != nil {
		log.Printf("screenshot: sending %d bytes failed: %v", len(img), err)
		d.bulkError(CodeCaptureFailed, fmt.Sprintf("send failed: %v", err))
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

// bulkError replies with the text error envelope. code is the stable value the
// client branches on; msg is for humans and may be reworded freely.
func (d *rtcDispatch) bulkError(code, msg string) {
	b, err := json.Marshal(bulkErr{Type: "error", Msg: msg, Code: code})
	if err != nil || d.sendBulkText == nil {
		return
	}
	if err := d.sendBulkText(string(b)); err != nil {
		log.Printf("bulk: sending error envelope %q failed: %v", msg, err)
	}
}
