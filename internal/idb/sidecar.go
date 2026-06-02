package idb

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Sidecar is a spawned `idb_companion --udid X --grpc-port N` process plus a
// gRPC connection to it.
type Sidecar struct {
	cmd    *exec.Cmd
	port   int
	conn   *grpc.ClientConn
	client *Client
}

// Spawn launches an idb_companion sidecar for udid on a free port and blocks
// until its gRPC server answers a describe call (readiness), or fails.
func Spawn(ctx context.Context, bin, udid string) (*Sidecar, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick port: %w", err)
	}
	cmd := exec.Command(bin, "--udid", udid, "--grpc-port", fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start idb_companion: %w", err)
	}
	sc := &Sidecar{cmd: cmd, port: port}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		sc.Close()
		return nil, fmt.Errorf("grpc client: %w", err)
	}
	sc.conn = conn
	sc.client = NewClient(conn)

	ready, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if _, derr := sc.client.Describe(ready); derr == nil {
			return sc, nil
		}
		select {
		case <-ready.Done():
			sc.Close()
			// Wrap ready.Err() so callers can distinguish a readiness timeout
			// (DeadlineExceeded) from explicit parent cancellation (Canceled).
			return nil, fmt.Errorf("sidecar for %s not ready: %w", udid, ready.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Client returns the gRPC client for this sidecar.
func (s *Sidecar) Client() *Client { return s.client }

// Close terminates the gRPC connection and kills the idb_companion process.
func (s *Sidecar) Close() error {
	if s.conn != nil {
		s.conn.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait() // reap the process; returns "signal: killed"
	}
	return nil
}
