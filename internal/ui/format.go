package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/mpv"
)

// fmtDuration is an item's length for the Length column; blank when the
// server has none.
func fmtDuration(ticks int64) string {
	if ticks <= 0 {
		return ""
	}
	return mpv.Clock(float64(ticks / jellyfin.TicksPerSecond))
}

// itemInfo is the secondary column: the series for an episode, the year for
// a film, a count for a container.
func itemInfo(it jellyfin.Item) string {
	switch it.Type {
	case "Episode", "Season":
		return it.SeriesName
	case "Series":
		var bits []string
		if it.ProductionYear != 0 {
			bits = append(bits, strconv.Itoa(it.ProductionYear))
		}
		if n := it.ChildCount; n != 0 {
			plural := "s"
			if n == 1 {
				plural = ""
			}
			bits = append(bits, fmt.Sprintf("%d season%s", n, plural))
		}
		return strings.Join(bits, " · ")
	case "CollectionFolder", "UserView":
		if it.CollectionType != "" {
			return it.CollectionType
		}
		return "library"
	case "BoxSet":
		if it.ChildCount != 0 {
			return fmt.Sprintf("%d items", it.ChildCount)
		}
		return "collection"
	}
	if it.ProductionYear != 0 {
		return strconv.Itoa(it.ProductionYear)
	}
	return ""
}

// itemState is the last column: favourite heart, then progress, an
// unwatched count or a watched tick.
func itemState(it jellyfin.Item) string {
	var ud jellyfin.UserData
	if it.UserData != nil {
		ud = *it.UserData
	}
	marks := ""
	if ud.IsFavorite {
		marks = "♥"
	}
	if ud.PlaybackPositionTicks != 0 && it.RunTimeTicks != 0 {
		return fmt.Sprintf("%s▶%d%%", marks, ud.PlaybackPositionTicks*100/it.RunTimeTicks)
	}
	switch it.Type {
	case "Series", "Season", "BoxSet":
		if ud.UnplayedItemCount != 0 {
			return fmt.Sprintf("%s%d new", marks, ud.UnplayedItemCount)
		}
	}
	if ud.Played {
		return marks + "✓"
	}
	return marks
}

// firstRunes is s cut to at most n characters.
func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
