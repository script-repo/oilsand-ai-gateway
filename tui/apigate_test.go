package main

import (
	"strings"
	"testing"
)

// The install script must set up TLS on 443, allow nginx to proxy under
// SELinux, stream SSE unbuffered, open the firewall, and stay idempotent
// (cert generated once, key store only created when absent).
func TestAPIGateInstallScript(t *testing.T) {
	s := apiGateInstallScript()
	for _, want := range []string{
		"listen " + apiGatePort + " ssl",
		"openssl req -x509",
		"httpd_can_network_connect",
		"proxy_buffering off",
		"proxy_set_header Authorization $olla_api_upstream_auth",
		"proxy_pass $olla_api_target$1$is_args$args",
		"return 401",
		"--add-port=" + apiGatePort + "/tcp",
		"if [ ! -f /etc/nginx/olla-api.crt ]",
		"if [ ! -f " + apiKeysConfPath + " ]",
		"nginx -t",
		"systemctl reload nginx",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("install script missing %q:\n%s", want, s)
		}
	}
}

// The key store renders to nginx maps whose defaults reject unknown keys
// (empty target -> 401) and pass the client Authorization header through
// unless a per-key upstream key overrides it — and parses back losslessly.
func TestAPIKeysConfRoundTrip(t *testing.T) {
	keys := []apiKeyEntry{
		{Name: "team-a", Key: "sk-oilsand-aaa", Target: "http://127.0.0.1:40114/olla/openai/v1"},
		{Name: "nai only", Key: "sk-oilsand-bbb", Target: "https://10.0.0.7/api/v1", UpstreamKey: "nai-secret"},
	}
	conf := renderAPIKeysConf(keys)
	for _, want := range []string{
		`default "";`,                 // unknown keys resolve to "" -> 401
		"default $http_authorization", // pass-through unless overridden
		`"Bearer sk-oilsand-aaa" "http://127.0.0.1:40114/olla/openai/v1"; # name=team-a`,
		`"Bearer sk-oilsand-bbb" "Bearer nai-secret"; # name=nai-only`,
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("rendered conf missing %q:\n%s", want, conf)
		}
	}

	got := parseAPIKeysConf(conf)
	if len(got) != 2 {
		t.Fatalf("parsed %d keys, want 2: %+v", len(got), got)
	}
	if got[0].Name != "team-a" || got[0].Key != "sk-oilsand-aaa" ||
		got[0].Target != "http://127.0.0.1:40114/olla/openai/v1" || got[0].UpstreamKey != "" {
		t.Fatalf("key 0 mismatch: %+v", got[0])
	}
	if got[1].Name != "nai-only" || got[1].UpstreamKey != "nai-secret" ||
		got[1].Target != "https://10.0.0.7/api/v1" {
		t.Fatalf("key 1 mismatch: %+v", got[1])
	}
}

// An empty key store still renders both maps with safe defaults, so a fresh
// front door rejects everything instead of proxying unauthenticated traffic.
func TestAPIKeysConfEmpty(t *testing.T) {
	conf := renderAPIKeysConf(nil)
	if !strings.Contains(conf, `default "";`) || !strings.Contains(conf, "default $http_authorization") {
		t.Fatalf("empty conf lacks safe defaults:\n%s", conf)
	}
	if got := parseAPIKeysConf(conf); len(got) != 0 {
		t.Fatalf("empty conf parsed keys: %+v", got)
	}
}

// Key targets: the pool maps to Olla's OpenAI namespace over loopback (same
// port as the gateway URL), and endpoint targets append /v1 to the backend
// URL — including base-path backends, where /api + /v1 = /api/v1.
func TestAPIKeyTargets(t *testing.T) {
	if got := poolTarget("http://10.0.0.1:40114"); got != "http://127.0.0.1:40114/olla/openai/v1" {
		t.Fatalf("poolTarget = %q", got)
	}
	if got := poolTarget(""); got != "http://127.0.0.1:40114/olla/openai/v1" {
		t.Fatalf("poolTarget fallback = %q", got)
	}
	if got := endpointTarget(endpointEntry{URL: "http://10.0.0.9:20128"}); got != "http://10.0.0.9:20128/v1" {
		t.Fatalf("endpointTarget = %q", got)
	}
	if got := endpointTarget(endpointEntry{URL: "https://10.0.0.7/api/"}); got != "https://10.0.0.7/api/v1" {
		t.Fatalf("endpointTarget base path = %q", got)
	}
}
