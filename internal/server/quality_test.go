package server

import "testing"

// Zero fields mean "unset", which is exactly what an old client's attach (no
// scale/bitrate on the wire) unmarshals to. Resolving them MUST yield the values
// that used to be hardcoded, or upgrading the daemon silently re-tunes every
// existing client's stream.
//
// This pins the resolved values, not the resulting pixels: the scale filter
// changed shape at the same time, so an odd-dimension source lands within a
// couple of px of the old output rather than on it exactly (see encoder.Encode).
func TestResolveDefaultsMatchLegacyValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		defScale float64
		want     QualityOpts
	}{
		{"sim halves retina", 0.5, QualityOpts{Scale: 0.5, Bitrate: 8_000_000}},
		{"browser encodes 1:1", 1.0, QualityOpts{Scale: 1.0, Bitrate: 8_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (QualityOpts{}).Resolve(tc.defScale); got != tc.want {
				t.Fatalf("Resolve(%v) = %+v, want %+v", tc.defScale, got, tc.want)
			}
		})
	}
}

// Out-of-range values clamp rather than fail: a client asking for more than the
// daemon allows should get the daemon's best, not a broken attach.
func TestResolveClamps(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   QualityOpts
		want QualityOpts
	}{
		{"scale above max", QualityOpts{Scale: 4, Bitrate: 8_000_000}, QualityOpts{Scale: MaxScale, Bitrate: 8_000_000}},
		{"scale below min", QualityOpts{Scale: 0.01, Bitrate: 8_000_000}, QualityOpts{Scale: MinScale, Bitrate: 8_000_000}},
		{"bitrate above max", QualityOpts{Scale: 0.5, Bitrate: 999_000_000}, QualityOpts{Scale: 0.5, Bitrate: MaxBitrate}},
		{"bitrate below min", QualityOpts{Scale: 0.5, Bitrate: 1}, QualityOpts{Scale: 0.5, Bitrate: MinBitrate}},
		{"negative scale is unset, not clamped", QualityOpts{Scale: -1, Bitrate: 8_000_000}, QualityOpts{Scale: 0.5, Bitrate: 8_000_000}},
		{"in range passes through", QualityOpts{Scale: 0.75, Bitrate: 2_000_000}, QualityOpts{Scale: 0.75, Bitrate: 2_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Resolve(0.5); got != tc.want {
				t.Fatalf("Resolve(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// Resolve must be idempotent: the session layer resolves to build the client's
// echo and the backend resolves again defensively, so a double pass may not
// drift the values.
func TestResolveIdempotent(t *testing.T) {
	for _, in := range []QualityOpts{
		{},
		{Scale: 4, Bitrate: 999_000_000},
		{Scale: 0.75, Bitrate: 2_000_000},
	} {
		once := in.Resolve(0.5)
		if twice := once.Resolve(0.5); twice != once {
			t.Fatalf("Resolve(%+v): once=%+v twice=%+v", in, once, twice)
		}
	}
}
