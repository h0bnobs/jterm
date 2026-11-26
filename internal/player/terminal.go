package player

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/mpv"
)

const (
	footerIPCTimeout = 400 * time.Millisecond
	footerTick       = 250 * time.Millisecond
	// Clears what is visible but leaves the terminal's scrollback alone.
	clearScreen = "\x1b[H\x1b[2J"
)

// SubCache remembers each item's external subtitle URLs so they are fetched
// once, not on every relaunch.
type SubCache struct {
	mu   sync.Mutex
	urls map[string][]string
}

// For returns the external subtitle delivery URLs for an item. Embedded
// subtitles already travel in the direct-play container, so only these need
// adding to mpv as side-loaded tracks. A failed lookup is not cached.
func (c *SubCache) For(jf *jellyfin.Client, item jellyfin.Item) []string {
	if item.ID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if urls, ok := c.urls[item.ID]; ok {
		return urls
	}
	subs, err := jf.ExternalSubtitles(item.ID)
	if err != nil {
		return nil
	}
	urls := make([]string, 0, len(subs))
	for _, s := range subs {
		urls = append(urls, s.URL)
	}
	if c.urls == nil {
		c.urls = map[string][]string{}
	}
	c.urls[item.ID] = urls
	return urls
}

// Request is everything one in-terminal playback needs.
type Request struct {
	JF         *jellyfin.Client
	Item       jellyfin.Item
	StartTicks int64
	Audio      bool // audio only: no video, no footer
	Cap        config.QualityCap
	MPV        mpv.Options
	Subs       *SubCache
}

// Result is what a playback leaves behind for the browser.
type Result struct {
	// PlayedIDs is every item that was started, in order, so their
	// progress can be refreshed.
	PlayedIDs []string
	// Cap is the quality cap in force at the end; the user may have
	// changed it, or a refused transcode may have reset it to source.
	Cap config.QualityCap
}

// Terminal plays one request with mpv in the current terminal. It is handed
// to tea.Exec, which releases the terminal, calls Run and takes the
// terminal back when Run returns. Run blocks the UI's event loop for the
// whole playback, so it shares nothing with the model: everything it needs
// is in the Request and everything it reports is in the Result.
type Terminal struct {
	req  Request
	res  Result
	sock string
	in   *os.File
	out  *os.File
}

// NewTerminal prepares a playback.
func NewTerminal(req Request) *Terminal {
	if req.Subs == nil {
		req.Subs = &SubCache{}
	}
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("jterm-mpv-%d-%s.sock", os.Getpid(), config.NewID()[:8]))
	return &Terminal{req: req, res: Result{Cap: req.Cap}, sock: sock, in: os.Stdin, out: os.Stdout}
}

// Result is valid once Run has returned.
func (t *Terminal) Result() Result { return t.res }

// SetStdin, SetStdout and SetStderr receive the terminal from tea.Exec.
// mpv must be given real file descriptors: through a pipe it would not see
// a tty, and could neither read keys nor size the video.
func (t *Terminal) SetStdin(r io.Reader) {
	if f, ok := r.(*os.File); ok {
		t.in = f
	}
}

func (t *Terminal) SetStdout(w io.Writer) {
	if f, ok := w.(*os.File); ok {
		t.out = f
	}
}

func (t *Terminal) SetStderr(io.Writer) {}

// Run plays the request and returns when mpv has exited for good.
func (t *Terminal) Run() error {
	_, _ = io.WriteString(t.out, clearScreen)
	defer func() {
		// Leave a clean screen behind, and drop any keys still queued for
		// mpv so a second press of q does not go on to quit the browser.
		_, _ = io.WriteString(t.out, "\x1b[0m"+clearScreen)
		mpv.FlushInput()
	}()
	defer os.Remove(t.sock)
	if t.req.Audio {
		return t.runAudio()
	}
	return t.runVideo()
}

