package player

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/jftest"
	"github.com/h0bnobs/jterm/internal/mpv"
	"github.com/h0bnobs/jterm/internal/mpv/mpvtest"
)

const tps = jellyfin.TicksPerSecond

// fakeJellyfin serves the in-memory library and returns a signed-in client.
func fakeJellyfin(t *testing.T) (*jftest.Server, *jellyfin.Client) {
	t.Helper()
	fake := jftest.New()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	return fake, jellyfin.New(ts.URL, "dev", jftest.Token, jftest.UserID)
}

// fastReports shortens the progress interval for the length of a test.
func fastReports(t *testing.T) {
	t.Helper()
	old := progressInterval
	progressInterval = 40 * time.Millisecond
	t.Cleanup(func() { progressInterval = old })
}

// waitFor polls until the server has logged an event matching ok.
func waitFor(t *testing.T, fake *jftest.Server, what string, ok func(jftest.Event) bool) jftest.Event {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, e := range fake.Events() {
			if ok(e) {
				return e
			}
		}
	}
	t.Fatalf("timed out waiting for %s; events: %+v", what, fake.Events())
	return jftest.Event{}
}

func kinds(events []jftest.Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Kind+":"+e.ItemID)
	}
	return strings.Join(slices.Compact(out), " ")
}

func TestReporterMirrorsPlayback(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	player := mpvtest.Start(t, map[string]any{"time-pos": 12.0, "pause": false})
	item := jellyfin.Item{ID: "movie-1"}

	// a transcode that began 100 s in: mpv's clock is 100 s behind the item's
	r := StartReporter(jf, item, player.Path, 100*tps, 100*tps, jellyfin.Transcode, "sess")
	waitFor(t, fake, "progress at 112 s", func(e jftest.Event) bool {
		return e.Kind == "progress" && e.Ticks == 112*tps && !e.Paused
	})
	player.Set("time-pos", 20.5)
	player.Set("pause", true)
	waitFor(t, fake, "paused progress at 120.5 s", func(e jftest.Event) bool {
		return e.Kind == "progress" && e.Ticks == 1205*tps/10 && e.Paused
	})
	r.Stop()

	events := fake.Events()
	first, last := events[0], events[len(events)-1]
	if first != (jftest.Event{Kind: "start", ItemID: "movie-1", SessionID: "sess", Ticks: 100 * tps, Method: "Transcode"}) {
		t.Errorf("first report = %+v; want Playing at the start position", first)
	}
	if last.Kind != "stopped" || last.Ticks != 1205*tps/10 || last.SessionID != "sess" {
		t.Errorf("last report = %+v; want Stopped at the final position", last)
	}
	if got := kinds(events); got != "start:movie-1 progress:movie-1 stopped:movie-1" {
		t.Errorf("report order: %s", got)
	}
	if r.LastTicks() != 1205*tps/10 {
		t.Errorf("LastTicks = %d", r.LastTicks())
	}
	if pos := fake.UserData("movie-1").PlaybackPositionTicks; pos != 1205*tps/10 {
		t.Errorf("server resume point = %d", pos)
	}
}

func TestReporterEndsWhenMpvExits(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	player := mpvtest.Start(t, map[string]any{"time-pos": 30.0, "pause": false})
	r := StartReporter(jf, jellyfin.Item{ID: "movie-1"}, player.Path, 0, 0, jellyfin.DirectPlay, "")
	waitFor(t, fake, "first progress", func(e jftest.Event) bool { return e.Kind == "progress" })
	player.Close() // the user pressed q in mpv
	waitFor(t, fake, "Stopped without being told", func(e jftest.Event) bool {
		return e.Kind == "stopped" && e.Ticks == 30*tps
	})
	r.Stop()
	if r.SessionID == "" {
		t.Error("a session id should have been minted")
	}
}

func TestReporterThatNeverConnectsReportsNothing(t *testing.T) {
	fake, jf := fakeJellyfin(t)
	r := StartReporter(jf, jellyfin.Item{ID: "movie-1"}, filepath.Join(t.TempDir(), "absent.sock"), 0, 0, jellyfin.DirectPlay, "")
	started := time.Now()
	r.Stop()
	if time.Since(started) > 2*time.Second {
		t.Error("Stop should not wait out the connect retries")
	}
	if events := fake.Events(); len(events) != 0 {
		t.Errorf("mpv never started, yet the server was told %+v", events)
	}
}

