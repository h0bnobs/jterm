package ui

import (
	"fmt"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/mpv"
	"github.com/h0bnobs/jterm/internal/player"
)

type (
	playbackDoneMsg struct {
		res   player.Result
		err   error
		title string
	}
	// refreshedMsg carries UserData fetched one item at a time, which is
	// always current, for the items that were just played.
	refreshedMsg struct {
		fresh map[string]jellyfin.UserData
	}
)

// mpvOptions is the shared part of every mpv launch. ok is false, with the
// reason in the status line, when mpv is not installed.
func (m *Model) mpvOptions() (opts mpv.Options, ok bool) {
	bin, err := exec.LookPath("mpv")
	if err != nil {
		m.setStatus("mpv is not installed — install it and restart jterm")
		return opts, false
	}
	conf := config.InputConfPath()
	mpv.WriteInputConf(conf)
	return mpv.Options{
		Bin:       bin,
		InputConf: conf,
		UserAgent: jellyfin.ClientName + "/" + jellyfin.ClientVersion,
		VO:        m.vo,
		HWDec:     m.cfg.HWDec,
	}, true
}

// play hands the terminal to mpv for one item. The browser is suspended
// until playback ends.
func (m *Model) play(item jellyfin.Item, startTicks int64, audio bool) tea.Cmd {
	if m.jf == nil {
		return nil
	}
	opts, ok := m.mpvOptions()
	if !ok {
		return nil
	}
	t := player.NewTerminal(player.Request{
		JF: m.jf, Item: item, StartTicks: startTicks, Audio: audio,
		Cap: m.cfg.QualityCap, MPV: opts, Media: &m.media,
	})
	title := item.DisplayTitle()
	return tea.Exec(t, func(err error) tea.Msg {
		return playbackDoneMsg{res: t.Result(), err: err, title: title}
	})
}

// playSelected plays the item under the cursor, resuming unless told to
// start over.
func (m *Model) playSelected(audio, fromStart bool) tea.Cmd {
	item, ok := m.selected()
	if !ok {
		return nil
	}
	if item.Folder() {
		m.setStatus("that is a folder — press Enter to open it")
		return nil
	}
	start := item.ResumeTicks()
	if fromStart {
		start = 0
	}
	return m.play(item, start, audio)
}

// playbackDone runs when mpv has handed the terminal back.
func (m *Model) playbackDone(msg playbackDoneMsg) tea.Cmd {
	if msg.res.Cap != m.cfg.QualityCap {
		m.cfg.QualityCap = msg.res.Cap // remembered for next time
		m.saveConfig()
	}
	m.returnToParent()
	if msg.err != nil {
		m.setStatus(fmt.Sprintf("playback failed: %v", msg.err))
	} else {
		m.setStatus("finished: " + firstRunes(msg.title, 60))
	}
	return refreshUserData(m.jf, msg.res.PlayedIDs)
}

// returnToParent lands back on the page you were browsing from: the nearest
// ancestor library or collection, or Home when the item came from Home,
// Continue Watching, Next Up or a search. If that is already the page on
// show, the cursor stays where it was.
func (m *Model) returnToParent() {
	if len(m.stack) == 0 {
		return
	}
	keep := 0 // Home
	for i, p := range m.stack {
		if p.Source != nil && p.Source.Library() {
			keep = i
		}
	}
	if keep < len(m.stack)-1 {
		m.stack = m.stack[:keep+1]
		m.show(m.top().Cursor)
	}
}

// refreshUserData pulls the fresh truth for the items just played. The
// list endpoints cache UserData (see patchTTL), so without this the new
// progress would not show until the cache caught up.
func refreshUserData(jf *jellyfin.Client, ids []string) tea.Cmd {
	return func() tea.Msg {
		fresh := map[string]jellyfin.UserData{}
		for _, id := range ids {
			if item, err := jf.Item(id); err == nil && item.UserData != nil {
				fresh[id] = *item.UserData
			}
		}
		return refreshedMsg{fresh}
	}
}

func (m *Model) refreshed(msg refreshedMsg) tea.Cmd {
	for id, ud := range msg.fresh {
		m.noteUserData(id, ud)
	}
	return m.reload()
}
