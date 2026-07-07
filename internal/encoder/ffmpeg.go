package encoder

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

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
// of constant latency. -vf scale=iw/2:ih/2 halves each dimension so keyframes
// and per-frame payloads are ~4x smaller, cutting encode + transit latency; the
// browser scales the H.264 back up in <video> (decision №40).
func ffmpegArgs(fps int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "nobuffer", "-flags", "low_delay", "-analyzeduration", "0",
		"-f", "image2pipe", "-vcodec", "png", "-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		"-an", "-vf", "scale=iw/2:ih/2",
		"-c:v", encoderName(),
	}
	// Low-latency knobs are encoder-specific; the shared flags above/below carry
	// the pipeline contract (short GOP, baseline profile, raw h264 out).
	if encoderName() == "h264_videotoolbox" {
		args = append(args, "-realtime", "1")
	} else {
		args = append(args, "-preset", "ultrafast", "-tune", "zerolatency")
	}
	return append(args,
		"-profile:v", "baseline",
		"-g", strconv.Itoa(fps*2), "-b:v", "8M", "-pix_fmt", "yuv420p",
		"-flush_packets", "1", "-max_delay", "0", "-f", "h264", "pipe:1",
	)
}

// Encode spawns ffmpeg, feeds PNG frames from png to its stdin, and emits one
// Frame per H.264 access unit on the returned channel until ctx is cancelled
// (then the channel closes and ffmpeg is killed via the command context).
func Encode(ctx context.Context, png <-chan []byte, fps int) (<-chan Frame, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(fps)...)
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
