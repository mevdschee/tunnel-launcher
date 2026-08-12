// SSH tunnel runtime.
//
// Supports the three modes the GUI exposes: local forward (-L), remote
// forward (-R), and dynamic local SOCKS5 (-D). SSH connection settings are
// merged from the user's ~/.ssh/config (via kevinburke/ssh_config) with any
// per-tunnel overrides taking precedence. Authentication tries an explicit
// identity file, then ssh-agent, then the standard default identities.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Mode int

const (
	ModeLocal Mode = iota
	ModeRemote
	ModeSocks
)

func (m Mode) String() string {
	switch m {
	case ModeRemote:
		return "remote"
	case ModeSocks:
		return "socks"
	default:
		return "local"
	}
}

type Status int

const (
	StatusClosed Status = iota
	StatusOpen
)

// Desc is the user-visible description of a tunnel.
//
// Status and LastConn are runtime fields, populated only on snapshots
// returned by tunnelManager — they're zero on a freshly-loaded config entry.
type Desc struct {
	Name          string
	Host          string
	User          string
	Port          int
	Identity      string
	JumpHosts     string // comma-separated: "user@host:port,user@host2:port"
	Mode          Mode
	Local         string
	Remote        string
	KeepAlive     int // seconds; 0 disables
	AutoReconnect bool

	Status   Status
	LastConn time.Time
}

type logFn func(format string, a ...any)

// Tunnel is the live, runnable instance of a Desc.
type Tunnel struct {
	Desc     Desc
	Status   Status
	LastConn time.Time

	log        logFn
	prompts    CredentialPrompts
	keyCache   *signerCache
	hostKeyCB  ssh.HostKeyCallback
	mu         sync.Mutex
	client     *ssh.Client
	listener   net.Listener
	closed     chan struct{}
	stop       chan struct{}
	userClosed bool
}

// UserClosed reports whether Close() was called on this tunnel (vs the SSH
// connection dropping spontaneously). Read after <-Closed().
func (t *Tunnel) UserClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.userClosed
}

func NewTunnel(d Desc, log logFn, prompts CredentialPrompts, keyCache *signerCache, hostKeyCB ssh.HostKeyCallback) *Tunnel {
	if log == nil {
		log = func(string, ...any) {}
	}
	if hostKeyCB == nil {
		hostKeyCB = ssh.InsecureIgnoreHostKey()
	}
	return &Tunnel{
		Desc:      d,
		log:       log,
		prompts:   prompts,
		keyCache:  keyCache,
		hostKeyCB: hostKeyCB,
		closed:    make(chan struct{}),
		stop:      make(chan struct{}),
	}
}

// Closed exposes a channel that is closed once the tunnel has fully torn down.
func (t *Tunnel) Closed() <-chan struct{} { return t.closed }

func (t *Tunnel) Open() error {
	prefix := "[" + t.Desc.Name + "] "
	tagged := func(format string, args ...any) {
		t.log(prefix+format, args...)
	}
	r := resolveHostLogged(t.Desc, tagged)
	tagged("connecting to %s@%s:%d (mode=%s)", r.user, r.host, r.port, t.Desc.Mode)

	cli, err := dialChain(r, t.prompts, t.keyCache, t.hostKeyCB, tagged)
	if err != nil {
		return fmt.Errorf("ssh dial: %v", err)
	}
	t.mu.Lock()
	t.client = cli
	t.mu.Unlock()
	t.log("[%s] ssh connection established", t.Desc.Name)

	if err := t.startMode(); err != nil {
		cli.Close()
		return err
	}

	t.Status = StatusOpen
	t.LastConn = time.Now()

	go t.run()
	return nil
}

func (t *Tunnel) Close() error {
	t.mu.Lock()
	select {
	case <-t.stop:
		t.mu.Unlock()
		return errors.New("tunnel already closed")
	default:
		close(t.stop)
	}
	t.userClosed = true
	if t.listener != nil {
		t.listener.Close()
	}
	if t.client != nil {
		t.client.Close()
	}
	t.mu.Unlock()
	return nil
}

