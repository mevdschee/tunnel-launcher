// ssh-agent integration.
//
// The agent lets the user unlock a passphrase-protected key once (via
// `ssh-add`) and have every later connection use it without being asked
// again. Reaching the agent is platform-specific: unix-likes publish a
// socket path in SSH_AUTH_SOCK, Windows runs a service on a named pipe and
// sets no environment variable at all. See sshagent_unix.go /
// sshagent_windows.go for the dialing halves.
package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// The agent is a per-user singleton, so one connection is shared by every
// tunnel and reused across reconnects. Redialed when the cached connection
// has gone stale (agent restarted, service bounced, pipe closed).
var (
	agentMu     sync.Mutex
	agentConn   io.Closer
	agentClient agent.Agent
	agentAddr   string
)

// sshAgent returns a client for the running ssh-agent along with the keys it
// currently holds. Returns (nil, nil) when there is no reachable agent; that
// is a normal, non-fatal condition — auth then falls back to key files.
func sshAgent(log logFn) (agent.Agent, []*agent.Key) {
	agentMu.Lock()
	defer agentMu.Unlock()

	addr := agentAddress()
	if addr == "" {
		log.infof("auth: no ssh-agent configured (SSH_AUTH_SOCK not set)")
		closeAgentLocked()
		return nil, nil
	}

	if agentClient != nil && agentAddr == addr {
		if keys, err := agentClient.List(); err == nil {
			return agentClient, keys
		}
		log.infof("auth: ssh-agent connection went stale, redialing %s", addr)
	}
	closeAgentLocked()

	conn, err := dialAgentConn(addr)
	if err != nil {
		log.warnf("auth: ssh-agent at %s unreachable: %v", addr, err)
		return nil, nil
	}
	cli := agent.NewClient(conn)
	keys, err := cli.List()
	if err != nil {
		log.warnf("auth: ssh-agent at %s list error: %v", addr, err)
		conn.Close()
		return nil, nil
	}
	agentConn, agentClient, agentAddr = conn, cli, addr
	log.infof("auth: ssh-agent at %s holds %d key(s)", addr, len(keys))
	return cli, keys
}

func closeAgentLocked() {
	if agentConn != nil {
		agentConn.Close()
	}
	agentConn, agentClient, agentAddr = nil, nil, ""
}

// agentFingerprints indexes the keys an agent holds by SHA256 fingerprint,
// so a key file can be recognised as "the agent already has this one". For
// certificates both the certificate and the key it certifies are indexed:
// the file on disk is the plain key, while the agent may only advertise the
// signed certificate.
func agentFingerprints(keys []*agent.Key) map[string]bool {
	fps := map[string]bool{}
	for _, k := range keys {
		fps[ssh.FingerprintSHA256(k)] = true
		if pub, err := ssh.ParsePublicKey(k.Blob); err == nil {
			if cert, ok := pub.(*ssh.Certificate); ok && cert.Key != nil {
				fps[ssh.FingerprintSHA256(cert.Key)] = true
			}
		}
	}
	return fps
}

// publicKeyOf reads the public half of a private key file without needing
// the passphrase. OpenSSH-format keys carry the public key in cleartext even
// when encrypted; for the older PEM formats, which don't, fall back to the
// sibling ".pub" file.
func publicKeyOf(path string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer.PublicKey(), nil
	}
	var miss *ssh.PassphraseMissingError
	if errors.As(err, &miss) && miss.PublicKey != nil {
		return miss.PublicKey, nil
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		return nil, err
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// normalizePipePath recognises a Windows named-pipe path in either the
// native `\\.\pipe\name` spelling or the forward-slash `//./pipe/name` one
// that shells and config files tend to carry, and returns it in the native
// form. Lives here rather than in sshagent_windows.go so it stays testable
// on every platform.
func normalizePipePath(p string) (string, bool) {
	q := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(q, `\\.\pipe\`) || strings.HasPrefix(q, `\\?\pipe\`) {
		return q, true
	}
	return p, false
}
