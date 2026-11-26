// Package ui is the terminal browser: a stack of pages over a table, a
// search box, and the keys that open, play and mark things.
package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/player"
)

// The list endpoints serve UserData from a server-side cache that lags
// writes by 30 s or more, while the single-item endpoint is always fresh.
// Freshly learned UserData is overlaid on list responses for this long so
// toggles and playback progress show up immediately.
const patchTTL = 300 * time.Second

type patch struct {
	at time.Time
	ud jellyfin.UserData
}

// Messages delivered to Update by background work.
type (
	pageLoadedMsg struct {
		page    *Page
		rows    []Row
		err     error
		restore bool // keep the cursor where it is rather than jumping to the top
	}
	userDataMsg struct {
		itemID string
		ud     *jellyfin.UserData
		err    error
	}
)

// Model is the whole application state. Everything on it is only ever
// touched from Update; background work talks back through messages.
type Model struct {
	cfg        config.Config
	vo         string
	jf         *jellyfin.Client
	serverName string
	username   string

	stack   []*Page
	patches map[string]patch // item id -> fresh UserData, see patchTTL
	subs    player.SubCache  // external subtitle URLs, fetched once per item

	table     table.Model
	search    textinput.Model
	help      help.Model
	status    string
	searching bool // the search box has the keyboard
	width     int
	height    int
}

var (
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// shown along the bottom, like the mobile app's tab bar of essentials
	helpKeys = []key.Binding{
		key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open/play")),
		key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "window")),
		key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "watched")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "home")),
		key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
	}
)

// tableKeys leaves the table only the navigation keys. Its defaults also
// bind b, f, d, g, u, j, k and space to scrolling, which would swallow
// jterm's own play-from-start, favourite, decode and home keys.
func tableKeys() table.KeyMap {
	return table.KeyMap{
		LineUp:     key.NewBinding(key.WithKeys("up")),
		LineDown:   key.NewBinding(key.WithKeys("down")),
		PageUp:     key.NewBinding(key.WithKeys("pgup")),
		PageDown:   key.NewBinding(key.WithKeys("pgdown")),
		GotoTop:    key.NewBinding(key.WithKeys("home")),
		GotoBottom: key.NewBinding(key.WithKeys("end")),
	}
}

func tableStyles() table.Styles {
	return table.Styles{
		Header: lipgloss.NewStyle().Bold(true).Padding(0, 1).Foreground(lipgloss.Color("245")),
		Cell:   lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Bold(true).
			Background(lipgloss.Color("#39446e")).Foreground(lipgloss.Color("#ececf5")),
	}
}

// columns sizes the table to the terminal. The title takes whatever is
// left; on a narrow terminal the secondary columns drop out rather than
// pushing the row off the edge (a zero width hides a column).
func columns(width int) []table.Column {
	section, info, length, state := 14, 24, 9, 8
	const padding = 2 // each cell is padded by one on either side
	if width < 90 {
		info = 0
	}
	if width < 64 {
		section = 0
	}
	used := 0
	for _, w := range []int{section, info, length, state} {
		if w > 0 {
			used += w + padding
		}
	}
	title := max(width-used-padding, 16)
	return []table.Column{
		{Title: "", Width: section}, {Title: "Title", Width: title},
		{Title: "", Width: info}, {Title: "Length", Width: length}, {Title: "", Width: state},
	}
}

// New builds the application around a loaded config and the terminal video
// output mpv should use.
func New(cfg config.Config, vo string) *Model {
	m := &Model{cfg: cfg, vo: vo, username: cfg.Username, patches: map[string]patch{}}
	m.table = table.New(
		table.WithColumns(columns(110)),
		table.WithFocused(true),
		table.WithKeyMap(tableKeys()),
		table.WithStyles(tableStyles()),
	)
	m.search = textinput.New()
	m.search.Prompt = " / "
	m.search.Placeholder = "Search your library…"
	m.help = help.New()
	return m
}

// Init starts on the home screen when there is a saved sign-in.
func (m *Model) Init() tea.Cmd {
	if m.cfg.Server != "" && m.cfg.Token != "" {
		m.jf = jellyfin.New(m.cfg.Server, m.cfg.DeviceID, m.cfg.Token, m.cfg.UserID)
		m.serverName = m.cfg.ServerName
		if m.serverName == "" {
			m.serverName = m.cfg.Server
		}
		return m.goHome()
	}
	m.setStatus("not signed in")
	return nil
}

// Update is the single place state changes.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	case pageLoadedMsg:
		return m, m.pageLoaded(msg)
	case userDataMsg:
		m.userDataChanged(msg)
		return m, nil
	case playbackDoneMsg:
		return m, m.playbackDone(msg)
	case refreshedMsg:
		return m, m.refreshed(msg)
	}
	// anything else (the cursor blink) belongs to the search box
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return m, cmd
}

