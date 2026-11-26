package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
)

func press(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func typeText(md modal, text string) {
	for _, r := range text {
		md.key(press(string(r)))
	}
}

// testModel is a signed-in model with a stack of already-loaded pages,
// named by the type of item each was opened from ("" for Home or a search).
func testModel(t *testing.T, sources ...string) *Model {
	t.Helper()
	t.Setenv("JTERM_CONFIG_DIR", t.TempDir())
	m := New(config.Config{HWDec: true}, "tct")
	m.jf = jellyfin.New("http://unused", "dev", "tok", "uid")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	for i, src := range sources {
		p := &Page{Title: src, Cursor: i, load: func() ([]Row, error) { return nil, nil }}
		if src != "" {
			p.Source = &jellyfin.Item{Type: src}
		}
		p.Rows = []Row{
			{"", jellyfin.Item{ID: "a", Name: "A", Type: "Movie"}},
			{"", jellyfin.Item{ID: "b", Name: "B", Type: "Movie"}},
			{"", jellyfin.Item{ID: "c", Name: "C", Type: "Movie"}},
		}
		m.stack = append(m.stack, p)
	}
	if len(m.stack) > 0 {
		m.show(0)
	}
	return m
}

func titles(m *Model) string {
	var out []string
	for _, p := range m.stack {
		out = append(out, p.Title)
	}
	return strings.Join(out, ">")
}

func TestReturnToParent(t *testing.T) {
	cases := []struct {
		name    string
		sources []string
		want    string
	}{
		{"played from Home", []string{""}, ""},
		{"played from a search", []string{"", ""}, ""},
		{"library > series > season", []string{"", "CollectionFolder", "Series", "Season"}, ">CollectionFolder"},
		{"library > collection: the nearest one wins", []string{"", "CollectionFolder", "BoxSet"}, ">CollectionFolder>BoxSet"},
		{"view > series", []string{"", "UserView", "Series"}, ">UserView"},
		{"search > series > season has no library above it", []string{"", "", "Series", "Season"}, ""},
	}
	for _, c := range cases {
		m := testModel(t, c.sources...)
		m.returnToParent()
		if got := titles(m); got != c.want {
			t.Errorf("%s: stack is %q; want %q", c.name, got, c.want)
		}
	}
}

func TestReturnToParentRestoresTheCursor(t *testing.T) {
	m := testModel(t, "", "CollectionFolder", "Series")
	// the library page was left with its cursor on row 1
	m.table.SetCursor(2)
	m.returnToParent()
	if m.table.Cursor() != 1 {
		t.Errorf("cursor on the library page = %d; want the row it was left on", m.table.Cursor())
	}

	// already on the right page: the cursor does not move
	m = testModel(t, "", "CollectionFolder")
	m.table.SetCursor(2)
	m.returnToParent()
	if m.table.Cursor() != 2 {
		t.Errorf("cursor moved to %d on a page that did not change", m.table.Cursor())
	}
}

func TestStalePageLoadIsKeptButNotDrawn(t *testing.T) {
	m := testModel(t, "", "CollectionFolder")
	left := &Page{Title: "left behind"}
	fresh := []Row{{"", jellyfin.Item{ID: "z", Name: "Z", Type: "Movie"}}}
	m.Update(pageLoadedMsg{page: left, rows: fresh})
	if len(left.Rows) != 1 {
		t.Error("the result should still be stored on its page")
	}
	if item, _ := m.selected(); item.ID != "a" {
		t.Errorf("a page the user left was drawn over the current one (selected %q)", item.ID)
	}
}

func TestReloadKeepsTheCursorAndAPushStartsAtTheTop(t *testing.T) {
	m := testModel(t, "")
	m.table.SetCursor(2)
	rows := m.top().Rows
	m.Update(pageLoadedMsg{page: m.top(), rows: rows, restore: true})
	if m.table.Cursor() != 2 {
		t.Errorf("refresh moved the cursor to %d", m.table.Cursor())
	}
	m.Update(pageLoadedMsg{page: m.top(), rows: rows})
	if m.table.Cursor() != 0 {
		t.Errorf("a newly opened page should start at the top, got %d", m.table.Cursor())
	}
}

func TestFailedLoadSaysSo(t *testing.T) {
	m := testModel(t, "")
	m.Update(pageLoadedMsg{page: m.top(), err: errors.New("HTTP 500 for /x")})
	if m.status != "failed: HTTP 500 for /x" {
		t.Errorf("status %q", m.status)
	}
	if len(m.top().Rows) != 3 {
		t.Error("a failed refresh must not empty the page")
	}
}

func TestRejectedTokenAsksToSignInAgain(t *testing.T) {
	m := testModel(t, "", "CollectionFolder")
	m.cfg.Token, m.cfg.Server = "tok", "http://unused"
	m.Update(pageLoadedMsg{page: m.top(), err: &jellyfin.Error{Status: 401, Msg: "HTTP 401"}})
	if _, ok := m.modal.(*loginForm); !ok {
		t.Fatalf("modal is %T; want the sign-in form", m.modal)
	}
	if m.cfg.Token != "" || len(m.stack) != 0 {
		t.Errorf("the dead token should be forgotten and the pages cleared: token=%q stack=%d", m.cfg.Token, len(m.stack))
	}
	if saved := config.Load(); saved.Token != "" {
		t.Error("the dead token was left in the config file")
	}
}

func TestUserDataOverlay(t *testing.T) {
	m := testModel(t, "", "CollectionFolder")
	// w on item b: the answer updates every loaded page at once
	m.table.SetCursor(1)
	m.Update(userDataMsg{itemID: "b", ud: &jellyfin.UserData{Played: true}})
	for _, p := range m.stack {
		if ud := p.Rows[1].Item.UserData; ud == nil || !ud.Played {
			t.Errorf("page %q does not show b as watched", p.Title)
		}
	}
	if m.table.Cursor() != 1 {
		t.Errorf("toggling moved the cursor to %d", m.table.Cursor())
	}

	// a list response still carrying the server's stale cache is patched
	stale := []Row{{"", jellyfin.Item{ID: "b", Name: "B", Type: "Movie", UserData: &jellyfin.UserData{}}}}
	m.applyPatches(stale)
	if !stale[0].Item.UserData.Played {
		t.Error("fresh UserData was not overlaid on a stale list response")
	}

	// ...but only for so long: after the TTL the server's word is taken
	m.patches["b"] = patch{time.Now().Add(-patchTTL - time.Second), jellyfin.UserData{Played: true}}
	stale[0].Item.UserData = &jellyfin.UserData{}
	m.applyPatches(stale)
	if stale[0].Item.UserData.Played {
		t.Error("an expired patch was still applied")
	}
}

func TestLettersTypeIntoTheSearchBox(t *testing.T) {
	m := testModel(t, "")
	m.Update(press("/"))
	for _, k := range []string{"d", "q", "g"} { // each is an action key in the list
		m.Update(press(k))
	}
	if m.search.Value() != "dqg" {
		t.Errorf("search box holds %q; the keys were taken as actions", m.search.Value())
	}
	if !m.cfg.HWDec {
		t.Error("d toggled hardware decoding while typing")
	}
	m.Update(press("esc"))
	if m.searching {
		t.Error("Esc should hand the keyboard back to the list")
	}
}

func TestFolderCannotBePlayed(t *testing.T) {
	m := testModel(t, "")
	m.top().Rows[0].Item = jellyfin.Item{ID: "lib", Name: "Movies", Type: "CollectionFolder"}
	for _, k := range []string{"b", "a", "o", "e"} {
		m.status = ""
		if _, cmd := m.Update(press(k)); cmd != nil || m.status != "that is a folder — press Enter to open it" {
			t.Errorf("%s on a folder: status %q, started something: %v", k, m.status, cmd != nil)
		}
	}
}

func TestServerPick(t *testing.T) {
	servers := []jellyfin.ServerInfo{{Address: "http://one:8096", Name: "one"}, {Address: "http://two:8096"}}
	var chosen []string
	done := func(addr string) tea.Cmd { chosen = append(chosen, addr); return nil }

	p, _ := newServerPick(servers, false, "", done)
	p.key(press("esc")) // not cancellable on first run
	p.key(press("down"))
	p.key(press("enter"))
	p, _ = newServerPick(servers, true, "", done)
	p.key(press("down"))
	p.key(press("down")) // onto the address field
	typeText(p, "10.0.0.5:8096")
	p.key(press("enter"))
	p, _ = newServerPick(nil, true, "", done) // nothing found: straight to typing
	typeText(p, "https://jf.example")
	p.key(press("enter"))
	p.key(press("esc"))

	want := []string{"http://two:8096", "http://10.0.0.5:8096", "https://jf.example", ""}
	if strings.Join(chosen, " ") != strings.Join(want, " ") {
		t.Errorf("chosen %q; want %q", chosen, want)
	}
	if view := p.view(64, 30); !strings.Contains(view, "No Jellyfin server answered") {
		t.Errorf("empty picker view: %q", view)
	}
}

func TestLoginForm(t *testing.T) {
	var got []string
	f, _ := newLoginForm("home", "", func(u, p string) tea.Cmd { got = append(got, u+"/"+p); return nil })
	f.key(press("enter")) // no username yet: moves on to the password
	typeText(f, "secret")
	f.key(press("enter")) // still no username: back to it, nothing submitted
	if len(got) != 0 || f.onPass {
		t.Fatalf("submitted %v with an empty username (onPass=%v)", got, f.onPass)
	}
	typeText(f, " max ")
	f.key(press("enter"))
	f.key(press("enter"))
	if len(got) != 1 || got[0] != "max/secret" {
		t.Errorf("submitted %q; want max/secret", got)
	}
	if view := f.view(64, 30); strings.Contains(view, "secret") {
		t.Error("the password is shown in the clear")
	}
}