func (t *Tunnel) run() {
	disconn := make(chan struct{})
	go func() {
		t.client.Wait()
		close(disconn)
	}()

	keepAliveDone := make(chan struct{})
	go func() {
		t.runKeepAlive(disconn)
		close(keepAliveDone)
	}()

	select {
	case <-t.stop:
		t.log("[%s] stop signal received", t.Desc.Name)
	case <-disconn:
		t.log("[%s] disconnected", t.Desc.Name)
	}

	t.mu.Lock()
	if t.listener != nil {
		t.listener.Close()
	}
	if t.client != nil {
		t.client.Close()
	}
	t.mu.Unlock()

	<-keepAliveDone
	t.Status = StatusClosed
	close(t.closed)
}

func (t *Tunnel) runKeepAlive(cancel <-chan struct{}) {
	if t.Desc.KeepAlive <= 0 {
		return
	}
	tick := time.NewTicker(time.Duration(t.Desc.KeepAlive) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-cancel:
			return
		case <-t.stop:
			return
		case <-tick.C:
			_, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				t.log("[%s] keepalive failed: %v", t.Desc.Name, err)
				t.client.Close()
				return
			}
		}
	}
}

func (t *Tunnel) startMode() error {
	switch t.Desc.Mode {
	case ModeLocal:
		return t.startLocal()
	case ModeRemote:
		return t.startRemote()
	case ModeSocks:
		return t.startSocks()
	}
	return fmt.Errorf("unknown mode")
}

func (t *Tunnel) startLocal() error {
	la := parseAddr(t.Desc.Local)
	ln, err := net.Listen(la.network, la.address)
	if err != nil {
		return fmt.Errorf("listen %s: %v", la, err)
	}
	t.listener = ln
	t.log("[%s] listening on %s, forwarding to %s", t.Desc.Name, la, t.Desc.Remote)
	go t.acceptLocal(ln)
	return nil
}

func (t *Tunnel) acceptLocal(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			ra := parseAddr(t.Desc.Remote)
			up, err := t.client.Dial(ra.network, ra.address)
			if err != nil {
				t.log("[%s] dial %s: %v", t.Desc.Name, ra, err)
				return
			}
			defer up.Close()
			t.log("[%s] forward %s ↔ %s", t.Desc.Name, c.RemoteAddr(), ra)
			pipe(c, up)
		}(c)
	}
}

func (t *Tunnel) startRemote() error {
	ra := parseAddr(t.Desc.Remote)
	ln, err := t.client.Listen(ra.network, ra.address)
	if err != nil {
		return fmt.Errorf("remote listen %s: %v", ra, err)
	}
	t.listener = ln
	t.log("[%s] remote listening on %s, forwarding to %s", t.Desc.Name, ra, t.Desc.Local)
	go t.acceptRemote(ln)
	return nil
}

func (t *Tunnel) acceptRemote(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			la := parseAddr(t.Desc.Local)
			down, err := net.Dial(la.network, la.address)
			if err != nil {
				t.log("[%s] dial %s: %v", t.Desc.Name, la, err)
				return
			}
			defer down.Close()
			t.log("[%s] reverse %s ↔ %s", t.Desc.Name, c.RemoteAddr(), la)
			pipe(c, down)
		}(c)
	}
}

func (t *Tunnel) startSocks() error {
	la := parseAddr(t.Desc.Local)
	ln, err := net.Listen(la.network, la.address)
	if err != nil {
		return fmt.Errorf("listen %s: %v", la, err)
	}
	t.listener = ln
	t.log("[%s] SOCKS5 listening on %s", t.Desc.Name, la)
	go t.acceptSocks(ln)
	return nil
}

func (t *Tunnel) acceptSocks(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			target, err := socksHandshake(c)
			if err != nil {
				t.log("[%s] socks handshake: %v", t.Desc.Name, err)
				return
			}
			up, err := t.client.Dial("tcp", target)
			if err != nil {
				socksReply(c, 0x05) // connection refused
				t.log("[%s] socks dial %s: %v", t.Desc.Name, target, err)
				return
			}
			defer up.Close()
			socksReply(c, 0x00)
			t.log("[%s] socks %s ↔ %s", t.Desc.Name, c.RemoteAddr(), target)
			pipe(c, up)
		}(c)
	}
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// addr is a parsed network address: either tcp ("host:port") or unix
// ("/path/to/socket"). A bare port is shorthand for "localhost:port".
type addr struct {
	network string
	address string
}

