package encoder

import (
	"strings"
	"testing"
)

func TestFFmpegArgs(t *testing.T) {
	got := strings.Join(ffmpegArgs(15), " ")
	for _, want := range []string{
		"-analyzeduration 0", encoderName(), "-g 30",
		"-framerate 15", "-f h264", "-flush_packets 1",
		"-vf scale=iw/2:ih/2", "-profile:v baseline",
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
		got := strings.Join(ffmpegArgs(tc.fps), " ")
		if !strings.Contains(got, tc.wantG) || !strings.Contains(got, tc.wantFR) {
			t.Fatalf("fps=%d: want %q and %q in: %s", tc.fps, tc.wantG, tc.wantFR, got)
		}
	}
}

func TestAvailableSkipIfMissing(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("ffmpeg/h264_videotoolbox not available: %v", err)
	}
}
