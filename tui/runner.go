package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// LocalOllaPort is the port the locally-installed Olla gateway listens on (must
// match scripts/remote/install-olla.sh's OLLA_PORT default).
const LocalOllaPort = "40114"

// primaryIP returns the host's primary (default-route) IPv4 address — the
// external IP of the NIC used to reach the network — falling back to 127.0.0.1.
// It opens no traffic; the UDP "dial" just makes the kernel pick a source IP.
func primaryIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if ip := addr.IP.To4(); ip != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	// Fallback: first non-loopback IPv4 from the interface list.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip := ipnet.IP.To4(); ip != nil {
					return ip.String()
				}
			}
		}
	}
	return "127.0.0.1"
}

// LocalOllaURL is the gateway URL to connect to after a local Olla install. It
// prefers the host's external IP so the gateway is reachable from other hosts,
// not just loopback.
func LocalOllaURL() string {
	return "http://" + primaryIP() + ":" + LocalOllaPort
}

// ProcEvent is a line of subprocess output or a terminal exit signal.
type ProcEvent struct {
	Line string
	Done bool
	Code int
}

// streamProc starts cmd (with stdout+stderr merged), streams each output line to
// ch, and sends a final Done event with the exit code. It does not close ch.
func streamProc(cmd *exec.Cmd, ch chan<- ProcEvent) {
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

// localOllaSupported reports whether installing Olla on this machine is possible
// (the installer is a bash/systemd script, so Linux only).
func localOllaSupported() bool { return runtime.GOOS == "linux" }

// openBrowser opens url in the OS default browser. When the TUI runs over SSH
// this targets the remote host's browser; the OSC 8 hyperlink (handled by the
// local terminal) is the better path in that case.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// localOllaScriptPath locates scripts/remote/install-olla.sh relative to this
// binary (the installer copies the scripts/ tree next to the binary) or the repo.
func localOllaScriptPath() string {
	if p := os.Getenv("OILSAND_OLLA_SCRIPT"); p != "" {
		return p
	}
	rel := filepath.Join("scripts", "remote", "install-olla.sh")
	var roots []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		roots = append(roots, dir, filepath.Join(dir, ".."))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd, filepath.Join(wd, ".."))
	}
	for _, root := range roots {
		p := filepath.Join(root, rel)
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return rel
}

// RunLocalOllaInstall installs Olla on the machine running the TUI by executing
// scripts/remote/install-olla.sh, streaming its output to ch. The script needs
// root, so it runs under `sudo -n` unless we are already root; if passwordless
// sudo isn't configured, sudo exits immediately with a clear message (rather
// than hanging on a password prompt the TUI can't service).
func RunLocalOllaInstall(ch chan<- ProcEvent) {
	defer close(ch)
	if !localOllaSupported() {
		ch <- ProcEvent{Line: "local Olla install is only supported on Linux — run the TUI on the target server (e.g. over SSH)", Done: true, Code: 1}
		return
	}
	script := localOllaScriptPath()
	if _, err := os.Stat(script); err != nil {
		ch <- ProcEvent{Line: "could not find install-olla.sh (" + err.Error() + "); set OILSAND_OLLA_SCRIPT", Done: true, Code: 1}
		return
	}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("bash", script)
	} else {
		cmd = exec.Command("sudo", "-n", "bash", script)
	}
	cmd.Env = append(os.Environ(), "OLLA_PORT="+LocalOllaPort)
	streamProc(cmd, ch)
}

// buildVMCmd constructs the python helper command for args with the PC
// credentials injected via env. Shared by the single and parallel runners.
func buildVMCmd(cfg *PCConfig, args []string) *exec.Cmd {
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
	return cmd
}

// RunVMScript runs the python helper with the PC credentials injected via env and
// streams stdout/stderr lines to ch. A final Done event carries the exit code.
func RunVMScript(cfg *PCConfig, args []string, ch chan<- ProcEvent) {
	defer close(ch)
	streamProc(buildVMCmd(cfg, args), ch)
}

// vmJob is a single python helper invocation for the parallel runner.
type vmJob struct {
	tag  string
	args []string
}

// tagLine prefixes a log line with "[tag] " so interleaved parallel output stays
// attributable to a specific worker.
func tagLine(tag, line string) string {
	if tag == "" {
		return line
	}
	return "[" + tag + "] " + line
}