func (a addr) String() string { return a.address }

func parseAddr(s string) addr {
	if s == "" {
		return addr{network: "tcp", address: "localhost:0"}
	}
	if _, err := strconv.Atoi(s); err == nil {
		return addr{network: "tcp", address: "localhost:" + s}
	}
	if strings.Contains(s, ":") {
		return addr{network: "tcp", address: s}
	}
	return addr{network: "unix", address: s}
}

// resolved is the final set of SSH connection parameters after merging the
// user's ssh_config defaults with the per-tunnel overrides.
type resolved struct {
	host     string
	port     int
	user     string
	identity string
	jumps    []resolvedJump
}

type resolvedJump struct {
	host string
	port int
	user string
}

func resolveHost(d Desc) resolved {
	return resolveHostLogged(d, nil)
}

func resolveHostLogged(d Desc, log logFn) resolved {
	if log == nil {
		log = func(string, ...any) {}
	}
	r := resolved{}
	r.host = ssh_config.Get(d.Host, "HostName")
	if r.host == "" {
		r.host = d.Host
	}
	r.user = d.User
	if r.user == "" {
		r.user = ssh_config.Get(d.Host, "User")
	}
	if r.user == "" {
		r.user = os.Getenv("USER")
	}
	r.port = d.Port
	if r.port == 0 {
		if p, err := strconv.Atoi(ssh_config.Get(d.Host, "Port")); err == nil {
			r.port = p
		}
	}
	if r.port == 0 {
		r.port = 22
	}
	r.identity = expandTilde(d.Identity)
	if r.identity != "" {
		log("resolve: identity from tunnel config: %s", r.identity)
	} else {
		// kevinburke/ssh_config returns only the FIRST IdentityFile entry
		// from ssh_config; OpenSSH would try every match. If your "right"
		// key sits behind a second IdentityFile line, it won't be picked
		// up here — which is the most common cause of "tool fails, CLI
		// works" once SHA-1 has been ruled out.
		if id, err := ssh_config.GetStrict(d.Host, "IdentityFile"); err == nil && id != "" {
			candidate := expandTilde(id)
			if _, e := os.Stat(candidate); e == nil {
				r.identity = candidate
				log("resolve: identity from ssh_config: %s", r.identity)
			} else {
				log("resolve: ssh_config IdentityFile %s missing on disk: %v", candidate, e)
			}
		} else if err != nil {
			log("resolve: ssh_config IdentityFile lookup error: %v", err)
		}
	}
	// Per-tunnel jump hosts override ssh_config ProxyJump.
	if d.JumpHosts != "" {
		for _, j := range strings.Split(d.JumpHosts, ",") {
			r.jumps = append(r.jumps, parseJump(strings.TrimSpace(j)))
		}
	} else if pj := ssh_config.Get(d.Host, "ProxyJump"); pj != "" {
		for _, j := range strings.Split(pj, ",") {
			r.jumps = append(r.jumps, parseJump(strings.TrimSpace(j)))
		}
	}
	return r
}

// parseJump turns a ProxyJump entry like "user@host:port" (any field
// optional) into a resolvedJump.
func parseJump(s string) resolvedJump {
	j := resolvedJump{port: 22}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		j.user = s[:at]
		s = s[at+1:]
	}
	if c := strings.LastIndex(s, ":"); c >= 0 {
		if p, err := strconv.Atoi(s[c+1:]); err == nil {
			j.port = p
			s = s[:c]
		}
	}
	j.host = s
	if j.user == "" {
		j.user = os.Getenv("USER")
	}
	return j
}

