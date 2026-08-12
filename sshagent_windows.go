//go:build windows

package main

import (
	"net"
	"os"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// defaultAgentPipe is where the Windows OpenSSH ssh-agent service listens.
// Windows sets no SSH_AUTH_SOCK: ssh.exe has this path built in. A tool that
// only consults the environment therefore concludes "no agent" and prompts
// for a passphrase, while the command line client happily uses the agent.
const defaultAgentPipe = `\\.\pipe\openssh-ssh-agent`

func agentAddress() string {
	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" {
		return s
	}
	return defaultAgentPipe
}

// dialAgentConn connects to a named pipe, or to an AF_UNIX socket when
// SSH_AUTH_SOCK points at one — Windows 10 1803+ supports those and some
// third-party agents (and WSL relays) use them.
func dialAgentConn(addr string) (net.Conn, error) {
	if pipe, ok := normalizePipePath(addr); ok {
		timeout := agentDialTimeout
		return winio.DialPipe(pipe, &timeout)
	}
	return net.DialTimeout("unix", addr, agentDialTimeout)
}

const agentDialTimeout = 5 * time.Second
