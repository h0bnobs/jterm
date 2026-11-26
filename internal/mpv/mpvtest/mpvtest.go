// Package mpvtest is a fake mpv IPC endpoint: a unix socket that answers
// get_property from a table and records every other command it is sent.
package mpvtest

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// Fake stands in for a running mpv.
type Fake struct {
	// Path is the socket to hand to mpv.Dial.
	Path string

	ln       net.Listener
	mu       sync.Mutex
	props    map[string]any
	commands [][]any
	conns    []net.Conn
}

// Start listens on a fresh socket that is removed when the test ends.
func Start(t testing.TB, props map[string]any) *Fake {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{Path: path, ln: ln, props: map[string]any{}}
	for k, v := range props {
		f.props[k] = v
	}
	t.Cleanup(f.Close)
	go f.accept()
	return f
}

// Set changes a property; nil makes it unavailable.
func (f *Fake) Set(prop string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value == nil {
		delete(f.props, prop)
		return
	}
	f.props[prop] = value
}

// Commands returns every non-get_property command received so far.
func (f *Fake) Commands() [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]any(nil), f.commands...)
}

// Close shuts the socket and drops every client, as mpv exiting would.
func (f *Fake) Close() {
	_ = f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
}

func (f *Fake) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.serve(c)
	}
}

func (f *Fake) serve(c net.Conn) {
	// real mpv interleaves events with replies; make clients cope
	_, _ = c.Write([]byte(`{"event":"idle"}` + "\n"))
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var req struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.Command) == 0 {
			continue
		}
		resp := map[string]any{"request_id": req.RequestID, "error": "success"}
		f.mu.Lock()
		switch {
		case req.Command[0] == "get_property" && len(req.Command) == 2:
			name, _ := req.Command[1].(string)
			if v, ok := f.props[name]; ok {
				resp["data"] = v
			} else {
				resp["error"] = "property unavailable"
			}
		case req.Command[0] == "set_property" && len(req.Command) == 3:
			name, _ := req.Command[1].(string)
			f.props[name] = req.Command[2]
			f.commands = append(f.commands, req.Command)
		default:
			f.commands = append(f.commands, req.Command)
		}
		f.mu.Unlock()
		out, _ := json.Marshal(resp)
		_, _ = c.Write(append(out, '\n'))
	}
}