func dialChain(r resolved, prompts CredentialPrompts, keyCache *signerCache, hostKeyCB ssh.HostKeyCallback, log logFn) (*ssh.Client, error) {
	auth := buildAuth(r, prompts, keyCache, log)
	cfg := &ssh.ClientConfig{
		User:            r.user,
		Auth:            auth,
		HostKeyCallback: hostKeyCB,
		Timeout:         15 * time.Second,
		BannerCallback: func(msg string) error {
			log("banner: %s", strings.TrimRight(msg, "\r\n"))
			return nil
		},
	}

	var cli *ssh.Client
	for _, j := range r.jumps {
		jcfg := &ssh.ClientConfig{
			User:            j.user,
			Auth:            auth,
			HostKeyCallback: hostKeyCB,
			Timeout:         15 * time.Second,
		}
		jaddr := fmt.Sprintf("%s:%d", j.host, j.port)
		nc, err := dialThrough(cli, jaddr, jcfg)
		if err != nil {
			if cli != nil {
				cli.Close()
			}
			return nil, fmt.Errorf("jump %s: %v", jaddr, err)
		}
		cli = nc
	}

	target := fmt.Sprintf("%s:%d", r.host, r.port)
	final, err := dialThrough(cli, target, cfg)
	if err != nil {
		if cli != nil {
			cli.Close()
		}
		return nil, err
	}
	return final, nil
}

func dialThrough(via *ssh.Client, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	if via == nil {
		return ssh.Dial("tcp", addr, cfg)
	}
	c, err := via.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(c, addr, cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

// signerCache memoises decrypted private-key signers across reconnects so the
// user is asked for a passphrase at most once per key per session.
type signerCache struct {
	mu      sync.Mutex
	signers map[string]ssh.Signer
}

func newSignerCache() *signerCache {
	return &signerCache{signers: map[string]ssh.Signer{}}
}

func (c *signerCache) get(path string) (ssh.Signer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.signers[path]
	return s, ok
}

func (c *signerCache) put(path string, s ssh.Signer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signers[path] = s
}

func buildAuth(r resolved, prompts CredentialPrompts, keyCache *signerCache, log logFn) []ssh.AuthMethod {
	var methods []ssh.AuthMethod

	// Agent first. Wrap signers with preferRSASHA2 so RSA keys advertise
	// rsa-sha2-512/256 instead of legacy ssh-rsa (SHA-1) — modern OpenSSH
	// (8.7+) refuses SHA-1 by default. OpenSSH CLI sends SHA2 hint flags
	// to the agent; the Go agent client doesn't unless we force it via
	// MultiAlgorithmSigner.
	var ag agent.Agent
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag = agent.NewClient(conn)
			// One-shot inventory log so the user can see what the agent
			// actually offers — invaluable when "ssh works but tool fails".
			if list, lerr := ag.List(); lerr != nil {
				log("auth: ssh-agent list error: %v", lerr)
			} else if len(list) == 0 {
				log("auth: ssh-agent at %s has 0 keys", sock)
			} else {
				for i, k := range list {
					log("auth: ssh-agent[%d] %s %s (%s)", i, k.Type(), ssh.FingerprintSHA256(k), k.Comment)
				}
			}
		} else {
			log("auth: ssh-agent unreachable: %v", err)
		}
	} else {
		log("auth: SSH_AUTH_SOCK not set — no agent")
	}

	var fileSigners []ssh.Signer
	tryKey := func(p string) {
		if p == "" {
			return
		}
		if keyCache != nil {
			if s, ok := keyCache.get(p); ok {
				fileSigners = append(fileSigners, s)
				log("auth: cached identity %s", p)
				return
			}
		}
		signer, err := loadKeyInteractive(p, prompts, log)
		if err != nil {
			if !os.IsNotExist(err) {
				log("auth: skip %s: %v", p, err)
			}
			return
		}
		if signer == nil {
			return
		}
		signer = preferRSASHA2(signer)
		if keyCache != nil {
			keyCache.put(p, signer)
		}
		fileSigners = append(fileSigners, signer)
		log("auth: identity %s %s %s", p, signer.PublicKey().Type(), ssh.FingerprintSHA256(signer.PublicKey()))
	}

	if r.identity != "" {
		log("auth: configured identity %s", r.identity)
		tryKey(r.identity)
	} else if home, err := os.UserHomeDir(); err == nil {
		log("auth: trying default keys in %s/.ssh", home)
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa", "id_dsa"} {
			tryKey(filepath.Join(home, ".ssh", name))
		}
	} else {
		log("auth: no home dir: %v", err)
	}

	// Combine agent + file signers into ONE publickey AuthMethod. Splitting
	// them across two ssh.AuthMethod entries (both with method() == "publickey")
	// is a footgun: Go's auth loop adds "publickey" to its `tried` set after
	// the first method returns, so an empty-agent attempt marks publickey as
	// done and the file-based AuthMethod is never invoked. Symptom: ssh CLI
	// works but this tool fails with "no supported methods remain".
	if ag != nil || len(fileSigners) > 0 {
		methods = append(methods, ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			var sigs []ssh.Signer
			if ag != nil {
				ags, err := ag.Signers()
				if err != nil {
					log("auth: ssh-agent signers error: %v", err)
				} else {
					for _, s := range ags {
						sigs = append(sigs, preferRSASHA2(s))
					}
				}
			}
			sigs = append(sigs, fileSigners...)
			return sigs, nil
		}))
	}

	// Password fallback — invoked lazily by the SSH library only if the
	// server offers password auth and prior methods didn't satisfy it.
	if prompts != nil {
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			pass, ok := prompts.Password(r.user, r.host)
			if !ok {
				return "", fmt.Errorf("password prompt cancelled")
			}
			return pass, nil
		}))
	}

	return methods
}

