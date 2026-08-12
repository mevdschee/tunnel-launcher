package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// startAgent runs an in-process ssh-agent on a unix socket holding the given
// keys, points SSH_AUTH_SOCK at it, and returns the socket path.
func startAgent(t *testing.T, keys ...any) string {
	t.Helper()
	keyring := agent.NewKeyring()
	for _, k := range keys {
		if err := keyring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, c)
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	return sock
}

// writeKey writes an OpenSSH-format private key file, encrypted when a
// passphrase is given, and returns the private key it wrote.
func writeKey(t *testing.T, path, passphrase string) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return priv
}

// This is the issue: the user ran ssh-add once, the agent holds the unlocked
// key, the ssh CLI no longer asks for anything — but tunnel-launcher read the
// key file eagerly and popped a passphrase dialog anyway.
func TestBuildAuth_KeyHeldByAgentIsNotUnlockedFromDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	keyPath := filepath.Join(dir, ".ssh", "id_ed25519")
	priv := writeKey(t, keyPath, "secret")
	startAgent(t, priv)

	prompts := &fakePrompts{passphrase: "secret", passphraseOK: true}
	got := buildAuth(resolved{identity: keyPath}, prompts, nil, silentLog)

	if prompts.passphraseAsked {
		t.Error("passphrase prompt shown for a key the agent already holds")
	}
	if len(got) != 2 {
		t.Errorf("methods = %d, want 2 (publickey via agent + password fallback)", len(got))
	}
}

// Same, for the default-identity scan: with no identity configured the
// ~/.ssh/id_* files are walked, and those must not be unlocked either.
func TestBuildAuth_DefaultKeyHeldByAgentIsNotUnlockedFromDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	priv := writeKey(t, filepath.Join(dir, ".ssh", "id_ed25519"), "secret")
	startAgent(t, priv)

	prompts := &fakePrompts{passphrase: "secret", passphraseOK: true}
	buildAuth(resolved{}, prompts, nil, silentLog)

	if prompts.passphraseAsked {
		t.Error("passphrase prompt shown for a default key the agent already holds")
	}
}

// The skip must be driven by the agent actually holding *this* key, not by
// an agent merely being reachable.
func TestBuildAuth_KeyNotHeldByAgentStillPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	keyPath := filepath.Join(dir, ".ssh", "id_ed25519")
	writeKey(t, keyPath, "secret")

	// Agent is up but holds an unrelated key.
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, other)

	prompts := &fakePrompts{passphrase: "secret", passphraseOK: true}
	buildAuth(resolved{identity: keyPath}, prompts, nil, silentLog)

	if !prompts.passphraseAsked {
		t.Error("passphrase prompt skipped although the agent does not hold this key")
	}
}

// End-to-end: encrypted key on disk, unlocked copy in the agent, server
// accepts only publickey. Authentication must succeed with no prompt at all.
func TestBuildAuth_AgentAuthenticatesWithoutPassphrase_LiveHandshake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	keyPath := filepath.Join(dir, ".ssh", "id_ed25519")
	priv := writeKey(t, keyPath, "secret")
	startAgent(t, priv)

	userSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	authorizedFP := ssh.FingerprintSHA256(userSigner.PublicKey())

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if ssh.FingerprintSHA256(key) == authorizedFP {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key %s", ssh.FingerprintSHA256(key))
		},
	}
	srvCfg.AddHostKey(hostSigner)

	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srvLn.Close()
	go func() {
		nc, err := srvLn.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		_, chans, reqs, err := ssh.NewServerConn(nc, srvCfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			newCh.Reject(ssh.UnknownChannelType, "no channels in test")
		}
	}()

	prompts := &fakePrompts{passphraseOK: false}
	methods := buildAuth(resolved{identity: keyPath}, prompts, nil, silentLog)

	cli, err := ssh.Dial("tcp", srvLn.Addr().String(), &ssh.ClientConfig{
		User:            "tester",
		Auth:            methods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	cli.Close()

	if prompts.passphraseAsked {
		t.Error("passphrase prompt shown although the agent could sign")
	}
}

// A stale cached connection (agent restarted) must be redialed rather than
// reported as "no agent".
func TestSSHAgent_RedialsAfterConnectionDrops(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, priv)

	if _, keys := sshAgent(silentLog); len(keys) != 1 {
		t.Fatalf("first call: %d keys, want 1", len(keys))
	}

	agentMu.Lock()
	if agentConn != nil {
		agentConn.Close() // simulate the agent going away
	}
	agentMu.Unlock()

	if _, keys := sshAgent(silentLog); len(keys) != 1 {
		t.Fatalf("after drop: %d keys, want 1 (connection should be redialed)", len(keys))
	}
}