func TestWindowReporterFollowsThePlaylist(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	player := mpvtest.Start(t, map[string]any{"path": "http://u1", "time-pos": 5.0, "pause": false})

	w := NewWindowReporter(jf, player.Path)
	w.Track("http://u1", jellyfin.Item{ID: "movie-1"}, 0)
	w.Start()
	first := waitFor(t, fake, "movie-1 progress", func(e jftest.Event) bool {
		return e.Kind == "progress" && e.ItemID == "movie-1" && e.Ticks == 5*tps
	})

	// e queues a second item; the playlist advances to it by itself
	w.Track("http://u2", jellyfin.Item{ID: "movie-2"}, 30*tps)
	player.Set("time-pos", 40.0)
	player.Set("path", "http://u2")
	second := waitFor(t, fake, "movie-2 progress", func(e jftest.Event) bool {
		return e.Kind == "progress" && e.ItemID == "movie-2" && e.Ticks == 40*tps
	})
	if first.SessionID == second.SessionID {
		t.Error("each item needs its own play session")
	}

	// the playlist runs out and the window idles
	player.Set("path", nil)
	waitFor(t, fake, "movie-2 stopped", func(e jftest.Event) bool { return e.Kind == "stopped" && e.ItemID == "movie-2" })
	w.Stop()

	want := "start:movie-1 progress:movie-1 stopped:movie-1 start:movie-2 progress:movie-2 stopped:movie-2"
	if got := kinds(fake.Events()); got != want {
		t.Errorf("reports:\n got %s\nwant %s", got, want)
	}
	for _, e := range fake.Events() {
		if e.Kind == "start" && e.ItemID == "movie-2" && e.Ticks != 30*tps {
			t.Errorf("movie-2 should start from its resume point, got %d", e.Ticks)
		}
	}
}

func TestSubCacheFetchesOnce(t *testing.T) {
	_, jf := fakeJellyfin(t)
	var cache SubCache
	urls := cache.For(jf, jellyfin.Item{ID: "movie-2"})
	if len(urls) != 1 || !strings.Contains(urls[0], "/Subtitles/2/0/Stream.srt") {
		t.Fatalf("urls = %v", urls)
	}
	jf.Token = "revoked" // any further request would now fail
	if again := cache.For(jf, jellyfin.Item{ID: "movie-2"}); len(again) != 1 {
		t.Errorf("second lookup went back to the server: %v", again)
	}
	if failed := cache.For(jf, jellyfin.Item{ID: "movie-1"}); failed != nil {
		t.Errorf("a failed lookup gave %v", failed)
	}
	jf.Token = jftest.Token
	if retried := cache.For(jf, jellyfin.Item{ID: "movie-1"}); retried == nil {
		t.Error("a failed lookup must not be cached as 'no subtitles'")
	}
}

// -- the real thing: mpv itself, headless --------------------------------------

// realPlayer builds a Terminal around the real mpv binary with no video or
// audio output, reading streams from the fake server. The test is skipped
// where mpv or ffmpeg is missing.
func realPlayer(t *testing.T, fake *jftest.Server, jf *jellyfin.Client, req Request) (*Terminal, *os.File) {
	t.Helper()
	bin, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv is not installed")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed (needed to make a test clip)")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mkv")
	gen := exec.Command(ffmpeg, "-v", "error",
		"-f", "lavfi", "-i", "testsrc=duration=120:size=160x90:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=330:duration=120",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "10", "-c:a", "aac", "-b:a", "32k", "-shortest", clip)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not make a test clip: %v: %s", err, out)
	}
	fake.VideoPath = clip
	// keep the user's own mpv settings and speakers out of it
	if err := os.WriteFile(filepath.Join(dir, "mpv.conf"), []byte("ao=null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MPV_HOME", dir)
	conf := filepath.Join(dir, "input.conf")
	mpv.WriteInputConf(conf)

	req.JF = jf
	req.MPV = mpv.Options{Bin: bin, InputConf: conf, UserAgent: "jterm-test", VO: "null"}
	term := NewTerminal(req)
	screen, err := os.Create(filepath.Join(dir, "screen"))
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { screen.Close(); devnull.Close() })
	term.SetStdin(devnull)
	term.SetStdout(screen)
	return term, screen
}

// control connects to whichever mpv the Terminal is currently running.
func control(t *testing.T, term *Terminal) *mpv.Conn {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		c, err := mpv.Dial(term.sock)
		if err != nil {
			continue
		}
		if pos, _ := c.Get("time-pos", time.Second); pos != nil {
			t.Cleanup(c.Close)
			return c
		}
		c.Close()
	}
	t.Fatal("mpv never became ready")
	return nil
}

