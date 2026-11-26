package player

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/mpv"
)

// Window is a single reusable mpv window driven over IPC, so the browser
// stays interactive while a video plays. Its methods block while they talk
// to mpv, so the UI calls them from background commands; a mutex keeps two
// such calls from overlapping.
type Window struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
	sock   string
	title  string
	rep    *WindowReporter
}

func (w *Window) running() bool {
	if w.cmd == nil {
		return false
	}
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}

// NowPlaying is the title last loaded into the window, or empty when no
// window is open.
func (w *Window) NowPlaying() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running() {
		return ""
	}
	return w.title
}

// Play shows item in the window and returns a line for the status bar. If a
// window is already open it is told to replace what it is playing, or with
// enqueue to play this after it; otherwise a window is opened. The
// WindowReporter follows whichever tracked item is actually playing.
func (w *Window) Play(jf *jellyfin.Client, opts mpv.Options, item jellyfin.Item, startTicks int64, enqueue bool) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	title := item.DisplayTitle()
	url := jf.DirectURL(item.ID)
	if w.running() {
		// tell the reporter first, so it knows the URL the moment mpv
		// switches to it
		w.rep.Track(url, item, startTicks)
		if w.loadFile(url, title, startTicks, enqueue) {
			verb := "now playing"
			if enqueue {
				verb = "queued"
			} else {
				w.title = title
			}
			return fmt.Sprintf("%s in window: %s — browsing stays live", verb, firstRunes(title, 55))
		}
		// the window could not be reached: start afresh
		w.rep.Stop()
	}
	if err := w.spawn(jf, opts, item, url, title, startTicks); err != nil {
		return fmt.Sprintf("could not open the window player: %v", err)
	}
	return fmt.Sprintf("now playing in window: %s — browsing stays live", firstRunes(title, 55))
}

// loadFile tells the running window to load a URL. It reports false if the
// window could not be reached, so the caller can open a fresh one.
func (w *Window) loadFile(url, title string, startTicks int64, enqueue bool) bool {
	mode := "replace"
	if enqueue {
		mode = "append-play"
	}
	// the socket may lag a just-spawned mpv
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		c, err := mpv.Dial(w.sock)
		if err != nil {
			continue
		}
		args := []any{"loadfile", url, mode}
		if startTicks > 0 {
			// the third argument is the options string on mpv <= 0.37
			args = append(args, fmt.Sprintf("start=%d", startTicks/jellyfin.TicksPerSecond))
		}
		c.Command(args...)
		if !enqueue {
			c.Command("set_property", "title", mpv.WindowTitle(title))
		}
		// Commands are processed in order per connection, so a round trip
		// ensures they all landed before we disconnect: mpv drops
		// still-queued lines when the client goes away.
		_, _ = c.Get("pid", time.Second)
		c.Close()
		return true
	}
	return false
}

func (w *Window) spawn(jf *jellyfin.Client, opts mpv.Options, item jellyfin.Item, url, title string, startTicks int64) error {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("jterm-window-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	cmd := exec.Command(opts.Bin, opts.Window(sock, url, title, startTicks/jellyfin.TicksPerSecond)...)
	// its own session, and no terminal: the window must not fight the
	// browser for the tty or die with a Ctrl+C aimed at it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	w.cmd, w.exited, w.sock, w.title = cmd, exited, sock, title
	w.rep = NewWindowReporter(jf, sock)
	w.rep.Track(url, item, startTicks)
	w.rep.Start()
	return nil
}

// Stop closes the window, after the reporter has told the server where
// playback got to, and returns a line for the status bar.
func (w *Window) Stop() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running() {
		w.cmd, w.rep, w.title = nil, nil, ""
		return "no window player is running"
	}
	w.rep.Stop()
	if c, err := mpv.Dial(w.sock); err == nil {
		c.Command("quit")
		defer c.Close()
	}
	select {
	case <-w.exited:
	case <-time.After(3 * time.Second):
		_ = w.cmd.Process.Signal(syscall.SIGTERM)
	}
	_ = os.Remove(w.sock)
	w.cmd, w.rep, w.title = nil, nil, ""
	return "stopped the window player"
}

func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
