package encoder

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// encodeStats gates the once-a-second frames-in-flight log (framesIn = PNGs fed
// to ffmpeg stdin, framesOut = access units emitted; the difference is what sits
// inside ffmpeg). This is the send-side instrumentation from
// docs/research/2026-06-08-latency-pipeline.md («Методика», п.3), kept behind an
// env var so latency work can re-measure without patching the encoder again.
var encodeStats = os.Getenv("SIMBEAM_ENCODER_STATS") != ""

// encoderName returns the H.264 encoder for this platform: hardware
// videotoolbox on macOS (the sim backend's home), software x264 everywhere else
// (the Linux demo daemon — no hardware encoder to rely on, and the demo's
// halved frame is cheap enough for x264 ultrafast).
func encoderName() string {
	if runtime.GOOS == "darwin" {
		return "h264_videotoolbox"
	}
	return "libx264"
}

// Available reports whether ffmpeg and this platform's H.264 encoder are present.
func Available() error {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH (try: brew install ffmpeg): %w", err)
	}
	out, err := exec.Command(path, "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	if !strings.Contains(string(out), encoderName()) {
		return fmt.Errorf("ffmpeg has no %s encoder", encoderName())
	}
	return nil
}

// ffmpegArgs builds the low-latency PNG→H.264 argv (see decision №37). The
// screenshot sources emit PNG, hence image2pipe/png input. -analyzeduration 0
// is critical: it removes the demuxer's startup backlog that otherwise pins ~3s
// of constant latency.
//
// scale downsizes each dimension by that factor: at 0.5 keyframes and per-frame
// payloads are ~4x smaller, cutting encode + transit latency, and the browser
// scales the H.264 back up in <video> (decision №40).
//
// The trunc(…/2)*2 width and -2 height are not decoration — they are what makes
// an arbitrary client-chosen factor (decision №88) safe. The scale filter does
// NOT round to even by itself: with a naive scale=iw*S:ih*S, an odd result is
// silently fixed up by h264_videotoolbox but makes libx264 refuse to open
// ("width not divisible by 2"), which would kill the Linux demo backend on any
// factor the old hardcoded 0.5 never produced. Verified against both encoders.
//
// At 1.0 the filter is omitted entirely rather than made an identity — a source
// already at target resolution should not pay for a scaler pass.
//
// Note this is NOT byte-identical to the old scale=iw/2:ih/2 on a source with an
// odd dimension: that halved each axis independently (1179x2556 → 588x1278),
// while deriving the height with -2 tracks the scaled width's aspect
// (1179x2556 → 588x1274). The new size is 4px shorter and its aspect is closer
// to the source's; both are cosmetic, since coordinates are normalized.
func ffmpegArgs(fps int, scale float64, bitrate int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "nobuffer", "-flags", "low_delay", "-analyzeduration", "0",
		"-f", "image2pipe", "-vcodec", "png", "-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		"-an",
	}
	if scale != 1 {
		// 'f' (never scientific) matters: %g would render a small factor as
		// "1e-05", which ffmpeg's expression parser does not accept. Today
		// Resolve's floor keeps scale well clear of that, but the filter should
		// not silently depend on a bound set two packages away.
		args = append(args, "-vf", "scale=trunc(iw*"+strconv.FormatFloat(scale, 'f', -1, 64)+"/2)*2:-2")
	}
	args = append(args, "-c:v", encoderName())
	// Low-latency knobs are encoder-specific; the shared flags above/below carry
	// the pipeline contract (short GOP, baseline profile, raw h264 out).
	if encoderName() == "h264_videotoolbox" {
		args = append(args, "-realtime", "1")
	} else {
		args = append(args, "-preset", "ultrafast", "-tune", "zerolatency")
	}
	return append(args,
		"-profile:v", "baseline",
		"-g", strconv.Itoa(fps*2), "-b:v", strconv.Itoa(bitrate), "-pix_fmt", "yuv420p",
		"-flush_packets", "1", "-max_delay", "0", "-f", "h264", "pipe:1",
	)
}

// Encode spawns ffmpeg, feeds PNG frames from png to its stdin, and emits one
// Frame per H.264 access unit on the returned channel until ctx is cancelled
// (then the channel closes and ffmpeg is killed via the command context).
//
// scale (0 < scale <= 1) downsizes each dimension; bitrate is the target in
// bits/s. Both are rejected outright when out of range rather than passed to
// ffmpeg: scale 0 builds a zero-width filter and ffmpeg dies at startup, which
// reaches the caller only as "ffmpeg exited unexpectedly" on a log line with no
// hint of the cause. Deciding the *default* for an unset value stays the
// backend's job (server.QualityOpts.Resolve) — this is only a precondition.
func Encode(ctx context.Context, png <-chan []byte, fps int, scale float64, bitrate int) (<-chan Frame, error) {
	if scale <= 0 || scale > 1 {
		return nil, fmt.Errorf("encode: scale %v out of range (0,1]", scale)
	}
	if bitrate <= 0 {
		return nil, fmt.Errorf("encode: bitrate %d must be positive", bitrate)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(fps, scale, bitrate)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	cmd.Stderr = nil // ffmpeg's own logging is suppressed via -loglevel warning in the argv
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	var framesIn, framesOut atomic.Int64
	if encodeStats {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					in, out := framesIn.Load(), framesOut.Load()
					log.Printf("encoder: stats in=%d out=%d in-flight=%d", in, out, in-out)
				}
			}
		}()
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
				framesIn.Add(1)
			}
		}
	}()

	frames := make(chan Frame)
	dur := time.Second / time.Duration(fps)
	go func() {
		defer close(frames)
		defer cmd.Wait()
		defer stdout.Close()
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
						framesOut.Add(1)
					case <-ctx.Done():
						return
					}
				}
			}
			if rerr != nil {
				if rerr != io.EOF && ctx.Err() == nil {
					// ffmpeg died unexpectedly; log it, then close the channel to tear the session down.
					log.Printf("encoder: ffmpeg exited unexpectedly: %v", rerr)
				}
				return
			}
		}
	}()
	return frames, nil
}