func finish(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after mpv quit")
	}
}

// sessionsClosed checks that every play session opened was also closed, in
// order: the property that keeps resume points right across relaunches.
func sessionsClosed(t *testing.T, events []jftest.Event) {
	t.Helper()
	open := ""
	for _, e := range events {
		switch e.Kind {
		case "start":
			if open != "" {
				t.Errorf("session %s started before %s was stopped", e.SessionID, open)
			}
			open = e.SessionID
		case "progress":
			if e.SessionID != open {
				t.Errorf("progress for session %s while %q was open", e.SessionID, open)
			}
		case "stopped":
			if e.SessionID != open {
				t.Errorf("stopped session %s while %q was open", e.SessionID, open)
			}
			open = ""
		}
	}
	if open != "" {
		t.Errorf("session %s was never stopped", open)
	}
}

func TestTerminalEpisodeStepAndQualitySwitch(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	eps, err := jf.Episodes("series-1", "")
	if err != nil {
		t.Fatal(err)
	}
	term, screen := realPlayer(t, fake, jf, Request{Item: eps[0], Cap: config.Source})
	done := make(chan error, 1)
	go func() { done <- term.Run() }()

	// n: step to the next episode
	waitFor(t, fake, "the pilot to start", func(e jftest.Event) bool { return e.Kind == "start" && e.ItemID == "ep-1-1" })
	control(t, term).Command("set_property", mpv.ReqProp, "next")
	waitFor(t, fake, "episode 2 to start", func(e jftest.Event) bool {
		return e.Kind == "start" && e.ItemID == "ep-1-2" && e.Method == jellyfin.DirectPlay
	})

	// Ctrl+Down: one rung down the ladder means a transcode at 1080
	c := control(t, term)
	c.Command("seek", 30, "absolute")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if pos, _ := c.Get("time-pos", time.Second); pos != nil && pos.(float64) >= 30 {
			break
		}
	}
	c.Command("set_property", mpv.ReqProp, "down")
	transcode := waitFor(t, fake, "the transcode to start", func(e jftest.Event) bool {
		return e.Kind == "start" && e.Method == jellyfin.Transcode
	})
	if transcode.ItemID != "ep-1-2" || transcode.Ticks < 30*tps || transcode.Ticks > 40*tps {
		t.Errorf("transcode started as %+v; want episode 2 at about 30 s, where playback was", transcode)
	}

	control(t, term).Command("quit")
	finish(t, done)

	res := term.Result()
	if !slices.Equal(res.PlayedIDs, []string{"ep-1-1", "ep-1-2"}) {
		t.Errorf("PlayedIDs = %v", res.PlayedIDs)
	}
	if res.Cap != 1080 {
		t.Errorf("Cap = %v; want the new cap handed back so it can be remembered", res.Cap)
	}
	events := fake.Events()
	sessionsClosed(t, events)
	last := events[len(events)-1]
	if last.Kind != "stop-encoding" || last.SessionID != transcode.SessionID {
		t.Errorf("the transcode job should be cancelled last, got %+v", last)
	}
	if _, err := os.Stat(term.sock); !os.IsNotExist(err) {
		t.Error("the IPC socket was left behind")
	}
	drawn, _ := os.ReadFile(screen.Name())
	// not a terminal, so the footer is cut at the 80-column fallback width
	for _, want := range []string{"Test Show S01E01", "Test Show S01E02", "(≤1080p)", "loading… (source)"} {
		if !strings.Contains(string(drawn), want) {
			t.Errorf("the footer never showed %q", want)
		}
	}
}

func TestTerminalFallsBackWhenTranscodeIsRefused(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	fake.FailTranscode = true
	movie, _ := jf.Item("movie-1")
	term, screen := realPlayer(t, fake, jf, Request{Item: movie, StartTicks: 20 * tps, Cap: 720})
	done := make(chan error, 1)
	go func() { done <- term.Run() }()

	direct := waitFor(t, fake, "direct play after the failed transcode", func(e jftest.Event) bool {
		return e.Kind == "start" && e.Method == jellyfin.DirectPlay
	})
	if direct.Ticks != 20*tps {
		t.Errorf("fallback started at %d; want the original 20 s", direct.Ticks)
	}
	control(t, term).Command("quit")
	finish(t, done)

	if cap := term.Result().Cap; cap != config.Source {
		t.Errorf("Cap = %v; want source, so the next play does not fail the same way", cap)
	}
	sessionsClosed(t, fake.Events())
	if drawn, _ := os.ReadFile(screen.Name()); !strings.Contains(string(drawn), "transcode failed — back to source quality") {
		t.Error("the footer never explained the fallback")
	}
}