func TestSSHAgent_NoAgentConfigured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows always has a default agent pipe address")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	ag, keys := sshAgent(silentLog)
	if ag != nil || keys != nil {
		t.Errorf("sshAgent() = (%v, %v), want (nil, nil)", ag, keys)
	}
}

// publicKeyOf --------------------------------------------------------------

func TestPublicKeyOf_EncryptedKeyWithoutPassphrase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	priv := writeKey(t, p, "secret")

	pub, err := publicKeyOf(p)
	if err != nil {
		t.Fatalf("publicKeyOf: %v", err)
	}
	want, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(pub) != ssh.FingerprintSHA256(want) {
		t.Errorf("fingerprint = %s, want %s", ssh.FingerprintSHA256(pub), ssh.FingerprintSHA256(want))
	}
}

func TestPublicKeyOf_UnencryptedKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	priv := writeKey(t, p, "")

	pub, err := publicKeyOf(p)
	if err != nil {
		t.Fatalf("publicKeyOf: %v", err)
	}
	want, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(pub) != ssh.FingerprintSHA256(want) {
		t.Error("fingerprint mismatch")
	}
}

// Older PEM formats keep no cleartext public key, so the sibling .pub file
// is the only way to identify the key without the passphrase.
func TestPublicKeyOf_FallsBackToPubFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	priv := writeKey(t, p, "secret")
	pubKey, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	// Make the private key unparsable so only the .pub file can answer.
	if err := os.WriteFile(p, []byte("not a key at all\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".pub", ssh.MarshalAuthorizedKey(pubKey), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := publicKeyOf(p)
	if err != nil {
		t.Fatalf("publicKeyOf: %v", err)
	}
	if ssh.FingerprintSHA256(got) != ssh.FingerprintSHA256(pubKey) {
		t.Error("fingerprint mismatch")
	}
}

func TestPublicKeyOf_MissingFile(t *testing.T) {
	if _, err := publicKeyOf(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// agentFingerprints --------------------------------------------------------

// An agent may advertise only a certificate while the file on disk is the
// plain key it certifies; both must count as "held by the agent".
func TestAgentFingerprints_IndexesCertifiedKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{
		Key:         signer.PublicKey(),
		CertType:    ssh.UserCert,
		ValidBefore: ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}

	fps := agentFingerprints([]*agent.Key{{
		Format: cert.Type(),
		Blob:   cert.Marshal(),
	}})

	if !fps[ssh.FingerprintSHA256(cert)] {
		t.Error("certificate fingerprint not indexed")
	}
	if !fps[ssh.FingerprintSHA256(signer.PublicKey())] {
		t.Error("certified key fingerprint not indexed")
	}
}

// normalizePipePath --------------------------------------------------------

func TestNormalizePipePath(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		isPipe bool
	}{
		{`\\.\pipe\openssh-ssh-agent`, `\\.\pipe\openssh-ssh-agent`, true},
		{`//./pipe/openssh-ssh-agent`, `\\.\pipe\openssh-ssh-agent`, true},
		{`\\?\pipe\some-agent`, `\\?\pipe\some-agent`, true},
		{`C:\Users\me\agent.sock`, `C:\Users\me\agent.sock`, false},
		{"/tmp/ssh-XXXX/agent.1234", "/tmp/ssh-XXXX/agent.1234", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, isPipe := normalizePipePath(c.in)
		if got != c.want || isPipe != c.isPipe {
			t.Errorf("normalizePipePath(%q) = (%q, %v), want (%q, %v)", c.in, got, isPipe, c.want, c.isPipe)
		}
	}
}