func (t *Terminal) command(args []string) *exec.Cmd {
	cmd := exec.Command(t.req.MPV.Bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = t.in, t.out, os.Stderr
	return cmd
}

func (t *Terminal) note(msg string) {
	cols, lines := mpv.TermSize()
	mpv.DrawFooter(t.out, mpv.Note(msg), lines, cols)
}

// -- audio --------------------------------------------------------------------

// runAudio plays sound only. With no video to overdraw, a static banner
// plus mpv's own status line is the right tool, so there is no footer loop.
func (t *Terminal) runAudio() error {
	item, start := t.req.Item, t.req.StartTicks
	desc := "audio only"
	if start > 0 {
		desc += " │ resuming from " + mpv.Clock(float64(start)/jellyfin.TicksPerSecond)
	}
	cols, _ := mpv.TermSize()
	bar := strings.Repeat("─", min(cols-1, 110))
	for _, line := range []string{
		"▶ " + item.DisplayTitle(), bar,
		" q quit to browser │ space pause │ ←/→ seek 5s │ ↑/↓ volume │ m mute │ [ ] speed",
		" " + desc, bar,
	} {
		fmt.Fprintln(t.out, ansi.Truncate(line, cols-1, ""))
	}
	url := t.req.JF.StreamURL(item.ID, 0, 0, 0, "")
	cmd := t.command(t.req.MPV.Audio(t.sock, url, start/jellyfin.TicksPerSecond))
	rep := StartReporter(t.req.JF, item, t.sock, start, 0, jellyfin.DirectPlay, "")
	defer rep.Stop()
	t.res.PlayedIDs = append(t.res.PlayedIDs, item.ID)
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		return err // mpv could not be started at all
	}
	return nil
}

// -- video --------------------------------------------------------------------

// instance is one running mpv together with the clients attached to it.
type instance struct {
	cmd    *exec.Cmd
	exited chan struct{} // closed once the process has been reaped
	ipc    *mpv.Conn     // nil if the socket never appeared
	rep    *Reporter
	// offset is how far mpv's clock is behind the item's, in seconds: a
	// mid-item transcode starts its clock at zero.
	offset     float64
	launchedAt time.Time
}

func (i *instance) hasExited() bool {
	select {
	case <-i.exited:
		return true
	default:
		return false
	}
}

// wait sleeps for d or until mpv exits, whichever is first.
func (i *instance) wait(d time.Duration) {
	select {
	case <-i.exited:
	case <-time.After(d):
	}
}

func (i *instance) failed() bool {
	return i.cmd.ProcessState == nil || i.cmd.ProcessState.ExitCode() != 0
}

// launch starts mpv on item at posTicks under the given cap, with a fresh
// reporter and play session.
func (t *Terminal) launch(item jellyfin.Item, qcap config.QualityCap, posTicks int64) (*instance, error) {
	_ = os.Remove(t.sock)
	t.note("loading… (" + qcap.Label() + ")")
	jf := t.req.JF
	sessionID := config.NewID()
	subs := t.req.Subs.For(jf, item)

	var url, method string
	var startSeconds, offsetTicks int64
	if qcap == config.Source {
		url, method = jf.StreamURL(item.ID, 0, 0, 0, ""), jellyfin.DirectPlay
		startSeconds = posTicks / jellyfin.TicksPerSecond
	} else {
		// The server starts the transcode at posTicks, so mpv itself
		// starts from zero and the difference is carried as an offset.
		url, method = jf.StreamURL(item.ID, int(qcap), qcap.Bitrate(), posTicks, sessionID), jellyfin.Transcode
		offsetTicks = posTicks
	}
	_, lines := mpv.TermSize()
	inst := &instance{
		cmd:    t.command(t.req.MPV.Video(t.sock, url, startSeconds, lines, subs)),
		exited: make(chan struct{}),
		offset: float64(offsetTicks) / jellyfin.TicksPerSecond,
	}
	if err := inst.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = inst.cmd.Wait()
		close(inst.exited)
	}()
	inst.rep = StartReporter(jf, item, t.sock, posTicks, offsetTicks, method, sessionID)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !inst.hasExited(); {
		if c, err := mpv.Dial(t.sock); err == nil {
			inst.ipc = c
			break
		}
		inst.wait(150 * time.Millisecond)
	}
	inst.launchedAt = time.Now()
	return inst, nil
}

