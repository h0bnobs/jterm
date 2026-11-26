package mpv

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReqProp is the mpv user-data property the key bindings below write to.
// It is how a keypress inside mpv reaches the jterm loop that owns it.
const ReqProp = "user-data/jterm/req"

// InputConf is jterm's mpv key bindings. Up/Down adjust volume by 5%;
// Ctrl+Up/Down change the quality cap by signalling jterm through ReqProp;
// n/b step to the next/previous episode the same way (Ctrl+Right/Left as
// alternates for terminals that eat plain letters); v toggles subtitles and
// j/J cycle the subtitle track. Everything else keeps mpv defaults. These
// bindings only exist inside mpv during playback, so they never clash with
// the browser's keys.
const InputConf = "UP add volume 5\n" +
	"DOWN add volume -5\n" +
	"Ctrl+UP set user-data/jterm/req up\n" +
	"Ctrl+DOWN set user-data/jterm/req down\n" +
	"n set user-data/jterm/req next\n" +
	"b set user-data/jterm/req prev\n" +
	"Ctrl+RIGHT set user-data/jterm/req next\n" +
	"Ctrl+LEFT set user-data/jterm/req prev\n" +
	"v cycle sub-visibility\n" +
	"j cycle sub\n" +
	"J cycle sub down\n"

// AudioStatus is mpv's own status line, used for audio-only playback where
// there is no video for a footer to sit under.
const AudioStatus = "${?pause==yes:⏸ }${!pause==yes:▶ }" +
	"${time-pos} / ${duration} (${percent-pos}%)  vol ${volume}" +
	" │ q quit · spc pause · ←/→ 5s · ↑/↓ vol · m mute · [ ] speed"

// WriteInputConf writes the key bindings to path. A failure is ignored:
// mpv then simply runs with its default keys.
func WriteInputConf(path string) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, []byte(InputConf), 0o644)
}

// Options are the settings every mpv launch shares.
type Options struct {
	Bin       string // path to the mpv binary
	InputConf string // path to the key bindings file
	UserAgent string
	VO        string // terminal video output: kitty or tct
	HWDec     bool
}

func (o Options) base(sock string) []string {
	return []string{
		"--osc=no", "--msg-level=all=error,statusline=status",
		"--term-osd-bar=no", "--force-seekable=yes",
		"--input-conf=" + o.InputConf,
		"--input-ipc-server=" + sock,
		"--user-agent=" + o.UserAgent,
	}
}

func withStart(args []string, startSeconds int64, url string) []string {
	if startSeconds > 0 {
		args = append(args, fmt.Sprintf("--start=%d", startSeconds))
	}
	return append(args, url)
}

// Video is the in-terminal command line with the footer band reserved.
//
// --video-margin-ratio-bottom physically keeps the video out of the
// footer's rows (unlike --vo-kitty-rows, which only overrides mpv's size
// detection and lets the image draw over the band anyway). External
// subtitle tracks are side-loaded with --sub-file, since the direct-play
// stream of the original file does not carry them.
func (o Options) Video(sock, url string, startSeconds int64, termLines int, subs []string) []string {
	args := append(o.base(sock),
		"--vo="+o.VO,
		"--term-status-msg=",
		fmt.Sprintf("--video-margin-ratio-bottom=%.4f", MarginRatio(termLines)),
	)
	// With GPU decoding on, mpv picks a safe hardware decoder; otherwise
	// the fast software profile keeps CPU scaling cheap for terminal output.
	if o.HWDec {
		args = append(args, "--hwdec=auto-safe")
	} else {
		args = append(args, "--profile=sw-fast")
	}
	if o.VO == "kitty" {
		args = append(args, "--vo-kitty-use-shm=yes")
	}
	for _, sub := range subs {
		args = append(args, "--sub-file="+sub)
	}
	return withStart(args, startSeconds, url)
}

// Audio is the audio-only command line, with mpv's own status line.
func (o Options) Audio(sock, url string, startSeconds int64) []string {
	args := append(o.base(sock), "--term-status-msg="+AudioStatus, "--no-video")
	return withStart(args, startSeconds, url)
}

// Window is the command line for the reusable window player. It idles
// between videos instead of exiting, so later items can be loaded over IPC.
func (o Options) Window(sock, url, title string, startSeconds int64) []string {
	args := o.base(sock)
	if o.HWDec {
		args = append(args, "--hwdec=auto-safe")
	}
	args = append(args, "--idle=yes", "--force-window=yes", "--title="+WindowTitle(title))
	return withStart(args, startSeconds, url)
}

// WindowTitle is the window player's title bar text for an item.
func WindowTitle(title string) string {
	return "jterm ▶ " + title
}
