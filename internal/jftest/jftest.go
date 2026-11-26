// Package jftest is a small in-memory stand-in for a Jellyfin server. The
// tests use it to assert exactly what jterm reports, and the fakejf command
// serves it so the whole TUI can be driven without touching a real library.
package jftest

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/h0bnobs/jterm/internal/jellyfin"
)

// The one account the fake server knows.
const (
	Username = "demo"
	Password = "demo"
	Token    = "test-token"
	UserID   = "user-1"
)

const runtime = 600 * jellyfin.TicksPerSecond // every video is ten minutes

// Event is one write the server received: a playback report or the end of
// a transcode.
type Event struct {
	Kind      string // start, progress, stopped, stop-encoding
	ItemID    string
	SessionID string
	Ticks     int64
	Paused    bool
	Method    string
}

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	// VideoPath is the file served for every stream request.
	VideoPath string
	// FailTranscode makes transcode requests fail, as a server with
	// transcoding switched off would.
	FailTranscode bool

	mu       sync.Mutex
	items    map[string]*jellyfin.Item
	children map[string][]string // parent id -> ordered child ids
	events   []Event
	revoked  bool
}

func ptr(n int) *int { return &n }

// New returns a server holding two films and a series of three episodes
// across two seasons.
func New() *Server {
	s := &Server{items: map[string]*jellyfin.Item{}, children: map[string][]string{}}
	add := func(parent string, it jellyfin.Item) {
		it.UserData = &jellyfin.UserData{}
		s.items[it.ID] = &it
		s.children[parent] = append(s.children[parent], it.ID)
	}
	add("", jellyfin.Item{ID: "lib-movies", Name: "Movies", Type: "CollectionFolder", CollectionType: "movies"})
	add("", jellyfin.Item{ID: "lib-shows", Name: "Shows", Type: "CollectionFolder", CollectionType: "tvshows"})
	add("lib-movies", jellyfin.Item{ID: "movie-1", Name: "Big Buck Bunny", Type: "Movie", ProductionYear: 2008, RunTimeTicks: runtime})
	add("lib-movies", jellyfin.Item{ID: "movie-2", Name: "Sintel", Type: "Movie", ProductionYear: 2010, RunTimeTicks: runtime,
		MediaSources: []jellyfin.MediaSource{{ID: "src-2", MediaStreams: []jellyfin.MediaStream{
			{Type: "Video", Index: 0, Codec: "h264"},
			{Type: "Subtitle", Index: 2, Codec: "subrip", Language: "eng", DisplayTitle: "English", IsExternal: true},
		}}}})
	add("lib-shows", jellyfin.Item{ID: "series-1", Name: "Test Show", Type: "Series", ProductionYear: 2020, ChildCount: 2})
	add("series-1", jellyfin.Item{ID: "season-1", Name: "Season 1", Type: "Season", SeriesID: "series-1", SeriesName: "Test Show", IndexNumber: ptr(1)})
	add("series-1", jellyfin.Item{ID: "season-2", Name: "Season 2", Type: "Season", SeriesID: "series-1", SeriesName: "Test Show", IndexNumber: ptr(2)})
	ep := func(season, id, name string, s1, e int) {
		add(season, jellyfin.Item{ID: id, Name: name, Type: "Episode", SeriesID: "series-1", SeriesName: "Test Show",
			ParentIndexNumber: ptr(s1), IndexNumber: ptr(e), RunTimeTicks: runtime})
	}
	ep("season-1", "ep-1-1", "Pilot", 1, 1)
	ep("season-1", "ep-1-2", "Second", 1, 2)
	ep("season-2", "ep-2-1", "Return", 2, 1)
	return s
}

// Events returns a copy of everything reported so far.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// UserData returns a copy of an item's per-user state.
func (s *Server) UserData(itemID string) jellyfin.UserData {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it := s.items[itemID]; it != nil {
		return *it.UserData
	}
	return jellyfin.UserData{}
}

// SetPosition gives an item a resume point, as if it had been part watched.
func (s *Server) SetPosition(itemID string, ticks int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[itemID].UserData.PlaybackPositionTicks = ticks
}