func (m *Model) layout() {
	m.table.SetColumns(columns(m.width))
	m.table.SetWidth(m.width)
	m.table.SetHeight(max(m.height-3, 3)) // search, status and key rows
	m.search.SetWidth(max(m.width-6, 10))
	m.help.SetWidth(m.width)
}

// View draws the search box, the status line, the table and the key row.
func (m *Model) View() tea.View {
	status := mutedStyle.Render(ansi.Truncate("  "+m.status, max(m.width, 20), "…"))
	v := tea.NewView(strings.Join([]string{
		m.search.View(),
		status,
		m.table.View(),
		" " + m.help.ShortHelpView(helpKeys),
	}, "\n"))
	v.AltScreen = true
	v.WindowTitle = "jterm"
	return v
}

// -- helpers ----------------------------------------------------------------

func (m *Model) setStatus(text string) { m.status = text }

// idleStatus is the resting status line: where you are connected, how video
// will be shown, and the path down to the current page.
func (m *Model) idleStatus(extra string) {
	vo := m.vo
	if vo == "tct" {
		vo += " (block art — run jterm inside kitty for sharp video)"
	}
	where := "not connected"
	if m.jf != nil {
		where = m.serverName + " · " + m.username
	}
	titles := make([]string, 0, len(m.stack))
	for _, p := range m.stack {
		titles = append(titles, p.Title)
	}
	decode := "cpu"
	if m.cfg.HWDec {
		decode = "gpu"
	}
	var bits []string
	for _, bit := range []string{where, "video: " + vo + " · decode " + decode, strings.Join(titles, " › "), extra} {
		if bit != "" {
			bits = append(bits, bit)
		}
	}
	m.setStatus(strings.Join(bits, " │ "))
}

func (m *Model) top() *Page {
	if len(m.stack) == 0 {
		return nil
	}
	return m.stack[len(m.stack)-1]
}

// selected is the item under the cursor, if the page has one.
func (m *Model) selected() (jellyfin.Item, bool) {
	p := m.top()
	if p == nil {
		return jellyfin.Item{}, false
	}
	if i := m.table.Cursor(); i >= 0 && i < len(p.Rows) {
		return p.Rows[i].Item, true
	}
	return jellyfin.Item{}, false
}

// show puts the top page in the table with the cursor on the given row.
func (m *Model) show(cursor int) {
	p := m.top()
	if p == nil {
		m.table.SetRows(nil)
		return
	}
	rows := make([]table.Row, 0, len(p.Rows))
	for _, c := range cells(p.Rows) {
		rows = append(rows, c)
	}
	m.table.SetRows(rows)
	if len(rows) == 0 {
		m.idleStatus("nothing here")
		return
	}
	m.table.SetCursor(min(cursor, len(rows)-1))
	m.blurSearch()
	m.idleStatus("? keys")
}

func (m *Model) blurSearch() {
	m.searching = false
	m.search.Blur()
}

func (m *Model) saveConfig() {
	if err := config.Save(m.cfg); err != nil {
		m.setStatus(fmt.Sprintf("could not save settings: %v", err))
	}
}

// -- page loading -------------------------------------------------------------

func (m *Model) push(title string, load Loader, source *jellyfin.Item) tea.Cmd {
	if p := m.top(); p != nil {
		p.Cursor = m.table.Cursor()
	}
	p := &Page{Title: title, load: load, Source: source}
	m.stack = append(m.stack, p)
	m.table.SetRows(nil)
	m.setStatus("loading " + title + "…")
	return loadPage(p, false)
}

// loadPage fetches a page's rows in the background.
func loadPage(p *Page, restore bool) tea.Cmd {
	return func() tea.Msg {
		rows, err := p.load()
		return pageLoadedMsg{page: p, rows: rows, err: err, restore: restore}
	}
}

func (m *Model) pageLoaded(msg pageLoadedMsg) tea.Cmd {
	if msg.err != nil {
		if jellyfin.Unauthorized(msg.err) {
			// token expired or revoked
			m.setStatus("session expired — please sign in again")
			return m.signOut("user")
		}
		m.setStatus(fmt.Sprintf("failed: %v", msg.err))
		return nil
	}
	m.applyPatches(msg.rows)
	msg.page.Rows = msg.rows
	// A result for a page the user has since left is kept on that page but
	// not drawn.
	if msg.page == m.top() {
		cursor := 0
		if msg.restore {
			cursor = m.table.Cursor()
		}
		m.show(cursor)
	}
	return nil
}

