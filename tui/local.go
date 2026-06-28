package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// isLocalHost reports whether host refers to the machine the TUI runs on. This
// is true for the loopback names and for any IP bound to a local interface
// (notably the primary/default-route IP we connect a locally-installed Olla on),
// so agent deploy/launch can run directly instead of SSHing to ourselves.
func isLocalHost(host string) bool {
	host = strings.TrimSpace(host)
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	if host == primaryIP() {
		return true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ipnet.IP.String() == host {
					return true
				}
			}
		}
	}
	return false
}

// expandHome turns a leading ~ into the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// localWriteScript writes an executable script to path (~ expanded) and returns
// the absolute path it wrote, for running in a login shell.
func localWriteScript(path, content string) (string, error) {
	abs := expandHome(path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(content), 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

// localMergeCrushConfig deep-merges the TUI-managed crush.json fragment into the
// local ~/.config/crush/crush.json (preserving user-added sections such as mcp
// servers), mirroring sshMergeCrushConfig but without SSH.
func localMergeCrushConfig(config string) error {
	var add map[string]any
	if err := json.Unmarshal([]byte(config), &add); err != nil {
		return err
	}
	path := expandHome("~/.config/crush/crush.json")
	cur := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cur) // tolerate a malformed/empty existing file
	}
	deepMerge(cur, add)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// deepMerge recursively merges b into a; nested objects are merged, while lists
// and scalars are replaced wholesale (so the olla model list updates cleanly).
func deepMerge(a, b map[string]any) {
	for k, v := range b {
		if vm, ok := v.(map[string]any); ok {
			if am, ok := a[k].(map[string]any); ok {
				deepMerge(am, vm)
				continue
			}
		}
		a[k] = v
	}
}

// localCrushCmd prepares Crush on the local host: it merges the olla provider
// config locally and, for a deploy (script != ""), stages the install script;
// then returns a consoleReadyMsg that runs locally.
func localCrushCmd(config, script, label, agent string) tea.Cmd {
	return func() tea.Msg {
		if err := localMergeCrushConfig(config); err != nil {
			return notifyMsg("crush config write failed: " + err.Error())
		}
		cmd := "crush"
		if script != "" {
			abs, err := localWriteScript("~/.oilsand-deploy.sh", script)
			if err != nil {
				return notifyMsg(label + " prep failed: " + err.Error())
			}
			cmd = "bash -l " + abs
		}
		return consoleReadyMsg{local: true, host: primaryIP(), cmd: cmd, label: label, agent: agent}
	}
}

// localDeployAgentCmd stages an agent's bootstrap script locally and returns a
// consoleReadyMsg that runs it in a login shell on this machine.
func localDeployAgentCmd(script, agent, label string) tea.Cmd {
	return func() tea.Msg {
		abs, err := localWriteScript("~/.oilsand-deploy.sh", script)
		if err != nil {
			return notifyMsg(label + " prep failed: " + err.Error())
		}
		return consoleReadyMsg{local: true, host: primaryIP(), cmd: "bash -l " + abs, label: label, agent: agent}
	}
}

// localLaunchCmd suspends the TUI and runs cmd in a local login shell.
func localLaunchCmd(cmd, label string) tea.Cmd {
	c := localLoginCmd(cmd)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return notifyMsg(label + " ended: " + err.Error())
		}
		return notifyMsg(label + " session ended")
	})
}

// localLaunchRegisterCmd is like localLaunchCmd but registers the agent on a
// clean exit so the Agents menu marks it deployed.
func localLaunchRegisterCmd(cmd, label, agent, host string) tea.Cmd {
	c := localLoginCmd(cmd)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return notifyMsg(label + " ended: " + err.Error())
		}
		return agentRegisteredMsg{agent: agent, host: host}
	})
}
