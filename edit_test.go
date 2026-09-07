package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSSHSpec(t *testing.T) {
	cases := []struct {
		in                string
		mode, local, rmt  string
		wantErr           bool
		errContainsSubstr string
	}{
		// -L
		{in: "-L 9000:localhost:9000", mode: "local", local: "9000", rmt: "localhost:9000"},
		{in: "-L 0.0.0.0:9000:localhost:9000", mode: "local", local: "0.0.0.0:9000", rmt: "localhost:9000"},
		{in: "  -L  9000:localhost:9000  ", mode: "local", local: "9000", rmt: "localhost:9000"}, // whitespace tolerance
		{in: "-L 9000", wantErr: true, errContainsSubstr: "[bind:]port:host:hostport"},
		{in: "-L 9000:host", wantErr: true, errContainsSubstr: "[bind:]port:host:hostport"},
		{in: "-L a:b:c:d:e", wantErr: true, errContainsSubstr: "[bind:]port:host:hostport"},

		// -R
		{in: "-R 8080:localhost:80", mode: "remote", local: "localhost:80", rmt: "8080"},
		{in: "-R 0.0.0.0:8080:localhost:80", mode: "remote", local: "localhost:80", rmt: "0.0.0.0:8080"},
		{in: "-R 8080", wantErr: true, errContainsSubstr: "[bind:]remoteport:host:hostport"},
		{in: "-R 8080:host", wantErr: true, errContainsSubstr: "[bind:]remoteport:host:hostport"},

		// -D
		{in: "-D 1080", mode: "socks", local: "1080"},
		{in: "-D 0.0.0.0:1080", mode: "socks", local: "0.0.0.0:1080"},
		{in: "-D 1080:foo:bar", wantErr: true, errContainsSubstr: "[bind:]port"},

		// errors
		{in: "", wantErr: true},
		{in: "-L", wantErr: true},
		{in: "-X 9000:host:9000", wantErr: true, errContainsSubstr: "unknown flag"},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.in, " ", "_"), func(t *testing.T) {
			mode, local, rmt, err := parseSSHSpec(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q/%q/%q", mode, local, rmt)
				}
				if c.errContainsSubstr != "" && !strings.Contains(err.Error(), c.errContainsSubstr) {
					t.Fatalf("error %q does not contain %q", err, c.errContainsSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != c.mode || local != c.local || rmt != c.rmt {
				t.Fatalf("got (%q,%q,%q), want (%q,%q,%q)", mode, local, rmt, c.mode, c.local, c.rmt)
			}
		})
	}
}

