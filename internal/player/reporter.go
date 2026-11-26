// Package player runs mpv for jterm: in the terminal under a live control
// footer, as audio only, or in a reusable window, and in every case mirrors
// what is playing back to Jellyfin.
package player

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/mpv"
)

// progressInterval is how often the position is sent to the server, the
// same cadence as the official clients. A variable so tests can shorten it.
var progressInterval = 5 * time.Second

const reporterIPCTimeout = 2 * time.Second

// halter is the stop-and-wait half of a background reporter.
type halter struct {
	halt chan struct{}
	done chan struct{}
	once sync.Once
}

func newHalter() halter {
	return halter{halt: make(chan struct{}), done: make(chan struct{})}
}

// Stop asks the reporter to finish and waits for its final report to the
// server, but not for ever: a dead network must not hang the UI.
func (h *halter) Stop() {
	h.once.Do(func() { close(h.halt) })
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
	}
}

func (h *halter) halted() bool {
	select {
	case <-h.halt:
		return true
	default:
		return false
	}
}

// pause waits for d, returning false if the reporter was stopped meanwhile.
func (h *halter) pause(d time.Duration) bool {
	select {
	case <-h.halt:
		return false
	case <-time.After(d):
		return true
	}
}

// connect dials mpv's IPC socket. mpv creates it shortly after launch, so
// this retries for a while.
func (h *halter) connect(sock string) *mpv.Conn {
	for range 120 {
		if h.halted() {
			return nil
		}
		if c, err := mpv.Dial(sock); err == nil {
			return c
		}
		if !h.pause(250 * time.Millisecond) {
			return nil
		}
	}
	return nil
}

// Reporter connects to mpv's IPC socket and mirrors one item's playback to
// Jellyfin: Playing once, Progress every few seconds, Stopped at the end.
// That is what keeps resume positions and watched state in sync with the
// rest of the Jellyfin ecosystem. Failed reports are dropped: a network
// blip must never interrupt playback.
type Reporter struct {
	halter
	jf         *jellyfin.Client
	item       jellyfin.Item
	sock       string
	SessionID  string
	PlayMethod string
	// A transcode started mid-item has its clock rebased to zero, so mpv's
	// time-pos is offset behind the true position.
	offset int64
	last   atomic.Int64
}

// StartReporter begins reporting item as playing from startTicks. An empty
// sessionID gets a fresh one.
func StartReporter(jf *jellyfin.Client, item jellyfin.Item, sock string,
	startTicks, offsetTicks int64, playMethod, sessionID string) *Reporter {
	if sessionID == "" {
		sessionID = config.NewID()
	}
	r := &Reporter{halter: newHalter(), jf: jf, item: item, sock: sock,
		SessionID: sessionID, PlayMethod: playMethod, offset: offsetTicks}
	r.last.Store(startTicks)
	go r.run()
	return r
}

// LastTicks is the most recent position seen, in the item's own timeline.
func (r *Reporter) LastTicks() int64 { return r.last.Load() }

func (r *Reporter) run() {
	defer close(r.done)
	c := r.connect(r.sock)
	if c == nil {
		return
	}
	_ = r.jf.ReportStart(r.item.ID, r.SessionID, r.LastTicks(), r.PlayMethod)
	for !r.halted() {
		pos, err := c.Get("time-pos", reporterIPCTimeout)
		if err != nil {
			break // mpv has gone
		}
		paused, err := c.Get("pause", reporterIPCTimeout)
		if err != nil {
			break
		}
		if p, ok := mpv.Float(pos); ok && p > 0 {
			r.last.Store(r.offset + int64(p*jellyfin.TicksPerSecond))
		}
		_ = r.jf.ReportProgress(r.item.ID, r.SessionID, r.LastTicks(), mpv.Truthy(paused), r.PlayMethod)
		if !r.pause(progressInterval) {
			break
		}
	}
	c.Close()
	_ = r.jf.ReportStopped(r.item.ID, r.SessionID, r.LastTicks())
}

// WindowReporter is the reporter for the reusable window player.
//
// The window outlives any single item (o replaces, e enqueues, the playlist
// advances by itself), so this follows whatever mpv is actually playing:
// each poll reads the current path, and when it moves to a different tracked
// item the old one is reported stopped and the new one started under a fresh
// play session.
type WindowReporter struct {
	halter
	jf   *jellyfin.Client
	sock string

	mu      sync.Mutex
	tracked map[string]trackedItem // stream URL -> what it is

	current   *jellyfin.Item
	sessionID string
	last      int64
}

type trackedItem struct {
	item       jellyfin.Item
	startTicks int64
}

// NewWindowReporter prepares a reporter for the mpv window on sock. Track
// the first item, then call Start.
func NewWindowReporter(jf *jellyfin.Client, sock string) *WindowReporter {
	return &WindowReporter{halter: newHalter(), jf: jf, sock: sock, tracked: map[string]trackedItem{}}
}

// Start begins following the window.
func (w *WindowReporter) Start() { go w.run() }

// Track tells the reporter which item a stream URL belongs to, so it can
// recognise it when the window starts playing it.
func (w *WindowReporter) Track(url string, item jellyfin.Item, startTicks int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tracked[url] = trackedItem{item, startTicks}
}

func (w *WindowReporter) lookup(path any) (trackedItem, bool) {
	url, _ := path.(string)
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.tracked[url]
	return t, ok
}

func (w *WindowReporter) run() {
	defer close(w.done)
	c := w.connect(w.sock)
	if c == nil {
		return
	}
	for !w.halted() {
		path, err := c.Get("path", reporterIPCTimeout)
		if err != nil {
			break
		}
		pos, err := c.Get("time-pos", reporterIPCTimeout)
		if err != nil {
			break
		}
		paused, err := c.Get("pause", reporterIPCTimeout)
		if err != nil {
			break
		}
		entry, known := w.lookup(path)
		switch {
		case !known:
			w.switchTo(nil, 0)
		case w.current == nil || w.current.ID != entry.item.ID:
			w.switchTo(&entry.item, entry.startTicks)
		}
		if w.current != nil {
			if p, ok := mpv.Float(pos); ok && p > 0 {
				w.last = int64(p * jellyfin.TicksPerSecond)
			}
			_ = w.jf.ReportProgress(w.current.ID, w.sessionID, w.last, mpv.Truthy(paused), jellyfin.DirectPlay)
		}
		if !w.pause(progressInterval) {
			break
		}
	}
	c.Close()
	w.switchTo(nil, 0)
}

// switchTo ends the play session of whatever was playing and opens one for
// the new item, if any.
func (w *WindowReporter) switchTo(item *jellyfin.Item, startTicks int64) {
	if w.current != nil {
		_ = w.jf.ReportStopped(w.current.ID, w.sessionID, w.last)
	}
	w.current = item
	if item != nil {
		w.sessionID = config.NewID()
		w.last = startTicks
		_ = w.jf.ReportStart(item.ID, w.sessionID, startTicks, jellyfin.DirectPlay)
	}
}
