package mpv

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
)

// In-terminal video layout: a fixed, coloured control footer that mpv is
// kept out of by a reserved bottom video margin.
const (
	FooterRows = 2
	footerBG   = "\x1b[48;2;40;46;66m" // subtle blue-grey, distinct from the bg
	footerFG   = "\x1b[38;2;236;236;245m"
	KeyHints   = "q quit · spc pause · ←/→ 5s · ↑/↓ vol · ⌃↑/↓ quality · n/b ep · v subs · j track · m mute"
)

// MarginRatio is the fraction of the video area to reserve so the footer's
// rows stay clear. It is recomputed from the live terminal height so a
// resize keeps it exact.
func MarginRatio(lines int) float64 {
	return math.Round(float64(FooterRows)/float64(max(lines, FooterRows+1))*1e4) / 1e4
}

// TermSize is the live terminal size straight from the tty ioctl. $LINES
// and $COLUMNS are deliberately ignored: they go stale the moment the pane
// is split or resized, which would leave the footer painted at the old
// bottom while mpv sizes the video to the true, smaller pane.
func TermSize() (cols, lines int) {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err == nil && ws.Col > 0 && ws.Row > 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return 80, 24
}

// FlushInput discards keystrokes the terminal has queued but nobody has
// read. Called when handing the terminal back to the browser, so a second
// press of q meant for mpv does not go on to quit jterm.
func FlushInput() {
	flushInput(int(os.Stdin.Fd()))
}

// Clock formats seconds as m:ss, or h:mm:ss from an hour up.
func Clock(seconds float64) string {
	s := int(seconds)
	h, m, sec := s/3600, s%3600/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

// State is what the footer shows, read from mpv once per tick.
type State struct {
	Pause      bool
	TimePos    *float64 // nil until mpv knows the position
	Duration   float64  // 0 when unknown
	PercentPos *float64
	Volume     *float64
	Width      int
	Height     int
	HasSub     bool // a subtitle track is selected
	SubHidden  bool // ...but visibility is switched off
	CapLabel   string
}

// ReadState polls the properties the footer needs. Anything mpv does not
// answer in time is simply left unset.
func ReadState(c *Conn, timeout time.Duration) State {
	get := func(prop string) any {
		v, _ := c.Get(prop, timeout)
		return v
	}
	num := func(prop string) *float64 {
		if f, ok := Float(get(prop)); ok {
			return &f
		}
		return nil
	}
	var st State
	st.TimePos = num("time-pos")
	if d := num("duration"); d != nil {
		st.Duration = *d
	}
	st.PercentPos = num("percent-pos")
	st.Volume = num("volume")
	st.Pause = Truthy(get("pause"))
	if w := num("width"); w != nil {
		st.Width = int(*w)
	}
	if h := num("height"); h != nil {
		st.Height = int(*h)
	}
	if vis, ok := get("sub-visibility").(bool); ok && !vis {
		st.SubHidden = true
	}
	st.HasSub = Truthy(get("sid"))
	return st
}

func subLabel(st State) string {
	if !st.HasSub {
		return "no subs"
	}
	if st.SubHidden {
		return "subs off"
	}
	return "subs on"
}

// FooterLines is the two text lines shown in the control footer.
func FooterLines(st State, title string) []string {
	icon := "▶"
	if st.Pause {
		icon = "⏸"
	}
	pos := "0:00"
	if st.TimePos != nil {
		pos = Clock(*st.TimePos)
	}
	dur := "?"
	if st.Duration > 0 {
		dur = Clock(st.Duration)
	}
	pct := "0%"
	if st.PercentPos != nil {
		pct = fmt.Sprintf("%d%%", int(*st.PercentPos))
	}
	vol := "?"
	if st.Volume != nil {
		vol = fmt.Sprintf("%d", int(*st.Volume))
	}
	res := "…"
	if st.Width > 0 && st.Height > 0 {
		res = fmt.Sprintf("%dx%d", st.Width, st.Height)
	}
	label := st.CapLabel
	if label == "" {
		label = "source"
	}
	line1 := fmt.Sprintf(" %s %s / %s (%s)   vol %s   %s (%s)   %s   %s",
		icon, pos, dur, pct, vol, res, label, subLabel(st), title)
	return []string{line1, " " + KeyHints}
}

// Note is a two-line footer carrying a short message over the key hints,
// used while loading or when something needs saying.
func Note(msg string) []string {
	return []string{" " + msg, " " + KeyHints}
}

// DrawFooter paints the coloured footer band across its reserved bottom
// rows. Autowrap is disabled so filling the final cell never scrolls, and
// the cursor is parked at the top afterwards so any stray mpv output lands
// there instead of scrolling the footer out of its rows.
func DrawFooter(w io.Writer, lines []string, termLines, cols int) {
	var b strings.Builder
	b.WriteString("\x1b[?7l")
	first := termLines - len(lines) + 1
	for i, text := range lines {
		cell := ansi.Truncate(text, cols, "")
		if pad := cols - ansi.StringWidth(cell); pad > 0 {
			cell += strings.Repeat(" ", pad)
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s%s\x1b[0m", first+i, footerBG, footerFG, cell)
	}
	b.WriteString("\x1b[?7h\x1b[H")
	_, _ = io.WriteString(w, b.String())
}
