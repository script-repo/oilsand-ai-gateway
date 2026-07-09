// Command oilsand-tui is a Charm/Bubble Tea terminal UI for managing an Olla
// LLM gateway and its Ollama worker pool, plus the Nutanix VMs behind them.
//
// It uses several Charm projects: Bubble Tea (runtime), Huh (modal forms),
// Bubbles (list, help, key, progress, textarea, viewport, spinner), Lip Gloss
// (styling/layout) and Glamour (markdown rendering for streamed chat responses).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>" (see .goreleaser.yaml). It defaults to "dev"
// for local/go-run builds.
var version = "dev"

// tuiVersion returns a display string for the running build, e.g. "v0.2.1" for a
// release or "dev" for an un-tagged local build.
func tuiVersion() string {
	v := strings.TrimSpace(version)
	if v == "" || v == "dev" {
		return "dev"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

func main() {
	// Avoid probing the terminal for its background color. termenv (via Glamour's
	// auto-style and Lip Gloss) otherwise sends an OSC/cursor-position query and
	// reads the reply from stdin; over SSH and serial/VM consoles that reply isn't
	// consumed cleanly and leaks into the focused input as artifacts like
	// "[48;1R", corrupting the gateway URL / PC host. Declaring the background via
	// COLORFGBG makes termenv skip the query entirely.
	if os.Getenv("COLORFGBG") == "" {
		_ = os.Setenv("COLORFGBG", "15;0") // light text on a dark background
	}

	// Headless subcommand: rewrite the gateway's olla.yaml so its load balancer
	// spreads traffic (least-connections) instead of pinning to the first
	// endpoint, then restart Olla. Reuses the same SSH path as the TUI.
	if len(os.Args) > 1 && os.Args[1] == "apply-balancer" {
		applyBalancer(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "show-config" {
		showConfig(os.Args[2:])
		return
	}
	// Headless subcommand: install the capability-aware models.yaml on the
	// gateway so agentic clients (tools/function_calling/code) route cleanly
	// instead of falling back with a capability WARN.
	if len(os.Args) > 1 && os.Args[1] == "apply-capabilities" {
		applyCapabilities(os.Args[2:])
		return
	}
	// Headless subcommand: install the TLS + API-key /api/v1 front door
	// (nginx) on the gateway VM, same as pressing `v` in the Access section.
	if len(os.Args) > 1 && os.Args[1] == "apply-api-gate" {
		applyAPIGate(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "ssh-exec" {
		sshExec(os.Args[2:])
		return
	}

	gateway := flag.String("gateway", os.Getenv("OLLA_GATEWAY"), "Olla gateway URL, e.g. http://gateway-host:40114")
	sshUser := flag.String("ssh-user", os.Getenv("OLLA_SSH_USER"), "SSH user for endpoint add/remove on the gateway VM")
	sshPass := flag.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password for the gateway VM")
	flag.Parse()

	m := newModel(*gateway, *sshUser, *sshPass)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func showConfig(args []string) {
	fs := flag.NewFlagSet("show-config", flag.ExitOnError)
	host := fs.String("gateway", os.Getenv("OLLA_GATEWAY"), "gateway host or URL")
	user := fs.String("ssh-user", envOr("OLLA_SSH_USER", "rocky"), "SSH user")
	pass := fs.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password")
	_ = fs.Parse(args)
	h := hostFromURL(normalizeGateway(*host))
	client, err := dialSSH(h, *user, *pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ssh:", err)
		os.Exit(1)
	}
	defer client.Close()
	txt, _ := readRemoteFile(client, "/etc/olla/olla.yaml")
	fmt.Println(txt)
}

func sshExec(args []string) {
	fs := flag.NewFlagSet("ssh-exec", flag.ExitOnError)
	host := fs.String("gateway", os.Getenv("OLLA_GATEWAY"), "host or URL")
	user := fs.String("ssh-user", envOr("OLLA_SSH_USER", "rocky"), "SSH user")
	pass := fs.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password")
	cmd := fs.String("cmd", "", "command to run remotely")
	_ = fs.Parse(args)
	h := hostFromURL(normalizeGateway(*host))
	client, err := dialSSH(h, *user, *pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ssh:", err)
		os.Exit(1)
	}
	defer client.Close()
	out, _ := runSSH(client, *cmd)
	fmt.Print(out)
}

func applyCapabilities(args []string) {
	fs := flag.NewFlagSet("apply-capabilities", flag.ExitOnError)
	host := fs.String("gateway", os.Getenv("OLLA_GATEWAY"), "gateway host or URL (the SSH host is derived from it)")
	user := fs.String("ssh-user", envOr("OLLA_SSH_USER", "rocky"), "SSH user for the gateway VM")
	pass := fs.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password for the gateway VM")
	_ = fs.Parse(args)

	h := hostFromURL(normalizeGateway(*host))
	if h == "" {
		fmt.Fprintln(os.Stderr, "apply-capabilities: --gateway is required")
		os.Exit(2)
	}
	fmt.Printf("installing /etc/olla/models.yaml on %s (advertise tools/function_calling/code) and restarting Olla…\n", h)
	msg, err := ApplyModelCapabilities(h, *user, *pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply-capabilities failed:", err)
		os.Exit(1)
	}
	fmt.Println("ok:", msg)
}

func applyAPIGate(args []string) {
	fs := flag.NewFlagSet("apply-api-gate", flag.ExitOnError)
	host := fs.String("gateway", os.Getenv("OLLA_GATEWAY"), "gateway host or URL (the SSH host is derived from it)")
	user := fs.String("ssh-user", envOr("OLLA_SSH_USER", "rocky"), "SSH user for the gateway VM")
	pass := fs.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password for the gateway VM")
	_ = fs.Parse(args)

	h := hostFromURL(normalizeGateway(*host))
	if h == "" {
		fmt.Fprintln(os.Stderr, "apply-api-gate: --gateway is required")
		os.Exit(2)
	}
	fmt.Printf("installing the https://%s/api/v1 front door (nginx + TLS + API keys)…\n", h)
	out, err := runOnGateway(h, *user, *pass, apiGateInstallScript())
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply-api-gate failed:", err, strings.TrimSpace(out))
		os.Exit(1)
	}
	fmt.Println(strings.TrimSpace(out))
}

func applyBalancer(args []string) {
	fs := flag.NewFlagSet("apply-balancer", flag.ExitOnError)
	host := fs.String("gateway", os.Getenv("OLLA_GATEWAY"), "gateway host or URL (the SSH host is derived from it)")
	user := fs.String("ssh-user", envOr("OLLA_SSH_USER", "rocky"), "SSH user for the gateway VM")
	pass := fs.String("ssh-password", os.Getenv("OLLA_SSH_PASSWORD"), "SSH password for the gateway VM")
	_ = fs.Parse(args)

	h := hostFromURL(normalizeGateway(*host))
	if h == "" {
		fmt.Fprintln(os.Stderr, "apply-balancer: --gateway is required")
		os.Exit(2)
	}
	fmt.Printf("tuning /etc/olla/olla.yaml on %s (least-connections + 900s timeouts) and restarting Olla…\n", h)
	// Identity transform: keep the endpoint list, but mutateEndpoints also runs
	// ensureGatewayDefaults (balancer + generous timeouts).
	msg, err := ApplyEndpointChange(h, *user, *pass, func(e []endpointEntry) []endpointEntry { return e })
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply-balancer failed:", err)
		os.Exit(1)
	}
	fmt.Println("ok:", msg)
}
