package encoder

import (
	"strings"
	"testing"
)

func TestFFmpegArgs(t *testing.T) {
	got := strings.Join(ffmpegArgs(15, 0.5, 8_000_000), " ")
	for _, want := range []string{
		"-analyzeduration 0", encoderName(), "-g 30",
		"-framerate 15", "-f h264", "-flush_packets 1",
		"-vf scale=trunc(iw*0.5/2)*2:-2", "-profile:v baseline",
		"-b:v 8000000",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ffmpegArgs missing %q in: %s", want, got)
		}
	}
}

func TestFFmpegArgsGOPCoupling(t *testing.T) {
	for _, tc := range []struct {
		fps    int
		wantG  string
		wantFR string
	}{
		{15, "-g 30", "-framerate 15"},
		{30, "-g 60", "-framerate 30"},
	} {
		got := strings.Join(ffmpegArgs(tc.fps, 0.5, 8_000_000), " ")
		if !strings.Contains(got, tc.wantG) || !strings.Contains(got, tc.wantFR) {
			t.Fatalf("fps=%d: want %q and %q in: %s", tc.fps, tc.wantG, tc.wantFR, got)
		}
	}
}

// -threads 1 must be an INPUT option (before -i pipe:0): it pins the
// frame-threaded PNG decoder to one thread, killing its ~N-1 frames of
// pipeline latency. After -i it would land on the encoder instead and the
// decoder would silently go back to auto threads.
func TestFFmpegArgsInputDecoderSingleThreaded(t *testing.T) {
	args := ffmpegArgs(15, 0.5, 8_000_000)
	threads, input := -1, -1
	for i, a := range args {
		if a == "-threads" && threads == -1 {
			threads = i
		}
		if a == "-i" && input == -1 {
			input = i
		}
	}
	if threads == -1 || args[threads+1] != "1" {
		t.Fatalf("missing -threads 1 in: %v", args)
	}
	if input == -1 || threads > input {
		t.Fatalf("-threads 1 must precede -i (input option), got: %v", args)
	}
}

func TestAvailableSkipIfMissing(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("ffmpeg/h264_videotoolbox not available: %v", err)
	}
}

// Scale 1.0 must omit the filter entirely rather than emit an identity scale —
// a source already at target resolution should not pay for a scaler pass.
func TestFFmpegArgsFullRes(t *testing.T) {
	got := strings.Join(ffmpegArgs(15, 1, 8_000_000), " ")
	if strings.Contains(got, "scale=") {
		t.Fatalf("scale=1 must not add a filter: %s", got)
	}
	if !strings.Contains(got, encoderName()) {
		t.Fatalf("missing encoder in: %s", got)
	}
}

// The scale filter is client-driven, so it must render a clean ffmpeg expression
// for any factor — no scientific notation, no trailing float noise — and always
// force even dimensions (yuv420p rejects odd ones).
func TestFFmpegArgsScaleFilter(t *testing.T) {
	for _, tc := range []struct {
		scale float64
		want  string
	}{
		{0.25, "-vf scale=trunc(iw*0.25/2)*2:-2"},
		{0.5, "-vf scale=trunc(iw*0.5/2)*2:-2"},
		{0.75, "-vf scale=trunc(iw*0.75/2)*2:-2"},
		{0.9, "-vf scale=trunc(iw*0.9/2)*2:-2"},
	} {
		got := strings.Join(ffmpegArgs(15, tc.scale, 8_000_000), " ")
		if !strings.Contains(got, tc.want) {
			t.Fatalf("scale=%v: want %q in: %s", tc.scale, tc.want, got)
		}
	}
}

func TestFFmpegArgsBitrate(t *testing.T) {
	for _, tc := range []struct {
		bitrate int
		want    string
	}{
		{500_000, "-b:v 500000"},
		{16_000_000, "-b:v 16000000"},
	} {
		got := strings.Join(ffmpegArgs(15, 0.5, tc.bitrate), " ")
		if !strings.Contains(got, tc.want) {
			t.Fatalf("bitrate=%d: want %q in: %s", tc.bitrate, tc.want, got)
		}
	}
}
