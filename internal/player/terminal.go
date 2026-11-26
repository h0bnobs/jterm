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

// MediaCache remembers what each item's playback needs from the server (its
// media source id and external subtitle URLs) so it is fetched once, not on
// every relaunch.
type MediaCache struct {
	mu    sync.Mutex
	items map[string]jellyfin.Media
}

// For returns the media details for an item. A failed lookup is not cached
// and yields a usable default: no subtitles to side-load, and the item's
// own id as its media source, which is what it is for all but multi-version
// items.
func (c *MediaCache) For(jf *jellyfin.Client, item jellyfin.Item) jellyfin.Media {
	c.mu.Lock()
	defer c.mu.Unlock()
	if media, ok := c.items[item.ID]; ok {
		return media
	}
	media, err := jf.Media(item.ID)
	if err != nil {
		return jellyfin.Media{SourceID: item.ID}
	}
	if c.items == nil {
		c.items = map[string]jellyfin.Media{}
	}
	c.items[item.ID] = media
	return media
}

func subtitleURLs(media jellyfin.Media) []string {
	urls := make([]string, 0, len(media.Subtitles))
	for _, s := range media.Subtitles {
		urls = append(urls, s.URL)
	}
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
	Media      *MediaCache
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
	if req.Media == nil {
		req.Media = &MediaCache{}
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
	url := t.req.JF.DirectURL(item.ID)
	cmd := t.command(t.req.MPV.Audio(t.sock, url, start/jellyfin.TicksPerSecond))
	rep := StartReporter(t.req.JF, item, t.sock, start, jellyfin.DirectPlay, "")
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
	cmd        *exec.Cmd
	exited     chan struct{} // closed once the process has been reaped
	ipc        *mpv.Conn     // nil if the socket never appeared
	rep        *Reporter
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
	media := t.req.Media.For(jf, item)

	// Direct play and a transcode both expose the whole item on its real
	// timeline, so either way mpv is simply told where to start and its
	// clock is the item's clock.
	url, method := jf.DirectURL(item.ID), jellyfin.DirectPlay
	if qcap != config.Source {
		url = jf.TranscodeURL(item.ID, media.SourceID, int(qcap), qcap.Bitrate(), sessionID)
		method = jellyfin.Transcode
	}
	_, lines := mpv.TermSize()
	args := t.req.MPV.Video(t.sock, url, posTicks/jellyfin.TicksPerSecond, lines, subtitleURLs(media))
	inst := &instance{cmd: t.command(args), exited: make(chan struct{})}
	if err := inst.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = inst.cmd.Wait()
		close(inst.exited)
	}()
	inst.rep = StartReporter(jf, item, t.sock, posTicks, method, sessionID)
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
				// carry on from where the picture is, or failing that
				// from the last position the reporter saw
				pos := float64(inst.rep.LastTicks()) / jellyfin.TicksPerSecond
				if st.TimePos != nil {
					pos = *st.TimePos
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

		// Until mpv knows the length, show the one the server has.
		runtime := current.RuntimeSeconds()
		if runtime > 0 && st.Duration == 0 {
			st.Duration = runtime
		}
		if runtime > 0 && st.TimePos != nil && st.PercentPos == nil {
			pct := *st.TimePos * 100 / runtime
			st.PercentPos = &pct
		}
		st.CapLabel = qcap.Label()
		mpv.DrawFooter(t.out, mpv.FooterLines(st, current.DisplayTitle()), lines, cols)
		inst.wait(footerTick)
	}
}
