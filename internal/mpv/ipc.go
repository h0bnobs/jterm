// Package mpv drives a running mpv over its JSON IPC socket, builds the
// command lines jterm launches it with, and paints the control footer that
// sits under in-terminal video. Nothing in here knows about Jellyfin.
package mpv

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

// ErrClosed means the IPC socket went away, which is how a client learns
// that mpv has exited.
var ErrClosed = errors.New("mpv ipc closed")

// Conn is a tiny JSON-IPC client for a running mpv (--input-ipc-server).
// mpv accepts any number of clients on one socket, so the footer loop and
// a progress reporter each hold their own Conn. A Conn is not safe for
// concurrent use.
type Conn struct {
	conn net.Conn
	buf  []byte
	rid  int
}

// Dial connects to mpv's IPC socket.
func Dial(path string) (*Conn, error) {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return nil, err
	}
	return &Conn{conn: c}, nil
}

type reply struct {
	RequestID int    `json:"request_id"`
	Error     string `json:"error"`
	Data      any    `json:"data"`
}

// Get reads one property. The value is nil when the property is
// unavailable or mpv did not answer within the timeout; the error is
// ErrClosed only when the socket itself has gone.
func (c *Conn) Get(prop string, timeout time.Duration) (any, error) {
	c.rid++
	rid := c.rid
	req, _ := json.Marshal(map[string]any{
		"command":    []any{"get_property", prop},
		"request_id": rid,
	})
	deadline := time.Now().Add(timeout)
	_ = c.conn.SetWriteDeadline(deadline)
	if _, err := c.conn.Write(append(req, '\n')); err != nil {
		return nil, ErrClosed
	}
	chunk := make([]byte, 65536)
	for {
		// Replies are interleaved with events and with answers to
		// fire-and-forget commands, so skip lines until ours turns up.
		for {
			i := bytes.IndexByte(c.buf, '\n')
			if i < 0 {
				break
			}
			line := c.buf[:i]
			c.buf = c.buf[i+1:]
			var msg reply
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			if msg.RequestID == rid {
				if msg.Error == "success" {
					return msg.Data, nil
				}
				return nil, nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		_ = c.conn.SetReadDeadline(deadline)
		n, err := c.conn.Read(chunk)
		c.buf = append(c.buf, chunk[:n]...)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, nil
			}
			return nil, ErrClosed
		}
	}
}

// Command fires a command without waiting for its reply.
func (c *Conn) Command(args ...any) {
	req, err := json.Marshal(map[string]any{"command": args})
	if err != nil {
		return
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.conn.Write(append(req, '\n'))
}

// Close drops the connection.
func (c *Conn) Close() {
	_ = c.conn.Close()
}

// Float converts a property value to a number, reporting whether it was one.
func Float(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// Truthy mirrors how the Python client read loosely typed properties:
// missing, false, zero and the empty string all count as "no".
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	}
	return true
}
