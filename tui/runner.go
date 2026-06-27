package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// pythonExe picks an interpreter, in order of preference:
//  1. $OILSAND_PYTHON (explicit override)
//  2. the virtualenv the installer creates next to the binary (so the bundled
//     requests/paramiko deps are always available without touching system Python)
//  3. python/python3 (py on Windows) on PATH
func pythonExe() string {
	if p := os.Getenv("OILSAND_PYTHON"); p != "" {
		return p
	}
	if p := bundledVenvPython(); p != "" {
		return p
	}
	candidates := []string{"python", "python3"}
	if runtime.GOOS == "windows" {
		candidates = []string{"python", "py", "python3"}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return "python"
}

// bundledVenvPython returns the path to the interpreter in a "venv" directory
// alongside the binary (created by the installer), or "" if there isn't one.
func bundledVenvPython() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	rel := []string{"venv", "bin", "python"}
	if runtime.GOOS == "windows" {
		rel = []string{"venv", "Scripts", "python.exe"}
	}
	p := filepath.Join(append([]string{dir}, rel...)...)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// vmScriptPath locates scripts/nutanix_olla_vm.py relative to this binary's repo.
func vmScriptPath() string {
	if p := os.Getenv("OILSAND_VM_SCRIPT"); p != "" {
		return p
	}
	// When run via `go run` or the built binary inside tui/, the repo root is one up.
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, up := range []string{
			filepath.Join(dir, "..", "scripts", "nutanix_olla_vm.py"),
			filepath.Join(dir, "scripts", "nutanix_olla_vm.py"),
		} {
			if _, err := os.Stat(up); err == nil {
				abs, _ := filepath.Abs(up)
				return abs
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		for _, up := range []string{
			filepath.Join(wd, "..", "scripts", "nutanix_olla_vm.py"),
			filepath.Join(wd, "scripts", "nutanix_olla_vm.py"),
		} {
			if _, err := os.Stat(up); err == nil {
				abs, _ := filepath.Abs(up)
				return abs
			}
		}
	}
	return filepath.Join("..", "scripts", "nutanix_olla_vm.py")
}

// ProcEvent is a line of subprocess output or a terminal exit signal.
type ProcEvent struct {
	Line string
	Done bool
	Code int
}

// RunVMScript runs the python helper with the PC credentials injected via env and
// streams stdout/stderr lines to ch. A final Done event carries the exit code.
func RunVMScript(cfg *PCConfig, args []string, ch chan<- ProcEvent) {
	defer close(ch)
	script := vmScriptPath()
	full := append([]string{script}, args...)
	if cfg != nil {
		full = append(full, "--prism-url", fmt.Sprintf("https://%s:%s", cfg.Host, cfg.Port))
	}
	cmd := exec.Command(pythonExe(), full...)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	if cfg != nil {
		if cfg.APIKey != "" {
			cmd.Env = append(cmd.Env, "PRISM_API_KEY="+cfg.APIKey)
		}
		if cfg.User != "" {
			cmd.Env = append(cmd.Env, "PRISM_USER="+cfg.User, "PRISM_PASSWORD="+cfg.Password)
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ch <- ProcEvent{Line: "failed to pipe stdout: " + err.Error(), Done: true, Code: 1}
		return
	}
	cmd.Stderr = cmd.Stdout // merge; reuse same pipe writer
	if err := cmd.Start(); err != nil {
		ch <- ProcEvent{Line: "failed to start: " + err.Error(), Done: true, Code: 1}
		return
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ch <- ProcEvent{Line: strings.TrimRight(sc.Text(), "\r\n")}
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	ch <- ProcEvent{Done: true, Code: code}
}

// NextName calls the helper synchronously to get the next free indexed name.
func NextName(cfg *PCConfig, role string) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("prism central not configured")
	}
	script := vmScriptPath()
	cmd := exec.Command(pythonExe(), script, "next-name", "--role", role,
		"--prism-url", fmt.Sprintf("https://%s:%s", cfg.Host, cfg.Port))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	if cfg.APIKey != "" {
		cmd.Env = append(cmd.Env, "PRISM_API_KEY="+cfg.APIKey)
	}
	if cfg.User != "" {
		cmd.Env = append(cmd.Env, "PRISM_USER="+cfg.User, "PRISM_PASSWORD="+cfg.Password)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// Output may include log lines; take the last JSON object line.
	var name string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(line), &obj) == nil && obj.Name != "" {
			name = obj.Name
		}
	}
	if name == "" {
		return "", fmt.Errorf("could not parse next-name output")
	}
	return name, nil
}
