package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"
)

// lockdownKey makes the private key readable only by the current user. On
// Windows, OpenSSH refuses ("bad permissions") and silently falls back to a
// password unless the file's ACLs are locked down — POSIX 0600 isn't enough.
func lockdownKey(path string) {
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
		return
	}
	user := os.Getenv("USERNAME")
	if d := os.Getenv("USERDOMAIN"); d != "" && user != "" {
		user = d + "\\" + user
	}
	// Disable inheritance and grant only the current user full control.
	_ = exec.Command("icacls", path, "/inheritance:r").Run()
	if user != "" {
		_ = exec.Command("icacls", path, "/grant:r", user+":F").Run()
	}
}

// keyDir is where the TUI keeps the SSH keypair it installs on workers/gateways.
func keyDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".oilsand-ai-gateway", "keys")
}

// privateKeyPath returns the path to the managed private key (may not exist yet).
func privateKeyPath() string {
	return filepath.Join(keyDir(), "id_ed25519")
}

// managedKeyPath returns the private key path if it exists, else "".
func managedKeyPath() string {
	p := privateKeyPath()
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ensureLocalKey creates the ed25519 keypair on first use and returns the
// private key path and the authorized_keys line for the public key.
func ensureLocalKey() (privPath, authLine string, err error) {
	priv := privateKeyPath()
	pubFile := priv + ".pub"
	if b, e := os.ReadFile(pubFile); e == nil {
		if _, e2 := os.Stat(priv); e2 == nil {
			lockdownKey(priv) // ensure perms even for a pre-existing key
			return priv, strings.TrimSpace(string(b)), nil
		}
	}
	if err = os.MkdirAll(keyDir(), 0o700); err != nil {
		return "", "", err
	}
	pub, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, "oilsand-tui")
	if err != nil {
		return "", "", err
	}
	if err = os.WriteFile(priv, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	lockdownKey(priv)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	authLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err = os.WriteFile(pubFile, []byte(authLine+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return priv, authLine, nil
}

// EnsureKeyAuth makes sure the managed public key is installed in the remote
// host's authorized_keys (idempotent), using password auth to bootstrap. After
// this, sshConsoleCmd with the managed private key logs in without a password.
func EnsureKeyAuth(host, user, password string) (privPath string, err error) {
	priv, authLine, err := ensureLocalKey()
	if err != nil {
		return "", err
	}
	if host == "" {
		return priv, nil
	}
	client, err := dialSSH(host, user, password)
	if err != nil {
		return priv, fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()

	// Append the key only if it isn't already present (idempotent).
	cmd := fmt.Sprintf(
		"install -d -m 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && "+
			"grep -qxF '%s' ~/.ssh/authorized_keys || echo '%s' >> ~/.ssh/authorized_keys",
		authLine, authLine)
	if out, e := runSSH(client, cmd); e != nil {
		return priv, fmt.Errorf("install key: %v (%s)", e, strings.TrimSpace(out))
	}
	return priv, nil
}
