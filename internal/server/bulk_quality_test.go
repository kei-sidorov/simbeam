package server

import (
	"encoding/json"
	"testing"
	"time"
)

// applied returns the quality echoes among the sink's text frames — what the
// daemon says actually took effect.
func (s *bulkSink) applied() []bulkQuality {
	var out []bulkQuality
	for _, raw := range s.txt {
		var q bulkQuality
		if err := json.Unmarshal([]byte(raw), &q); err != nil || q.Type != "quality" {
			continue
		}
		out = append(out, q)
	}
	return out
}

// waitAttaches blocks until the backend has seen n attaches: doQuality
// re-attaches on its own goroutine so it never blocks the bulk channel.
func waitAttaches(t *testing.T, c *stubComp, n int) []QualityOpts {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q := c.qualities(); len(q) >= n {
			return q
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d attaches, got %d", n, len(c.qualities()))
	return nil
}

// The starting quality rides attach. An old client omits the fields entirely,
// which must land on the backend's defaults — i.e. today's behaviour.
func TestAttachCarriesQuality(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want QualityOpts
	}{
		{
			"explicit quality is honoured",
			`{"type":"attach","udid":"ABC","scale":0.75,"bitrate":2000000}`,
			QualityOpts{Scale: 0.75, Bitrate: 2_000_000},
		},
		{
			"old client without fields gets defaults",
			`{"type":"attach","udid":"ABC"}`,
			QualityOpts{Scale: stubDefaultScale, Bitrate: DefaultBitrate},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &stubComp{}
			d := newBulkDispatch(c, &bulkSink{})
			d.handle([]byte(tc.msg))

			got := c.qualities()
			if len(got) != 1 {
				t.Fatalf("want 1 attach, got %d", len(got))
			}
			// The stub records Attach's argument verbatim; filling unset fields
			// is the backend's job, so resolve to compare like for like.
			if r := got[0].Resolve(stubDefaultScale); r != tc.want {
				t.Fatalf("attach quality = %+v, want %+v", r, tc.want)
			}
		})
	}
}

// A mid-session change re-attaches the SAME device with the new quality and
// echoes what took effect.
func TestBulkQualityReattachesAndEchoes(t *testing.T) {
	c := &stubComp{}
	sink := &bulkSink{}
	d := newBulkDispatch(c, sink)
	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))

	d.handleBulk([]byte(`{"type":"quality","scale":0.25,"bitrate":1000000}`))

	got := waitAttaches(t, c, 2)
	if want := (QualityOpts{Scale: 0.25, Bitrate: 1_000_000}); got[1] != want {
		t.Fatalf("re-attach quality = %+v, want %+v", got[1], want)
	}
	if udids := c.attaches(); udids[1] != "ABC" {
		t.Fatalf("re-attach hit %q, want the attached device ABC", udids[1])
	}

	echo := sink.applied()
	if len(echo) != 1 {
		t.Fatalf("want 1 quality echo, got %d (%v)", len(echo), sink.txt)
	}
	if echo[0].Scale != 0.25 || echo[0].Bitrate != 1_000_000 {
		t.Fatalf("echo = %+v, want the applied quality", echo[0])
	}
}

// The echo must report the CLAMPED value, not the request — a client whose
// request was cut down would otherwise render a preset the daemon never applied.
func TestBulkQualityEchoesClampedValue(t *testing.T) {
	c := &stubComp{}
	sink := &bulkSink{}
	d := newBulkDispatch(c, sink)
	d.handle([]byte(`{"type":"attach","udid":"ABC"}`))

	d.handleBulk([]byte(`{"type":"quality","scale":9,"bitrate":999000000}`))
	got := waitAttaches(t, c, 2)

	if want := (QualityOpts{Scale: MaxScale, Bitrate: MaxBitrate}); got[1] != want {
		t.Fatalf("re-attach quality = %+v, want the clamped %+v", got[1], want)
	}
	echo := sink.applied()
	if len(echo) != 1 || echo[0].Scale != MaxScale || echo[0].Bitrate != MaxBitrate {
		t.Fatalf("echo = %+v, want scale=%v bitrate=%v", echo, MaxScale, MaxBitrate)
	}
}

// Unset fields in a quality request fall back to the backend default, same as
// attach — the request means "this quality", not "keep whatever you had".
func TestBulkQualityUnsetFieldsDefault(t *testing.T) {
	c := &stubComp{}
	sink := &bulkSink{}
	d := newBulkDispatch(c, sink)
	d.handle([]byte(`{"type":"attach","udid":"ABC","scale":0.25,"bitrate":1000000}`))

	d.handleBulk([]byte(`{"type":"quality"}`))
	got := waitAttaches(t, c, 2)

	if want := (QualityOpts{Scale: stubDefaultScale, Bitrate: DefaultBitrate}); got[1] != want {
		t.Fatalf("re-attach quality = %+v, want %+v", got[1], want)
	}
	if echo := sink.applied(); len(echo) != 1 || echo[0].Scale != stubDefaultScale {
		t.Fatalf("echo = %+v, want the default scale %v", echo, stubDefaultScale)
	}
}

// Quality without a live feed is an error, not a stored preference: the starting
// quality is attach's job and there is no feed here to re-encode.
func TestBulkQualityWithoutAttachErrors(t *testing.T) {
	c := &stubComp{}
	sink := &bulkSink{}
	d := newBulkDispatch(c, sink)

	d.handleBulk([]byte(`{"type":"quality","scale":0.5}`))

	if n := len(c.qualities()); n != 0 {
		t.Fatalf("want no attach, got %d", n)
	}
	if errs := sink.errors(); len(errs) != 1 {
		t.Fatalf("want 1 error envelope, got %v", sink.txt)
	}
}

// An old daemon replies "unknown bulk type" to a quality request, and that is the
// client's ONLY reliable way to detect that quality is unsupported — attach
// silently ignores unknown fields (encoding/json). Pin the shape.
func TestBulkUnknownTypeStillErrors(t *testing.T) {
	sink := &bulkSink{}
	d := newBulkDispatch(&stubComp{}, sink)

	d.handleBulk([]byte(`{"type":"nonsense"}`))

	errs := sink.errors()
	if len(errs) != 1 {
		t.Fatalf("want 1 error envelope, got %v", sink.txt)
	}
	if errs[0].Msg != `unknown bulk type "nonsense"` {
		t.Fatalf("msg = %q, want the unknown-type wording the client matches on", errs[0].Msg)
	}
}