// loadKeyInteractive reads and parses a private key, prompting the user for a
// passphrase if the key is encrypted. Returns (nil, nil) if the user cancels
// the passphrase prompt.
func loadKeyInteractive(path string, prompts CredentialPrompts, log logFn) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}
	var miss *ssh.PassphraseMissingError
	if !errors.As(err, &miss) {
		return nil, err
	}
	if prompts == nil {
		return nil, err
	}
	pass, ok := prompts.Passphrase(path)
	if !ok {
		log("auth: passphrase prompt cancelled for %s", path)
		return nil, nil
	}
	signer, perr := ssh.ParsePrivateKeyWithPassphrase(data, []byte(pass))
	if perr != nil {
		return nil, perr
	}
	return signer, nil
}

// preferRSASHA2 wraps an RSA signer so it advertises rsa-sha2-512 and
// rsa-sha2-256 first, with the legacy ssh-rsa (SHA-1) as last resort.
// Modern OpenSSH (8.7+) rejects ssh-rsa by default.
func preferRSASHA2(s ssh.Signer) ssh.Signer {
	if s.PublicKey().Type() != ssh.KeyAlgoRSA {
		return s
	}
	as, ok := s.(ssh.AlgorithmSigner)
	if !ok {
		return s
	}
	ms, err := ssh.NewSignerWithAlgorithms(as, []string{
		ssh.KeyAlgoRSASHA512,
		ssh.KeyAlgoRSASHA256,
		ssh.KeyAlgoRSA,
	})
	if err != nil {
		return s
	}
	return ms
}

func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// socksHandshake performs a minimal SOCKS5 negotiation supporting the
// NO_AUTH method and the CONNECT command for IPv4, IPv6, and domain
// destinations. Returns the target "host:port" string.
func socksHandshake(c net.Conn) (string, error) {
	buf := make([]byte, 257)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", err
	}
	if buf[0] != 5 {
		return "", fmt.Errorf("not SOCKS5")
	}
	n := int(buf[1])
	if _, err := io.ReadFull(c, buf[:n]); err != nil {
		return "", err
	}
	// Reply: VER, METHOD = NO_AUTH (0).
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	// Request: VER CMD RSV ATYP ...
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return "", err
	}
	if buf[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", fmt.Errorf("unsupported command %d", buf[1])
	}
	var host string
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return "", err
		}
		host = net.IP(buf[:4]).String()
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return "", err
		}
		l := int(buf[0])
		if _, err := io.ReadFull(c, buf[:l]); err != nil {
			return "", err
		}
		host = string(buf[:l])
	case 4:
		if _, err := io.ReadFull(c, buf[:16]); err != nil {
			return "", err
		}
		host = "[" + net.IP(buf[:16]).String() + "]"
	default:
		c.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", fmt.Errorf("bad atyp %d", buf[3])
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", err
	}
	port := uint16(buf[0])<<8 | uint16(buf[1])
	return fmt.Sprintf("%s:%d", host, port), nil
}

func socksReply(c net.Conn, status byte) {
	c.Write([]byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
}
