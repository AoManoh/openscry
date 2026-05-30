package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"sync"
)

// Conn is a bidirectional, newline-delimited JSON-RPC message connection.
// Read returns one frame (without the trailing newline); Write sends one
// frame (appending a newline). Both honour ctx cancellation.
//
// The engine is the sole reader, but writes may come from multiple worker
// goroutines, so implementations MUST make Write safe for concurrent use.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, payload []byte) error
	Close() error
}

// stdioConn implements Conn over an io.Reader/io.Writer pair (typically
// os.Stdin/os.Stdout).
//
// A background goroutine performs the blocking line reads so Read can honour
// ctx cancellation: a raw bufio read on stdin cannot otherwise be
// interrupted. On shutdown the goroutine may remain parked in the final
// blocking read until the underlying stream closes; that is the inherent
// limitation of stdio and is reclaimed on process exit.
type stdioConn struct {
	lines   chan []byte
	readErr chan error

	wmu sync.Mutex
	w   io.Writer

	closeOnce sync.Once
	done      chan struct{}
}

func newStdioConn(r io.Reader, w io.Writer) *stdioConn {
	c := &stdioConn{
		lines:   make(chan []byte),
		readErr: make(chan error, 1),
		w:       w,
		done:    make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

func (c *stdioConn) readLoop(r io.Reader) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			frame := make([]byte, len(trimmed))
			copy(frame, trimmed)
			select {
			case c.lines <- frame:
			case <-c.done:
				return
			}
		}
		if err != nil {
			select {
			case c.readErr <- err:
			case <-c.done:
			}
			return
		}
	}
}

func (c *stdioConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, io.EOF
	case frame := <-c.lines:
		return frame, nil
	case err := <-c.readErr:
		return nil, err
	}
}

func (c *stdioConn) Write(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, payload...)
	frame = append(frame, '\n')

	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(frame)
	return err
}

func (c *stdioConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}
