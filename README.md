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
- when an episode ends, the next one starts after a 3-second countdown
  (Ctrl-C to stay put)

## Playback control centre

During playback the video fills the pane from the top and a constantly
redrawn control bar sits directly under it: position / duration / volume
plus the key hints (`q` quit, `space` pause, `←/→` seek 5 s, `↑/↓` seek
1 min, `9/0` volume, `m` mute, `[ ]` speed).

## Video output and quality

jterm picks the best mpv video output for your terminal: `kitty` (kitty
graphics protocol, full pixel resolution) when running in kitty, otherwise
`tct` true-colour half-blocks which work in any terminal. Override with
`JTERM_VO=kitty|tct jterm`.

Playback is always direct play — the original file is streamed untouched
(seekable via HTTP byte ranges), so nothing is transcoded on the server. For
sharp video run jterm inside kitty, or press `o` for a real mpv window.

## Internals

- Python + Textual TUI, stdlib-only Jellyfin client: `jterm.py`
- Server discovery: Jellyfin UDP broadcast protocol on port 7359
- Playback: system mpv suspending the TUI; a sidecar thread polls mpv's JSON
  IPC socket and posts `/Sessions/Playing[/Progress|/Stopped]` to the server
- Config: `~/.config/jterm/config.json` (server address, device id, user id,
  session token — never your password)
