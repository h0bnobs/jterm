package ui

import (
	"reflect"
	"testing"

	"github.com/h0bnobs/jterm/internal/jellyfin"
)

const tps = jellyfin.TicksPerSecond

func n(v int) *int { return &v }

// Every expected value here is what the Python jterm's item_title,
// item_info, item_state, is_folder and fmt_duration returned for the same
// item.
func TestItemFormatting(t *testing.T) {
	cases := []struct {
		name                       string
		item                       jellyfin.Item
		title, info, state, length string
		display                    string
		folder                     bool
	}{
		{"film", jellyfin.Item{Name: "Heat", Type: "Movie", ProductionYear: 1995, RunTimeTicks: 600 * tps},
			"Heat", "1995", "", "10:00", "Heat", false},
		{"film in progress, favourite", jellyfin.Item{Name: "Heat", Type: "Movie", RunTimeTicks: 600 * tps,
			UserData: &jellyfin.UserData{PlaybackPositionTicks: 150 * tps, IsFavorite: true}},
			"Heat", "", "♥▶25%", "10:00", "Heat", false},
		{"film watched, favourite", jellyfin.Item{Name: "Heat", Type: "Movie",
			UserData: &jellyfin.UserData{Played: true, IsFavorite: true}},
			"Heat", "", "♥✓", "", "Heat", false},
		{"nameless", jellyfin.Item{Type: "Movie"}, "?", "", "", "", "?", false},
		{"episode", jellyfin.Item{Name: "Pilot", Type: "Episode", SeriesName: "Show", ParentIndexNumber: n(1),
			IndexNumber: n(2), RunTimeTicks: 600 * tps, UserData: &jellyfin.UserData{Played: true}},
			"S01E02 · Pilot", "Show", "✓", "10:00", "Show S01E02 · Pilot", false},
		// season 0 is specials: zero is a number, not a gap
		{"special", jellyfin.Item{Name: "Extra", Type: "Episode", SeriesName: "Show", ParentIndexNumber: n(0), IndexNumber: n(0)},
			"S00E00 · Extra", "Show", "", "", "Show S00E00 · Extra", false},
		{"episode without a season number", jellyfin.Item{Name: "Loose", Type: "Episode", SeriesName: "Show", IndexNumber: n(3)},
			"Loose", "Show", "", "", "Show Loose", false},
		{"series", jellyfin.Item{Name: "Show", Type: "Series", ProductionYear: 2020, ChildCount: 2,
			UserData: &jellyfin.UserData{UnplayedItemCount: 5, IsFavorite: true}},
			"Show", "2020 · 2 seasons", "♥5 new", "", "Show", true},
		{"series of one season, watched", jellyfin.Item{Name: "Show", Type: "Series", ChildCount: 1,
			UserData: &jellyfin.UserData{Played: true}},
			"Show", "1 season", "✓", "", "Show", true},
		{"bare series", jellyfin.Item{Name: "Show", Type: "Series"}, "Show", "", "", "", "Show", true},
		{"season", jellyfin.Item{Name: "Season 1", Type: "Season", SeriesName: "Show", UserData: &jellyfin.UserData{}},
			"Season 1", "Show", "", "", "Season 1", true},
		{"library", jellyfin.Item{Name: "Movies", Type: "CollectionFolder", CollectionType: "movies"},
			"Movies", "movies", "", "", "Movies", true},
		{"untyped library", jellyfin.Item{Name: "Mixed", Type: "UserView"}, "Mixed", "library", "", "", "Mixed", true},
		{"collection", jellyfin.Item{Name: "Trilogy", Type: "BoxSet", ChildCount: 3},
			"Trilogy", "3 items", "", "", "Trilogy", true},
		{"empty collection", jellyfin.Item{Name: "Trilogy", Type: "BoxSet"}, "Trilogy", "collection", "", "", "Trilogy", true},
		{"folder by flag", jellyfin.Item{Name: "Dir", Type: "Video", IsFolder: true}, "Dir", "", "", "", "Dir", true},
	}
	for _, c := range cases {
		got := []any{c.item.Title(), itemInfo(c.item), itemState(c.item), fmtDuration(c.item.RunTimeTicks),
			c.item.DisplayTitle(), c.item.Folder()}
		want := []any{c.title, c.info, c.state, c.length, c.display, c.folder}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	for ticks, want := range map[int64]string{
		0: "", 59 * tps: "0:59", 65 * tps: "1:05", 3600 * tps: "1:00:00", 3661*tps + 5: "1:01:01",
	} {
		if got := fmtDuration(ticks); got != want {
			t.Errorf("fmtDuration(%d) = %q; want %q", ticks, got, want)
		}
	}
}

func TestCellsPrintEachSectionOnce(t *testing.T) {
	rows := []Row{
		{"Continue", jellyfin.Item{Name: "A", Type: "Movie"}},
		{"Continue", jellyfin.Item{Name: "B", Type: "Movie"}},
		{"Libraries", jellyfin.Item{Name: "Movies", Type: "CollectionFolder"}},
		{"", jellyfin.Item{Name: "C", Type: "Movie"}},
	}
	var labels []string
	for _, c := range cells(rows) {
		labels = append(labels, c[0])
	}
	if want := []string{"Continue", "", "Libraries", ""}; !reflect.DeepEqual(labels, want) {
		t.Errorf("section labels %q; want %q", labels, want)
	}
}
