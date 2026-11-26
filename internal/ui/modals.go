package ui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/h0bnobs/jterm/internal/jellyfin"
)

// A modal is a box drawn over the browser that takes every key until it is
// dismissed. Each one reports its outcome through a callback it was built
// with, which also closes it.
type modal interface {
	key(msg tea.KeyPressMsg) tea.Cmd
	// other receives everything that is not a key, such as a cursor blink.
	other(msg tea.Msg) tea.Cmd
	// view is the box's contents for the given inner width and the
	// terminal height it has to fit inside.
	view(width, height int) string
	// width is how wide the box would like to be, border included.
	width() int
}

var (
	boxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7c8cd6")).Padding(1, 2)
	headingStyle = lipgloss.NewStyle().Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	pickedStyle  = lipgloss.NewStyle().Bold(true).
			Background(lipgloss.Color("#39446e")).Foreground(lipgloss.Color("#ececf5"))
)

const boxWidth = 70

// overlay draws a modal's box centred over the page behind it.
func overlay(base string, md modal, width, height int) string {
	outer := min(md.width(), max(width-2, 24))
	inner := outer - boxStyle.GetHorizontalFrameSize()
	box := boxStyle.Width(outer).Render(md.view(inner, height))
	x := max((width-lipgloss.Width(box))/2, 0)
	y := max((height-lipgloss.Height(box))/2, 0)
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(base),
		lipgloss.NewLayer(box).X(x).Y(y).Z(1),
	).Render()
}

func newInput(placeholder string, width int) textinput.Model {
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = placeholder
	in.SetWidth(width)
	return in
}

// options renders a vertical menu with one entry picked out.
func options(labels []string, cursor int, width int) string {
	lines := make([]string, len(labels))
	for i, label := range labels {
		if i == cursor {
			lines[i] = pickedStyle.Width(width).Render(" " + label)
		} else {
			lines[i] = " " + label
		}
	}
	return strings.Join(lines, "\n")
}

// -- server picker ------------------------------------------------------------

// serverPick offers the servers that answered discovery and a field for
// typing an address by hand.
type serverPick struct {
	servers     []jellyfin.ServerInfo
	allowCancel bool
	note        string // why we are back here, if something went wrong
	cursor      int    // an index into servers; len(servers) is the address field
	input       textinput.Model
	done        func(address string) tea.Cmd // "" means cancelled
}

func newServerPick(servers []jellyfin.ServerInfo, allowCancel bool, note string,
	done func(string) tea.Cmd) (*serverPick, tea.Cmd) {
	p := &serverPick{servers: servers, allowCancel: allowCancel, note: note, done: done,
		input: newInput("…or type an address, e.g. http://192.168.1.50:8096", boxWidth-10)}
	return p, p.move(0)
}

func (p *serverPick) onInput() bool { return p.cursor == len(p.servers) }

func (p *serverPick) move(by int) tea.Cmd {
	p.cursor = min(max(p.cursor+by, 0), len(p.servers))
	if p.onInput() {
		return p.input.Focus()
	}
	p.input.Blur()
	return nil
}

func (p *serverPick) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		if p.allowCancel {
			return p.done("")
		}
		return nil
	case "up", "shift+tab":
		return p.move(-1)
	case "down", "tab":
		return p.move(1)
	case "enter":
		if !p.onInput() {
			return p.done(p.servers[p.cursor].Address)
		}
		addr := strings.TrimSpace(p.input.Value())
		if addr == "" {
			return nil
		}
		if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
			addr = "http://" + addr
		}
		return p.done(addr)
	}
	if p.onInput() {
		return p.other(msg)
	}
	return nil
}

func (p *serverPick) other(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return cmd
}

func (p *serverPick) width() int { return boxWidth }

func (p *serverPick) view(width, _ int) string {
	blurb := "No Jellyfin server answered the network broadcast."
	if len(p.servers) > 0 {
		blurb = "Found these Jellyfin servers on your network:"
	}
	parts := []string{"Connect to Jellyfin", "", mutedStyle.Render(blurb)}
	if p.note != "" {
		parts = append(parts, "", errorStyle.Width(width).Render(p.note))
	}
	if len(p.servers) > 0 {
		labels := make([]string, len(p.servers))
		for i, s := range p.servers {
			name := s.Name
			if name == "" {
				name = "Jellyfin"
			}
			labels[i] = name + "  ·  " + s.Address
		}
		parts = append(parts, "", options(labels, p.cursor, width))
	}
	return strings.Join(append(parts, "", p.input.View()), "\n")
}

// -- sign-in ------------------------------------------------------------------

// loginForm asks for a username and password. The password is sent once
// and never stored.
type loginForm struct {
	serverName string
	errText    string
	user, pass textinput.Model
	onPass     bool
	done       func(username, password string) tea.Cmd
}

func newLoginForm(serverName, errText string, done func(string, string) tea.Cmd) (*loginForm, tea.Cmd) {
	f := &loginForm{serverName: serverName, errText: errText, done: done,
		user: newInput("username", boxWidth-10), pass: newInput("password", boxWidth-10)}
	f.pass.EchoMode = textinput.EchoPassword
	return f, f.focus(false)
}

func (f *loginForm) focus(pass bool) tea.Cmd {
	f.onPass = pass
	if pass {
		f.user.Blur()
		return f.pass.Focus()
	}
	f.pass.Blur()
	return f.user.Focus()
}

func (f *loginForm) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "shift+tab", "up", "down":
		return f.focus(!f.onPass)
	case "enter":
		user := strings.TrimSpace(f.user.Value())
		switch {
		case !f.onPass:
			return f.focus(true)
		case user == "":
			return f.focus(false)
		}
		return f.done(user, f.pass.Value())
	}
	return f.other(msg)
}

