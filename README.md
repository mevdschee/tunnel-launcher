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
./package.sh   # produces fyne-cross/dist/tunnel-launcher-{amd64,arm64}.{tar.xz,exe.zip,app.zip}
```

### Windows OpenGL

Fyne draws through OpenGL 2, but Windows itself only ships the GDI generic
OpenGL 1.1 implementation, so on a machine without working graphics drivers the
window and the tray never come up. Installing the GPU driver fixes it. Where
that is not an option, such as over RDP or in a virtual machine, install
[Mesa3D for Windows](https://github.com/pal1000/mesa-dist-win), which replaces
`opengl32.dll` with a software renderer either system wide or next to the exe.
Tested with Mesa 26.0.6 on Windows 11.

### macOS build

Cross compiling to macOS needs a copy of the macOS SDK, which fyne-cross does
not ship. Use 12.3 or newer, because the Go standard library links against
`SecTrustCopyCertificateChain`, which was added in macOS 12.

Download the SDK:

```sh
mkdir -p ~/SDKs && cd ~/SDKs
curl -LO https://github.com/joseluisq/macosx-sdks/releases/download/12.3/MacOSX12.3.sdk.tar.xz
tar xf MacOSX12.3.sdk.tar.xz
```

fyne-cross 1.6.3 mounts the SDK at /sdk and tells zig to link against
`-F/System/Library/Frameworks`, but zig only applies the sysroot to `-L`, not to
`-F`, so every framework fails to resolve. Build a darwin image that has the
path symlinked into the mounted SDK:

```sh
docker build -t fyne-cross-darwin-sdk - <<EOF
FROM fyneio/fyne-cross-images:darwin
RUN mkdir -p /System/Library && ln -s /sdk/System/Library/Frameworks /System/Library/Frameworks
EOF
```

Then build the app bundles:

```sh
~/go/bin/fyne-cross darwin -arch=amd64,arm64 \
  -app-id com.tqdev.tunnel-launcher \
  -macosx-sdk-path ~/SDKs/MacOSX12.3.sdk -image fyne-cross-darwin-sdk
```

The first run pulls the darwin container image, which is a few gigabytes. Unlike
the linux and windows targets, `-app-id` is required.

The package.sh script builds the image, runs this for both architectures and
zips the resulting app bundles. It expects the SDK in the location above,
override it with the `MACOSX_SDK` environment variable.

The resulting app is unsigned, so macOS refuses to open it on first launch. Use
the Open entry in the right click menu, or drop the quarantine flag with
`xattr -dr com.apple.quarantine tunnel-launcher.app`.

macOS has no accelerated OpenGL renderer in a virtual machine, and glfw always
asks for one, so the tray and the window would fail to come up with "NSGL:
Failed to find a suitable pixel format". `third_party/glfw` carries a patched
copy of glfw that retries with Apple's CPU renderer when that happens, wired up
through a `replace` in go.mod. See third_party/README.md for the details.

Enjoy!