func TestFormatSSHSpec(t *testing.T) {
	cases := []struct {
		mode, local, rmt string
		want             string
	}{
		{"local", "9000", "localhost:9000", "-L 9000:localhost:9000"},
		{"", "9000", "localhost:9000", "-L 9000:localhost:9000"}, // empty mode == local
		{"local", "0.0.0.0:9000", "localhost:9000", "-L 0.0.0.0:9000:localhost:9000"},
		{"remote", "localhost:80", "8080", "-R 8080:localhost:80"},
		{"remote", "localhost:80", "0.0.0.0:8080", "-R 0.0.0.0:8080:localhost:80"},
		{"socks", "1080", "", "-D 1080"},
		// fallback when local/remote can't fit ssh-style notation
		// (multiple colons fail isPortLike).
		{"local", "1:2:3", "localhost:9000", "-L 1:2:3 localhost:9000"},
		{"remote", "1:2:3", "localhost:80", "-R 1:2:3 localhost:80"},
		{"weird", "a", "b", "weird a b"},
	}
	for _, c := range cases {
		t.Run(c.mode+"_"+c.local+"_"+c.rmt, func(t *testing.T) {
			got := formatSSHSpec(c.mode, c.local, c.rmt)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Round-trip: every standard form parsed by parseSSHSpec must reformat to
// the same canonical string.
func TestSSHSpecRoundTrip(t *testing.T) {
	canonical := []string{
		"-L 9000:localhost:9000",
		"-L 0.0.0.0:9000:localhost:9000",
		"-R 8080:localhost:80",
		"-R 0.0.0.0:8080:localhost:80",
		"-D 1080",
		"-D 0.0.0.0:1080",
	}
	for _, in := range canonical {
		mode, local, rmt, err := parseSSHSpec(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		out := formatSSHSpec(mode, local, rmt)
		if out != in {
			t.Fatalf("round-trip mismatch: %q → (%q,%q,%q) → %q", in, mode, local, rmt, out)
		}
	}
}

func TestIsPortLike(t *testing.T) {
	cases := map[string]bool{
		"":            false,
		"9000":        true,
		"0.0.0.0:80":  true,
		"localhost:5": true,
		":80":         false,
		"9000:":       false,
		"a:b:c":       false,
	}
	for in, want := range cases {
		if got := isPortLike(in); got != want {
			t.Errorf("isPortLike(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHasOnePortColon(t *testing.T) {
	cases := map[string]bool{
		"host:80": true,
		"host":    false,
		":80":     false,
		"host:":   false,
		"a:b:c":   false,
		"":        false,
	}
	for in, want := range cases {
		if got := hasOnePortColon(in); got != want {
			t.Errorf("hasOnePortColon(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestConfigPath_EnvOverride(t *testing.T) {
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", "/explicit/override.toml")
	if got := configPath(); got != "/explicit/override.toml" {
		t.Fatalf("got %q", got)
	}
}

func TestConfigPath_LinuxXDG(t *testing.T) {
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	t.Setenv("HOME", "/home/x")
	prev := runtimeGOOS
	runtimeGOOS = "linux"
	defer func() { runtimeGOOS = prev }()

	want := "/xdg/tunnel-launcher/config.toml"
	if got := configPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestConfigPath_LinuxDefault(t *testing.T) {
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/x")
	prev := runtimeGOOS
	runtimeGOOS = "linux"
	defer func() { runtimeGOOS = prev }()

	want := "/home/x/.config/tunnel-launcher/config.toml"
	if got := configPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestConfigPath_Darwin(t *testing.T) {
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", "")
	t.Setenv("HOME", "/Users/x")
	prev := runtimeGOOS
	runtimeGOOS = "darwin"
	defer func() { runtimeGOOS = prev }()

	want := "/Users/x/.tunnel-launcher.toml"
	if got := configPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLoadTunnelsFile_MissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", filepath.Join(dir, "nope.toml"))

	tf, err := loadTunnelsFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tf == nil || len(tf.Tunnels) != 0 || tf.KeepAlive != nil {
		t.Fatalf("expected empty tunnelsFile, got %+v", tf)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", path)

	ka := 60
	override := 30
	tf := &tunnelsFile{
		KeepAlive: &ka,
		LogLevel:  "debug",
		Tunnels: []tunnelEntry{
			{Name: "a", Host: "h1", Forward: "-L 9000:localhost:9000"},
			{Name: "b", Host: "h2", Forward: "-D 1080", KeepAlive: &override, App: "code ."},
			{Name: "c", Host: "h3", Forward: "-L 80:localhost:80", AutoReconnect: true},
		},
	}
	if err := saveTunnelsFile(tf); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File must be 0600 and live at the configured path.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}

	got, err := loadTunnelsFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, tf) {
		t.Fatalf("round-trip mismatch:\n got:  %+v\n want: %+v", got, tf)
	}

	// File-level keys must be written before the [[tunnels]] tables — TOML
	// would otherwise read them back as fields of the last tunnel.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{"keep_alive", "log_level"} {
		if i, j := bytes.Index(raw, []byte(key)), bytes.Index(raw, []byte("[[tunnels]]")); i < 0 || i > j {
			t.Errorf("%s at %d, want before [[tunnels]] at %d:\n%s", key, i, j, raw)
		}
	}
}

// SaveTunnelsFile writes through a temp file then renames; on save
// failure, the original file must remain intact.
func TestSaveTunnelsFile_AtomicOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("TUNNEL_LAUNCHER_CONFIG", path)

	if err := saveTunnelsFile(&tunnelsFile{Tunnels: []tunnelEntry{{Name: "first", Forward: "-D 1080"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := saveTunnelsFile(&tunnelsFile{Tunnels: []tunnelEntry{{Name: "second", Forward: "-D 2080"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	tf, err := loadTunnelsFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(tf.Tunnels) != 1 || tf.Tunnels[0].Name != "second" {
		t.Fatalf("expected only 'second' to remain, got %+v", tf.Tunnels)
	}
	// The .tmp side-file should not linger.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected .tmp to be cleaned up, stat err=%v", err)
	}
}

func TestEntryToDesc_KeepAlivePrecedence(t *testing.T) {
	override := 30
	fileLevel := 60
	cases := []struct {
		name     string
		entry    tunnelEntry
		fileKA   *int
		expectKA int
	}{
		{"entry-wins", tunnelEntry{Name: "x", Forward: "-D 1080", KeepAlive: &override}, &fileLevel, 30},
		{"file-fallback", tunnelEntry{Name: "x", Forward: "-D 1080"}, &fileLevel, 60},
		{"default-when-both-nil", tunnelEntry{Name: "x", Forward: "-D 1080"}, nil, 120},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := c.entry.toDesc(c.fileKA)
			if err != nil {
				t.Fatalf("toDesc: %v", err)
			}
			if d.KeepAlive != c.expectKA {
				t.Errorf("KeepAlive = %d, want %d", d.KeepAlive, c.expectKA)
			}
		})
	}
}

func TestEntryToDesc_ModeMapping(t *testing.T) {
	cases := []struct {
		fwd      string
		wantMode Mode
	}{
		{"-L 9000:localhost:9000", ModeLocal},
		{"-R 8080:localhost:80", ModeRemote},
		{"-D 1080", ModeSocks},
	}
	for _, c := range cases {
		t.Run(c.fwd, func(t *testing.T) {
			d, err := tunnelEntry{Name: "x", Forward: c.fwd}.toDesc(nil)
			if err != nil {
				t.Fatalf("toDesc: %v", err)
			}
			if d.Mode != c.wantMode {
				t.Errorf("Mode = %v, want %v", d.Mode, c.wantMode)
			}
		})
	}
}

func TestEntryToDesc_PassesAutoReconnect(t *testing.T) {
	d, err := tunnelEntry{
		Name:          "x",
		Forward:       "-D 1080",
		AutoReconnect: true,
	}.toDesc(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.AutoReconnect {
		t.Errorf("AutoReconnect = false, want true")
	}
}

func TestEntryToDesc_BadForward(t *testing.T) {
	_, err := tunnelEntry{Name: "broken", Forward: "garbage"}.toDesc(nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `tunnel "broken"`) {
		t.Errorf("error %q does not include tunnel name", err)
	}
}
