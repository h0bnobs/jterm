package mpv_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/h0bnobs/jterm/internal/mpv"
	"github.com/h0bnobs/jterm/internal/mpv/mpvtest"
)

const wait = 400 * time.Millisecond

// Expected values in this file were produced by running the Python jterm's
// own functions on the same inputs.

func TestMarginRatio(t *testing.T) {
	for lines, want := range map[int]float64{
		1: 0.6667, 2: 0.6667, 3: 0.6667, 17: 0.1176, 24: 0.0833, 30: 0.0667, 50: 0.04, 200: 0.01,
	} {
		if got := mpv.MarginRatio(lines); got != want {
			t.Errorf("MarginRatio(%d) = %v; want %v", lines, got, want)
		}
	}
}

func TestClock(t *testing.T) {
	for secs, want := range map[float64]string{
		0: "0:00", 59.9: "0:59", 65: "1:05", 3599: "59:59", 3661: "1:01:01", 36000: "10:00:00",
	} {
		if got := mpv.Clock(secs); got != want {
			t.Errorf("Clock(%v) = %q; want %q", secs, got, want)
		}
	}
}

func f(v float64) *float64 { return &v }

func TestFooterLines(t *testing.T) {
	const title = "Show S01E02 · Pilot"
	hints := " q quit · spc pause · ←/→ 5s · ↑/↓ vol · ⌃↑/↓ quality · n/b ep · v subs · j track · m mute"
	cases := []struct {
		name string
		st   mpv.State
		want string
	}{
		{"nothing known yet", mpv.State{},
			" ▶ 0:00 / ? (0%)   vol ?   … (source)   no subs   " + title},
		{"playing at source", mpv.State{TimePos: f(75.6), Duration: 600, PercentPos: f(12.6), Volume: f(100),
			Width: 1920, Height: 1080, HasSub: true, CapLabel: "source"},
			" ▶ 1:15 / 10:00 (12%)   vol 100   1920x1080 (source)   subs on   " + title},
		{"paused, capped, subs hidden", mpv.State{Pause: true, TimePos: f(3700.2), Duration: 7200, PercentPos: f(51.39),
			Volume: f(55), Width: 1280, Height: 720, HasSub: true, SubHidden: true, CapLabel: "≤720p"},
			" ⏸ 1:01:40 / 2:00:00 (51%)   vol 55   1280x720 (≤720p)   subs off   " + title},
		{"zeros are values, not gaps", mpv.State{TimePos: f(0), PercentPos: f(0), Volume: f(0), CapLabel: "≤360p"},
			" ▶ 0:00 / ? (0%)   vol 0   … (≤360p)   no subs   " + title},
	}
	for _, c := range cases {
		got := mpv.FooterLines(c.st, title)
		if len(got) != 2 || got[0] != c.want || got[1] != hints {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, []string{c.want, hints})
		}
	}
}