// shutdown ends an instance: optionally asks mpv to exit, waits for it,
// then stops the reporter so the final position reaches the server. A
// transcode's server-side job is cancelled too.
func (t *Terminal) shutdown(inst *instance, terminate bool) {
	if inst.ipc != nil {
		inst.ipc.Close()
	}
	if terminate && !inst.hasExited() {
		_ = inst.cmd.Process.Signal(syscall.SIGTERM)
	}
	inst.wait(5 * time.Second)
	if !inst.hasExited() {
		if terminate {
			_ = inst.cmd.Process.Kill()
		} else {
			_ = inst.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	inst.rep.Stop()
	if inst.rep.PlayMethod == jellyfin.Transcode {
		_ = t.req.JF.StopEncoding(inst.rep.SessionID)
	}
}

// runVideo launches mpv on the item and paints the live control footer
// until it exits. The footer loop and the Reporter are independent clients
// of the same mpv IPC socket: the loop owns the process and the screen, the
// reporter mirrors progress to the server. Ctrl+Up/Down requests forwarded
// by mpv reload the stream at a new quality cap, preserving the position;
// n/b reload on the neighbouring episode. Each (re)launch gets a fresh
// reporter, so resume positions and watched state stay correct throughout.
func (t *Terminal) runVideo() error {
	current := t.req.Item // rebound by the manual n/b episode keys
	qcap := t.req.Cap
	t.res.PlayedIDs = append(t.res.PlayedIDs, current.ID)

	inst, err := t.launch(current, qcap, t.req.StartTicks)
	if err != nil {
		return err
	}
	defer func() { t.shutdown(inst, false) }()
	// relaunch replaces the running mpv with a new one elsewhere: another
	// position, cap or episode.
	relaunch := func(item jellyfin.Item, qcap config.QualityCap, posTicks int64) error {
		t.shutdown(inst, true)
		next, err := t.launch(item, qcap, posTicks)
		if err != nil {
			return err
		}
		inst = next
		return nil
	}

	lastLines := -1
	for {
		if inst.hasExited() {
			// A transcode that died straight away: the server refused
			// or cannot keep up, so drop back to source.
			if qcap != config.Source && inst.failed() && time.Since(inst.launchedAt) < 8*time.Second {
				pos := inst.rep.LastTicks()
				t.note("transcode failed — back to source quality")
				time.Sleep(1500 * time.Millisecond)
				qcap = config.Source
				t.res.Cap = qcap
				if err := relaunch(current, qcap, pos); err != nil {
					return err
				}
				lastLines = -1
				continue
			}
			return nil
		}

		cols, lines := mpv.TermSize()
		var st mpv.State
		var req string
		if inst.ipc != nil {
			// Keep mpv's reserved bottom band exactly FooterRows tall as
			// the pane is resized, so the video can never creep over it.
			if lines != lastLines {
				inst.ipc.Command("set_property", "video-margin-ratio-bottom", mpv.MarginRatio(lines))
				lastLines = lines
			}
			st = mpv.ReadState(inst.ipc, footerIPCTimeout)
			if v, _ := inst.ipc.Get(mpv.ReqProp, footerIPCTimeout); v != nil {
				req, _ = v.(string)
			}
		}

		switch req {
		case "up", "down":
			inst.ipc.Command("set_property", mpv.ReqProp, "none")
			if next := qcap.Step(req == "up"); next != qcap {
				pos := inst.offset
				if st.TimePos != nil {
					pos += *st.TimePos
				}
				qcap = next
				t.res.Cap = qcap
				if err := relaunch(current, qcap, int64(pos*jellyfin.TicksPerSecond)); err != nil {
					return err
				}
				lastLines = -1
			}
			continue
		case "next", "prev":
			inst.ipc.Command("set_property", mpv.ReqProp, "none")
			t.note("looking up the episode…")
			direction, word := 1, "next"
			if req == "prev" {
				direction, word = -1, "previous"
			}
			neighbour, ok := t.req.JF.AdjacentEpisode(current, direction)
			if !ok {
				t.note("no " + word + " episode")
				time.Sleep(1200 * time.Millisecond)
				continue
			}
			current = neighbour
			t.res.PlayedIDs = append(t.res.PlayedIDs, current.ID)
			if err := relaunch(current, qcap, current.ResumeTicks()); err != nil {
				return err
			}
			lastLines = -1
			continue
		}

		// A transcode plays a partial, clock-rebased stream: shift the
		// position and take duration and percent from the item itself.
		runtime := current.RuntimeSeconds()
		transcoding := qcap != config.Source
		if st.TimePos != nil {
			pos := *st.TimePos + inst.offset
			st.TimePos = &pos
		}
		if runtime > 0 && (transcoding || st.Duration == 0) {
			st.Duration = runtime
		}
		if runtime > 0 && st.TimePos != nil && (transcoding || st.PercentPos == nil) {
			pct := *st.TimePos * 100 / runtime
			st.PercentPos = &pct
		}
		st.CapLabel = qcap.Label()
		mpv.DrawFooter(t.out, mpv.FooterLines(st, current.DisplayTitle()), lines, cols)
		inst.wait(footerTick)
	}
}
