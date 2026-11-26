package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/h0bnobs/jterm/internal/jellyfin"
)

// The first-run flow: find a server, ask who you are, remember the token.
// Each network step runs in the background and reports back with one of
// these.
type (
	discoveredMsg struct {
		servers     []jellyfin.ServerInfo
		allowCancel bool
		note        string
	}
	serverInfoMsg struct {
		name string
		err  error
	}
	loginMsg struct {
		client *jellyfin.Client // signed in, when err is nil
		user   jellyfin.User
		typed  string // the username as entered, in case the server gives none
		err    error
	}
)

// beginSetup broadcasts for servers. note explains why, when we are back
// here because something failed.
func (m *Model) beginSetup(allowCancel bool, note string) tea.Cmd {
	m.setStatus("looking for Jellyfin servers on your network…")
	return func() tea.Msg {
		return discoveredMsg{jellyfin.Discover(jellyfin.DiscoveryTimeout), allowCancel, note}
	}
}

func (m *Model) discovered(msg discoveredMsg) tea.Cmd {
	if len(msg.servers) == 1 && !msg.allowCancel {
		// exactly one server on the network — connect to it, no questions
		return m.serverChosen(msg.servers[0].Address)
	}
	pick, cmd := newServerPick(msg.servers, msg.allowCancel, msg.note, func(address string) tea.Cmd {
		m.modal = nil
		return m.serverChosen(address)
	})
	m.modal = pick
	return cmd
}

func (m *Model) serverChosen(address string) tea.Cmd {
	if address == "" { // the picker was cancelled
		m.idleStatus("")
		return nil
	}
	m.cfg.Server = strings.TrimRight(address, "/")
	m.jf = jellyfin.New(m.cfg.Server, m.cfg.DeviceID, "", "")
	m.setStatus("connecting to " + address + "…")
	jf := m.jf
	return func() tea.Msg {
		name, err := jf.ServerName()
		return serverInfoMsg{name, err}
	}
}

func (m *Model) serverInfo(msg serverInfoMsg) tea.Cmd {
	if msg.err != nil {
		// back to the picker, this time with a way out and the reason
		return m.beginSetup(true, msg.err.Error())
	}
	name := msg.name
	if name == "" {
		name = m.cfg.Server
	}
	m.cfg.ServerName, m.serverName = name, name
	return m.askLogin("")
}

func (m *Model) askLogin(errText string) tea.Cmd {
	form, cmd := newLoginForm(m.serverName, errText, func(username, password string) tea.Cmd {
		m.modal = nil
		m.setStatus("signing in…")
		// sign in on a copy, so the token only reaches the model through
		// the message, on the UI goroutine
		client := *m.jf
		return func() tea.Msg {
			user, err := client.Authenticate(username, password)
			return loginMsg{client: &client, user: user, typed: username, err: err}
		}
	})
	m.modal = form
	return cmd
}

func (m *Model) loggedIn(msg loginMsg) tea.Cmd {
	if msg.err != nil {
		text := msg.err.Error()
		if jellyfin.Unauthorized(msg.err) {
			text = "wrong username or password"
		}
		return m.askLogin(text)
	}
	m.jf = msg.client
	m.username = msg.user.Name
	if m.username == "" {
		m.username = msg.typed
	}
	m.cfg.Token, m.cfg.UserID, m.cfg.Username = m.jf.Token, m.jf.UserID, m.username
	m.setStatus("signed in as " + m.username)
	m.saveConfig()
	return m.goHome()
}

// signOut forgets the session token and asks again: for the password when
// then is "user", for the server too when it is "server".
func (m *Model) signOut(then string) tea.Cmd {
	m.cfg.Token, m.cfg.UserID = "", ""
	m.saveConfig()
	m.stack = nil
	m.table.SetRows(nil)
	m.blurSearch()
	if then == "server" {
		m.cfg.Server = ""
		return m.beginSetup(false, "")
	}
	m.jf = jellyfin.New(m.cfg.Server, m.cfg.DeviceID, "", "")
	return m.askLogin("")
}

func (m *Model) openAccountMenu() {
	m.modal = &accountMenu{server: m.serverName, username: m.username, done: func(choice string) tea.Cmd {
		m.modal = nil
		if choice == "" {
			return nil
		}
		return m.signOut(choice)
	}}
}

func (m *Model) openHelp() {
	m.modal = newHelpScreen(func() tea.Cmd {
		m.modal = nil
		return nil
	})
}
