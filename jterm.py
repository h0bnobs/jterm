#!/usr/bin/env python3
"""jterm - browse and stream your Jellyfin library in the terminal.

Auto-discovers Jellyfin servers on the local network, signs in once and
remembers the session token, then gives you a mobile-app style home screen:
Continue Watching, Next Up, your libraries and the latest additions.
Enter streams the selection inside the terminal via mpv (kitty graphics
protocol where available, ANSI half-blocks otherwise); playback progress is
reported back to the server every few seconds so resume positions and
watched state stay in sync with every other Jellyfin client.
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from textual import work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.screen import ModalScreen
from textual.widgets import DataTable, Footer, Input, OptionList, Static
from textual.widgets.option_list import Option

CONFIG_PATH = os.path.expanduser("~/.config/jterm/config.json")
CLIENT_NAME = "jterm"
CLIENT_VERSION = "0.1.0"
TICKS_PER_SECOND = 10_000_000
DISCOVERY_PORT = 7359
DISCOVERY_MESSAGE = b"who is JellyfinServer?"
DISCOVERY_TIMEOUT = 2.0
PROGRESS_INTERVAL = 5.0
PAGE_LIMIT = 200
# The /Users/{id}/Items list endpoint serves UserData from a server-side
# cache that lags writes by 30s or more, while the single-item endpoint is
# always fresh. Freshly learned UserData is overlaid on list responses for
# this long so toggles and playback progress show up immediately.
UD_PATCH_TTL = 300.0

MPV_STATUS = (
    "${?pause==yes:⏸ }${!pause==yes:▶ }"
    "${time-pos} / ${duration} (${percent-pos}%)  vol ${volume}"
    " │ q quit · spc pause · ←/→ 5s · ↑/↓ 1m · 9/0 vol · m mute · [ ] speed"
)

FOLDER_TYPES = {
    "CollectionFolder", "UserView", "Folder", "BoxSet", "Series", "Season",
    "MusicAlbum", "Playlist",
}
# Page sources counted as a library/collection for the end-of-playback
# "return to where you were browsing" behaviour.
LIBRARY_TYPES = {"CollectionFolder", "UserView", "BoxSet"}

HELP_TEXT = """\
[b]Browsing[/b]
  /        search your whole library
  Enter    open folder / play (resumes where you left off)
  b        play from the beginning
  a        play audio only
  o        open in an mpv window (browse continues)
  w        toggle watched / unwatched
  f        toggle favourite
  Esc      back (also jumps from the search box to the list)
  g        home screen
  ctrl+r   refresh the current view
  s        server / account menu
  ?        this help
  q        quit

[b]During playback (mpv owns the terminal)[/b]
  A live control footer sits under the video with position, duration
  and volume — the video can never draw over it, even when resizing.
  q        stop (position is saved to the server)
  space    pause / resume
  ←/→      seek 5 s        ↑/↓   seek 1 min
  9/0      volume          m     mute
  [ / ]    playback speed  ,/.   frame step (paused)

[b]Sync[/b]
  Progress is reported to Jellyfin every few seconds, exactly like the
  mobile app: resume points, watched ticks and Next Up all stay in sync.
  When playback ends you land back on the library or collection you
  were browsing, or Home.
