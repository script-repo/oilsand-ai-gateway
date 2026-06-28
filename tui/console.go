package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// directHTTP talks straight to a worker's Ollama API (bypassing the gateway), so
// pull/delete/warm can be fanned out to every worker individually. Streaming
// pulls need no client-side timeout; per-request contexts bound the rest.
var directHTTP = &http.Client{Timeout: 0}

// workerRef is one Ollama backend addressed directly (not via the gateway).
type workerRef struct {
	name string
	url  string // e.g. http://worker-host:11434
	host string // e.g. worker-host
}

// workersFromEndpoints keeps only the Ollama-type endpoints and resolves hosts.
func workersFromEndpoints(eps []endpointEntry) []workerRef {
	var out []workerRef
	for _, e := range eps {
		if e.Type != "" && e.Type != "ollama" {
			continue
		}
		out = append(out, workerRef{
			name: e.Name,
			url:  strings.TrimRight(e.URL, "/"),
			host: hostFromURL(e.URL),
		})
	}
	return out
}

// modelMatches reports whether an installed tag refers to the same model,
// tolerating an implicit :latest tag on either side.
func modelMatches(installed, want string) bool {
	norm := func(s string) string {
		if !strings.Contains(s, ":") {
			return s + ":latest"
		}
		return s
	}
	return norm(installed) == norm(want)
}

// ---- direct Ollama operations ----------------------------------------------

func ollamaTags(base string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	resp, err := directHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// ollamaModelList returns the full model entries (with details) installed on a
// single worker.
func ollamaModelList(base string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	resp, err := directHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []Model `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func ollamaHasModel(base, model string) bool {
	names, err := ollamaTags(base)
	if err != nil {
		return false
	}
	for _, n := range names {
		if modelMatches(n, model) {
			return true
		}
	}
	return false
}

func ollamaDelete(base, model string) error {
	raw, _ := json.Marshal(map[string]any{"name": model})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/delete", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ollamaWarm loads a model into memory (and pins it with a long keep-alive) so
// the first real request doesn't pay the cold-load penalty.
func ollamaWarm(base, model string) error {
	raw, _ := json.Marshal(map[string]any{"model": model, "prompt": "", "keep_alive": "24h"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("warm %d", resp.StatusCode)
	}
	return nil
}

// ollamaPullInto streams `ollama pull` progress for one worker into ch, tagging
// each event with the worker name so a parallel multi-pull can track per-worker
// progress. It does not close ch (the caller owns the channel).
func ollamaPullInto(base, model, worker string, ch chan<- PullEvent) error {
	raw, _ := json.Marshal(map[string]any{"name": model, "stream": true})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		if obj.Error != "" {
			return fmt.Errorf("%s", obj.Error)
		}
		ch <- PullEvent{Worker: worker, Status: model + " " + obj.Status, Completed: obj.Completed, Total: obj.Total}
	}
	return sc.Err()
}

// localLoginCmd builds a child process that runs cmd in an interactive login
// shell on this machine (so the user's full PATH and agent TUIs work), used for
// deploying/launching agents on a locally-installed Olla without SSH.
func localLoginCmd(cmd string) *exec.Cmd {
	return exec.Command("bash", "-lc", cmd)
}

// ---- SSH console (Bubble Tea ExecProcess) ----------------------------------

// sshConsoleCmd suspends the TUI and drops into an interactive SSH session using
// Bubble Tea's ExecProcess (the Charm-sanctioned way to run a full-screen child
// process). If keyPath is set we offer it for password-less login; otherwise ssh
// falls back to prompting for a password in the handed-over terminal.
func sshConsoleCmd(user, host, keyPath string) tea.Cmd {
	if host == "" {
		return func() tea.Msg { return notifyMsg("no host to console into") }
	}
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + osNull(),
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, fmt.Sprintf("%s@%s", orDefault(user, "rocky"), host))
	c := exec.Command("ssh", args...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return notifyMsg("ssh session ended: " + err.Error())
		}
		return notifyMsg("ssh session to " + host + " ended")
	})
}

// sshLaunchCmd suspends the TUI and SSHes into host to run a single interactive
// remote command (e.g. `crush`, `ollama launch hermes`). It forces a PTY (-t) so
// the remote program's own TUI renders correctly, then returns control to us.
func sshLaunchCmd(user, host, keyPath, remoteCmd, label string) tea.Cmd {
	if host == "" {
		return func() tea.Msg { return notifyMsg("no host to launch " + label) }
	}
	args := []string{
		"-t",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + osNull(),
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, fmt.Sprintf("%s@%s", orDefault(user, "rocky"), host), remoteCmd)
	c := exec.Command("ssh", args...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return notifyMsg(label + " ended: " + err.Error())
		}
		return notifyMsg(label + " session ended")
	})
}

// sshLaunchRegisterCmd is like sshLaunchCmd but on a clean exit (rc 0) it emits
// an agentRegisteredMsg so the Agents menu can mark the agent as deployed.
func sshLaunchRegisterCmd(user, host, keyPath, remoteCmd, label, agent string) tea.Cmd {
	if host == "" {
		return func() tea.Msg { return notifyMsg("no host to launch " + label) }
	}
	args := []string{
		"-t",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + osNull(),
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, fmt.Sprintf("%s@%s", orDefault(user, "rocky"), host), remoteCmd)
	c := exec.Command("ssh", args...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return notifyMsg(label + " ended: " + err.Error())
		}
		return agentRegisteredMsg{agent: agent, host: host}
	})
}
