# jterm

Browse and stream your Jellyfin library in the terminal.

Sister project of [yterm](https://github.com/h0bnobs/yterm) — same idea, but
pointed at your own Jellyfin server instead of YouTube, and as seamless as the
mobile app: auto-discovery, remembered sign-in, resume points, watched-state
sync and next/previous episode keys.

## Install

```
go install github.com/h0bnobs/jterm@latest
```

That puts a single `jterm` binary in `~/go/bin`; make sure it is on your
`PATH`. Building needs Go 1.26, which any Go from 1.21 on fetches by itself.
Playing needs [mpv](https://mpv.io) (`apt install mpv` or similar).

From a checkout, `go build` leaves the binary at `./jterm`.

## First run

jterm broadcasts on your network and finds your Jellyfin server by itself —
if there is exactly one, it connects without asking. Sign in once with your
Jellyfin username and password; the password is exchanged for a session token
and only the token is stored (`~/.config/jterm/config.json`, mode 600). You
will not be asked again until the server revokes the token.

## Home screen

Just like the mobile app, the home screen shows:

- **Continue** — partially watched items, Enter resumes where you left off
- **Next Up** — the next unwatched episode of each show you are watching
- **Libraries** — Movies, Shows, Collections… drill in with Enter
- **Latest** — recent additions to each library

## Keys

| Key      | Action                                                   |
|----------|----------------------------------------------------------|
| `/`      | Search your whole library                                |
| `↑`/`↓`  | Move through the list                                    |
| `Enter`  | Open folder / play (resumes where you left off)          |
| `b`      | Play from the beginning                                  |
| `a`      | Play audio only                                          |
| `o`      | Play in the reusable mpv window — browsing continues     |
| `e`      | Enqueue after what is playing in the window              |
| `x`      | Stop the window player                                   |
| `w`      | Toggle watched / unwatched                               |
| `f`      | Toggle favourite                                         |
| `d`      | Toggle GPU / hardware decoding (on by default)           |
| `Esc`    | Back (also jumps from the search box to the list)        |
| `g`      | Home screen                                              |
| `Ctrl+r` | Refresh the current view                                 |
| `s`      | Server / account menu (switch user or server)            |
| `?`      | Help screen with everything above                        |
| `q`      | Quit (`Ctrl+c` works anywhere)                           |

## Stays in sync

While anything plays, jterm talks to mpv over its IPC socket and reports the
position to Jellyfin every few seconds — exactly what the official clients do.
That means:

- stop a video in jterm, pick it up on your phone at the same spot (and vice
  versa — Continue Watching positions from other devices appear in jterm)
- finished items are ticked watched automatically and Next Up advances
- sync survives quality switches and `n`/`b` episode jumps — every reload
  gets its own play session at the right position
- when playback ends (or you press `q`) you land back on the library or
  collection you were browsing — or Home if that is where you came from

## Playback control centre

During playback a fixed, coloured two-row control footer sits at the bottom
of the pane: position / duration / volume / live resolution / quality cap /
subtitle state plus the key hints (`q` quit, `space` pause, `←/→` seek 5 s,
`↑/↓` volume, `Ctrl+↑/↓` quality, `n`/`b` next/previous episode, `v`
subtitles on/off, `j`/`J` cycle subtitle track, `m` mute, `[ ]` speed). The
footer is updated live over mpv's IPC socket, and the video is kept out of
its rows with a reserved bottom margin. The footer is pinned to the real
terminal bottom read straight from the tty, so it stays put and can never be
drawn over — including when the pane is split or resized mid-playback.

Subtitles match what Jellyfin shows. Embedded tracks travel inside the
stream, and external tracks (sidecar files the server lists but which are
not muxed into the original file) are side-loaded from Jellyfin's subtitle
endpoint, so they are no longer missing in the terminal. `v` toggles them on
and off, `j`/`J` cycle the track, and the footer shows the current state.
One limit: below source quality the server's stream carries only video and
audio, so embedded tracks are unavailable there and only external ones show.

While an episode of a show is playing, `n` jumps to the next episode and
`b` to the previous one (`Ctrl+→`/`Ctrl+←` work too), reloading in place at
that episode's resume point — entirely manual, nothing ever autoplays. For
movies there is no sequence and the footer says so. These keys live inside
mpv, so they never clash with the browser's `b` (play from the beginning).

## Video output and quality

jterm picks the best mpv video output for your terminal: `kitty` (kitty
graphics protocol, full pixel resolution) when running in kitty, otherwise
`tct` true-colour half-blocks which work in any terminal. Override with
`JTERM_VO=kitty|tct jterm`.

By default playback is direct play — the original file is streamed untouched
(seekable via HTTP byte ranges), so nothing is transcoded on the server. For
sharp video run jterm inside kitty, or press `o` for a real mpv window.

`Ctrl+↑/↓` during playback steps a quality cap across source / 1080 / 720 /
480 / 360. Anything below source asks the server to transcode (h264/aac at a
bitrate mapped from the cap), reloading the stream in place at the current
position; the choice is remembered for next time.

A transcode is requested as HLS, the way the official clients do it, so the
whole item stays on its real timeline: you can seek anywhere in it, in both
directions, and a quality switch carries on from where the picture
was. Each seek takes a couple of seconds while the server restarts the
encode at the new position; direct play seeks instantly.

Notes: transcoding costs server CPU; some servers pick the transcode
resolution from the bitrate rather than the requested height, so trust the
footer's live resolution readout; and if a transcode fails outright jterm
falls back to source quality with a note in the footer.

## Browse while playing

In-terminal playback (`Enter`) hands the terminal to mpv, so to keep
browsing press `o` instead: it plays in a single reusable mpv window and the
TUI stays fully live. Press `o` on another result to swap it into the same
window, `e` to queue it after the current one, `x` to stop. The status bar
shows what the window is playing, and progress keeps syncing to Jellyfin
for whichever item that is — including queued items as the playlist
advances.

## GPU / hardware decoding

`d` toggles hardware decoding (`--hwdec=auto-safe`) on or off; the choice is
remembered in the config and shown in the status line (`decode cpu`/`gpu`).
It is on by default because terminal playback is CPU bound on decode and the
GPU takes that load off the main thread — roughly a third less CPU on a
1080p stream, which is the main lever against laggy playback. `auto-safe`
only enables decoders known to be reliable and falls back to software by
itself, so it is safe to leave on. Turn it off only if a particular
machine's hardware decoder misbehaves.

## Environment

| Variable               | Effect                                                    |
|------------------------|-----------------------------------------------------------|
| `JTERM_VO`             | mpv video output, `kitty` or `tct`, instead of detecting  |
| `JTERM_CONFIG_DIR`     | where the config lives, instead of `~/.config/jterm`      |
| `JTERM_DISCOVERY_ADDR` | `host:port` to send the discovery datagram to, instead of the local broadcast address — another subnet's broadcast address, say |

## Internals

- Go, with [Bubble Tea](https://github.com/charmbracelet/bubbletea) for the
  TUI; everything that talks to Jellyfin or mpv uses only the standard library
- `internal/jellyfin` — API client, and server discovery over Jellyfin's UDP
  broadcast protocol on port 7359
- `internal/mpv` — JSON IPC client, mpv command lines, the control footer
- `internal/player` — the in-terminal playback loop, audio, the window
  player, and the reporters that post
  `/Sessions/Playing[/Progress|/Stopped]` to the server
- `internal/ui` — the browser: page stack, table, search, modal boxes
- `internal/config` — `~/.config/jterm/config.json` (server address, device
  id, user id, session token — never your password)
- Playback: the browser hands the terminal to system mpv; one loop polls
  mpv's IPC socket to draw the footer and act on forwarded keys, while a
  reporter mirrors progress to the server over a second connection

## Development

```
go test ./...
```

The playback tests drive the real mpv binary with no video or audio output,
and need ffmpeg to make a test clip; they are skipped where either is
missing.

To try the TUI without a real server, `internal/jftest/fakejf` serves a small
in-memory Jellyfin (two films, one series):

```
go run ./internal/jftest/fakejf -video some.mkv
JTERM_CONFIG_DIR=$(mktemp -d) JTERM_DISCOVERY_ADDR=127.0.0.1:17359 jterm
```

Sign in as `demo` / `demo`.

jterm began as a single Python file built on Textual. That version is at the
`python-final` tag.
