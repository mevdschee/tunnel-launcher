//go:build !windows

package main

import (
	"net"
	"os"
	"time"
)

// agentAddress is the unix socket the agent listens on. There is no
// well-known default: without SSH_AUTH_SOCK there is no agent.
func agentAddress() string { return os.Getenv("SSH_AUTH_SOCK") }

func dialAgentConn(addr string) (net.Conn, error) {
	return net.DialTimeout("unix", addr, agentDialTimeout)
}

const agentDialTimeout = 5 * time.Second
