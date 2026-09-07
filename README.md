# tunnel-launcher

A cross-platform tray-icon GUI for managing SSH tunnels (-L / -R / -D). Single
statically-linked executable per platform (Linux / macOS / Windows).

![Tunnel Launcher Tray Menu](tray-menu.png)

> With Tunnel Launcher I no longer have to remember the exact ssh -L flags for
> each project. One click in the tray and the tunnel is up.

![Tunnel Launcher Main Window](main-window.png)

Blog: https://www.tqdev.com/2026-tunnel-launcher-ssh-tray-gui/

## Features

- Tray icon with per-tunnel status (open / closed) and click-to-toggle
- In-window list with Add / Edit / Remove forms — no text editor required
- SSH-style tunnel specification (`-L 9000:localhost:9000`, `-R …`, `-D …`)
- Reads `~/.ssh/config` for HostName, User, Port, IdentityFile, ProxyJump
- Auth via ssh-agent, explicit identity, or default identities
- Per-tunnel **launch app**: start a GUI program when the tunnel opens; the
  tunnel auto-closes when that program exits
- In-memory connection log viewable from a button, with a level selector
- Single-instance via pidfile — second invocation re-shows the window

## ssh-agent

Keys held by a running ssh-agent are offered first, so a key you unlocked once
with `ssh-add` never triggers a passphrase prompt. If the agent already holds
the key that a tunnel is configured to use, the file on disk is left alone.

On Linux and macOS the agent is found through `SSH_AUTH_SOCK`. On Windows there
is no such variable: the OpenSSH agent service listens on the named pipe
`\\.\pipe\openssh-ssh-agent`, which is used by default, and `SSH_AUTH_SOCK`
still wins when set (to either a named pipe or a unix socket). To start the
service and add a key:

```powershell
Get-Service ssh-agent | Set-Service -StartupType Automatic
Start-Service ssh-agent
ssh-add $env:USERPROFILE\.ssh\id_ed25519
```

The connection log (the Log button) lists every key the agent offers, which is
the first place to look if a tunnel still asks for a passphrase.

## Configuration

Default location:

- Linux: `~/.config/tunnel-launcher/config.toml`
- macOS / Windows: `~/.tunnel-launcher.toml`
- Override with `$TUNNEL_LAUNCHER_CONFIG`

```toml
log_level = "info"                    # optional: error, warn, info or debug

[[tunnels]]
name    = "dev"
host    = "dev-server"                # ssh alias or hostname
forward = "-L 9000:localhost:9000"    # ssh-style spec (-L / -R / -D)
user    = "neo"                       # optional
port    = 22                          # optional
identity = "~/.ssh/id_dev"            # optional
keep_alive = 120                      # optional, seconds; 0 to disable
app     = "code ."                    # optional, GUI app to launch
```

## Logging

Four levels, each showing everything above it:

| Level   | Shows                                                           |
| ------- | --------------------------------------------------------------- |
| `error` | a tunnel that would not open or close, a config file that would not load |
| `warn`  | a key that would not parse, an unreachable agent, an unexpected disconnect |
| `info`  | the connection lifecycle: resolve, auth, connect, listen, stop (default) |
| `debug` | one line per forwarded connection, on top of all of the above    |

The default is `info`, which is a handful of lines per tunnel. Pick `debug`
when a connection misbehaves and you want to see the individual forwards; it
scales with what the tunnel carries, so it is not what you want left on. The
log window keeps the last 2000 lines per tunnel, and messages below the
current level are dropped before they reach it, so debug traffic can never
push the connection log out of view.

The level is app-wide. Set it in the log window (the selector next to Clear,
which writes the choice to `log_level` in the config), in the config file, or
on the command line:

```sh
./tunnel-launcher -log-level debug   # wins over the config for this run
./tunnel-launcher -v                 # also stream the log to stdout
```

## Build

```sh
sudo apt-get install golang gcc libgl1-mesa-dev xorg-dev libxkbcommon-dev
```

This installs the [Fyne build dependencies](https://docs.fyne.io/started/) on
Debian (based) Linux.

```sh
./bundle.sh    # regenerates bundled.go from icon.png
go build       # produces ./tunnel-launcher
```

### Package using fyne-cross

Install fyne-cross using:

```sh
go install github.com/fyne-io/fyne-cross@latest
```

Now run the package.sh script to build all binaries (Docker required):

```sh
./package.sh   # produces fyne-cross/dist/tunnel-launcher-{amd64,arm64}.{tar.xz,exe.zip}
```

Enjoy!
