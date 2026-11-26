// Package jellyfin is a small client for the parts of the Jellyfin API
// jterm uses: discovery, sign-in, browsing, stream URLs and the playback
// reports that keep resume points and watched state in sync.
package jellyfin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// How jterm identifies itself to the server and to mpv.
const ClientName = "jterm"

// ClientVersion is a variable so a release build can stamp it.
var ClientVersion = "0.2.0"

// PageLimit caps how many children one folder page asks for.
const PageLimit = 200

// Error is a failed API call. Status is the HTTP status, or 0 when the
// server could not be reached at all.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// Unauthorized reports whether err means the session token was rejected,
// in which case the user has to sign in again.
func Unauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// Client talks to one server as one device and, once signed in, one user.
type Client struct {
	Server   string
	DeviceID string
	Token    string
	UserID   string
	http     *http.Client
}

// New returns a client. Token and userID are empty until sign-in.
func New(server, deviceID, token, userID string) *Client {
	return &Client{
		Server:   strings.TrimRight(server, "/"),
		DeviceID: deviceID,
		Token:    token,
		UserID:   userID,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// -- plumbing ---------------------------------------------------------------

func (c *Client) authHeader() string {
	device, err := os.Hostname()
	if err != nil || device == "" {
		device = "terminal"
	}
	parts := []string{
		fmt.Sprintf(`Client="%s"`, ClientName),
		fmt.Sprintf(`Device="%s"`, device),
		fmt.Sprintf(`DeviceId="%s"`, c.DeviceID),
		fmt.Sprintf(`Version="%s"`, ClientVersion),
	}
	if c.Token != "" {
		parts = append(parts, fmt.Sprintf(`Token="%s"`, c.Token))
	}
	return "MediaBrowser " + strings.Join(parts, ", ")
}

// do performs a request and returns the raw response body, which may be
// empty: several endpoints answer 204.
func (c *Client) do(method, path string, params url.Values, body any) ([]byte, error) {
	u := c.Server + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{0, err.Error()}
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, u, payload)
	if err != nil {
		return nil, &Error{0, fmt.Sprintf("cannot reach %s: %v", c.Server, err)}
	}
	req.Header.Set("Authorization", c.authHeader())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{0, fmt.Sprintf("cannot reach %s: %v", c.Server, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &Error{resp.StatusCode, fmt.Sprintf("HTTP %d for %s", resp.StatusCode, path)}
	}
	if err != nil {
		return nil, &Error{0, fmt.Sprintf("cannot reach %s: %v", c.Server, err)}
	}
	return raw, nil
}

// fetch decodes a response into T. An empty or unexpected body is not an
// error: it leaves T at its zero value, the "nothing there" answer.
func fetch[T any](c *Client, method, path string, params url.Values, body any) (T, error) {
	var out T
	raw, err := c.do(method, path, params, body)
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

type itemList struct {
	Items []Item `json:"Items"`
}

func (c *Client) items(path string, params url.Values) ([]Item, error) {
	list, err := fetch[itemList](c, http.MethodGet, path, params, nil)
	return list.Items, err
}

func (c *Client) userPath(format string, args ...any) string {
	return "/Users/" + c.UserID + fmt.Sprintf(format, args...)
}

// -- auth -------------------------------------------------------------------

// Authenticate exchanges a username and password for a session token and
// remembers it on the client. The password goes no further than this call.
func (c *Client) Authenticate(username, password string) (User, error) {
	type result struct {
		AccessToken string `json:"AccessToken"`
		User        User   `json:"User"`
	}
	res, err := fetch[result](c, http.MethodPost, "/Users/AuthenticateByName", nil,
		map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return User{}, err
	}
	if res.AccessToken == "" || res.User.ID == "" {
		return User{}, &Error{0, "the server did not return a session token"}
	}
	c.Token, c.UserID = res.AccessToken, res.User.ID
	return res.User, nil
}

// ServerName asks the server what it is called.
func (c *Client) ServerName() (string, error) {
	type info struct {
		ServerName string `json:"ServerName"`
	}
	res, err := fetch[info](c, http.MethodGet, "/System/Info/Public", nil, nil)
	return res.ServerName, err
}

// -- browsing ---------------------------------------------------------------

// Views lists the user's libraries.
func (c *Client) Views() ([]Item, error) {
	return c.items(c.userPath("/Views"), nil)
}

// ResumeItems lists partially watched videos.
func (c *Client) ResumeItems(limit int) ([]Item, error) {
	return c.items(c.userPath("/Items/Resume"), url.Values{
		"Limit": {strconv.Itoa(limit)}, "MediaTypes": {"Video"},
		"Fields": {"ProductionYear"},
	})
}

// NextUp lists the next unwatched episode of each show in progress.
func (c *Client) NextUp(limit int) ([]Item, error) {
	return c.items("/Shows/NextUp", url.Values{
		"UserId": {c.UserID}, "Limit": {strconv.Itoa(limit)},
		"Fields": {"ProductionYear"},
	})
}

// Latest lists recent additions to a library. Unlike every other list
// endpoint this one answers with a bare array.
func (c *Client) Latest(parentID string, limit int) ([]Item, error) {
	return fetch[[]Item](c, http.MethodGet, c.userPath("/Items/Latest"), url.Values{
		"ParentId": {parentID}, "Limit": {strconv.Itoa(limit)},
		"Fields": {"ProductionYear"},
	}, nil)
}

// Children lists a folder's contents by name.
func (c *Client) Children(parentID string) ([]Item, error) {
	return c.items(c.userPath("/Items"), url.Values{
		"ParentId": {parentID}, "SortBy": {"SortName"},
		"SortOrder": {"Ascending"}, "Limit": {strconv.Itoa(PageLimit)},
		"Fields": {"ProductionYear,ChildCount"},
	})
}

// Seasons lists a series' seasons.
func (c *Client) Seasons(seriesID string) ([]Item, error) {
	return c.items("/Shows/"+seriesID+"/Seasons", url.Values{"UserId": {c.UserID}})
}

// Episodes lists a series' episodes in order, or just one season's when
// seasonID is given.
func (c *Client) Episodes(seriesID, seasonID string) ([]Item, error) {
	params := url.Values{"UserId": {c.UserID}, "Fields": {"ProductionYear"}}
	if seasonID != "" {
		params.Set("SeasonId", seasonID)
	}
	return c.items("/Shows/"+seriesID+"/Episodes", params)
}

// Search looks through the whole library.
func (c *Client) Search(term string, limit int) ([]Item, error) {
	return c.items(c.userPath("/Items"), url.Values{
		"SearchTerm": {term}, "Recursive": {"true"}, "Limit": {strconv.Itoa(limit)},
		"IncludeItemTypes": {"Movie,Series,Episode,Video,BoxSet"},
		"Fields":           {"ProductionYear"},
	})
}

// Item fetches one item in full. Its UserData is always fresh, unlike the
// list endpoints, which serve it from a cache that lags writes.
func (c *Client) Item(itemID string) (Item, error) {
	return fetch[Item](c, http.MethodGet, c.userPath("/Items/%s", itemID), nil, nil)
}

// AdjacentEpisode is the episode before (-1) or after (+1) this one in its
// series, crossing season boundaries. ok is false at either end and for
// anything that is not an episode: films have no sequence.
func (c *Client) AdjacentEpisode(item Item, direction int) (next Item, ok bool) {
	if item.Type != "Episode" || item.SeriesID == "" {
		return Item{}, false
	}
	eps, err := c.Episodes(item.SeriesID, "")
	if err != nil {
		return Item{}, false
	}
	for i, ep := range eps {
		if ep.ID == item.ID {
			if j := i + direction; j >= 0 && j < len(eps) {
				return eps[j], true
			}
			break
		}
	}
	return Item{}, false
}

// -- playback ---------------------------------------------------------------

// StreamURL is the direct-play URL when maxHeight is zero. Otherwise it is a
// server-side transcode capped at that height and bitrate, optionally
// starting mid-item, in which case the transcoded stream's clock starts at
// zero.
func (c *Client) StreamURL(itemID string, maxHeight, bitrate int, startTicks int64, sessionID string) string {
	if maxHeight == 0 {
		return fmt.Sprintf("%s/Videos/%s/stream?static=true&api_key=%s&deviceId=%s",
			c.Server, itemID, c.Token, c.DeviceID)
	}
	u := fmt.Sprintf("%s/Videos/%s/stream.mkv?static=false&api_key=%s&deviceId=%s"+
		"&videoCodec=h264&audioCodec=aac&maxHeight=%d&videoBitRate=%d",
		c.Server, itemID, c.Token, c.DeviceID, maxHeight, bitrate)
	if startTicks > 0 {
		u += fmt.Sprintf("&startTimeTicks=%d", startTicks)
	}
	if sessionID != "" {
		u += "&PlaySessionId=" + sessionID
	}
	return u
}

// StopEncoding tells the server to kill the transcode job for a session.
func (c *Client) StopEncoding(sessionID string) error {
	_, err := c.do(http.MethodDelete, "/Videos/ActiveEncodings", url.Values{
		"deviceId": {c.DeviceID}, "playSessionId": {sessionID},
	}, nil)
	return err
}

// ExternalSubtitles returns delivery URLs for an item's external subtitle
// tracks.
//
// Embedded text subtitles travel inside the direct-play container, so mpv
// already sees them. External ones (sidecar files the server found next to
// the media, or anything not muxed into the original file) are in the
// Jellyfin database but absent from the raw stream, which is why they show
// in the web UI yet never reach mpv. The server can extract and serve each
// as text on demand at this route.
func (c *Client) ExternalSubtitles(itemID string) ([]Subtitle, error) {
	full, err := c.Item(itemID)
	if err != nil {
		return nil, err
	}
	var out []Subtitle
	for _, src := range full.MediaSources {
		sourceID := src.ID
		if sourceID == "" {
			sourceID = itemID
		}
		for _, st := range src.MediaStreams {
			if st.Type != "Subtitle" || !st.IsExternal {
				continue
			}
			ext := strings.ToLower(st.Codec)
			switch ext {
			case "ass", "ssa", "vtt", "sub":
			default:
				ext = "srt"
			}
			title := st.DisplayTitle
			if title == "" {
				title = st.Title
			}
			out = append(out, Subtitle{
				URL: fmt.Sprintf("%s/Videos/%s/%s/Subtitles/%d/0/Stream.%s?api_key=%s",
					c.Server, itemID, sourceID, st.Index, ext, c.Token),
				Title:   title,
				Lang:    st.Language,
				Default: st.IsDefault || st.IsForced,
			})
		}
	}
	return out, nil
}

// Play methods the server distinguishes in its playback reports.
const (
	DirectPlay = "DirectPlay"
	Transcode  = "Transcode"
)

// ReportStart tells the server playback has begun.
func (c *Client) ReportStart(itemID, sessionID string, ticks int64, playMethod string) error {
	_, err := c.do(http.MethodPost, "/Sessions/Playing", nil, map[string]any{
		"ItemId": itemID, "PlaySessionId": sessionID,
		"PositionTicks": ticks, "CanSeek": true,
		"PlayMethod": playMethod,
	})
	return err
}

// ReportProgress sends the current position, as the official clients do
// every few seconds.
func (c *Client) ReportProgress(itemID, sessionID string, ticks int64, paused bool, playMethod string) error {
	_, err := c.do(http.MethodPost, "/Sessions/Playing/Progress", nil, map[string]any{
		"ItemId": itemID, "PlaySessionId": sessionID,
		"PositionTicks": ticks, "IsPaused": paused,
		"CanSeek": true, "PlayMethod": playMethod,
	})
	return err
}

// ReportStopped ends the play session. The server saves the resume point,
// or marks the item watched if it was near the end.
func (c *Client) ReportStopped(itemID, sessionID string, ticks int64) error {
	_, err := c.do(http.MethodPost, "/Sessions/Playing/Stopped", nil, map[string]any{
		"ItemId": itemID, "PlaySessionId": sessionID,
		"PositionTicks": ticks,
	})
	return err
}

// -- user data --------------------------------------------------------------

// setFlag switches a per-user flag and returns the item's fresh UserData.
// The server normally answers with it; when it does not, the item is
// fetched instead. The result is nil only if neither produced any.
func (c *Client) setFlag(path string, on bool) (*UserData, error) {
	method := http.MethodDelete
	if on {
		method = http.MethodPost
	}
	raw, err := c.do(method, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		var ud UserData
		if json.Unmarshal(trimmed, &ud) == nil {
			return &ud, nil
		}
	}
	return nil, nil
}

// SetPlayed marks an item watched or unwatched.
func (c *Client) SetPlayed(itemID string, played bool) (*UserData, error) {
	return c.freshUserData(itemID, c.userPath("/PlayedItems/%s", itemID), played)
}

// SetFavourite adds or removes an item from the user's favourites.
func (c *Client) SetFavourite(itemID string, favourite bool) (*UserData, error) {
	return c.freshUserData(itemID, c.userPath("/FavoriteItems/%s", itemID), favourite)
}

func (c *Client) freshUserData(itemID, path string, on bool) (*UserData, error) {
	ud, err := c.setFlag(path, on)
	if err != nil || ud != nil {
		return ud, err
	}
	item, err := c.Item(itemID)
	return item.UserData, err
}
