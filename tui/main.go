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

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
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
