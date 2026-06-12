# jterm

Browse and stream your Jellyfin library in the terminal.

Sister project of [yterm](https://github.com/h0bnobs/yterm) — same idea, but
pointed at your own Jellyfin server instead of YouTube, and as seamless as the
mobile app: auto-discovery, remembered sign-in, resume points, watched-state
sync and episode autoplay.

## Install

```
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
```

mpv must be installed (`apt install mpv` or similar). Then run `./jterm`
(symlink it into `~/.local/bin` if you like).

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
| `o`      | Open in an mpv window — browsing continues               |
| `w`      | Toggle watched / unwatched                               |
| `f`      | Toggle favourite                                         |
| `d`      | Toggle GPU / hardware decoding (off by default)          |
| `Esc`    | Back (also jumps from the search box to the list)        |
| `g`      | Home screen                                              |
| `Ctrl+r` | Refresh the current view                                 |
| `s`      | Server / account menu (switch user or server)            |
| `?`      | Help screen with everything above                        |
| `q`      | Quit                                                     |

## Stays in sync

While anything plays, jterm talks to mpv over its IPC socket and reports the
position to Jellyfin every few seconds — exactly what the official clients do.
That means:

- stop a video in jterm, pick it up on your phone at the same spot (and vice
  versa — Continue Watching positions from other devices appear in jterm)
- finished items are ticked watched automatically and Next Up advances
- when playback ends (or you press `q`) you land back on the library or
  collection you were browsing — or Home if that is where you came from

## Playback control centre

During playback a fixed, coloured two-row control footer sits at the bottom
of the pane: position / duration / volume / live resolution / quality cap
plus the key hints (`q` quit, `space` pause, `←/→` seek 5 s, `↑/↓` seek
1 min, `Ctrl+↑/↓` quality, `9/0` volume, `m` mute, `[ ]` speed). The footer
is updated live over mpv's IPC socket, and the video is kept out of its
rows with a reserved bottom margin, so it can never be drawn over —
including when the pane is resized mid-playback.

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
position; the choice is remembered for next time. Notes: transcoding costs
server CPU; some servers (Jellyfin 10.11 among them) pick the transcode
resolution from the bitrate rather than the requested height, so trust the
footer's live resolution readout; seeking far ahead of a transcode can stall
while the server catches up; and if a transcode fails outright jterm falls
back to source quality with a note in the footer.

## GPU / hardware decoding

`d` toggles hardware decoding (`--hwdec=auto-safe`) on or off; the choice is
remembered in the config and shown in the status line (`decode cpu`/`gpu`).
It lowers CPU during decode and helps the `o` window most — for in-terminal
video the decoded frames still copy back to the CPU to be drawn through the
graphics protocol, so the gain there is smaller. Off by default: turn it on
if playback is choppy or the CPU runs hot.

## Internals

- Python + Textual TUI, stdlib-only Jellyfin client: `jterm.py`
- Server discovery: Jellyfin UDP broadcast protocol on port 7359
- Playback: system mpv suspending the TUI; a sidecar thread polls mpv's JSON
  IPC socket and posts `/Sessions/Playing[/Progress|/Stopped]` to the server
- Config: `~/.config/jterm/config.json` (server address, device id, user id,
  session token — never your password)
