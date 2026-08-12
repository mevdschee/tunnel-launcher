package main

import (
	"flag"
	"fmt"
	"image/color"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/mevdschee/pidfile"
)

const (
	Version     = "v0.1.0"
	AppID       = "com.tqdev.tunnel-launcher"
	WindowTitle = "tunnel-launcher"
	// Initial size also acts as the minimum — sized to host the edit form
	// without clipping. Set as MinSize on the content so the user can't
	// drag the window any smaller.
	WindowWidth  = 600
	WindowHeight = 600
)

func init() {
	if runtime.GOOS != "windows" {
		return
	}

	// Force Mesa to use the CPU-based llvmpipe driver instead of hardware GPU.
	os.Setenv("GALLIUM_DRIVER", "llvmpipe")
	os.Setenv("LIBGL_ALWAYS_SOFTWARE", "1")

	// Make sure the executable directory is on PATH so local Mesa DLLs can be found.
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if exeDir != "" {
			currentPath := os.Getenv("PATH")
			if currentPath == "" {
				os.Setenv("PATH", exeDir)
			} else if !strings.Contains(currentPath, exeDir) {
				os.Setenv("PATH", exeDir+string(os.PathListSeparator)+currentPath)
			}
		}
	}
}

func main() {
	flag.BoolVar(&verbose, "v", false, "verbose: stream all log output to stdout")
	flag.Parse()
	runGUI()
}

