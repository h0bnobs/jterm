package jellyfin_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/jftest"
)

func serve(t *testing.T) (*jftest.Server, *jellyfin.Client) {
	t.Helper()
	fake := jftest.New()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	return fake, jellyfin.New(ts.URL+"/", "dev", jftest.Token, jftest.UserID)
}

func names(items []jellyfin.Item) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Name)
	}
	return strings.Join(out, ",")
}

func TestAuthHeader(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer ts.Close()
	host, _ := os.Hostname()

	_, _ = jellyfin.New(ts.URL, "dev", "", "").ServerName()
	_, _ = jellyfin.New(ts.URL, "dev", "tok", "uid").Views()

	// same shape as the Python client's header
	base := `MediaBrowser Client="jterm", Device="` + host + `", DeviceId="dev", Version="` + jellyfin.ClientVersion + `"`
	if seen[0] != base {
		t.Errorf("signed out:\n got %s\nwant %s", seen[0], base)
	}
	if want := base + `, Token="tok"`; seen[1] != want {
		t.Errorf("signed in:\n got %s\nwant %s", seen[1], want)
	}
}

func TestErrors(t *testing.T) {
	_, c := serve(t)
	c.Token = "wrong"
	_, err := c.Views()
	if !jellyfin.Unauthorized(err) {
		t.Errorf("a rejected token should read as Unauthorized, got %v", err)
	}
	if err.Error() != "HTTP 401 for /Users/"+jftest.UserID+"/Views" {
		t.Errorf("message: %q", err)
	}

	dead := jellyfin.New("http://127.0.0.1:1", "dev", "", "")
	_, err = dead.ServerName()
	var jerr *jellyfin.Error
	if e, ok := err.(*jellyfin.Error); ok {
		jerr = e
	}
	if jerr == nil || jerr.Status != 0 || !strings.HasPrefix(jerr.Msg, "cannot reach http://127.0.0.1:1: ") {
		t.Errorf("unreachable server: %v", err)
	}
	if jellyfin.Unauthorized(err) {
		t.Error("an unreachable server is not a sign-in problem")
	}
}

func TestAuthenticate(t *testing.T) {
	_, c := serve(t)
	c.Token, c.UserID = "", ""
	if _, err := c.Authenticate(jftest.Username, "nope"); !jellyfin.Unauthorized(err) {
		t.Errorf("wrong password: %v", err)
	}
	user, err := c.Authenticate(jftest.Username, jftest.Password)
	if err != nil || user.Name != jftest.Username {
		t.Fatalf("Authenticate = %+v, %v", user, err)
	}
	if c.Token != jftest.Token || c.UserID != jftest.UserID {
		t.Errorf("token and user id not remembered: %q %q", c.Token, c.UserID)
	}
	if name, _ := c.ServerName(); name != "fakefin" {
		t.Errorf("ServerName = %q", name)
	}
}

func TestBrowsing(t *testing.T) {
	fake, c := serve(t)
	check := func(what string, items []jellyfin.Item, err error, want string) {
		t.Helper()
		if err != nil || names(items) != want {
			t.Errorf("%s = %q, %v; want %q", what, names(items), err, want)
		}
	}
	views, err := c.Views()
	check("Views", views, err, "Movies,Shows")
	// Latest answers with a bare array, unlike the rest
	latest, err := c.Latest("lib-movies", 8)
	check("Latest", latest, err, "Big Buck Bunny,Sintel")
	children, err := c.Children("lib-shows")
	check("Children", children, err, "Test Show")
	seasons, err := c.Seasons("series-1")
	check("Seasons", seasons, err, "Season 1,Season 2")
	eps, err := c.Episodes("series-1", "season-1")
	check("Episodes of a season", eps, err, "Pilot,Second")
	eps, err = c.Episodes("series-1", "")
	check("Episodes of the series", eps, err, "Pilot,Second,Return")
	found, err := c.Search("sin", 60)
	check("Search", found, err, "Sintel")

	resume, err := c.ResumeItems(12)
	check("Resume when nothing is in progress", resume, err, "")
	fake.SetPosition("movie-1", 90*jellyfin.TicksPerSecond)
	resume, err = c.ResumeItems(12)
	check("Resume", resume, err, "Big Buck Bunny")
	if got := resume[0].ResumeTicks(); got != 90*jellyfin.TicksPerSecond {
		t.Errorf("resume point %d", got)
	}

	ep := eps[0]
	if ep.EpisodeCode() != "S01E01" || ep.DisplayTitle() != "Test Show S01E01 · Pilot" {
		t.Errorf("episode decoded as %q / %q", ep.EpisodeCode(), ep.DisplayTitle())
	}
}

func TestAdjacentEpisode(t *testing.T) {
	_, c := serve(t)
	eps, _ := c.Episodes("series-1", "")
	pilot, second, ret := eps[0], eps[1], eps[2]

	if next, ok := c.AdjacentEpisode(pilot, 1); !ok || next.ID != second.ID {
		t.Errorf("after the pilot: %v %v", next.Name, ok)
	}
	// crosses the season boundary in both directions
	if next, ok := c.AdjacentEpisode(second, 1); !ok || next.ID != ret.ID {
		t.Errorf("after the season finale: %v %v", next.Name, ok)
	}
	if prev, ok := c.AdjacentEpisode(ret, -1); !ok || prev.ID != second.ID {
		t.Errorf("before the season opener: %v %v", prev.Name, ok)
	}
	if _, ok := c.AdjacentEpisode(ret, 1); ok {
		t.Error("the last episode has no next")
	}
	if _, ok := c.AdjacentEpisode(pilot, -1); ok {
		t.Error("the first episode has no previous")
	}
	movie, _ := c.Item("movie-1")
	if _, ok := c.AdjacentEpisode(movie, 1); ok {
		t.Error("films have no sequence")
	}
}