func (f *loginForm) other(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if f.onPass {
		f.pass, cmd = f.pass.Update(msg)
	} else {
		f.user, cmd = f.user.Update(msg)
	}
	return cmd
}

func (f *loginForm) width() int { return boxWidth }

func (f *loginForm) view(width, _ int) string {
	parts := []string{
		"Sign in to " + f.serverName, "",
		mutedStyle.Render("Your password is sent to your server once to create a\nsession token. Only the token is saved."),
	}
	if f.errText != "" {
		parts = append(parts, "", errorStyle.Width(width).Render(f.errText))
	}
	return strings.Join(append(parts, "", f.user.View(), f.pass.View()), "\n")
}

// -- account menu -------------------------------------------------------------

type accountMenu struct {
	server, username string
	cursor           int
	done             func(choice string) tea.Cmd // "user", "server", or "" for cancel
}

var accountChoices = []struct{ label, id string }{
	{"Switch user", "user"}, {"Switch server", "server"}, {"Cancel", ""},
}

func (a *accountMenu) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return a.done("")
	case "up":
		a.cursor = max(a.cursor-1, 0)
	case "down":
		a.cursor = min(a.cursor+1, len(accountChoices)-1)
	case "enter":
		return a.done(accountChoices[a.cursor].id)
	}
	return nil
}

func (a *accountMenu) other(tea.Msg) tea.Cmd { return nil }

func (a *accountMenu) width() int { return boxWidth }

func (a *accountMenu) view(width, _ int) string {
	labels := make([]string, len(accountChoices))
	for i, c := range accountChoices {
		labels[i] = c.label
	}
	return mutedStyle.Render("Connected to "+a.server+"\nSigned in as "+a.username) +
		"\n\n" + options(labels, a.cursor, width)
}

// -- help ---------------------------------------------------------------------

type helpScreen struct {
	body viewport.Model
	done func() tea.Cmd
}

func newHelpScreen(done func() tea.Cmd) *helpScreen {
	h := &helpScreen{body: viewport.New(), done: done}
	h.body.SoftWrap = true // on a narrow terminal, wrap rather than cut
	return h
}

// width fits the longest help line (72 columns) inside the frame.
func (h *helpScreen) width() int { return 72 + boxStyle.GetHorizontalFrameSize() }

func (h *helpScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "?", "q":
		return h.done()
	}
	return h.other(msg) // the rest scroll
}

func (h *helpScreen) other(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	h.body, cmd = h.body.Update(msg)
	return cmd
}

func (h *helpScreen) view(width, height int) string {
	text := helpText()
	// leave room for the box's border and padding, and a little margin
	h.body.SetWidth(width)
	h.body.SetHeight(min(strings.Count(text, "\n")+1, max(height-6, 3)))
	h.body.SetContent(text)
	return h.body.View()
}

func helpText() string {
	var b strings.Builder
	for i, s := range helpSections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(headingStyle.Render(s.heading) + "\n" + s.body)
	}
	return b.String()
}

var helpSections = []struct{ heading, body string }{
	{"Browsing", `  /        search your whole library
  Enter    open folder / play (resumes where you left off)
  b        play from the beginning
  a        play audio only
  o        play in the reusable mpv window (replaces it) — the TUI
           stays live for browsing and searching
  e        enqueue the selected item after the one in the window
  x        stop the window player
  w        toggle watched / unwatched
  f        toggle favourite
  d        toggle GPU / hardware decoding (on by default)
  Esc      back (also jumps from the search box to the list)
  g        home screen
  ctrl+r   refresh the current view
  s        server / account menu
  ?        this help
  q        quit`},
	{"During playback (mpv owns the terminal)", `  A live control footer sits under the video with position, duration,
  volume, resolution and the quality cap — the video can never draw
  over it, even when resizing.
  q        stop (position is saved to the server)
  space    pause / resume
  ←/→      seek 5 s        ↑/↓   volume ±5%
  Ctrl+↑/↓ raise / lower quality (reloads in place, keeps position)
  n / b    next / previous episode of the series (manual only —
           movies have no sequence; Ctrl+→/← do the same)
  v        subtitles on/off  j / J cycle subtitle track
  m        mute            [ / ] playback speed   ,/. frame step`},
	{"Quality", `  The footer shows the live resolution. Ctrl+↑/↓ step the cap across
  source / 1080 / 720 / 480 / 360: anything below source asks the
  server to transcode (which costs server CPU), and the choice is
  remembered. Some servers pick the transcode resolution from the
  bitrate rather than the cap — the footer readout is the truth.
  Seeking in a transcoded stream takes a moment while the server
  restarts the encode there; direct play (source) seeks instantly.`},
	{"Sync", `  Progress is reported to Jellyfin every few seconds, exactly like the
  mobile app: resume points, watched ticks and Next Up all stay in sync
  — across quality switches and n/b episode jumps too. When playback
  ends you land back on the library or collection you were browsing,
  or Home. Nothing autoplays: episodes advance only when you press n.`},
	{"Browse while playing", `  Enter takes over the terminal, so to keep browsing press o to play in
  a reusable mpv window instead. The TUI stays live: search, then o on
  another result to swap it into the same window, or e to queue it
  next. x stops the window. Progress syncs for whichever item the
  window is playing, and the status bar shows what that is.`},
	{"GPU / hardware decoding", `  d toggles hardware decoding (--hwdec=auto-safe) on or off; the choice
  is remembered. On by default because it cuts decode CPU (roughly a
  third less on a 1080p stream) and so reduces playback lag. auto-safe
  only uses decoders known to be reliable and falls back to software on
  its own. Turn it off only if a particular machine's hwdec misbehaves.`},
}