func runGUI() {
	a := app.NewWithID(AppID)
	a.SetIcon(resourceIconPng)
	w := a.NewWindow(WindowTitle)
	w.Resize(fyne.NewSize(WindowWidth, WindowHeight))

	// Single-instance: second invocation re-shows the existing window.
	pf := pidfile.New(AppID)
	pf.OnSecond = func([]string) {
		w.Show()
		w.RequestFocus()
	}
	if err := pf.Create(); err != nil {
		log.Fatal(err)
	}
	defer pf.Remove()
	if pf.FirstPid != os.Getpid() {
		a.Quit()
		return
	}

	appLog("tunnel-launcher %s starting", Version)

	prompts := newGUIPrompts(a, w)
	hke, err := newHostKeyEnforcer(prompts)
	if err != nil {
		log.Fatalf("host key store: %v", err)
	}
	mgr := newTunnelManager(prompts, hke.Callback())
	apps := newLaunchedApps()
	st := newState()

	editMode := false

	// Forward declarations so closures can call each other.
	var (
		refresh       func()
		rebuildTray   func()
		openTun       func(*Desc, bool)
		closeTun      func(name string)
		launchTun     func(*Desc)
		toggleTun     func(*Desc)
		showEditDlg   func(idx int)
		applyEdits    func() error
		leaveEditMode func()
		list          *widget.List
	)

	addBtn := widget.NewButtonWithIcon("", theme.ContentAddIcon(), func() {
		showTunnelForm(w, tunnelEntry{Name: "new-tunnel", Forward: "-L 9000:localhost:9000"}, st.fileKeepAliveDefault(), func(updated tunnelEntry) {
			st.addEntry(updated)
			if err := applyEdits(); err != nil {
				st.deleteEntry(st.numConfigured() - 1)
				dialog.ShowError(err, w)
				return
			}
			list.Refresh()
			rebuildTray()
			if editMode {
				leaveEditMode()
			}
		})
	})
	editToggle := widget.NewButtonWithIcon("", theme.SettingsIcon(), nil)
	doneBtn := widget.NewButtonWithIcon("", theme.ConfirmIcon(), nil)
	doneBtn.Importance = widget.HighImportance
	doneBtn.Hide()

	bottomBar := container.NewBorder(nil, nil, container.NewVBox(editToggle, doneBtn), addBtn)

	// List (forward declared so its UpdateItem can call list.Refresh).
	list = widget.NewList(
		func() int { return st.len() },
		func() fyne.CanvasObject {
			status := widget.NewLabel("○")
			name := widget.NewLabel("name")
			name.Truncation = fyne.TextTruncateEllipsis
			uptime := widget.NewLabel("")

			toggleB := widget.NewButtonWithIcon("", theme.MediaPlayIcon(), nil)
			logB := widget.NewButtonWithIcon("", theme.DocumentIcon(), nil)
			runActions := container.NewHBox(toggleB, logB)

			editB := widget.NewButtonWithIcon("", theme.DocumentCreateIcon(), nil)
			upB := widget.NewButtonWithIcon("", theme.MoveUpIcon(), nil)
			downB := widget.NewButtonWithIcon("", theme.MoveDownIcon(), nil)
			delB := widget.NewButtonWithIcon("", theme.DeleteIcon(), nil)
			editActions := container.NewHBox(editB, upB, downB, delB)

			actions := container.NewStack(runActions, editActions)
			right := container.NewHBox(uptime, actions)
			return container.NewBorder(nil, nil, status, right, name)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			descs := st.descs()
			if i >= len(descs) {
				return
			}
			t := descs[i]

			border := o.(*fyne.Container)
			name := border.Objects[0].(*widget.Label)
			status := border.Objects[1].(*widget.Label)
			right := border.Objects[2].(*fyne.Container)
			uptime := right.Objects[0].(*widget.Label)
			actions := right.Objects[1].(*fyne.Container)

			runActions := actions.Objects[0].(*fyne.Container)
			editActions := actions.Objects[1].(*fyne.Container)
			toggleB := runActions.Objects[0].(*widget.Button)
			logB := runActions.Objects[1].(*widget.Button)
			editB := editActions.Objects[0].(*widget.Button)
			upB := editActions.Objects[1].(*widget.Button)
			downB := editActions.Objects[2].(*widget.Button)
			delB := editActions.Objects[3].(*widget.Button)

			name.TextStyle = fyne.TextStyle{Bold: t.Status == StatusOpen}
			name.SetText(displayName(t))
			status.SetText(statusGlyph(t))
			uptime.SetText(uptimeText(t))

			if editMode {
				runActions.Hide()
				editActions.Show()
				idx := i
				editB.OnTapped = func() { showEditDlg(idx) }
				upB.OnTapped = func() {
					if st.move(idx, idx-1) {
						list.Refresh()
						rebuildTray()
					}
				}
				downB.OnTapped = func() {
					if st.move(idx, idx+1) {
						list.Refresh()
						rebuildTray()
					}
				}
				delB.OnTapped = func() {
					t := descs[idx]
					dialog.ShowConfirm("Delete tunnel",
						fmt.Sprintf("Delete %q?", t.Name),
						func(ok bool) {
							if !ok {
								return
							}
							st.deleteEntry(idx)
							list.Refresh()
							rebuildTray()
						}, w)
				}
				if idx == 0 {
					upB.Disable()
				} else {
					upB.Enable()
				}
				if idx >= len(descs)-1 || !st.isConfigured(idx) {
					downB.Disable()
				} else {
					downB.Enable()
				}
				if !st.isConfigured(idx) {
					editB.Disable()
					delB.Disable()
				} else {
					editB.Enable()
					delB.Enable()
				}
				return
			}

			runActions.Show()
			editActions.Hide()
			if t.Status == StatusOpen {
				toggleB.SetIcon(theme.MediaStopIcon())
			} else {
				toggleB.SetIcon(theme.MediaPlayIcon())
			}
			toggleB.OnTapped = func() { toggleTun(t) }
			logB.OnTapped = func() { showLogWindow(a, mgr.bufferFor(t.Name), t.Name) }
		},
	)

	list.OnSelected = func(id widget.ListItemID) {
		defer list.Unselect(id)
		descs := st.descs()
		if id >= len(descs) {
			return
		}
		if editMode {
			if !st.isConfigured(id) {
				return
			}
			showEditDlg(id)
			return
		}
		// Outside edit mode, tapping the row toggles the tunnel.
		toggleTun(descs[id])
	}

	// Fyne windows derive their minimum size from their content's MinSize,
	// so a transparent canvas.Rectangle with an explicit MinSize stacked
	// behind the real content prevents the user from shrinking the window
	// below where the edit form (560×380) still fits comfortably.
	minSizer := canvas.NewRectangle(color.Transparent)
	minSizer.SetMinSize(fyne.NewSize(WindowWidth, WindowHeight))
	w.SetContent(container.NewStack(minSizer, container.NewBorder(nil, bottomBar, nil, nil, list)))
	w.SetCloseIntercept(func() {
		w.Hide()
		rebuildTray()
	})

	// Tray.
	var trayMenu *fyne.Menu
	if d, ok := a.(desktop.App); ok {
		d.SetSystemTrayIcon(resourceIconPng)
		trayMenu = fyne.NewMenu("tunnel-launcher")
		d.SetSystemTrayMenu(trayMenu)
	} else {
		w.Show()
	}

	enterEditMode := func() {
		editMode = true
		editToggle.Hide()
		doneBtn.Show()
		list.Refresh()
	}
	leaveEditMode = func() {
		editMode = false
		doneBtn.Hide()
		editToggle.Show()
		list.Refresh()
		go refresh()
	}
	editToggle.OnTapped = enterEditMode
	doneBtn.OnTapped = func() {
		if err := applyEdits(); err != nil {
			dialog.ShowError(err, w)
			return
		}
		leaveEditMode()
	}

	// Action implementations.
	openTun = func(t *Desc, fromTray bool) {
		if err := mgr.open(*t); err != nil {
			mgr.loggerFor(t.Name)("[%s] open failed: %v", t.Name, err)
			if isHostKeyMismatch(err) {
				// The host-key callback already opened a popup; don't double-show.
				return
			}
			wrapped := fmt.Errorf("open %s: %v", t.Name, err)
			// Always anchor the error dialog on the main window; if the
			// user triggered the open from the tray with the window
			// hidden, surface the window first so the dialog has a parent
			// that's actually on screen. A standalone error window used
			// to handle the tray case but caused subsequent dialogs on
			// the main window to render incorrectly.
			if fromTray {
				w.Show()
				w.RequestFocus()
			}
			dialog.ShowError(wrapped, w)
			return
		}
		refresh()
	}
	closeTun = func(name string) {
		apps.kill(name)
		if err := mgr.close(name); err != nil {
			mgr.loggerFor(name)("[%s] close failed: %v", name, err)
			dialog.ShowError(fmt.Errorf("close %s: %v", name, err), w)
			return
		}
		refresh()
	}
	launchTun = func(t *Desc) {
		cmd, ok := st.appFor(t.Name)
		if !ok {
			dialog.ShowInformation("No app configured",
				"Use Edit to set the `app` command for this tunnel.", w)
			return
		}
		if t.Status != StatusOpen {
			if err := mgr.open(*t); err != nil {
				dialog.ShowError(fmt.Errorf("open %s: %v", t.Name, err), w)
				return
			}
		}
		err := apps.start(t.Name, cmd, func(_ error) {
			mgr.loggerFor(t.Name)("[%s] launched app exited; closing tunnel", t.Name)
			_ = mgr.close(t.Name)
			refresh()
		})
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		mgr.loggerFor(t.Name)("[%s] launched: %s", t.Name, cmd)
		refresh()
	}
	toggleTun = func(t *Desc) {
		// Closed tunnels open through launchTun when an app is configured
		// so the app comes up alongside the tunnel; otherwise just open.
		// Open tunnels close (which also kills the launched app).
		if t.Status == StatusOpen {
			go closeTun(t.Name)
		} else if _, hasApp := st.appFor(t.Name); hasApp {
			go launchTun(t)
		} else {
			go openTun(t, false)
		}
	}

	showEditDlg = func(idx int) {
		entry, ok := st.entry(idx)
		if !ok {
			return
		}
		showTunnelForm(w, entry, st.fileKeepAliveDefault(), func(updated tunnelEntry) {
			st.replaceEntry(idx, updated)
			if err := applyEdits(); err != nil {
				st.replaceEntry(idx, entry)
				dialog.ShowError(err, w)
				return
			}
			list.Refresh()
			rebuildTray()
			if editMode {
				leaveEditMode()
			}
		})
	}

	applyEdits = func() error {
		tf := st.snapshotFile()
		seen := map[string]bool{}
		for _, t := range tf.Tunnels {
			if t.Name == "" {
				return fmt.Errorf("tunnel name cannot be empty")
			}
			if seen[t.Name] {
				return fmt.Errorf("duplicate tunnel name %q", t.Name)
			}
			seen[t.Name] = true
		}
		return saveTunnelsFile(tf)
	}

	// Tray rebuild — called on startup and on window-close.
	rebuildTray = func() {
		if trayMenu == nil {
			return
		}
		items := []*fyne.MenuItem{
			fyne.NewMenuItem("Show", func() { w.Show(); w.RequestFocus() }),
			fyne.NewMenuItemSeparator(),
		}
		for _, t := range st.descs() {
			t := t
			label := fmt.Sprintf("%s  %s", statusGlyph(t), displayName(t))
			_, hasApp := st.appFor(t.Name)
			it := fyne.NewMenuItem(label, func() {
				if t.Status == StatusOpen {
					go closeTun(t.Name)
				} else if hasApp {
					go launchTun(t)
				} else {
					go openTun(t, true)
				}
			})
			items = append(items, it)
		}
		items = append(items,
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Quit", func() {
				for _, n := range apps.runningNames() {
					apps.kill(n)
				}
				mgr.closeAll()
				a.Quit()
			}),
		)
		trayMenu.Items = items
		trayMenu.Refresh()
	}

	// lastRunKey tracks the previous (name, Status) signature of running
	// tunnels so refresh() can rebuild the tray only when the connection
	// set actually changes — covering manual open/close, spontaneous
	// disconnects, and reconnects — without churning the menu every tick.
	var lastRunKey string
	var lastConfigModTime time.Time
	var lastConfigSize int64

	runKey := func(snap map[string]Desc) string {
		keys := make([]string, 0, len(snap))
		for n, d := range snap {
			keys = append(keys, fmt.Sprintf("%s=%d", n, d.Status))
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}

	refresh = func() {
		needsRefresh := false

		if !editMode {
			path := configPath()
			stat, err := os.Stat(path)
			var modTime time.Time
			var size int64
			if err == nil {
				modTime = stat.ModTime()
				size = stat.Size()
			}
			
			if modTime != lastConfigModTime || size != lastConfigSize || lastConfigModTime.IsZero() {
				lastConfigModTime = modTime
				lastConfigSize = size
				tf, err := loadTunnelsFile()
				if err != nil {
					appLog("config error: %v", err)
				} else {
					st.setFile(tf)
					needsRefresh = true
				}
			}
		}
		
		snap := mgr.snapshot()
		st.setRunning(snap)
		
		if k := runKey(snap); k != lastRunKey {
			lastRunKey = k
			needsRefresh = true
			rebuildTray()
		}

		if needsRefresh {
			list.Refresh()
		}
	}

	go func() {
		refresh()
		rebuildTray()
	}()

	// Periodic in-window refresh only — tray is updated on close + startup.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()

	a.Run()
}