func TestDrawFooterFillsExactlyTheBottomRows(t *testing.T) {
	var buf bytes.Buffer
	// a title of double-width characters: slicing by byte or by rune would
	// both overflow the row
	lines := mpv.FooterLines(mpv.State{}, strings.Repeat("映画", 40))
	mpv.DrawFooter(&buf, lines, 30, 40)
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b[?7l") || !strings.HasSuffix(out, "\x1b[?7h\x1b[H") {
		t.Errorf("autowrap must be off while painting and the cursor parked after: %q", out)
	}
	for i, row := range []string{"\x1b[29;1H", "\x1b[30;1H"} {
		start := strings.Index(out, row)
		if start < 0 {
			t.Fatalf("row %d not positioned: %q", i, out)
		}
		cell := out[start+len(row):]
		cell = cell[:strings.Index(cell, "\x1b[0m")]
		if w := ansi.StringWidth(cell); w != 40 {
			t.Errorf("row %d is %d cells wide; want exactly 40", i, w)
		}
	}
}

func TestCommandLines(t *testing.T) {
	o := mpv.Options{Bin: "mpv", InputConf: "/c/input.conf", UserAgent: "jterm/0.2.0", VO: "kitty", HWDec: true}
	base := []string{
		"--osc=no", "--msg-level=all=error,statusline=status", "--term-osd-bar=no",
		"--force-seekable=yes", "--input-conf=/c/input.conf",
		"--input-ipc-server=/tmp/s.sock", "--user-agent=jterm/0.2.0",
	}
	with := func(extra ...string) []string { return append(append([]string(nil), base...), extra...) }

	got := o.Video("/tmp/s.sock", "http://u", 75, 30, []string{"http://sub"})
	want := with("--vo=kitty", "--term-status-msg=", "--video-margin-ratio-bottom=0.0667",
		"--hwdec=auto-safe", "--vo-kitty-use-shm=yes", "--sub-file=http://sub", "--start=75", "http://u")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Video:\n got %q\nwant %q", got, want)
	}

	o.VO, o.HWDec = "tct", false
	got = o.Video("/tmp/s.sock", "http://u", 0, 24, nil)
	want = with("--vo=tct", "--term-status-msg=", "--video-margin-ratio-bottom=0.0833",
		"--profile=sw-fast", "http://u")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Video (software, from the start):\n got %q\nwant %q", got, want)
	}

	got = o.Audio("/tmp/s.sock", "http://u", 9)
	want = with("--term-status-msg="+mpv.AudioStatus, "--no-video", "--start=9", "http://u")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Audio:\n got %q\nwant %q", got, want)
	}

	o.HWDec = true
	got = o.Window("/tmp/s.sock", "http://u", "Heat", 0)
	want = with("--hwdec=auto-safe", "--idle=yes", "--force-window=yes", "--title=jterm ▶ Heat", "http://u")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Window:\n got %q\nwant %q", got, want)
	}
}

func TestConnGet(t *testing.T) {
	fake := mpvtest.Start(t, map[string]any{"time-pos": 12.5, "pause": true, "path": "http://u"})
	c, err := mpv.Dial(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if v, err := c.Get("time-pos", wait); err != nil || v != 12.5 {
		t.Errorf("time-pos = %v, %v", v, err)
	}
	if v, err := c.Get("pause", wait); err != nil || v != true {
		t.Errorf("pause = %v, %v", v, err)
	}
	// unavailable is a nil value, not an error: mpv is still there
	if v, err := c.Get(mpv.ReqProp, wait); err != nil || v != nil {
		t.Errorf("unset property = %v, %v; want nil, nil", v, err)
	}
	// a fire-and-forget command's reply must not be mistaken for the next answer
	c.Command("set_property", mpv.ReqProp, "up")
	if v, _ := c.Get(mpv.ReqProp, wait); v != "up" {
		t.Errorf("after set_property, req = %v; want up", v)
	}
	if cmds := fake.Commands(); len(cmds) != 1 || cmds[0][0] != "set_property" {
		t.Errorf("commands seen by mpv: %v", cmds)
	}
}

func TestConnReportsMpvGone(t *testing.T) {
	fake := mpvtest.Start(t, map[string]any{"time-pos": 1.0})
	c, err := mpv.Dial(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Get("time-pos", wait); err != nil {
		t.Fatal(err)
	}
	fake.Close()
	if _, err := c.Get("time-pos", wait); !errors.Is(err, mpv.ErrClosed) {
		t.Errorf("after mpv exits Get returned %v; want ErrClosed", err)
	}
}

func TestReadState(t *testing.T) {
	fake := mpvtest.Start(t, map[string]any{
		"time-pos": 75.6, "duration": 600.0, "percent-pos": 12.6, "volume": 100.0,
		"pause": false, "width": 1920, "height": 1080, "sub-visibility": false, "sid": 2,
	})
	c, err := mpv.Dial(fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := mpv.ReadState(c, wait)
	if st.TimePos == nil || *st.TimePos != 75.6 || st.Duration != 600 || st.Width != 1920 || st.Height != 1080 {
		t.Errorf("numbers not read: %+v", st)
	}
	if !st.HasSub || !st.SubHidden || st.Pause {
		t.Errorf("flags: %+v; want a hidden subtitle track, not paused", st)
	}
	// sid is false when no track is selected
	fake.Set("sid", false)
	if st := mpv.ReadState(c, wait); st.HasSub {
		t.Error("sid=false should read as no subtitles")
	}
}
