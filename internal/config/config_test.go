package config

import (
	"encoding/json"
	"os"
	"testing"
)

func TestQualityCapJSON(t *testing.T) {
	// the on-disk form is the Python jterm's: "source" or a bare height
	for in, want := range map[string]QualityCap{
		`"source"`: Source, `1080`: 1080, `720`: 720, `360`: 360,
		// anything unrecognised falls back to source
		`999`: Source, `"weird"`: Source, `null`: Source, `true`: Source, `7.5`: Source,
	} {
		var c QualityCap = 480
		if err := json.Unmarshal([]byte(in), &c); err != nil || c != want {
			t.Errorf("unmarshal %s = %v, %v; want %v", in, c, err, want)
		}
	}
	for c, want := range map[QualityCap]string{Source: `"source"`, 720: `720`} {
		if got, _ := json.Marshal(c); string(got) != want {
			t.Errorf("marshal %d = %s; want %s", c, got, want)
		}
	}
}

func TestQualityCapStep(t *testing.T) {
	// expected values taken from the Python _next_cap
	steps := []struct {
		from     QualityCap
		up, down QualityCap
	}{
		{Source, Source, 1080}, {1080, Source, 720}, {720, 1080, 480},
		{480, 720, 360}, {360, 480, 360},
	}
	for _, s := range steps {
		if got := s.from.Step(true); got != s.up {
			t.Errorf("%v up = %v; want %v", s.from, got, s.up)
		}
		if got := s.from.Step(false); got != s.down {
			t.Errorf("%v down = %v; want %v", s.from, got, s.down)
		}
	}
	if got := QualityCap(999).Step(false); got != 1080 {
		t.Errorf("unknown cap steps from source; got %v", got)
	}
}

func TestQualityCapLabelAndBitrate(t *testing.T) {
	if Source.Label() != "source" || QualityCap(720).Label() != "≤720p" {
		t.Errorf("labels: %q %q", Source.Label(), QualityCap(720).Label())
	}
	if QualityCap(1080).Bitrate() != 8_000_000 || QualityCap(999).Bitrate() != 4_000_000 {
		t.Error("bitrate mapping changed")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("JTERM_CONFIG_DIR", t.TempDir())
	cfg := Load()
	if !cfg.HWDec || cfg.QualityCap != Source {
		t.Errorf("defaults: hwdec=%v cap=%v; want true, source", cfg.HWDec, cfg.QualityCap)
	}
	if len(cfg.DeviceID) != 32 {
		t.Errorf("device id %q; want 32 hex chars", cfg.DeviceID)
	}
}

func TestLoadPythonFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JTERM_CONFIG_DIR", dir)
	python := `{
  "device_id": "abc123",
  "server": "http://192.168.1.50:8096",
  "server_name": "home",
  "username": "max",
  "hwdec": false,
  "quality_cap": 1080,
  "token": "secret",
  "user_id": "u1"
}`
	if err := os.WriteFile(Path(), []byte(python), 0o600); err != nil {
		t.Fatal(err)
	}
	want := Config{Server: "http://192.168.1.50:8096", ServerName: "home", DeviceID: "abc123",
		Token: "secret", UserID: "u1", Username: "max", HWDec: false, QualityCap: 1080}
	if got := Load(); got != want {
		t.Errorf("Load() = %+v\nwant     %+v", got, want)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	t.Setenv("JTERM_CONFIG_DIR", t.TempDir()+"/nested")
	want := Config{Server: "http://s", DeviceID: "d", Token: "t", UserID: "u", HWDec: true, QualityCap: 720}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got != want {
		t.Errorf("round trip = %+v; want %+v", got, want)
	}
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode %o; want 600 — it holds the session token", mode)
	}
	entries, _ := os.ReadDir(Dir())
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestLoadCorruptFile(t *testing.T) {
	t.Setenv("JTERM_CONFIG_DIR", t.TempDir())
	if err := os.WriteFile(Path(), []byte(`{"server": "http://s", "hwdec": fal`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := Load(); cfg.Server != "" || !cfg.HWDec {
		t.Errorf("corrupt file should give defaults, got %+v", cfg)
	}
}