func TestStreamURL(t *testing.T) {
	c := jellyfin.New("http://srv:8096/", "dev", "tok", "uid")
	// expected strings are the Python client's output for the same arguments
	cases := []struct{ got, want string }{
		{c.StreamURL("i1", 0, 0, 0, ""),
			"http://srv:8096/Videos/i1/stream?static=true&api_key=tok&deviceId=dev"},
		{c.StreamURL("i1", 720, 4_000_000, 0, ""),
			"http://srv:8096/Videos/i1/stream.mkv?static=false&api_key=tok&deviceId=dev&videoCodec=h264&audioCodec=aac&maxHeight=720&videoBitRate=4000000"},
		{c.StreamURL("i1", 1080, 8_000_000, 1234, "sess"),
			"http://srv:8096/Videos/i1/stream.mkv?static=false&api_key=tok&deviceId=dev&videoCodec=h264&audioCodec=aac&maxHeight=1080&videoBitRate=8000000&startTimeTicks=1234&PlaySessionId=sess"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("\n got %s\nwant %s", tc.got, tc.want)
		}
	}
}

func TestExternalSubtitles(t *testing.T) {
	_, c := serve(t)
	subs, err := c.ExternalSubtitles("movie-2")
	if err != nil || len(subs) != 1 {
		t.Fatalf("subs = %+v, %v; want the one external track", subs, err)
	}
	// subrip is delivered as .srt; the embedded video stream is skipped
	want := c.Server + "/Videos/movie-2/src-2/Subtitles/2/0/Stream.srt?api_key=" + jftest.Token
	if subs[0].URL != want || subs[0].Title != "English" || subs[0].Lang != "eng" {
		t.Errorf("got %+v\nwant URL %s", subs[0], want)
	}
	if subs, _ := c.ExternalSubtitles("movie-1"); len(subs) != 0 {
		t.Errorf("an item with no external tracks gave %v", subs)
	}
}

func TestUserDataFlags(t *testing.T) {
	fake, c := serve(t)
	ud, err := c.SetPlayed("movie-1", true)
	if err != nil || ud == nil || !ud.Played || !fake.UserData("movie-1").Played {
		t.Errorf("SetPlayed(true) = %+v, %v", ud, err)
	}
	ud, err = c.SetPlayed("movie-1", false)
	if err != nil || ud == nil || ud.Played {
		t.Errorf("SetPlayed(false) = %+v, %v", ud, err)
	}
	ud, err = c.SetFavourite("movie-1", true)
	if err != nil || ud == nil || !ud.IsFavorite {
		t.Errorf("SetFavourite(true) = %+v, %v", ud, err)
	}
}

func TestUserDataFallsBackToTheItem(t *testing.T) {
	// a server that answers the flag change with an empty body
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"Id":"m","UserData":{"Played":true,"IsFavorite":true}}`))
		}
	}))
	defer ts.Close()
	ud, err := jellyfin.New(ts.URL, "dev", "tok", "uid").SetPlayed("m", true)
	if err != nil || ud == nil || !ud.Played || !ud.IsFavorite {
		t.Errorf("got %+v, %v; want the UserData from the item fetch", ud, err)
	}
}

func TestPlaybackReports(t *testing.T) {
	fake, c := serve(t)
	const T = jellyfin.TicksPerSecond
	if err := c.ReportStart("movie-1", "s1", 0, jellyfin.DirectPlay); err != nil {
		t.Fatal(err)
	}
	_ = c.ReportProgress("movie-1", "s1", 30*T, true, jellyfin.DirectPlay)
	_ = c.ReportStopped("movie-1", "s1", 45*T)
	_ = c.StopEncoding("s1")
	want := []jftest.Event{
		{Kind: "start", ItemID: "movie-1", SessionID: "s1", Method: "DirectPlay"},
		{Kind: "progress", ItemID: "movie-1", SessionID: "s1", Ticks: 30 * T, Paused: true, Method: "DirectPlay"},
		{Kind: "stopped", ItemID: "movie-1", SessionID: "s1", Ticks: 45 * T},
		{Kind: "stop-encoding", SessionID: "s1"},
	}
	got := fake.Events()
	if len(got) != len(want) {
		t.Fatalf("events %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v; want %+v", i, got[i], want[i])
		}
	}
	if pos := fake.UserData("movie-1").PlaybackPositionTicks; pos != 45*T {
		t.Errorf("resume point on the server is %d", pos)
	}
}

func TestDiscover(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go jftest.ServeDiscovery(conn, "http://127.0.0.1:18096")

	t.Setenv("JTERM_DISCOVERY_ADDR", conn.LocalAddr().String())
	found := jellyfin.Discover(300 * time.Millisecond)
	if len(found) != 1 || found[0].Address != "http://127.0.0.1:18096" || found[0].Name != "fakefin" {
		t.Errorf("Discover = %+v", found)
	}
}
