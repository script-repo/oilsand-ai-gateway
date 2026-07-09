package main

import (
	"strings"
	"testing"
)

// Base-path endpoints (e.g. Nutanix Enterprise AI at https://host/api, whose
// OpenAI API lives under /api/v1) get preserve_path so Olla keeps the prefix
// when proxying; plain host:port endpoints must not.
func TestBuildEndpointEntryPreservePath(t *testing.T) {
	e := buildEndpointEntry("nai-01", "https://nai-host/api", "openai", "50", "", "")
	if !e.PreservePath {
		t.Fatalf("base-path URL should set preserve_path: %+v", e)
	}
	if e.Type != "openai" || e.Priority != 50 {
		t.Fatalf("type/priority not carried through: %+v", e)
	}

	e = buildEndpointEntry("w1", "http://10.0.0.4:11434", "", "", "", "")
	if e.PreservePath {
		t.Fatalf("bare host:port URL should not set preserve_path: %+v", e)
	}
	if e.Type != "ollama" || e.Priority != 100 {
		t.Fatalf("defaults not applied: %+v", e)
	}

	e = buildEndpointEntry("nai-02", "https://nai-host/api", "openai", "100",
		"https://nai-host/api/v1/models", "https://nai-host/api/v1/models")
	if e.ModelURL == "" || e.HealthCheckURL == "" {
		t.Fatalf("model/health URLs not carried through: %+v", e)
	}
}

// Every endpoint edit round-trips the whole list through endpointEntry, so
// the Olla fields the TUI doesn't surface in forms (preserve_path, model_url,
// health_check_url) and even unknown operator-added keys must survive —
// previously they were silently stripped from olla.yaml.
func TestMutateEndpointsPreservesExtendedFields(t *testing.T) {
	base := `discovery:
  static:
    endpoints:
      - url: "https://10.0.0.7/api"
        name: "nai-01"
        type: "openai"
        priority: 100
        preserve_path: true
        model_url: "https://10.0.0.7/api/v1/models"
        health_check_url: "https://10.0.0.7/api/v1/models"
        some_future_field: "kept"
      - url: "http://10.0.0.4:11434"
        name: "ollama-worker-04"
        type: "ollama"
        priority: 100
`
	// An unrelated edit (adding a worker) must not touch the NAI entry.
	out, err := mutateEndpoints(base, addEndpointFn(endpointEntry{
		Name: "ollama-worker-05", URL: "http://10.0.0.5:11434", Type: "ollama", Priority: 100,
	}))
	if err != nil {
		t.Fatalf("mutateEndpoints: %v", err)
	}
	for _, want := range []string{
		"preserve_path: true",
		`model_url: https://10.0.0.7/api/v1/models`,
		`health_check_url: https://10.0.0.7/api/v1/models`,
		"some_future_field: kept",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("round-trip stripped %q:\n%s", want, out)
		}
	}
	eps := readEndpointsFromYAML(out)
	if len(eps) != 3 {
		t.Fatalf("expected 3 endpoints, got %d: %+v", len(eps), eps)
	}
	for _, ep := range eps {
		if ep.Name == "nai-01" {
			if !ep.PreservePath || ep.ModelURL == "" || ep.HealthCheckURL == "" {
				t.Fatalf("nai-01 lost fields: %+v", ep)
			}
			if ep.Extra["some_future_field"] != "kept" {
				t.Fatalf("nai-01 lost unknown key: %+v", ep.Extra)
			}
		}
	}
}