func TestWindowReplaceEnqueueAndStop(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	// borrow the headless mpv setup; with no video output no window opens
	term, _ := realPlayer(t, fake, jf, Request{})
	opts := term.req.MPV
	movie1, _ := jf.Item("movie-1")
	movie2, _ := jf.Item("movie-2")
	started := func(id string) func(jftest.Event) bool {
		return func(e jftest.Event) bool { return e.Kind == "start" && e.ItemID == id }
	}

	var win Window
	if got := win.Stop(); got != "no window player is running" {
		t.Errorf("Stop with no window: %q", got)
	}
	// e with no window open simply opens one
	if got := win.Play(jf, opts, movie1, 0, true); got != "now playing in window: Big Buck Bunny — browsing stays live" {
		t.Errorf("first Play: %q", got)
	}
	waitFor(t, fake, "movie-1 in the window", started("movie-1"))
	if win.NowPlaying() != "Big Buck Bunny" {
		t.Errorf("NowPlaying = %q", win.NowPlaying())
	}

	// o on another item swaps it into the same window, from its resume point
	pid := win.cmd.Process.Pid
	if got := win.Play(jf, opts, movie2, 40*tps, false); got != "now playing in window: Sintel — browsing stays live" {
		t.Errorf("replace: %q", got)
	}
	second := waitFor(t, fake, "movie-2 in the window", started("movie-2"))
	if second.Ticks != 40*tps {
		t.Errorf("movie-2 started at %d; want its resume point", second.Ticks)
	}
	if win.cmd.Process.Pid != pid {
		t.Error("replace opened a second window instead of reusing the first")
	}

	// e queues without interrupting; the queue is reached when the playlist advances
	if got := win.Play(jf, opts, movie1, 0, true); got != "queued in window: Big Buck Bunny — browsing stays live" {
		t.Errorf("enqueue: %q", got)
	}
	if win.NowPlaying() != "Sintel" {
		t.Errorf("queueing changed NowPlaying to %q", win.NowPlaying())
	}
	c, err := mpv.Dial(win.sock)
	if err != nil {
		t.Fatal(err)
	}
	c.Command("playlist-next")
	_, _ = c.Get("pid", time.Second)
	c.Close()
	first := waitFor(t, fake, "movie-1's first session", started("movie-1"))
	waitFor(t, fake, "the queued item to start", func(e jftest.Event) bool {
		return e.Kind == "start" && e.ItemID == "movie-1" && e.SessionID != first.SessionID
	})

	if got := win.Stop(); got != "stopped the window player" {
		t.Errorf("Stop: %q", got)
	}
	if win.NowPlaying() != "" {
		t.Error("the window still claims to be playing after Stop")
	}
	events := fake.Events()
	sessionsClosed(t, events)
	want := "start:movie-1 progress:movie-1 stopped:movie-1 start:movie-2 progress:movie-2 stopped:movie-2 " +
		"start:movie-1 progress:movie-1 stopped:movie-1"
	if got := kinds(events); got != want {
		t.Errorf("reports:\n got %s\nwant %s", got, want)
	}
}

func TestTerminalAudio(t *testing.T) {
	fastReports(t)
	fake, jf := fakeJellyfin(t)
	movie, _ := jf.Item("movie-1")
	term, screen := realPlayer(t, fake, jf, Request{Item: movie, StartTicks: 15 * tps, Audio: true, Cap: 720})
	done := make(chan error, 1)
	go func() { done <- term.Run() }()

	start := waitFor(t, fake, "audio to start", func(e jftest.Event) bool { return e.Kind == "start" })
	// audio always direct-plays, whatever the video cap
	if start.Method != jellyfin.DirectPlay || start.Ticks != 15*tps {
		t.Errorf("audio started as %+v", start)
	}
	control(t, term).Command("quit")
	finish(t, done)

	sessionsClosed(t, fake.Events())
	if !slices.Equal(term.Result().PlayedIDs, []string{"movie-1"}) {
		t.Errorf("PlayedIDs = %v", term.Result().PlayedIDs)
	}
	drawn, _ := os.ReadFile(screen.Name())
	if !strings.Contains(string(drawn), "audio only │ resuming from 0:15") {
		t.Errorf("banner missing: %q", drawn)
	}
}
