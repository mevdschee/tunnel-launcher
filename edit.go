package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// defaultKeepAliveSeconds is the fallback used when neither the per-tunnel
// nor the file-level keep_alive is set. Surfaced in the edit form so the
// user can see the effective default they're inheriting.
const defaultKeepAliveSeconds = 120

// tunnelEntry is the tunnel-launcher native config representation. The on-disk
// format is owned by us — we don't share with any other tool's TOML schema.
//
// `forward` carries the ssh-style spec (-L/-R/-D), so the same notation
// users see in the GUI is what they edit on disk.
type tunnelEntry struct {
	Name          string `toml:"name"`
	Host          string `toml:"host"`
	Forward       string `toml:"forward"`
	User          string `toml:"user,omitempty"`
	Port          int    `toml:"port,omitempty"`
	Identity      string `toml:"identity,omitempty"`
	JumpHosts     string `toml:"jump_hosts,omitempty"`
	KeepAlive     *int   `toml:"keep_alive,omitempty"`
	App           string `toml:"app,omitempty"`
	AutoReconnect bool   `toml:"auto_reconnect,omitempty"`
}

// tunnelsFile is the editable representation of the entire config file.
type tunnelsFile struct {
	KeepAlive *int          `toml:"keep_alive,omitempty"`
	Tunnels   []tunnelEntry `toml:"tunnels"`
}

// configPath returns the path to the tunnel-launcher config file, respecting
// $BORING_GUI_CONFIG and platform conventions.
func configPath() string {
	if p := os.Getenv("TUNNEL_LAUNCHER_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	switch {
	case home == "":
		return ".tunnel-launcher.toml"
	case runtimeGOOS == "linux":
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "tunnel-launcher", "config.toml")
		}
		return filepath.Join(home, ".config", "tunnel-launcher", "config.toml")
	default:
		return filepath.Join(home, ".tunnel-launcher.toml")
	}
}

func loadTunnelsFile() (*tunnelsFile, error) {
	tf := &tunnelsFile{}
	path := configPath()
	if _, err := os.Stat(path); err != nil {
		return tf, nil
	}
	if _, err := toml.DecodeFile(path, tf); err != nil {
		return nil, fmt.Errorf("decode %s: %v", path, err)
	}
	return tf, nil
}

func saveTunnelsFile(tf *tunnelsFile) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(tf); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// toDesc converts an editable entry into the runtime Desc.
func (e tunnelEntry) toDesc(fileKeepAlive *int) (Desc, error) {
	mode, local, remote, err := parseSSHSpec(e.Forward)
	if err != nil {
		return Desc{}, fmt.Errorf("tunnel %q: %v", e.Name, err)
	}
	var m Mode
	switch mode {
	case "remote":
		m = ModeRemote
	case "socks":
		m = ModeSocks
	default:
		m = ModeLocal
	}
	ka := defaultKeepAliveSeconds
	switch {
	case e.KeepAlive != nil:
		ka = *e.KeepAlive
	case fileKeepAlive != nil:
		ka = *fileKeepAlive
	}
	return Desc{
		Name:          e.Name,
		Host:          e.Host,
		User:          e.User,
		Port:          e.Port,
		Identity:      e.Identity,
		JumpHosts:     e.JumpHosts,
		Mode:          m,
		Local:         local,
		Remote:        remote,
		KeepAlive:     ka,
		AutoReconnect: e.AutoReconnect,
	}, nil
}

// parseSSHSpec accepts an ssh-style tunnel specification and returns the
// (mode, local, remote) triple. Recognised forms:
//
//	-L [bind:]port:host:hostport      → local
//	-R [bind:]port:host:hostport      → remote
//	-D [bind:]port                    → socks
//
// Whitespace between flag and operand is required.
func parseSSHSpec(spec string) (mode, local, remote string, err error) {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("expected `-L|-R|-D <spec>`, got %q", spec)
	}
	flag := fields[0]
	operand := ""
	if len(fields) >= 2 {
		operand = fields[1]
	}
	parts := strings.Split(operand, ":")
	// Once the flag is recognised, always return the flag-specific syntax
	// error — including when the operand is missing or has trailing junk.
	// Falling through to a generic "expected ..." message hides the shape
	// of the syntax the user was halfway through typing.
	switch flag {
	case "-L":
		if len(fields) == 2 {
			switch len(parts) {
			case 3:
				return "local", parts[0], parts[1] + ":" + parts[2], nil
			case 4:
				return "local", parts[0] + ":" + parts[1], parts[2] + ":" + parts[3], nil
			}
		}
		return "", "", "", fmt.Errorf("-L expects [bind:]port:host:hostport, got %q", operand)
	case "-R":
		if len(fields) == 2 {
			switch len(parts) {
			case 3:
				return "remote", parts[1] + ":" + parts[2], parts[0], nil
			case 4:
				return "remote", parts[2] + ":" + parts[3], parts[0] + ":" + parts[1], nil
			}
		}
		return "", "", "", fmt.Errorf("-R expects [bind:]remoteport:host:hostport, got %q", operand)
	case "-D":
		if len(fields) == 2 {
			switch len(parts) {
			case 1:
				return "socks", parts[0], "", nil
			case 2:
				return "socks", parts[0] + ":" + parts[1], "", nil
			}
		}
		return "", "", "", fmt.Errorf("-D expects [bind:]port, got %q", operand)
	}
	return "", "", "", fmt.Errorf("unknown flag %q (use -L/-R/-D)", flag)
}

// formatSSHSpec is the inverse of parseSSHSpec. For values that don't fit
// the standard notation (e.g. unix sockets), it falls back to a
// `<flag> <local> <remote>` form so a round-trip never silently drops data.
func formatSSHSpec(mode, local, remote string) string {
	switch mode {
	case "", "local":
		if isPortLike(local) && hasOnePortColon(remote) {
			return "-L " + local + ":" + remote
		}
		return fmt.Sprintf("-L %s %s", local, remote)
	case "remote":
		if isPortLike(remote) && hasOnePortColon(local) {
			return "-R " + remote + ":" + local
		}
		return fmt.Sprintf("-R %s %s", local, remote)
	case "socks":
		return "-D " + local
	}
	return fmt.Sprintf("%s %s %s", mode, local, remote)
}

func isPortLike(s string) bool {
	if s == "" {
		return false
	}
	c := strings.Count(s, ":")
	if c == 0 {
		return true
	}
	if c == 1 {
		parts := strings.SplitN(s, ":", 2)
		return parts[0] != "" && parts[1] != ""
	}
	return false
}

func hasOnePortColon(s string) bool {
	return strings.Count(s, ":") == 1 && !strings.HasPrefix(s, ":") && !strings.HasSuffix(s, ":")
}