// showTunnelForm displays a modal form to edit a tunnelEntry. onSave fires
// only when the user accepts the dialog. defaultKeepAlive is the value the
// tunnel will inherit if its own keep-alive is left blank — shown in the
// hint so the user sees what "empty" actually means right now.
func showTunnelForm(parent fyne.Window, entry tunnelEntry, defaultKeepAlive int, onSave func(tunnelEntry)) {
	nameE := widget.NewEntry()
	nameE.SetText(entry.Name)
	hostE := widget.NewEntry()
	hostE.SetText(entry.Host)
	forwardE := widget.NewEntry()
	forwardE.SetText(entry.Forward)
	forwardE.SetPlaceHolder("-L 9000:localhost:9000")
	// Empty is allowed (Forward is optional). The Validator drives Fyne's
	// standard validation behavior: the Save button is disabled while the
	// entry is invalid, and the red error indicator appears once the user
	// blurs the field — Fyne deliberately defers the indicator until then
	// so we don't yell at the user mid-keystroke.
	forwardE.Validator = func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		_, _, _, err := parseSSHSpec(s)
		return err
	}
	userE := widget.NewEntry()
	userE.SetText(entry.User)
	identityE := widget.NewEntry()
	identityE.SetText(entry.Identity)
	jumpE := widget.NewEntry()
	jumpE.SetText(entry.JumpHosts)
	jumpE.SetPlaceHolder("user@host:port,user@host2:port")
	portE := widget.NewEntry()
	if entry.Port != 0 {
		portE.SetText(strconv.Itoa(entry.Port))
	}
	appE := widget.NewEntry()
	appE.SetText(entry.App)
	appE.SetPlaceHolder("e.g. code . — closes tunnel when app exits")

	keepAliveE := widget.NewEntry()
	if entry.KeepAlive != nil {
		keepAliveE.SetText(strconv.Itoa(*entry.KeepAlive))
	}
	keepAliveE.Validator = func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if _, err := strconv.Atoi(s); err != nil {
			return fmt.Errorf("must be a number")
		}
		return nil
	}

	reconnectChk := widget.NewCheck("", nil)
	reconnectChk.SetChecked(entry.AutoReconnect)

	form := []*widget.FormItem{
		{Text: "Name", Widget: nameE},
		{Text: "Host", Widget: hostE},
		{Text: "Forward", Widget: forwardE, HintText: "-L|R port:host:hostport   -D port"},
		{Text: "User", Widget: userE},
		{Text: "Identity", Widget: identityE},
		{Text: "Jump Hosts", Widget: jumpE, HintText: "Comma-separated: user@host:port"},
		{Text: "Port", Widget: portE},
		{Text: "App (Launch)", Widget: appE},
		{Text: "Keep-alive", Widget: keepAliveE, HintText: fmt.Sprintf("seconds; 0 disables; empty defaults to %d", defaultKeepAlive)},
		{Text: "Auto-reconnect", Widget: reconnectChk, HintText: "Retry every 30s on disconnect"},
	}
	d := dialog.NewForm("Tunnel", "Save", "Cancel", form, func(ok bool) {
		if !ok {
			return
		}
		spec := strings.TrimSpace(forwardE.Text)
		if spec != "" {
			if _, _, _, err := parseSSHSpec(spec); err != nil {
				dialog.ShowError(err, parent)
				return
			}
		}
		port := 0
		if portE.Text != "" {
			p, err := strconv.Atoi(strings.TrimSpace(portE.Text))
			if err != nil {
				dialog.ShowError(fmt.Errorf("port must be a number: %v", err), parent)
				return
			}
			port = p
		}
		var keepAlive *int
		if s := strings.TrimSpace(keepAliveE.Text); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				dialog.ShowError(fmt.Errorf("keep-alive must be a number: %v", err), parent)
				return
			}
			keepAlive = &n
		}
		onSave(tunnelEntry{
			Name:          strings.TrimSpace(nameE.Text),
			Host:          strings.TrimSpace(hostE.Text),
			Forward:       spec,
			User:          strings.TrimSpace(userE.Text),
			Identity:      strings.TrimSpace(identityE.Text),
			JumpHosts:     strings.TrimSpace(jumpE.Text),
			Port:          port,
			App:           strings.TrimSpace(appE.Text),
			KeepAlive:     keepAlive,
			AutoReconnect: reconnectChk.Checked,
		})
	}, parent)
	d.Resize(fyne.NewSize(560, 420))
	d.Show()
}