"""


# --------------------------------------------------------------------------
# Terminal video-output detection
# --------------------------------------------------------------------------

def detect_video_output() -> str:
    """Pick the best mpv --vo for this terminal. Override with JTERM_VO."""
    override = os.environ.get("JTERM_VO")
    if override:
        return override
    if os.environ.get("KITTY_WINDOW_ID") or "kitty" in os.environ.get("TERM", ""):
        return "kitty"
    return "tct"  # true-colour half-blocks, works everywhere


# --------------------------------------------------------------------------
# Config (server address, user id, session token — never the password)
# --------------------------------------------------------------------------

def load_config() -> dict:
    try:
        with open(CONFIG_PATH) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_config(cfg: dict) -> None:
    os.makedirs(os.path.dirname(CONFIG_PATH), exist_ok=True)
    fd = os.open(CONFIG_PATH, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(cfg, f, indent=2)


# --------------------------------------------------------------------------
# Jellyfin API client (stdlib only)
# --------------------------------------------------------------------------

class JFError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


def discover_servers(timeout: float = DISCOVERY_TIMEOUT) -> list[dict]:
    """Broadcast the Jellyfin discovery datagram and collect responses."""
    found: dict[str, dict] = {}
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    sock.settimeout(0.5)
    try:
        sock.sendto(DISCOVERY_MESSAGE, ("255.255.255.255", DISCOVERY_PORT))
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                data, _addr = sock.recvfrom(8192)
            except socket.timeout:
                continue
            try:
                info = json.loads(data.decode("utf-8", "replace"))
            except ValueError:
                continue
            if info.get("Address"):
                found[info.get("Id") or info["Address"]] = info
    except OSError:
        pass
    finally:
        sock.close()
    return list(found.values())


class Jellyfin:
    def __init__(self, server: str, device_id: str,
                 token: str | None = None, user_id: str | None = None):
        self.server = server.rstrip("/")
        self.device_id = device_id
        self.token = token
        self.user_id = user_id

    # -- plumbing ----------------------------------------------------------

    def _auth_header(self) -> str:
        device = socket.gethostname() or "terminal"
        parts = [
            f'Client="{CLIENT_NAME}"',
            f'Device="{device}"',
            f'DeviceId="{self.device_id}"',
            f'Version="{CLIENT_VERSION}"',
        ]
        if self.token:
            parts.append(f'Token="{self.token}"')
        return "MediaBrowser " + ", ".join(parts)

    def _request(self, method: str, path: str, params: dict | None = None,
                 body: dict | None = None):
        url = self.server + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Authorization", self._auth_header())
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            raise JFError(exc.code, f"HTTP {exc.code} for {path}") from exc
        except (urllib.error.URLError, OSError) as exc:
            raise JFError(0, f"cannot reach {self.server}: {exc}") from exc
        if not raw:
            return None
        try:
            return json.loads(raw)
        except ValueError:
            return None

    def get(self, path: str, params: dict | None = None):
        return self._request("GET", path, params)

    def post(self, path: str, params: dict | None = None, body: dict | None = None):
        return self._request("POST", path, params, body)

    def delete(self, path: str, params: dict | None = None):
        return self._request("DELETE", path, params)

    # -- auth --------------------------------------------------------------

    def authenticate(self, username: str, password: str) -> dict:
        result = self.post("/Users/AuthenticateByName",
                           body={"Username": username, "Pw": password})
        self.token = result["AccessToken"]
        self.user_id = result["User"]["Id"]
        return result["User"]

    def public_info(self) -> dict:
        return self.get("/System/Info/Public") or {}

    # -- browsing ----------------------------------------------------------

    def views(self) -> list[dict]:
        return (self.get(f"/Users/{self.user_id}/Views") or {}).get("Items", [])

    def resume_items(self, limit: int = 12) -> list[dict]:
        result = self.get(f"/Users/{self.user_id}/Items/Resume", {
            "Limit": limit, "MediaTypes": "Video",
            "Fields": "ProductionYear",
        })
        return (result or {}).get("Items", [])

    def next_up(self, limit: int = 12) -> list[dict]:
        result = self.get("/Shows/NextUp", {
            "UserId": self.user_id, "Limit": limit,
            "Fields": "ProductionYear",
        })
        return (result or {}).get("Items", [])

    def latest(self, parent_id: str, limit: int = 8) -> list[dict]:
        return self.get(f"/Users/{self.user_id}/Items/Latest", {
            "ParentId": parent_id, "Limit": limit,
            "Fields": "ProductionYear",
        }) or []

    def children(self, parent_id: str) -> list[dict]:
        result = self.get(f"/Users/{self.user_id}/Items", {
            "ParentId": parent_id, "SortBy": "SortName",
            "SortOrder": "Ascending", "Limit": PAGE_LIMIT,
            "Fields": "ProductionYear,ChildCount",
        })
        return (result or {}).get("Items", [])

    def seasons(self, series_id: str) -> list[dict]:
        result = self.get(f"/Shows/{series_id}/Seasons", {
            "UserId": self.user_id,
        })
        return (result or {}).get("Items", [])

    def episodes(self, series_id: str, season_id: str | None = None,
                 start_item_id: str | None = None, limit: int | None = None) -> list[dict]:
        params: dict = {"UserId": self.user_id, "Fields": "ProductionYear"}
        if season_id:
            params["SeasonId"] = season_id
        if start_item_id:
            params["StartItemId"] = start_item_id
        if limit:
            params["Limit"] = limit
        result = self.get(f"/Shows/{series_id}/Episodes", params)
        return (result or {}).get("Items", [])

    def search(self, term: str, limit: int = 60) -> list[dict]:
        result = self.get(f"/Users/{self.user_id}/Items", {
            "SearchTerm": term, "Recursive": "true", "Limit": limit,
            "IncludeItemTypes": "Movie,Series,Episode,Video,BoxSet",
            "Fields": "ProductionYear",
        })
        return (result or {}).get("Items", [])

    def item(self, item_id: str) -> dict:
        return self.get(f"/Users/{self.user_id}/Items/{item_id}") or {}

    # -- playback ----------------------------------------------------------

    def stream_url(self, item_id: str) -> str:
        return (f"{self.server}/Videos/{item_id}/stream"
                f"?static=true&api_key={self.token}&deviceId={self.device_id}")

    def report_start(self, item_id: str, session_id: str, ticks: int) -> None:
        self.post("/Sessions/Playing", body={
            "ItemId": item_id, "PlaySessionId": session_id,
            "PositionTicks": ticks, "CanSeek": True,
            "PlayMethod": "DirectPlay",
        })

    def report_progress(self, item_id: str, session_id: str, ticks: int,
                        paused: bool) -> None:
        self.post("/Sessions/Playing/Progress", body={
            "ItemId": item_id, "PlaySessionId": session_id,
            "PositionTicks": ticks, "IsPaused": paused,
            "CanSeek": True, "PlayMethod": "DirectPlay",
        })

    def report_stopped(self, item_id: str, session_id: str, ticks: int) -> None:
        self.post("/Sessions/Playing/Stopped", body={
            "ItemId": item_id, "PlaySessionId": session_id,
            "PositionTicks": ticks,
        })

    # -- user data ---------------------------------------------------------

    def set_played(self, item_id: str, played: bool) -> dict | None:
        path = f"/Users/{self.user_id}/PlayedItems/{item_id}"
        return self.post(path) if played else self.delete(path)

    def set_favourite(self, item_id: str, favourite: bool) -> dict | None:
        path = f"/Users/{self.user_id}/FavoriteItems/{item_id}"
        return self.post(path) if favourite else self.delete(path)


# --------------------------------------------------------------------------
# mpv IPC progress reporter
# --------------------------------------------------------------------------

class PlaybackReporter(threading.Thread):
    """Connects to mpv's IPC socket and mirrors playback state to Jellyfin.

    Polls time-pos/pause every few seconds and sends Playing/Progress/Stopped
    reports, which is what keeps resume positions and watched state in sync
    with the rest of the Jellyfin ecosystem.
    """

    def __init__(self, jf: Jellyfin, item: dict, sock_path: str, start_ticks: int):
        super().__init__(daemon=True)
        self.jf = jf
        self.item = item
        self.sock_path = sock_path
        self.session_id = uuid.uuid4().hex
        self.last_ticks = start_ticks
        self._req_id = 0
        self._halt = threading.Event()

    def stop(self) -> None:
        self._halt.set()

    def _connect(self) -> socket.socket | None:
        # mpv creates the socket shortly after launch; retry briefly
        for _ in range(120):
            if self._halt.is_set():
                return None
            try:
                sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                sock.connect(self.sock_path)
                sock.settimeout(2.0)
                return sock
            except OSError:
                time.sleep(0.25)
        return None

    def _get_property(self, sock: socket.socket, rfile, prop: str):
        self._req_id += 1
        req = json.dumps({"command": ["get_property", prop],
                          "request_id": self._req_id}) + "\n"
        sock.sendall(req.encode())
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            line = rfile.readline()
            if not line:
                raise OSError("mpv ipc closed")
            try:
                msg = json.loads(line)
            except ValueError:
                continue
            if msg.get("request_id") == self._req_id:
                if msg.get("error") == "success":
                    return msg.get("data")
                return None
        return None

    def _report(self, fn, *args) -> None:
        try:
            fn(*args)
        except (JFError, OSError):
            pass  # never let a network blip kill playback

    def run(self) -> None:
        sock = self._connect()
        if sock is None:
            return
        rfile = sock.makefile("r", encoding="utf-8", errors="replace")
        self._report(self.jf.report_start, self.item["Id"], self.session_id,
                     self.last_ticks)
        try:
            while not self._halt.is_set():
                try:
                    pos = self._get_property(sock, rfile, "time-pos")
                    paused = self._get_property(sock, rfile, "pause")
                except (OSError, socket.timeout):
                    break
                if isinstance(pos, (int, float)) and pos > 0:
                    self.last_ticks = int(pos * TICKS_PER_SECOND)
                self._report(self.jf.report_progress, self.item["Id"],
                             self.session_id, self.last_ticks, bool(paused))
                if self._halt.wait(PROGRESS_INTERVAL):
                    break
        finally:
            try:
                rfile.close()
                sock.close()
            except OSError:
                pass
            self._report(self.jf.report_stopped, self.item["Id"],
                         self.session_id, self.last_ticks)


# --------------------------------------------------------------------------
# Formatting helpers
# --------------------------------------------------------------------------

def fmt_duration(ticks) -> str:
    if not ticks:
        return ""
    seconds = int(ticks // TICKS_PER_SECOND)
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    return f"{h}:{m:02}:{s:02}" if h else f"{m}:{s:02}"


def fmt_clock(seconds: float) -> str:
    seconds = int(seconds)
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    return f"{h}:{m:02}:{s:02}" if h else f"{m}:{s:02}"


def episode_code(item: dict) -> str:
    season = item.get("ParentIndexNumber")
    ep = item.get("IndexNumber")
    if season is None or ep is None:
        return ""
    return f"S{season:02}E{ep:02}"


def item_title(item: dict) -> str:
    name = item.get("Name") or "?"
    if item.get("Type") == "Episode":
        code = episode_code(item)
        return f"{code} · {name}" if code else name
    return name


def item_info(item: dict) -> str:
    itype = item.get("Type")
    if itype == "Episode":
        return item.get("SeriesName") or ""
    if itype == "Series":
        bits = []
        if item.get("ProductionYear"):
            bits.append(str(item["ProductionYear"]))
        if item.get("ChildCount"):
            n = item["ChildCount"]
            bits.append(f"{n} season{'s' if n != 1 else ''}")
        return " · ".join(bits)
    if itype == "Season":
        return item.get("SeriesName") or ""
    if itype in ("CollectionFolder", "UserView"):
        return item.get("CollectionType") or "library"
    if itype == "BoxSet":
        n = item.get("ChildCount")
        return f"{n} items" if n else "collection"
    if item.get("ProductionYear"):
        return str(item["ProductionYear"])
    return ""


def item_state(item: dict) -> str:
    ud = item.get("UserData") or {}
    marks = "♥" if ud.get("IsFavorite") else ""
    pos = ud.get("PlaybackPositionTicks") or 0
    runtime = item.get("RunTimeTicks") or 0
    if pos and runtime:
        return f"{marks}▶{int(pos * 100 / runtime)}%"
    if item.get("Type") in ("Series", "Season", "BoxSet"):
        unplayed = ud.get("UnplayedItemCount")
        if unplayed:
            return f"{marks}{unplayed} new"
        if ud.get("Played"):
            return marks + "✓"
        return marks
    if ud.get("Played"):
        return marks + "✓"
    return marks


def is_folder(item: dict) -> bool:
    return item.get("Type") in FOLDER_TYPES or item.get("IsFolder") is True


def resume_ticks(item: dict) -> int:
    return (item.get("UserData") or {}).get("PlaybackPositionTicks") or 0


# --------------------------------------------------------------------------
# In-terminal video layout: a fixed, coloured control footer that mpv is
# kept out of via a reserved bottom video margin (issue #1)
# --------------------------------------------------------------------------

FOOTER_ROWS = 2
FOOTER_BG = "\x1b[48;2;40;46;66m"     # subtle blue-grey, distinct from the bg
FOOTER_FG = "\x1b[38;2;236;236;245m"
KEY_HINTS = "q quit · spc pause · ←/→ 5s · ↑/↓ 1m · 9/0 vol · m mute · [ ] speed"


def footer_margin_ratio(lines: int) -> float:
    """Fraction of the video area to reserve so the footer's rows stay clear.
    Recomputed from the live terminal height so a resize keeps it exact."""
    return round(FOOTER_ROWS / max(lines, FOOTER_ROWS + 1), 4)


def footer_lines(st: dict, title: str, cols: int) -> list[str]:
    """The two text lines shown in the control footer."""
    icon = "⏸" if st.get("pause") else "▶"
    pos = fmt_clock(st["time-pos"]) if st.get("time-pos") is not None else "0:00"
    dur = fmt_clock(st["duration"]) if st.get("duration") else "?"
    pct = f"{int(st['percent-pos'])}%" if st.get("percent-pos") is not None else "0%"
    vol = f"{int(st['volume'])}" if st.get("volume") is not None else "?"
    line1 = f" {icon} {pos} / {dur} ({pct})   vol {vol}   {title}"
    return [line1, " " + KEY_HINTS]


def draw_footer(lines_text: list[str], term_lines: int, cols: int) -> None:
    """Paint the coloured footer band across its reserved bottom rows.
    Autowrap is disabled so filling the final cell never scrolls, and the
    cursor is parked at the top afterwards so any stray mpv output lands
    there instead of scrolling the footer out of its rows."""
    out = ["\x1b[?7l"]
    first = term_lines - len(lines_text) + 1
    for i, text in enumerate(lines_text):
        cell = (text[:cols]).ljust(cols)
        out.append(f"\x1b[{first + i};1H{FOOTER_BG}{FOOTER_FG}{cell}\x1b[0m")
    out.append("\x1b[?7h\x1b[H")
    sys.stdout.write("".join(out))
    sys.stdout.flush()


class MpvIPC:
    """Tiny JSON-IPC client for a running mpv (--input-ipc-server)."""

    def __init__(self, path: str):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.connect(path)
        self.sock.settimeout(0.4)
        self.buf = b""
        self._rid = 0

    def get(self, prop: str):
        self._rid += 1
        rid = self._rid
        try:
            self.sock.sendall(
                json.dumps({"command": ["get_property", prop], "request_id": rid}).encode() + b"\n"
            )
        except OSError:
            return None
        deadline = time.time() + 0.4
        while time.time() < deadline:
            try:
                self.buf += self.sock.recv(65536)
            except socket.timeout:
                break
            except OSError:
                return None
            while b"\n" in self.buf:
                line, self.buf = self.buf.split(b"\n", 1)
                if not line.strip():
                    continue
                try:
                    msg = json.loads(line)
                except ValueError:
                    continue
                if msg.get("request_id") == rid:
                    return msg.get("data") if msg.get("error") == "success" else None
        return None

    def command(self, cmd: list) -> None:
        """Fire a command without waiting for its reply."""
        try:
            self.sock.sendall(json.dumps({"command": cmd}).encode() + b"\n")
        except OSError:
            pass

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            pass


# --------------------------------------------------------------------------
# Modal screens
# --------------------------------------------------------------------------

class ServerPick(ModalScreen):
    """Pick a discovered server or type an address."""

    BINDINGS = [Binding("escape", "cancel", "Cancel")]

    def __init__(self, servers: list[dict], allow_cancel: bool):
        super().__init__()
        self.servers = servers
        self.allow_cancel = allow_cancel

    def compose(self) -> ComposeResult:
        opts = [
            Option(f"{s.get('Name') or 'Jellyfin'}  ·  {s['Address']}",
                   id=f"srv:{s['Address']}")
            for s in self.servers
        ]
        blurb = ("Found these Jellyfin servers on your network:"
                 if self.servers else
                 "No Jellyfin server answered the network broadcast.")
        with Vertical(id="pick-box"):
            yield Static(f"Connect to Jellyfin\n\n{blurb}", id="pick-blurb")
            if opts:
                yield OptionList(*opts)
            yield Input(placeholder="…or type an address, e.g. http://192.168.1.50:8096",
                        id="manual-server")

    def on_option_list_option_selected(self, event: OptionList.OptionSelected) -> None:
        oid = event.option.id or ""
        if oid.startswith("srv:"):
            self.dismiss(oid[4:])

    def on_input_submitted(self, event: Input.Submitted) -> None:
        addr = event.value.strip()
        if not addr:
            return
        if not addr.startswith(("http://", "https://")):
            addr = "http://" + addr
        self.dismiss(addr)

    def action_cancel(self) -> None:
        if self.allow_cancel:
            self.dismiss(None)


class LoginScreen(ModalScreen):
    """Username/password prompt. The password is sent once and never stored."""

    BINDINGS = [Binding("escape", "cancel", "Cancel")]

    def __init__(self, server_name: str, error: str = "", allow_cancel: bool = False):
        super().__init__()
        self.server_name = server_name
        self.error = error
        self.allow_cancel = allow_cancel

    def compose(self) -> ComposeResult:
        with Vertical(id="pick-box"):
            text = (f"Sign in to {self.server_name}\n\n"
                    "Your password is sent to your server once to create a\n"
                    "session token. Only the token is saved.")
            if self.error:
                text += f"\n\n[red]{self.error}[/red]"
            yield Static(text, id="pick-blurb")
            yield Input(placeholder="username", id="login-user")
            yield Input(placeholder="password", password=True, id="login-pass")

    def on_mount(self) -> None:
        self.query_one("#login-user", Input).focus()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        user = self.query_one("#login-user", Input).value.strip()
        pw = self.query_one("#login-pass", Input).value
        if event.input.id == "login-user" or not user:
            self.query_one("#login-pass", Input).focus()
            return
        self.dismiss((user, pw))

    def action_cancel(self) -> None:
        if self.allow_cancel:
            self.dismiss(None)


class AccountMenu(ModalScreen):
    BINDINGS = [Binding("escape", "cancel", "Cancel")]

    def __init__(self, server: str, username: str):
        super().__init__()
        self.server = server
        self.username = username

    def compose(self) -> ComposeResult:
        with Vertical(id="pick-box"):
            yield Static(f"Connected to {self.server}\nSigned in as {self.username}",
                         id="pick-blurb")
            yield OptionList(
                Option("Switch user", id="user"),
                Option("Switch server", id="server"),
                Option("Cancel", id="cancel"),
            )

    def on_option_list_option_selected(self, event: OptionList.OptionSelected) -> None:
        self.dismiss(event.option.id if event.option.id != "cancel" else None)

    def action_cancel(self) -> None:
        self.dismiss(None)


class HelpScreen(ModalScreen):
    BINDINGS = [
        Binding("escape", "close", "Close"),
        Binding("question_mark", "close", "Close", show=False),
        Binding("q", "close", "Close", show=False),
    ]

    def compose(self) -> ComposeResult:
        with Vertical(id="help-box"):
            yield Static(HELP_TEXT)

    def action_close(self) -> None:
        self.dismiss()


# --------------------------------------------------------------------------
# The TUI
# --------------------------------------------------------------------------

class Page:
    """One level of the navigation stack."""

    def __init__(self, title: str, loader,
                 source: dict | None = None):
        self.title = title
        self.loader = loader          # callable -> list[(section, item)]
        self.source = source          # the item this page was opened from
        self.rows: list[tuple[str, dict]] = []
        self.cursor = 0


class JTerm(App):
    TITLE = "jterm"

    CSS = """
    #search { dock: top; margin: 0 1; }
    #status { dock: top; height: 1; padding: 0 2; color: $text-muted; }
    #results { height: 1fr; }
    ServerPick, LoginScreen, AccountMenu, HelpScreen { align: center middle; }
    #pick-box, #help-box {
        width: 70; height: auto; max-height: 90%;
        border: round $accent; background: $surface; padding: 1 2;
    }
    #pick-blurb { margin-bottom: 1; color: $text-muted; }
    """

    BINDINGS = [
        Binding("slash", "focus_search", "Search", key_display="/"),
        Binding("enter", "open", "Open/Play", priority=False),
        Binding("b", "play_beginning", "From start", show=False),
        Binding("a", "play_audio", "Audio", show=False),
        Binding("o", "play_window", "Window"),
        Binding("w", "toggle_watched", "Watched"),
        Binding("f", "toggle_favourite", "Fav", show=False),
        Binding("d", "toggle_hwdec", "GPU", show=False),
        Binding("escape", "back", "Back"),
        Binding("backspace", "back", "Back", show=False),
        Binding("g", "home", "Home"),
        Binding("ctrl+r", "refresh", "Refresh", show=False),
        Binding("s", "account", "Account", show=False),
        Binding("question_mark", "help", "Help", key_display="?"),
        Binding("q", "quit", "Quit"),
    ]

    def __init__(self, vo: str):
        super().__init__()
        self.vo = vo
        self.cfg = load_config()
        if not self.cfg.get("device_id"):
            self.cfg["device_id"] = uuid.uuid4().hex
        self.hwdec = bool(self.cfg.get("hwdec", False))
        self.jf: Jellyfin | None = None
        self.server_name = ""
        self.username = self.cfg.get("username") or ""
        self.stack: list[Page] = []
        # item id -> (monotonic time, fresh UserData) — see UD_PATCH_TTL
        self._ud_patch: dict[str, tuple[float, dict]] = {}

    # -- layout --------------------------------------------------------------

    def compose(self) -> ComposeResult:
        yield Input(placeholder="Search your library…", id="search")
        yield Static("", id="status")
        yield DataTable(id="results", cursor_type="row", zebra_stripes=True)
        yield Footer()

    def on_mount(self) -> None:
        table = self.query_one(DataTable)
        table.add_column("", width=14, key="section")
        table.add_column("Title", width=56, key="title")
        table.add_column("", width=24, key="info")
        table.add_column("Length", width=9, key="length")
        table.add_column("", width=8, key="state")
        if self.cfg.get("server") and self.cfg.get("token"):
            self.jf = Jellyfin(self.cfg["server"], self.cfg["device_id"],
                               self.cfg["token"], self.cfg["user_id"])
            self.server_name = self.cfg.get("server_name") or self.cfg["server"]
            self.set_status("loading home…")
            self.go_home()
        else:
            self.begin_setup()

    # -- helpers -------------------------------------------------------------

    def set_status(self, text: str) -> None:
        self.query_one("#status", Static).update(text)

    def idle_status(self, extra: str = "") -> None:
        vo = self.vo
        if vo == "tct":
            vo += " (block art — run jterm inside kitty for sharp video)"
        where = f"{self.server_name} · {self.username}" if self.jf else "not connected"
        path = " › ".join(p.title for p in self.stack)
        decode = "gpu" if self.hwdec else "cpu"
        bits = [where, f"video: {vo} · decode {decode}", path or "", extra]
        self.set_status(" │ ".join(b for b in bits if b))

    def in_input(self) -> bool:
        return isinstance(self.focused, Input)

    def note_userdata(self, item_id: str, ud) -> None:
        if isinstance(ud, dict):
            self._ud_patch[item_id] = (time.monotonic(), ud)

    def apply_userdata_patches(self, rows: list[tuple[str, dict]]) -> None:
        now = time.monotonic()
        self._ud_patch = {k: v for k, v in self._ud_patch.items()
                          if now - v[0] < UD_PATCH_TTL}
        for _section, item in rows:
            patch = self._ud_patch.get(item.get("Id"))
            if patch:
                item["UserData"] = patch[1]

    def selected(self) -> tuple[str, dict] | None:
        if not self.stack:
            return None
        page = self.stack[-1]
        table = self.query_one(DataTable)
        if table.cursor_row is None or not page.rows:
            return None
        if 0 <= table.cursor_row < len(page.rows):
            return page.rows[table.cursor_row]
        return None

    def render_page(self, restore_cursor: bool = False) -> None:
        page = self.stack[-1]
        table = self.query_one(DataTable)
        saved = page.cursor if restore_cursor else 0
        table.clear()
        last_section = None
        for section, item in page.rows:
            label = "" if section == last_section else section
            last_section = section
            table.add_row(
                label,
                item_title(item)[:54],
                item_info(item)[:22],
                fmt_duration(item.get("RunTimeTicks")),
                item_state(item),
            )
        table.loading = False
        if page.rows:
            table.move_cursor(row=min(saved, len(page.rows) - 1))
            table.focus()
            self.idle_status("? keys")
        else:
            self.idle_status("nothing here")

    # -- first-run / login flow ----------------------------------------------

    def begin_setup(self, allow_cancel: bool = False) -> None:
        self.set_status("looking for Jellyfin servers on your network…")
        self.run_discovery(allow_cancel)

    @work(thread=True, exclusive=True, group="setup")
    def run_discovery(self, allow_cancel: bool) -> None:
        servers = discover_servers()
        self.call_from_thread(self._show_server_pick, servers, allow_cancel)

    def _show_server_pick(self, servers: list[dict], allow_cancel: bool) -> None:
        if len(servers) == 1 and not allow_cancel:
            # exactly one server on the network — connect to it, no questions
            self._server_chosen(servers[0]["Address"])
            return
        self.push_screen(ServerPick(servers, allow_cancel), self._server_chosen)

    def _server_chosen(self, address: str | None) -> None:
        if address is None:
            self.idle_status()
            return
        self.cfg["server"] = address.rstrip("/")
        self.jf = Jellyfin(self.cfg["server"], self.cfg["device_id"])
        self.set_status(f"connecting to {address}…")
        self.fetch_server_info()

    @work(thread=True, exclusive=True, group="setup")
    def fetch_server_info(self) -> None:
        assert self.jf is not None
        try:
            info = self.jf.public_info()
        except JFError as exc:
            self.call_from_thread(self.set_status, str(exc))
            self.call_from_thread(self.begin_setup, True)
            return
        name = info.get("ServerName") or self.cfg["server"]
        self.cfg["server_name"] = name
        self.server_name = name
        self.call_from_thread(self._ask_login, "")

    def _ask_login(self, error: str) -> None:
        self.push_screen(LoginScreen(self.server_name, error), self._login_submitted)

    def _login_submitted(self, creds: tuple[str, str] | None) -> None:
        if creds is None:
            return
        self.set_status("signing in…")
        self.run_login(creds[0], creds[1])

    @work(thread=True, exclusive=True, group="setup")
    def run_login(self, username: str, password: str) -> None:
        assert self.jf is not None
        try:
            user = self.jf.authenticate(username, password)
        except JFError as exc:
            msg = ("wrong username or password" if exc.status == 401
                   else str(exc))
            self.call_from_thread(self._ask_login, msg)
            return
        self.cfg.update({
            "token": self.jf.token,
            "user_id": self.jf.user_id,
            "username": user.get("Name") or username,
        })
        self.username = self.cfg["username"]
        save_config(self.cfg)
        self.call_from_thread(self.set_status, f"signed in as {self.username}")
        self.call_from_thread(self.go_home)

    def sign_out(self, then: str) -> None:
        for key in ("token", "user_id"):
            self.cfg.pop(key, None)
        save_config(self.cfg)
        self.stack.clear()
        self.query_one(DataTable).clear()
        if then == "server":
            self.cfg.pop("server", None)
            self.begin_setup()
        else:
            self.jf = Jellyfin(self.cfg["server"], self.cfg["device_id"])
            self._ask_login("")

    # -- page loading ----------------------------------------------------------

    def push_page(self, title: str, loader, source: dict | None = None) -> None:
        if self.stack:
            self.stack[-1].cursor = self.query_one(DataTable).cursor_row or 0
        self.stack.append(Page(title, loader, source))
        self.query_one(DataTable).loading = True
        self.set_status(f"loading {title}…")
        self.load_page(self.stack[-1], False)

    def reload_page(self, restore_cursor: bool = True) -> None:
        if not self.stack:
            return
        page = self.stack[-1]
        page.cursor = self.query_one(DataTable).cursor_row or page.cursor
        self.load_page(page, restore_cursor)

    @work(thread=True, exclusive=True, group="load")
    def load_page(self, page: Page, restore_cursor: bool) -> None:
        try:
            rows = page.loader()
            self.apply_userdata_patches(rows)
            page.rows = rows
        except JFError as exc:
            if exc.status == 401:
                # token expired or revoked — ask for the password again
                self.call_from_thread(self.set_status,
                                      "session expired — please sign in again")
                self.call_from_thread(self.sign_out, "user")
                return
            self.call_from_thread(self.set_status, f"failed: {exc}")
            self.call_from_thread(setattr, self.query_one(DataTable), "loading", False)
            return
        if self.stack and self.stack[-1] is page:
            self.call_from_thread(self.render_page, restore_cursor)

    # -- loaders (run in worker threads) ----------------------------------------

    def _load_home(self) -> list[tuple[str, dict]]:
        jf = self.jf
        rows: list[tuple[str, dict]] = []
        for item in jf.resume_items():
            rows.append(("Continue", item))
        for item in jf.next_up():
            rows.append(("Next Up", item))
        views = jf.views()
        for view in views:
            rows.append(("Libraries", view))
        for view in views:
            if view.get("CollectionType") in ("movies", "tvshows", "homevideos"):
                for item in jf.latest(view["Id"]):
                    rows.append((f"Latest {view.get('Name', '')[:6]}", item))
        return rows

    def _loader_children(self, item: dict):
        jf = self.jf
        itype = item.get("Type")
        item_id = item["Id"]
        if itype == "Series":
            return lambda: [("", i) for i in jf.seasons(item_id)]
        if itype == "Season":
            series_id = item.get("SeriesId") or item_id
            return lambda: [("", i) for i in jf.episodes(series_id, item_id)]
        return lambda: [("", i) for i in jf.children(item_id)]

    # -- navigation actions ------------------------------------------------------

    def go_home(self) -> None:
        self.stack.clear()
        self.push_page("Home", self._load_home)

    def action_home(self) -> None:
        if not self.in_input() and self.jf:
            self.go_home()

    def action_back(self) -> None:
        if self.in_input():
            self.query_one(DataTable).focus()
            return
        if len(self.stack) > 1:
            self.stack.pop()
            self.render_page(restore_cursor=True)

    def action_refresh(self) -> None:
        if self.jf:
            self.reload_page()

    def action_focus_search(self) -> None:
        if self.jf:
            inp = self.query_one("#search", Input)
            inp.value = ""
            inp.focus()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        if event.input.id != "search":
            return
        term = event.value.strip()
        if not term or not self.jf:
            return
        self.query_one("#search", Input).value = ""
        jf = self.jf
        self.push_page(f"search {term!r}",
                       lambda: [("", i) for i in jf.search(term)])

    def on_data_table_row_selected(self, event: DataTable.RowSelected) -> None:
        self.action_open()

    def action_open(self) -> None:
        if self.in_input():
            return
        sel = self.selected()
        if not sel:
            return
        _section, item = sel
        if is_folder(item):
            self.push_page(item.get("Name") or "?", self._loader_children(item), item)
        else:
            self._play("terminal", item, resume_ticks(item))

    def action_play_beginning(self) -> None:
        self._play_selected("terminal", from_start=True)

    def action_play_audio(self) -> None:
        self._play_selected("audio")

    def action_play_window(self) -> None:
        self._play_selected("window")

    def _play_selected(self, mode: str, from_start: bool = False) -> None:
        if self.in_input():
            return
        sel = self.selected()
        if not sel:
            return
        _section, item = sel
        if is_folder(item):
            self.set_status("that is a folder — press Enter to open it")
            return
        self._play(mode, item, 0 if from_start else resume_ticks(item))

    # -- watched / favourite -------------------------------------------------------

    def action_toggle_watched(self) -> None:
        if self.in_input():
            return
        sel = self.selected()
        if not sel:
            return
        _s, item = sel
        played = bool((item.get("UserData") or {}).get("Played"))
        self.run_userdata(item, "played", not played)

    def action_toggle_favourite(self) -> None:
        if self.in_input():
            return
        sel = self.selected()
        if not sel:
            return
        _s, item = sel
        fav = bool((item.get("UserData") or {}).get("IsFavorite"))
        self.run_userdata(item, "favourite", not fav)

    def action_toggle_hwdec(self) -> None:
        if self.in_input():
            return
        self.hwdec = not self.hwdec
        self.cfg["hwdec"] = self.hwdec
        save_config(self.cfg)
        if self.hwdec:
            self.set_status(
                "GPU/hardware decoding on — lower CPU on decode and in the o "
                "window; in-terminal frames still copy back to the CPU. "
                "Applies to the next play.")
        else:
            self.set_status("GPU/hardware decoding off — software decode (sw-fast)")

    @work(thread=True, group="userdata")
    def run_userdata(self, item: dict, what: str, value: bool) -> None:
        try:
            if what == "played":
                ud = self.jf.set_played(item["Id"], value)
            else:
                ud = self.jf.set_favourite(item["Id"], value)
            if not isinstance(ud, dict):
                ud = (self.jf.item(item["Id"]) or {}).get("UserData")
        except JFError as exc:
            self.call_from_thread(self.set_status, f"failed: {exc}")
            return
        if isinstance(ud, dict):
            item["UserData"] = ud
            self.note_userdata(item["Id"], ud)
        self.call_from_thread(self.render_page, True)

    # -- account menu ----------------------------------------------------------------

    def action_account(self) -> None:
        if self.in_input() or not self.jf:
            return
        self.push_screen(AccountMenu(self.server_name, self.username),
                         self._account_choice)

    def _account_choice(self, choice: str | None) -> None:
        if choice in ("user", "server"):
            self.sign_out(choice)

    def action_help(self) -> None:
        if not self.in_input():
            self.push_screen(HelpScreen())

    # -- playback ---------------------------------------------------------------------

    def _mpv_base(self, sock_path: str) -> list[str] | None:
        mpv = shutil.which("mpv")
        if not mpv:
            self.set_status("mpv is not installed — install it and restart jterm")
            return None
        return [
            mpv, "--osc=no", "--msg-level=all=error,statusline=status",
            "--term-osd-bar=no", "--force-seekable=yes",
            f"--input-ipc-server={sock_path}",
            f"--user-agent={CLIENT_NAME}/{CLIENT_VERSION}",
        ]

    def _decode_flags(self) -> list[str]:
        """Decode flags shared by the video paths. With GPU decoding on we
        let mpv pick a safe hardware decoder; otherwise the fast software
        profile keeps CPU scaling cheap for terminal output."""
        if self.hwdec:
            return ["--hwdec=auto-safe"]
        return ["--profile=sw-fast"]

    def _print_control_centre(self, title: str, mode_desc: str) -> None:
        cols = shutil.get_terminal_size().columns
        bar = "─" * min(cols - 1, 110)
        print(f"▶ {title}"[: cols - 1])
        print(bar)
        print(" q quit to browser │ space pause │ ←/→ seek 5s │ ↑/↓ seek 1m "
              "│ 9/0 volume │ m mute │ [ ] speed"[: cols - 1])
        print(f" {mode_desc}"[: cols - 1])
        print(bar)

    def _next_episode(self, item: dict) -> dict | None:
        if item.get("Type") != "Episode" or not item.get("SeriesId"):
            return None
        try:
            eps = self.jf.episodes(item["SeriesId"],
                                   start_item_id=item["Id"], limit=2)
        except JFError:
            return None
        for ep in eps:
            if ep.get("Id") != item["Id"]:
                return ep
        return None

    @staticmethod
    def _new_sock_path() -> str:
        return os.path.join(
            tempfile.gettempdir(),
            f"jterm-mpv-{os.getpid()}-{uuid.uuid4().hex[:8]}.sock")

    def _video_cmd(self, url: str, start_ticks: int, sock_path: str) -> list[str] | None:
        """Build the in-terminal mpv command with the footer band reserved.

        --video-margin-ratio-bottom physically keeps the video out of the
        footer's rows (unlike --vo-kitty-rows, which only overrides mpv's
        size detection and lets the image draw over the band anyway).
        """
        cmd = self._mpv_base(sock_path)
        if cmd is None:
            return None
        ratio = footer_margin_ratio(shutil.get_terminal_size().lines)
        cmd += [
            f"--vo={self.vo}",
            "--term-status-msg=",
            f"--video-margin-ratio-bottom={ratio:.4f}",
        ]
        cmd += self._decode_flags()
        if self.vo == "kitty":
            cmd.append("--vo-kitty-use-shm=yes")
        if start_ticks:
            cmd.append(f"--start={start_ticks // TICKS_PER_SECOND}")
        cmd.append(url)
        return cmd

    @staticmethod
    def _display_title(item: dict) -> str:
        title = item_title(item)
        if item.get("Type") == "Episode" and item.get("SeriesName"):
            title = f"{item['SeriesName']} {title}"
        return title

    def _play(self, mode: str, item: dict, start_ticks: int) -> None:
        if not self.jf:
            return
        title = self._display_title(item)

        if mode == "window":
            sock_path = self._new_sock_path()
            cmd = self._mpv_base(sock_path)
            if cmd is None:
                return
            if self.hwdec:
                cmd.append("--hwdec=auto-safe")
            if start_ticks:
                cmd.append(f"--start={start_ticks // TICKS_PER_SECOND}")
            cmd += [f"--title=jterm: {title}", self.jf.stream_url(item["Id"])]
            PlaybackReporter(self.jf, item, sock_path, start_ticks).start()
            subprocess.Popen(
                cmd, start_new_session=True,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            self.set_status(f"playing in window: {title[:60]} (browse continues)")
            return

        played_ids: list[str] = []
        with self.suspend():
            os.system("clear")
            try:
                if mode == "audio":
                    self._play_audio(item, start_ticks, played_ids)
                else:
                    self._run_mpv_with_footer(item, start_ticks, played_ids)
            except KeyboardInterrupt:
                pass
        self.set_status(f"finished: {title[:60]}")
        self._return_to_parent()
        self.refresh_after_play(played_ids)

    def _return_to_parent(self) -> None:
        """After playback ends, land back on the page you were browsing
        from: the nearest ancestor library or collection, or Home when the
        item came from Home, Continue Watching, Next Up or a search."""
        if not self.stack:
            return
        keep = 0  # Home
        for i, page in enumerate(self.stack):
            if (page.source or {}).get("Type") in LIBRARY_TYPES:
                keep = i
        if keep == len(self.stack) - 1:
            # already on the right page — keep the cursor where it was
            page = self.stack[-1]
            page.cursor = self.query_one(DataTable).cursor_row or page.cursor
        else:
            del self.stack[keep + 1:]

    def _play_audio(self, item: dict, start_ticks: int,
                    played_ids: list[str]) -> None:
        """Audio has no video to overdraw, so the static banner plus mpv's
        own status line is still the right tool."""
        sock_path = self._new_sock_path()
        cmd = self._mpv_base(sock_path)
        if cmd is None:
            return
        cmd += [f"--term-status-msg={MPV_STATUS}", "--no-video"]
        if start_ticks:
            cmd.append(f"--start={start_ticks // TICKS_PER_SECOND}")
        cmd.append(self.jf.stream_url(item["Id"]))
        desc = "audio only"
        if start_ticks:
            desc += f" │ resuming from {fmt_clock(start_ticks / TICKS_PER_SECOND)}"
        self._print_control_centre(self._display_title(item), desc)
        reporter = PlaybackReporter(self.jf, item, sock_path, start_ticks)
        reporter.start()
        played_ids.append(item["Id"])
        try:
            subprocess.call(cmd)
        finally:
            reporter.stop()
            reporter.join(timeout=10)

    def _run_mpv_with_footer(self, item: dict, start_ticks: int,
                             played_ids: list[str]) -> PlaybackReporter | None:
        """Launch mpv on the item and paint the live control footer until it
        exits. The footer loop and the PlaybackReporter are independent
        clients of the same mpv IPC socket: the loop owns the process and
        the screen, the reporter mirrors progress to the server."""
        sock_path = self._new_sock_path()
        title = self._display_title(item)
        size = shutil.get_terminal_size()
        draw_footer([" loading…", " " + KEY_HINTS], size.lines, size.columns)
        cmd = self._video_cmd(self.jf.stream_url(item["Id"]), start_ticks, sock_path)
        if cmd is None:
            return None
        proc = subprocess.Popen(cmd)
        reporter = PlaybackReporter(self.jf, item, sock_path, start_ticks)
        reporter.start()
        played_ids.append(item["Id"])
        ipc = None
        deadline = time.time() + 15
        while time.time() < deadline and proc.poll() is None:
            if os.path.exists(sock_path):
                try:
                    ipc = MpvIPC(sock_path)
                    break
                except OSError:
                    pass
            time.sleep(0.15)
        props = ("time-pos", "duration", "percent-pos", "volume", "pause")
        last_lines = None
        try:
            while proc.poll() is None:
                size = shutil.get_terminal_size()
                # Keep mpv's reserved bottom band exactly FOOTER_ROWS tall as
                # the pane is resized, so the video can never creep over it.
                if ipc and size.lines != last_lines:
                    ipc.command(["set_property", "video-margin-ratio-bottom",
                                 footer_margin_ratio(size.lines)])
                    last_lines = size.lines
                st = {p: ipc.get(p) for p in props} if ipc else {}
                draw_footer(footer_lines(st, title, size.columns),
                            size.lines, size.columns)
                time.sleep(0.25)
        except (BrokenPipeError, OSError):
            pass
        finally:
            if ipc:
                ipc.close()
            try:
                proc.wait(timeout=5)
            except Exception:
                proc.terminate()
            reporter.stop()
            reporter.join(timeout=10)
            try:
                os.unlink(sock_path)
            except OSError:
                pass
        return reporter

    @work(thread=True, group="userdata")
    def refresh_after_play(self, item_ids: list[str]) -> None:
        # the list endpoint caches UserData (see UD_PATCH_TTL) — pull the
        # fresh truth per item so progress shows up as soon as we return
        for item_id in item_ids:
            try:
                self.note_userdata(item_id,
                                   (self.jf.item(item_id) or {}).get("UserData"))
            except JFError:
                pass
        if self.stack:
            # not reload_page: the table may still show a deeper page after
            # _return_to_parent, so its cursor must not overwrite this one
            self.call_from_thread(self.load_page, self.stack[-1], True)


def main() -> None:
    vo = detect_video_output()
    JTerm(vo).run()


if __name__ == "__main__":
    main()
