// Package config reads and writes jterm's settings file. It holds the
// server address, a device id, the user id and a session token — never the
// password. The file format is shared with the Python jterm, so an existing
// sign-in carries straight over.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// QualityCap is the tallest video the server may send. Source (zero) is
// direct play of the original file; a height asks the server to transcode.
type QualityCap int

// Source is direct play, the default.
const Source QualityCap = 0

// Caps are the selectable quality caps, highest first.
var Caps = []QualityCap{Source, 1080, 720, 480, 360}

// transcodeBitrates maps a cap to the video bitrate requested. Some servers
// pick the actual resolution from the bitrate rather than honouring the
// height, so the footer's live resolution readout is the truth.
var transcodeBitrates = map[QualityCap]int{
	1080: 8_000_000, 720: 4_000_000, 480: 2_000_000, 360: 1_000_000,
}

// Bitrate is the video bitrate to request for a transcode at this cap.
func (c QualityCap) Bitrate() int {
	if b, ok := transcodeBitrates[c]; ok {
		return b
	}
	return 4_000_000
}

// Label is how the cap reads in the footer: "source" or "≤720p".
func (c QualityCap) Label() string {
	if c == Source {
		return "source"
	}
	return fmt.Sprintf("≤%dp", int(c))
}

// Step moves one rung up or down the ladder, stopping at either end.
func (c QualityCap) Step(up bool) QualityCap {
	i := max(slices.Index(Caps, c), 0)
	if up {
		i--
	} else {
		i++
	}
	return Caps[min(max(i, 0), len(Caps)-1)]
}

// MarshalJSON keeps the on-disk form the Python jterm wrote: the string
// "source", or the height as a number.
func (c QualityCap) MarshalJSON() ([]byte, error) {
	if c == Source {
		return []byte(`"source"`), nil
	}
	return json.Marshal(int(c))
}

// UnmarshalJSON accepts that same form. Anything unrecognised falls back to
// source rather than failing the whole file.
func (c *QualityCap) UnmarshalJSON(data []byte) error {
	*c = Source
	var n int
	if json.Unmarshal(data, &n) == nil && slices.Contains(Caps, QualityCap(n)) {
		*c = QualityCap(n)
	}
	return nil
}

// Config is the contents of config.json.
type Config struct {
	Server     string     `json:"server,omitempty"`
	ServerName string     `json:"server_name,omitempty"`
	DeviceID   string     `json:"device_id,omitempty"`
	Token      string     `json:"token,omitempty"`
	UserID     string     `json:"user_id,omitempty"`
	Username   string     `json:"username,omitempty"`
	HWDec      bool       `json:"hwdec"`
	QualityCap QualityCap `json:"quality_cap"`
}

// Dir is where jterm keeps its files. It is ~/.config/jterm on every
// platform, matching the Python version (os.UserConfigDir would move it on
// macOS and lose the existing sign-in). JTERM_CONFIG_DIR overrides it, which
// is how first-run flows are tested without touching a real sign-in.
func Dir() string {
	if d := os.Getenv("JTERM_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "jterm")
}

// Path is the settings file.
func Path() string { return filepath.Join(Dir(), "config.json") }

// InputConfPath is where the mpv key bindings are written.
func InputConfPath() string { return filepath.Join(Dir(), "input.conf") }

// Load reads the settings file. A missing or unreadable file gives the
// defaults: hardware decoding on, source quality. A device id is minted if
// there is none yet.
func Load() Config {
	cfg := Config{HWDec: true}
	if data, err := os.ReadFile(Path()); err == nil {
		if json.Unmarshal(data, &cfg) != nil {
			cfg = Config{HWDec: true}
		}
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = NewID()
	}
	return cfg
}

// Save writes the settings file with mode 600. It writes a temporary file
// and renames it into place, so a crash cannot leave half a config behind.
func Save(cfg Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir(), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), Path())
}

// NewID returns 32 random hex characters, used for device ids, play session
// ids and socket names.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