// showLogWindow opens a separate window streaming a per-tunnel log buffer.
// name is shown in the title; the buffer's contents are rendered as-is.
func showLogWindow(a fyne.App, buf *logBuffer, name string) {
	title := "tunnel-launcher — log"
	if name != "" {
		title += ": " + name
	}
	w := a.NewWindow(title)
	w.Resize(fyne.NewSize(720, 480))

	render := func() string {
		return strings.Join(buf.Snapshot(), "\n")
	}

	entry := widget.NewMultiLineEntry()
	entry.Wrapping = fyne.TextWrapOff
	entry.SetText(render())

	clearBtn := widget.NewButtonWithIcon("Clear", theme.DeleteIcon(), func() {
		buf.Clear()
		entry.SetText("")
	})
	closeBtn := widget.NewButton("Close", func() {
		buf.SetOnAdd(nil)
		w.Close()
	})

	buf.SetOnAdd(func() {
		entry.SetText(render())
	})
	w.SetOnClosed(func() { buf.SetOnAdd(nil) })

	bar := container.NewHBox(clearBtn, closeBtn)
	w.SetContent(container.NewBorder(nil, bar, nil, nil, container.NewScroll(entry)))
	w.Show()
}

// state holds the editable file plus running statuses, with a mutex so
// background goroutines and the UI stay safe.
type state struct {
	mu      sync.RWMutex
	tf      *tunnelsFile
	running map[string]Desc
}

