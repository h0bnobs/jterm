// jterm browses and streams a Jellyfin library in the terminal.
//
// It finds Jellyfin servers on the local network, signs in once and
// remembers the session token, then shows a mobile-app style home screen:
// Continue Watching, Next Up, your libraries and the latest additions.
// Enter streams the selection inside the terminal through mpv (the kitty
// graphics protocol where available, ANSI half-blocks otherwise); playback
// progress is reported back to the server every few seconds so resume
// positions and watched state stay in sync with every other Jellyfin client.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/h0bnobs/jterm/internal/config"
	"github.com/h0bnobs/jterm/internal/jellyfin"
	"github.com/h0bnobs/jterm/internal/ui"
)

// detectVideoOutput picks the best mpv --vo for this terminal. Override
// with JTERM_VO.
func detectVideoOutput() string {
	if override := os.Getenv("JTERM_VO"); override != "" {
		return override
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || strings.Contains(os.Getenv("TERM"), "kitty") {
		return "kitty"
	}
	return "tct" // true-colour half-blocks, works everywhere
}

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		fmt.Println(jellyfin.ClientName, jellyfin.ClientVersion)
		return
	}
	if _, err := tea.NewProgram(ui.New(config.Load(), detectVideoOutput())).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "jterm:", err)
		os.Exit(1)
	}
}
