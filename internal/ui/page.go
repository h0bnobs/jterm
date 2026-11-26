package ui

import (
	"fmt"
	"io"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
)

// Row is one line of a page: an item under a section heading.
type Row struct {
	Section string
	Item    jellyfin.Item
}

// Loader fetches a page's rows. It runs off the UI goroutine, so it must
// only touch what it captured.
type Loader func() ([]Row, error)

// Page is one level of the navigation stack.
type Page struct {
	Title  string
	load   Loader
	Source *jellyfin.Item // the item this page was opened from, nil for Home and searches
	Rows   []Row
	Cursor int // where the cursor was when the user last left this page
}

func section(name string, items []jellyfin.Item) []Row {
	rows := make([]Row, 0, len(items))
	for _, it := range items {
		rows = append(rows, Row{name, it})
	}
	return rows
}

func plain(items []jellyfin.Item, err error) ([]Row, error) {
	return section("", items), err
}

// homeLoader builds the home screen the way the mobile app lays it out:
// what you were watching, what is next, your libraries, then what is new in
// each of them.
func homeLoader(jf *jellyfin.Client) Loader {
	return func() ([]Row, error) {
		resume, err := jf.ResumeItems(12)
		if err != nil {
			return nil, err
		}
		nextUp, err := jf.NextUp(12)
		if err != nil {
			return nil, err
		}
		views, err := jf.Views()
		if err != nil {
			return nil, err
		}
		rows := section("Continue", resume)
		rows = append(rows, section("Next Up", nextUp)...)
		rows = append(rows, section("Libraries", views)...)
		for _, view := range views {
			switch view.CollectionType {
			case "movies", "tvshows", "homevideos":
				latest, err := jf.Latest(view.ID, 8)
				if err != nil {
					return nil, err
				}
				rows = append(rows, section("Latest "+firstRunes(view.Name, 6), latest)...)
			}
		}
		return rows, nil
	}
}

// childLoader lists what is inside a folder: seasons of a series, episodes
// of a season, or plain children for everything else.
func childLoader(jf *jellyfin.Client, item jellyfin.Item) Loader {
	switch item.Type {
	case "Series":
		return func() ([]Row, error) { return plain(jf.Seasons(item.ID)) }
	case "Season":
		seriesID := item.SeriesID
		if seriesID == "" {
			seriesID = item.ID
		}
		return func() ([]Row, error) { return plain(jf.Episodes(seriesID, item.ID)) }
	}
	return func() ([]Row, error) { return plain(jf.Children(item.ID)) }
}

func searchLoader(jf *jellyfin.Client, term string) Loader {
	return func() ([]Row, error) { return plain(jf.Search(term, 60)) }
}

// cells renders rows as table cells. A section heading is printed once, on
// the first row of its group.
func cells(rows []Row) [][]string {
	out := make([][]string, 0, len(rows))
	last := ""
	for i, r := range rows {
		label := r.Section
		if i > 0 && label == last {
			label = ""
		}
		last = r.Section
		out = append(out, []string{
			label, r.Item.Title(), itemInfo(r.Item),
			fmtDuration(r.Item.RunTimeTicks), itemState(r.Item),
		})
	}
	return out
}

// Dump prints the Home rows as plain text using the saved sign-in. It is a
// temporary aid for checking the port against the Python jterm.
func Dump(w io.Writer, cfg config.Config) error {
	if cfg.Server == "" || cfg.Token == "" {
		return fmt.Errorf("not signed in: no server or token in %s", config.Path())
	}
	rows, err := homeLoader(jellyfin.New(cfg.Server, cfg.DeviceID, cfg.Token, cfg.UserID))()
	if err != nil {
		return err
	}
	for _, c := range cells(rows) {
		fmt.Fprintf(w, "%s|%s|%s|%s|%s\n", c[0], c[1], c[2], c[3], c[4])
	}
	return nil
}