// applyPatches overlays recently learned UserData on freshly listed rows.
func (m *Model) applyPatches(rows []Row) {
	now := time.Now()
	for id, p := range m.patches {
		if now.Sub(p.at) >= patchTTL {
			delete(m.patches, id)
		}
	}
	for i := range rows {
		if p, ok := m.patches[rows[i].Item.ID]; ok {
			ud := p.ud
			rows[i].Item.UserData = &ud
		}
	}
}

// noteUserData records fresh UserData for an item and applies it to every
// page already loaded.
func (m *Model) noteUserData(itemID string, ud jellyfin.UserData) {
	m.patches[itemID] = patch{time.Now(), ud}
	for _, p := range m.stack {
		for i := range p.Rows {
			if p.Rows[i].Item.ID == itemID {
				fresh := ud
				p.Rows[i].Item.UserData = &fresh
			}
		}
	}
}

func (m *Model) goHome() tea.Cmd {
	m.stack = nil
	return m.push("Home", homeLoader(m.jf), nil)
}

func (m *Model) reload() tea.Cmd {
	if p := m.top(); p != nil {
		return loadPage(p, true)
	}
	return nil
}

// signOut is replaced by the real sign-in flow; until then a rejected token
// can only be reported.
func (m *Model) signOut(string) tea.Cmd {
	m.stack = nil
	m.table.SetRows(nil)
	return nil
}

// -- keys -------------------------------------------------------------------

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	if m.searching {
		switch k {
		case "enter":
			return m.submitSearch()
		case "esc":
			m.blurSearch()
			return nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return cmd
	}
	switch k {
	case "q":
		return tea.Quit
	case "/":
		if m.jf != nil {
			m.search.SetValue("")
			m.searching = true
			return m.search.Focus()
		}
	case "enter":
		return m.open()
	case "b":
		return m.playSelected(false, true)
	case "a":
		return m.playSelected(true, false)
	case "esc", "backspace":
		m.back()
	case "g":
		if m.jf != nil {
			return m.goHome()
		}
	case "ctrl+r":
		if m.jf != nil {
			return m.reload()
		}
	case "w":
		return m.toggleFlag(false)
	case "f":
		return m.toggleFlag(true)
	case "d":
		m.toggleHWDec()
	default:
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return cmd
	}
	return nil
}

func (m *Model) submitSearch() tea.Cmd {
	term := strings.TrimSpace(m.search.Value())
	if term == "" || m.jf == nil {
		return nil
	}
	m.search.SetValue("")
	return m.push(fmt.Sprintf("search '%s'", term), searchLoader(m.jf, term), nil)
}

func (m *Model) open() tea.Cmd {
	item, ok := m.selected()
	if !ok {
		return nil
	}
	if item.Folder() {
		title := item.Name
		if title == "" {
			title = "?"
		}
		return m.push(title, childLoader(m.jf, item), &item)
	}
	return m.play(item, item.ResumeTicks(), false)
}

func (m *Model) back() {
	if len(m.stack) > 1 {
		m.stack = m.stack[:len(m.stack)-1]
		m.show(m.top().Cursor)
	}
}

// toggleFlag flips watched, or favourite, on the selected item.
func (m *Model) toggleFlag(favourite bool) tea.Cmd {
	item, ok := m.selected()
	if !ok {
		return nil
	}
	var ud jellyfin.UserData
	if item.UserData != nil {
		ud = *item.UserData
	}
	jf, id := m.jf, item.ID
	return func() tea.Msg {
		var fresh *jellyfin.UserData
		var err error
		if favourite {
			fresh, err = jf.SetFavourite(id, !ud.IsFavorite)
		} else {
			fresh, err = jf.SetPlayed(id, !ud.Played)
		}
		return userDataMsg{itemID: id, ud: fresh, err: err}
	}
}

func (m *Model) userDataChanged(msg userDataMsg) {
	if msg.err != nil {
		m.setStatus(fmt.Sprintf("failed: %v", msg.err))
		return
	}
	if msg.ud != nil {
		m.noteUserData(msg.itemID, *msg.ud)
	}
	if m.top() != nil {
		m.show(m.table.Cursor())
	}
}

func (m *Model) toggleHWDec() {
	m.cfg.HWDec = !m.cfg.HWDec
	if m.cfg.HWDec {
		m.setStatus("GPU/hardware decoding on — lower CPU on decode and in the o " +
			"window; in-terminal frames still copy back to the CPU. " +
			"Applies to the next play.")
	} else {
		m.setStatus("GPU/hardware decoding off — software decode (sw-fast)")
	}
	m.saveConfig()
}