// streamProcLines streams cmd output to ch (each line prefixed with tag) and
// returns the exit code. Unlike streamProc it does NOT emit a Done event, so the
// caller can aggregate multiple concurrent jobs into one Done.
func streamProcLines(cmd *exec.Cmd, tag string, ch chan<- ProcEvent) int {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ch <- ProcEvent{Line: tagLine(tag, "failed to pipe stdout: "+err.Error())}
		return 1
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		ch <- ProcEvent{Line: tagLine(tag, "failed to start: "+err.Error())}
		return 1
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ch <- ProcEvent{Line: tagLine(tag, strings.TrimRight(sc.Text(), "\r\n"))}
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return code
}

// RunVMScripts runs several python helper invocations concurrently, tagging each
// output line with its job tag, and emits a single aggregate Done after all jobs
// finish. The Done code is non-zero if any job failed. ch is closed on return.
func RunVMScripts(cfg *PCConfig, jobs []vmJob, ch chan<- ProcEvent) {
	defer close(ch)
	var wg sync.WaitGroup
	var mu sync.Mutex
	worst := 0
	for _, j := range jobs {
		wg.Add(1)
		go func(j vmJob) {
			defer wg.Done()
			code := streamProcLines(buildVMCmd(cfg, j.args), j.tag, ch)
			if code != 0 {
				mu.Lock()
				worst = code
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	ch <- ProcEvent{Done: true, Code: worst}
}

// ---- update plans (Update section) -----------------------------------------

// updateStep is one maintenance action: a shell script run either on this
// machine (local) or on a remote host over SSH. sudo runs the whole script as
// root (non-interactively, so passwordless sudo is required on the target).
type updateStep struct {
	title  string
	host   string
	user   string
	pass   string
	script string
	local  bool
	sudo   bool
}

// localStepCmd runs a script on this machine. Non-sudo runs in a login shell so
// the user's PATH (ollama, npm bins) is available; sudo runs it as root.
func localStepCmd(script string, sudo bool) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("powershell", "-NoProfile", "-Command", script)
	}
	if sudo && os.Geteuid() != 0 {
		return exec.Command("sudo", "-n", "bash", "-lc", script)
	}
	return exec.Command("bash", "-lc", script)
}

// sshStepCmd runs a script on a remote host over SSH. The script is base64-piped
// to the remote shell so no quoting is needed. BatchMode avoids password prompts
// (we rely on the managed key); sudo escalates the whole script to root.
func sshStepCmd(user, host, keyPath, script string, sudo bool) *exec.Cmd {
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	sh := "bash -l -s"
	if sudo {
		sh = "sudo -n bash -s"
	}
	remote := "echo " + b64 + " | base64 -d | " + sh
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + osNull(),
		"-o", "BatchMode=yes",
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, fmt.Sprintf("%s@%s", orDefault(user, "rocky"), host), remote)
	return exec.Command("ssh", args...)
}

// RunUpdatePlan executes each step in order, streaming output into ch with a
// per-step header. It installs the managed SSH key on remote hosts first (so
// BatchMode SSH can authenticate), and emits a single aggregate Done at the end.
func RunUpdatePlan(steps []updateStep, ch chan<- ProcEvent) {
	defer close(ch)
	worst := 0
	for _, s := range steps {
		ch <- ProcEvent{Line: "=== " + s.title + " ==="}
		var code int
		if s.local || isLocalHost(s.host) {
			code = streamProcLines(localStepCmd(s.script, s.sudo), "", ch)
		} else {
			key, err := EnsureKeyAuth(s.host, s.user, s.pass)
			if err != nil {
				ch <- ProcEvent{Line: "  key auth: " + err.Error()}
			}
			code = streamProcLines(sshStepCmd(s.user, s.host, key, s.script, s.sudo), "", ch)
		}
		if code != 0 {
			worst = code
			ch <- ProcEvent{Line: fmt.Sprintf("  step failed (rc=%d)", code)}
		}
	}
	ch <- ProcEvent{Done: true, Code: worst}
}

// nextWorkerNames returns n consecutive VM names starting from base. If base ends
// in digits (e.g. "ollama-worker-04") the numeric suffix is incremented while
// preserving zero-pad width; otherwise "-01".."-NN" is appended.
func nextWorkerNames(base string, n int) []string {
	if n < 1 {
		n = 1
	}
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	names := make([]string, 0, n)
	if i == len(base) {
		for k := 0; k < n; k++ {
			names = append(names, fmt.Sprintf("%s-%02d", base, k+1))
		}
		return names
	}
	prefix, digits := base[:i], base[i:]
	width := len(digits)
	start, _ := strconv.Atoi(digits)
	for k := 0; k < n; k++ {
		names = append(names, fmt.Sprintf("%s%0*d", prefix, width, start+k))
	}
	return names
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
