package main

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	reconnectDelay  = 30 * time.Second
	tunnelLogBufMax = 2000
)

// tunnelManager owns the live SSH tunnels and their per-tunnel log buffers.
// Buffers persist across opens/closes so the user can still inspect logs
// from a recently-disconnected tunnel.
type tunnelManager struct {
	mu        sync.RWMutex
	running   map[string]*Tunnel
	pending   map[string]chan struct{} // name → cancel channel for scheduled reconnects
	bufMu     sync.Mutex               // guards bufs only — separate so loggerFor is safe under mu
	bufs      map[string]*logBuffer    // name → per-tunnel ring buffer
	prompts   CredentialPrompts
	keyCache  *signerCache
	hostKeyCB ssh.HostKeyCallback
}

func newTunnelManager(prompts CredentialPrompts, hostKeyCB ssh.HostKeyCallback) *tunnelManager {
	return &tunnelManager{
		running:   map[string]*Tunnel{},
		pending:   map[string]chan struct{}{},
		bufs:      map[string]*logBuffer{},
		prompts:   prompts,
		keyCache:  newSignerCache(),
		hostKeyCB: hostKeyCB,
	}
}

// bufferFor returns the per-tunnel log buffer for name, creating it on
// first request. Safe to call from any goroutine, including under m.mu.
func (m *tunnelManager) bufferFor(name string) *logBuffer {
	m.bufMu.Lock()
	defer m.bufMu.Unlock()
	if b, ok := m.bufs[name]; ok {
		return b
	}
	b := newLogBuffer(tunnelLogBufMax)
	m.bufs[name] = b
	return b
}

// loggerFor returns a logFn that writes to the named tunnel's buffer and
// (when -v is set) also to stdout. This is what every connection-scoped
// log call should go through.
func (m *tunnelManager) loggerFor(name string) logFn {
	buf := m.bufferFor(name)
	return func(format string, args ...any) {
		buf.Log(format, args...)
		stdoutLog(format, args...)
	}
}

func (m *tunnelManager) open(d Desc) error {
	return m.openInternal(d, false)
}

func (m *tunnelManager) openInternal(d Desc, isAutoReconnect bool) error {
	// An explicit open cancels any pending reconnect for this name.
	if !isAutoReconnect {
		m.cancelPending(d.Name)
	}

	tlog := m.loggerFor(d.Name)

	m.mu.Lock()
	if _, ok := m.running[d.Name]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already running")
	}
	t := NewTunnel(d, tlog, m.prompts, m.keyCache, m.hostKeyCB)
	if err := t.Open(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.running[d.Name] = t
	m.mu.Unlock()

	go func() {
		<-t.Closed()
		m.mu.Lock()
		delete(m.running, d.Name)
		m.mu.Unlock()
		tlog("[%s] tunnel removed from manager", d.Name)

		if !t.UserClosed() && d.AutoReconnect {
			m.scheduleReconnect(d)
		}
	}()
	return nil
}

func (m *tunnelManager) close(name string) error {
	// Cancel a scheduled reconnect even if the tunnel is currently down.
	cancelled := m.cancelPending(name)

	m.mu.RLock()
	t, ok := m.running[name]
	m.mu.RUnlock()
	if !ok {
		if cancelled {
			return nil
		}
		return fmt.Errorf("tunnel %q is not running", name)
	}
	if err := t.Close(); err != nil {
		return err
	}
	<-t.Closed()
	return nil
}

// scheduleReconnect retries open() every reconnectDelay until success or until
// the schedule is cancelled (via close() or a manual open()).
func (m *tunnelManager) scheduleReconnect(d Desc) {
	cancel := make(chan struct{})
	m.mu.Lock()
	if old, ok := m.pending[d.Name]; ok {
		close(old)
	}
	m.pending[d.Name] = cancel
	m.mu.Unlock()

	tlog := m.loggerFor(d.Name)

	go func() {
		defer func() {
			m.mu.Lock()
			if c, ok := m.pending[d.Name]; ok && c == cancel {
				delete(m.pending, d.Name)
			}
			m.mu.Unlock()
		}()
		tlog("[%s] auto-reconnect: retrying in %s", d.Name, reconnectDelay)
		for {
			select {
			case <-cancel:
				tlog("[%s] auto-reconnect: cancelled", d.Name)
				return
			case <-time.After(reconnectDelay):
			}
			if err := m.openInternal(d, true); err != nil {
				tlog("[%s] auto-reconnect failed: %v (retrying in %s)", d.Name, err, reconnectDelay)
				continue
			}
			tlog("[%s] auto-reconnect: connected", d.Name)
			return
		}
	}()
}

func (m *tunnelManager) cancelPending(name string) bool {
	m.mu.Lock()
	c, ok := m.pending[name]
	if ok {
		delete(m.pending, name)
	}
	m.mu.Unlock()
	if ok {
		close(c)
	}
	return ok
}

// snapshot returns the current set of running tunnels keyed by name. The
// returned descs reflect live Status / LastConn at the moment of the call.
func (m *tunnelManager) snapshot() map[string]Desc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Desc, len(m.running))
	for n, t := range m.running {
		d := t.Desc
		d.Status = t.Status
		d.LastConn = t.LastConn
		out[n] = d
	}
	return out
}

// closeAll closes every running tunnel and waits for each to finish, and
// cancels any pending reconnects.
func (m *tunnelManager) closeAll() {
	m.mu.Lock()
	pendings := make([]chan struct{}, 0, len(m.pending))
	for _, c := range m.pending {
		pendings = append(pendings, c)
	}
	m.pending = map[string]chan struct{}{}
	ts := make([]*Tunnel, 0, len(m.running))
	for _, t := range m.running {
		ts = append(ts, t)
	}
	m.mu.Unlock()

	for _, c := range pendings {
		close(c)
	}
	for _, t := range ts {
		_ = t.Close()
	}
	for _, t := range ts {
		<-t.Closed()
	}
}