// Revoke makes every later authenticated request fail with 401.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = true
}

func (s *Server) record(e Event) {
	s.events = append(s.events, e)
}

// list returns copies of the given items in order. Callers hold the lock.
func (s *Server) list(ids []string) []jellyfin.Item {
	out := make([]jellyfin.Item, 0, len(ids))
	for _, id := range ids {
		it := *s.items[id]
		ud := *it.UserData
		it.UserData = &ud
		out = append(out, it)
	}
	return out
}

func (s *Server) episodeIDs(seasonID string) []string {
	if seasonID != "" {
		return s.children[seasonID]
	}
	var ids []string
	for _, season := range s.children["series-1"] {
		ids = append(ids, s.children[season]...)
	}
	return ids
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeItems(w http.ResponseWriter, items []jellyfin.Item) {
	writeJSON(w, map[string]any{"Items": items, "TotalRecordCount": len(items)})
}

// Handler is the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// authed wraps a handler with the token check and the server lock.
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			ok := strings.Contains(r.Header.Get("Authorization"), `Token="`+Token+`"`) ||
				r.URL.Query().Get("api_key") == Token
			if !ok || s.revoked {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"ServerName": "fakefin", "Id": "fake-server", "Version": "0.0.0"})
	})
	mux.HandleFunc("POST /Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Pw string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != Username || body.Pw != Password {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.revoked = false
		s.mu.Unlock()
		writeJSON(w, map[string]any{"AccessToken": Token, "User": map[string]string{"Id": UserID, "Name": Username}})
	})

	mux.HandleFunc("GET /Users/{uid}/Views", authed(func(w http.ResponseWriter, r *http.Request) {
		writeItems(w, s.list(s.children[""]))
	}))
	mux.HandleFunc("GET /Users/{uid}/Items/Resume", authed(func(w http.ResponseWriter, r *http.Request) {
		var ids []string
		for _, id := range []string{"movie-1", "movie-2", "ep-1-1", "ep-1-2", "ep-2-1"} {
			if ud := s.items[id].UserData; ud.PlaybackPositionTicks > 0 && !ud.Played {
				ids = append(ids, id)
			}
		}
		writeItems(w, s.list(ids))
	}))
	mux.HandleFunc("GET /Shows/NextUp", authed(func(w http.ResponseWriter, r *http.Request) {
		// the first unwatched episode, once the show has been started
		eps := s.episodeIDs("")
		for i, id := range eps {
			if i > 0 && !s.items[id].UserData.Played && s.items[eps[i-1]].UserData.Played {
				writeItems(w, s.list([]string{id}))
				return
			}
		}
		writeItems(w, nil)
	}))
	mux.HandleFunc("GET /Users/{uid}/Items/Latest", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.list(s.children[r.URL.Query().Get("ParentId")])) // a bare array
	}))
	mux.HandleFunc("GET /Users/{uid}/Items", authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if term := strings.ToLower(q.Get("SearchTerm")); term != "" {
			var ids []string
			for _, id := range []string{"movie-1", "movie-2", "series-1", "ep-1-1", "ep-1-2", "ep-2-1"} {
				if strings.Contains(strings.ToLower(s.items[id].Name), term) {
					ids = append(ids, id)
				}
			}
			writeItems(w, s.list(ids))
			return
		}
		writeItems(w, s.list(s.children[q.Get("ParentId")]))
	}))
	mux.HandleFunc("GET /Users/{uid}/Items/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if s.items[id] == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, s.list([]string{id})[0])
	}))
	mux.HandleFunc("GET /Shows/{id}/Seasons", authed(func(w http.ResponseWriter, r *http.Request) {
		writeItems(w, s.list(s.children[r.PathValue("id")]))
	}))
	mux.HandleFunc("GET /Shows/{id}/Episodes", authed(func(w http.ResponseWriter, r *http.Request) {
		writeItems(w, s.list(s.episodeIDs(r.URL.Query().Get("SeasonId"))))
	}))

	// per-user flags: POST sets, DELETE clears, both answer with UserData
	flag := func(set func(ud *jellyfin.UserData, on bool)) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			it := s.items[r.PathValue("id")]
			if it == nil {
				http.NotFound(w, r)
				return
			}
			set(it.UserData, r.Method == http.MethodPost)
			writeJSON(w, it.UserData)
		})
	}
	played := flag(func(ud *jellyfin.UserData, on bool) {
		ud.Played = on
		ud.PlaybackPositionTicks = 0
	})
	favourite := flag(func(ud *jellyfin.UserData, on bool) { ud.IsFavorite = on })
	mux.HandleFunc("POST /Users/{uid}/PlayedItems/{id}", played)
	mux.HandleFunc("DELETE /Users/{uid}/PlayedItems/{id}", played)
	mux.HandleFunc("POST /Users/{uid}/FavoriteItems/{id}", favourite)
	mux.HandleFunc("DELETE /Users/{uid}/FavoriteItems/{id}", favourite)

	// playback reports
	report := func(kind string) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			var b struct {
				ItemId, PlaySessionId, PlayMethod string
				PositionTicks                     int64
				IsPaused                          bool
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			s.record(Event{kind, b.ItemId, b.PlaySessionId, b.PositionTicks, b.IsPaused, b.PlayMethod})
			if it := s.items[b.ItemId]; it != nil && kind != "start" {
				it.UserData.PlaybackPositionTicks = b.PositionTicks
				if kind == "stopped" && b.PositionTicks >= runtime*9/10 {
					it.UserData.Played = true
					it.UserData.PlaybackPositionTicks = 0
				}
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("POST /Sessions/Playing", report("start"))
	mux.HandleFunc("POST /Sessions/Playing/Progress", report("progress"))
	mux.HandleFunc("POST /Sessions/Playing/Stopped", report("stopped"))
	mux.HandleFunc("DELETE /Videos/ActiveEncodings", authed(func(w http.ResponseWriter, r *http.Request) {
		s.record(Event{Kind: "stop-encoding", SessionID: r.URL.Query().Get("playSessionId")})
		w.WriteHeader(http.StatusNoContent)
	}))

	// streams: the same file whatever the item
	serveVideo := func(transcode bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			ok := r.URL.Query().Get("api_key") == Token && !s.revoked
			fail, path := transcode && s.FailTranscode, s.VideoPath
			s.mu.Unlock()
			switch {
			case !ok:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			case fail:
				http.Error(w, "transcoding is disabled", http.StatusInternalServerError)
			case path == "":
				http.NotFound(w, r)
			default:
				http.ServeFile(w, r, path)
			}
		}
	}
	mux.HandleFunc("GET /Videos/{id}/stream", serveVideo(false))
	mux.HandleFunc("GET /Videos/{id}/stream.mkv", serveVideo(true))
	mux.HandleFunc("GET /Videos/{id}/{source}/Subtitles/{index}/0/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("1\n00:00:00,000 --> 00:09:59,000\nexternal subtitle " + r.PathValue("index") + "\n"))
	})

	// test-only: inspect and steer the fake from outside the process
	mux.HandleFunc("GET /_test/events", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Events()) })
	mux.HandleFunc("GET /_test/userdata/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.UserData(r.PathValue("id")))
	})
	mux.HandleFunc("POST /_test/revoke", func(w http.ResponseWriter, r *http.Request) { s.Revoke() })
	mux.HandleFunc("POST /_test/fail-transcode/{on}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.FailTranscode = r.PathValue("on") == "true"
		s.mu.Unlock()
	})
	mux.HandleFunc("POST /_test/position/{id}/{seconds}", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := strconv.ParseInt(r.PathValue("seconds"), 10, 64)
		s.SetPosition(r.PathValue("id"), secs*jellyfin.TicksPerSecond)
	})
	return mux
}

// ServeDiscovery answers Jellyfin discovery datagrams on conn, advertising
// address, until conn is closed.
func ServeDiscovery(conn *net.UDPConn, address string) {
	reply, _ := json.Marshal(jellyfin.ServerInfo{Address: address, ID: "fake-server", Name: "fakefin"})
	buf := make([]byte, 1024)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if strings.HasPrefix(string(buf[:n]), "who is JellyfinServer?") {
			_, _ = conn.WriteToUDP(reply, from)
		}
	}
}
