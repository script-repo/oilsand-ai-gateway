package main

import (
	_ "embed"
	"fmt"
	"strings"
)

// modelCapabilitiesYAML is Olla's model-unification override that makes our
// models advertise the exact capability tokens Olla's router requires for
// agentic clients (function_calling, tools, code). See olla-models.yaml for the
// full rationale. Embedded so the TUI can push it to a running gateway without
// shipping a separate file.
//
//go:embed olla-models.yaml
var modelCapabilitiesYAML string

// ApplyModelCapabilities installs the capability-aware models.yaml on the gateway
// (at /etc/olla/models.yaml), ensures Olla loads it via OLLA_CONFIG_DIR (systemd
// drop-in, no main-unit edit), and restarts Olla. After this, capability-based
// routing matches instead of logging "No models found with required capabilities"
// and falling back.
func ApplyModelCapabilities(host, user, password string) (string, error) {
	client, err := dialSSH(host, user, password)
	if err != nil {
		return "", fmt.Errorf("ssh connect failed: %w", err)
	}
	defer client.Close()

	writeCmd := fmt.Sprintf("cat > /tmp/olla-models.yaml.new <<'OLLAEOF'\n%s\nOLLAEOF", modelCapabilitiesYAML)
	if out, err := runSSH(client, writeCmd); err != nil {
		return "", fmt.Errorf("write temp models.yaml: %v (%s)", err, strings.TrimSpace(out))
	}

	applyCmd := strings.Join([]string{
		"sudo install -o olla -g olla -m 0644 /tmp/olla-models.yaml.new /etc/olla/models.yaml",
		"sudo mkdir -p /etc/systemd/system/olla.service.d",
		"printf '[Service]\\nEnvironment=OLLA_CONFIG_DIR=/etc/olla\\n' | sudo tee /etc/systemd/system/olla.service.d/10-config-dir.conf >/dev/null",
		"sudo systemctl daemon-reload",
		"sudo systemctl restart olla",
	}, " && ")
	if out, err := runSSH(client, applyCmd); err != nil {
		return "", fmt.Errorf("apply/restart failed: %v (%s)", err, strings.TrimSpace(out))
	}
	return "models.yaml installed (advertises tools/function_calling/code) and Olla restarted", nil
}