func newState() *state {
	return &state{tf: &tunnelsFile{}, running: map[string]Desc{}}
}

func (s *state) setFile(tf *tunnelsFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tf = tf
}

// fileKeepAliveDefault returns the effective keep-alive seconds inherited
// when a tunnel doesn't set one of its own: the file-level keep_alive if
// present, otherwise the hard-coded fallback.
func (s *state) fileKeepAliveDefault() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tf.KeepAlive != nil {
		return *s.tf.KeepAlive
	}
	return defaultKeepAliveSeconds
}

func (s *state) setRunning(r map[string]Desc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = r
}

func (s *state) snapshotFile() *tunnelsFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := &tunnelsFile{KeepAlive: s.tf.KeepAlive}
	out.Tunnels = append(out.Tunnels, s.tf.Tunnels...)
	return out
}

// descs returns the merged list: configured first (with running status
// overlaid where applicable), then running-but-not-configured tunnels.
func (s *state) descs() []*Desc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Desc
	visited := map[string]bool{}
	for _, e := range s.tf.Tunnels {
		d, err := e.toDesc(s.tf.KeepAlive)
		if err != nil {
			continue
		}
		if r, ok := s.running[e.Name]; ok {
			d.Status = r.Status
			d.LastConn = r.LastConn
			visited[e.Name] = true
		}
		dc := d
		out = append(out, &dc)
	}
	var extra []string
	for n := range s.running {
		if !visited[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		r := s.running[n]
		out = append(out, &r)
	}
	return out
}

