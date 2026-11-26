package jellyfin

import "fmt"

// TicksPerSecond converts Jellyfin's 100 ns ticks to seconds.
const TicksPerSecond = 10_000_000

// UserData is the per-user state of an item.
type UserData struct {
	IsFavorite            bool  `json:"IsFavorite"`
	Played                bool  `json:"Played"`
	PlaybackPositionTicks int64 `json:"PlaybackPositionTicks"`
	UnplayedItemCount     int   `json:"UnplayedItemCount"`
}

// MediaStream is one track inside a media source.
type MediaStream struct {
	Type         string `json:"Type"`
	Index        int    `json:"Index"`
	Codec        string `json:"Codec"`
	Language     string `json:"Language"`
	Title        string `json:"Title"`
	DisplayTitle string `json:"DisplayTitle"`
	IsExternal   bool   `json:"IsExternal"`
	IsDefault    bool   `json:"IsDefault"`
	IsForced     bool   `json:"IsForced"`
}

// MediaSource is one playable version of an item.
type MediaSource struct {
	ID           string        `json:"Id"`
	MediaStreams []MediaStream `json:"MediaStreams"`
}

// Item is a library entry: a film, series, season, episode, library or
// collection. Only the fields jterm reads are decoded.
type Item struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	Type           string `json:"Type"`
	SeriesName     string `json:"SeriesName"`
	SeriesID       string `json:"SeriesId"`
	CollectionType string `json:"CollectionType"`
	IsFolder       bool   `json:"IsFolder"`
	ProductionYear int    `json:"ProductionYear"`
	ChildCount     int    `json:"ChildCount"`
	RunTimeTicks   int64  `json:"RunTimeTicks"`
	// Pointers because zero is meaningful: season 0 is specials.
	ParentIndexNumber *int          `json:"ParentIndexNumber"`
	IndexNumber       *int          `json:"IndexNumber"`
	UserData          *UserData     `json:"UserData"`
	MediaSources      []MediaSource `json:"MediaSources"`
}

var folderTypes = map[string]bool{
	"CollectionFolder": true, "UserView": true, "Folder": true, "BoxSet": true,
	"Series": true, "Season": true, "MusicAlbum": true, "Playlist": true,
}

// Folder reports whether opening the item drills in rather than plays.
func (i Item) Folder() bool {
	return folderTypes[i.Type] || i.IsFolder
}

// Library reports whether the item is a library or collection, the kind of
// page playback returns to when it ends.
func (i Item) Library() bool {
	return i.Type == "CollectionFolder" || i.Type == "UserView" || i.Type == "BoxSet"
}

// ResumeTicks is where the user left off, or zero.
func (i Item) ResumeTicks() int64 {
	if i.UserData == nil {
		return 0
	}
	return i.UserData.PlaybackPositionTicks
}

// RuntimeSeconds is the item's length, or zero when the server has none.
func (i Item) RuntimeSeconds() float64 {
	return float64(i.RunTimeTicks) / TicksPerSecond
}

// EpisodeCode is S01E02 for an episode with both numbers, else empty.
func (i Item) EpisodeCode() string {
	if i.ParentIndexNumber == nil || i.IndexNumber == nil {
		return ""
	}
	return fmt.Sprintf("S%02dE%02d", *i.ParentIndexNumber, *i.IndexNumber)
}

// Title is the name as listed in the browser; episodes carry their code.
func (i Item) Title() string {
	name := i.Name
	if name == "" {
		name = "?"
	}
	if i.Type == "Episode" {
		if code := i.EpisodeCode(); code != "" {
			return code + " · " + name
		}
	}
	return name
}

// DisplayTitle is the title used where the item stands alone (the footer,
// the window title, status messages), so episodes also name their series.
func (i Item) DisplayTitle() string {
	if i.Type == "Episode" && i.SeriesName != "" {
		return i.SeriesName + " " + i.Title()
	}
	return i.Title()
}

// Subtitle is an external subtitle track the server can deliver as text.
type Subtitle struct {
	URL     string
	Title   string
	Lang    string
	Default bool
}

// Media is what playback needs to know about an item's files.
type Media struct {
	SourceID  string     // the media source a transcode must name
	Subtitles []Subtitle // external tracks to side-load
}

// ServerInfo is a server that answered the discovery broadcast.
type ServerInfo struct {
	Address string `json:"Address"`
	ID      string `json:"Id"`
	Name    string `json:"Name"`
}

// User is the signed-in account.
type User struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}
