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

// bulkFrameMax is the hard ceiling on EVERY frame written to "bulk", text or
// binary. The transfer must fit a single SCTP packet: on an IPv6 path (Tailscale,
// native cellular — 1280-byte minimum link MTU, no in-network fragmentation,
// PMTUD typically filtered) any bulk message pion splits across more than one
// SCTP packet black-holes. pion fragments at its fixed ~1200-byte SCTP MTU, the
// resulting IPv6+UDP+DTLS packet exceeds 1280, routers drop it silently, and
// SCTP retransmits the same oversized chunk forever. Small single-packet frames
// (hello, quality echo) get through, so the peer looks healthy while a multi-
// packet reply never lands and the client spins forever (issue #3). IPv4 hid
// this (in-network fragmentation reassembles). pion exposes no SCTP-MTU knob, so
// the fix is app-level: never put more than one packet on the channel. 1024
// leaves margin under 1280 for the UDP/IPv6/DTLS/SCTP headers.
const bulkFrameMax = 1024

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

// bulkSim is one simulator in the "sims" reply. Only the four fields a client
// actually renders travel the wire; model, architecture and type are dropped to
// keep the reply small (issue #3). The reply is a bare JSON array of these,
// delivered chunked like the screenshot (header + binary chunks) rather than as
// one text frame, so no frame exceeds a single packet.
type bulkSim struct {
	UDID      string `json:"udid"`
	Name      string `json:"name"`
	OSVersion string `json:"os_version"`
	State     string `json:"state"`
}

// bulkQuality echoes the quality that actually took effect, after unset fields
// were defaulted and out-of-range ones clamped — otherwise a client whose
// request was clamped would render a preset the daemon never applied.
type bulkQuality struct {
	Type string `json:"type"` // always "quality"
	QualityOpts
}

// bulkHeader announces a chunked transfer: the binary chunks that follow it
// concatenate to exactly Bytes bytes and are parsed per Type — "screenshot" → a
// PNG, "sims" → the JSON simulator array. The client needs the total because the
// channel gives it no other way to know when the last chunk has landed.
type bulkHeader struct {
	Type  string `json:"type"` // "screenshot" | "sims"
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
// reply cannot be silently dropped on a cellular/relay path (issue #2), and it
// is chunked like the screenshot so no frame exceeds one packet (issue #3).
func (d *rtcDispatch) doList() {
	sims, err := d.backend.List(d.baseCtx)
	if err != nil {
		d.bulkError(CodeListFailed, err.Error())
		return
	}
	wire := make([]bulkSim, len(sims))
	for i, s := range sims {
		wire[i] = bulkSim{UDID: s.UDID, Name: s.Name, OSVersion: s.OSVersion, State: s.State}
	}
	b, err := json.Marshal(wire)
	if err != nil {
		log.Printf("list: marshaling sims reply failed: %v", err)
		return
	}
	if err := d.sendChunked("sims", b); err != nil {
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
	if err := d.sendChunked("screenshot", img); err != nil {
		log.Printf("screenshot: sending %d bytes failed: %v", len(img), err)
		d.bulkError(CodeCaptureFailed, fmt.Sprintf("send failed: %v", err))
		return
	}
	log.Printf("screenshot: sent %d bytes in %v", len(img), time.Since(started))
}

// chunkSize is the binary frame size for one transfer: bulkFrameMax, the
// one-packet ceiling (issue #3), lowered only if the peer somehow negotiated a
// max message size smaller still. SCTP never settles that low in practice, but
// the min() is free insurance against a Send the peer would reject outright.
func (d *rtcDispatch) chunkSize() int {
	size := bulkFrameMax
	if d.bulkMaxMsg != nil {
		if negotiated := d.bulkMaxMsg(); negotiated > 0 && negotiated < size {
			size = negotiated
		}
	}
	return size
}

// sendChunked streams payload as a text header announcing its byte count and
// parse type, then binary chunks each within bulkFrameMax. The channel is
// reliable and ordered, so the chunks need no sequence numbers — the client
// appends until it holds the announced total, then parses per typ. Every bulk
// reply too large for one packet goes out this way: the screenshot PNG and the
// sims JSON.
func (d *rtcDispatch) sendChunked(typ string, payload []byte) error {
	if d.sendBulk == nil || d.sendBulkText == nil {
		return errors.New("bulk channel not wired")
	}
	header, err := json.Marshal(bulkHeader{Type: typ, Bytes: len(payload)})
	if err != nil {
		return fmt.Errorf("encode header: %w", err)
	}
	if err := d.sendBulkText(string(header)); err != nil {
		return fmt.Errorf("send header: %w", err)
	}
	size := d.chunkSize()
	for offset := 0; offset < len(payload); offset += size {
		end := min(offset+size, len(payload))
		if err := d.sendBulk(payload[offset:end]); err != nil {
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