func (s *state) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tf.Tunnels) + s.extraRunningCount()
}

func (s *state) extraRunningCount() int {
	configured := map[string]bool{}
	for _, e := range s.tf.Tunnels {
		configured[e.Name] = true
	}
	n := 0
	for k := range s.running {
		if !configured[k] {
			n++
		}
	}
	return n
}

func (s *state) numConfigured() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tf.Tunnels)
}

func (s *state) isConfigured(idx int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return idx < len(s.tf.Tunnels)
}

func (s *state) entry(idx int) (tunnelEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idx < 0 || idx >= len(s.tf.Tunnels) {
		return tunnelEntry{}, false
	}
	return s.tf.Tunnels[idx], true
}

func (s *state) replaceEntry(idx int, e tunnelEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < 0 || idx >= len(s.tf.Tunnels) {
		return
	}
	s.tf.Tunnels[idx] = e
}

func (s *state) addEntry(e tunnelEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tf.Tunnels = append(s.tf.Tunnels, e)
}

func (s *state) deleteEntry(idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < 0 || idx >= len(s.tf.Tunnels) {
		return
	}
	s.tf.Tunnels = append(s.tf.Tunnels[:idx], s.tf.Tunnels[idx+1:]...)
}

func (s *state) move(from, to int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from < 0 || from >= len(s.tf.Tunnels) || to < 0 || to >= len(s.tf.Tunnels) || from == to {
		return false
	}
	s.tf.Tunnels[from], s.tf.Tunnels[to] = s.tf.Tunnels[to], s.tf.Tunnels[from]
	return true
}

func (s *state) appFor(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.tf.Tunnels {
		if e.Name == name && e.App != "" {
			return e.App, true
		}
	}
	return "", false
}

func displayName(t *Desc) string {
	return t.Name
}

func statusGlyph(t *Desc) string {
	if t.Status == StatusOpen {
		return "●"
	}
	return "○"
}

func uptimeText(t *Desc) string {
	if t.Status != StatusOpen {
		return ""
	}
	since := time.Since(t.LastConn)
	days := int(since / (24 * time.Hour))
	hours := int(since/time.Hour) % 24
	mins := int(since/time.Minute) % 60
	secs := int(since/time.Second) % 60
	if days > 0 {
		return fmt.Sprintf("%02dd%02dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%02dh%02dm", hours, mins)
	}
	return fmt.Sprintf("%02dm%02ds", mins, secs)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}
