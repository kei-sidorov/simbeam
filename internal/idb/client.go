// Package idb is a gRPC client wrapper around a running idb_companion sidecar
// (idb_companion --udid X --grpc-port N). It exposes the minimal RPC surface
// simbeam needs: describe, screenshot, hid.
//
// Frames come from polling the screenshot RPC, not video_stream: on
// idb_companion 1.1.8 the MJPEG video stream emits only one frame for a static
// screen, and H264 (the one continuous format) is not directly renderable in an
// <img>. Continuous H264 over WebRTC is Phase 2.
package idb

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/kei-sidorov/simbeam/internal/idbpb"
	"google.golang.org/grpc"
)

// Screen holds the simulator screen geometry from describe.
// Note: TargetDescription also exposes a density (scale factor); it is omitted
// until coordinate scaling decides whether hid wants pixels or logical points.
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

// Screenshot captures a single frame and returns the raw image bytes (PNG or
// JPEG, per the companion's image_format).
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	resp, err := c.rpc.Screenshot(ctx, &idbpb.ScreenshotRequest{})
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	return resp.GetImageData(), nil
}

// ScreenshotStream polls Screenshot every interval and emits frames on the
// returned channel until ctx is cancelled (then the channel is closed).
// Transient screenshot errors are logged and skipped rather than ending the
// stream.
func (c *Client) ScreenshotStream(ctx context.Context, interval time.Duration) <-chan []byte {
	frames := make(chan []byte)
	go func() {
		defer close(frames)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				img, err := c.Screenshot(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return // shutting down
					}
					log.Printf("screenshot: %v", err)
					continue
				}
				if len(img) == 0 {
					continue
				}
				select {
				case frames <- img:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return frames
}

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

// Swipe drags from start to end over the given duration (seconds) as a single
// HID swipe event.
func (c *Client) Swipe(ctx context.Context, start, end Point, duration float64) error {
	return c.sendHID(ctx, &idbpb.HIDEvent{
		Event: &idbpb.HIDEvent_Swipe{Swipe: &idbpb.HIDEvent_HIDSwipe{
			Start:    &idbpb.Point{X: start.X, Y: start.Y},
			End:      &idbpb.Point{X: end.X, Y: end.Y},
			Duration: duration,
		}},
	})
}

// KeyPress presses and releases a USB HID key usage code. When shift is true
// the left-shift modifier (usage 225) is held around the key press.
func (c *Client) KeyPress(ctx context.Context, usage uint64, shift bool) error {
	var events []*idbpb.HIDEvent
	if shift {
		events = append(events, keyEvent(225, idbpb.HIDEvent_DOWN))
	}
	events = append(events,
		keyEvent(usage, idbpb.HIDEvent_DOWN),
		keyEvent(usage, idbpb.HIDEvent_UP),
	)
	if shift {
		events = append(events, keyEvent(225, idbpb.HIDEvent_UP))
	}
	return c.sendHID(ctx, events...)
}

func keyEvent(usage uint64, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return &idbpb.HIDEvent{
		Event: &idbpb.HIDEvent_Press{Press: &idbpb.HIDEvent_HIDPress{
			Action: &idbpb.HIDEvent_HIDPressAction{
				Action: &idbpb.HIDEvent_HIDPressAction_Key{
					Key: &idbpb.HIDEvent_HIDKey{Keycode: usage},
				},
			},
			Direction: dir,
		}},
	}
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
