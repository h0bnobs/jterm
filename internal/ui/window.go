package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/h0bnobs/jterm/internal/jellyfin"
)

// windowMsg is the outcome of a window-player action, as a status line.
type windowMsg struct{ status string }

// windowPlay shows an item in the reusable mpv window, or with enqueue
// queues it after what is playing there. The terminal is not handed over,
// so the browser stays live: that is the point of the window.
func (m *Model) windowPlay(item jellyfin.Item, startTicks int64, enqueue bool) tea.Cmd {
	if m.jf == nil {
		return nil
	}
	opts, ok := m.mpvOptions()
	if !ok {
		return nil
	}
	jf, win := m.jf, &m.window
	return func() tea.Msg {
		return windowMsg{win.Play(jf, opts, item, startTicks, enqueue)}
	}
}

// windowSelected sends the item under the cursor to the window.
func (m *Model) windowSelected(enqueue bool) tea.Cmd {
	item, ok := m.selected()
	if !ok {
		return nil
	}
	if item.Folder() {
		m.setStatus("that is a folder — press Enter to open it")
		return nil
	}
	return m.windowPlay(item, item.ResumeTicks(), enqueue)
}

// windowStop closes the window; the reporter tells the server where
// playback got to first.
func (m *Model) windowStop() tea.Cmd {
	win := &m.window
	return func() tea.Msg { return windowMsg{win.Stop()} }
}
