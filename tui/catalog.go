package main

import "fmt"

// catalogEntry is a model that can be pulled onto the worker pool. The list is a
// curated, offline catalog of popular Ollama models plus a few Ollama Cloud
// models. Any model name not listed here can still be pulled by hand via the
// "pull" modal (p) — this is just a convenient browse-and-download menu.
type catalogEntry struct {
	name  string // ollama model ref, e.g. "qwen2.5-coder:3b"
	size  string // approx download size
	desc  string // short description
	cloud bool   // true = Ollama Cloud model (runs on Ollama's servers; needs `ollama signin`)
}

func (c catalogEntry) Title() string {
	if c.cloud {
		return "☁ " + c.name
	}
	return c.name
}

func (c catalogEntry) Description() string {
	tag := c.size
	if c.cloud {
		tag = "cloud"
	}
	return fmt.Sprintf("%s · %s", tag, c.desc)
}

// modelCatalog is the curated set offered in the browse menu.
var modelCatalog = []catalogEntry{
	// --- coding models (local) ---
	{"qwen2.5-coder:1.5b", "1.0 GB", "tiny coder — fast autocomplete on CPU", false},
	{"qwen2.5-coder:3b", "1.9 GB", "coder — CPU-friendly chat", false},
	{"qwen2.5-coder:7b", "4.7 GB", "strong local coder", false},
	{"qwen2.5-coder:14b", "9.0 GB", "best local coder (RAM heavy)", false},
	{"starcoder2:3b", "1.7 GB", "fill-in-the-middle autocomplete", false},
	{"deepseek-coder-v2:16b", "8.9 GB", "code reasoning & refactors", false},
	// --- general / reasoning (local) ---
	{"llama3.2:1b", "1.3 GB", "tiny general model", false},
	{"llama3.2:3b", "2.0 GB", "general, fast", false},
	{"llama3.1:8b", "4.9 GB", "general assistant", false},
	{"gemma3:1b", "0.8 GB", "tiny general model", false},
	{"gemma3:4b", "3.3 GB", "general, balanced", false},
	{"gemma3:12b", "8.1 GB", "general, capable", false},
	{"phi4:14b", "9.1 GB", "reasoning-focused", false},
	{"mistral:7b", "4.4 GB", "general assistant", false},
	{"deepseek-r1:7b", "4.7 GB", "reasoning (R1 distill)", false},
	{"deepseek-r1:8b", "5.2 GB", "reasoning (R1 distill)", false},
	// --- embeddings (for Continue @codebase) ---
	{"nomic-embed-text", "0.3 GB", "embeddings for codebase search", false},
	// --- Ollama Cloud (require `ollama signin` on the worker) ---
	{"gpt-oss:20b-cloud", "", "OpenAI gpt-oss 20B (cloud)", true},
	{"gpt-oss:120b-cloud", "", "OpenAI gpt-oss 120B (cloud)", true},
	{"qwen3-coder:480b-cloud", "", "Qwen3 Coder 480B (cloud)", true},
	{"deepseek-v3.1:671b-cloud", "", "DeepSeek V3.1 671B (cloud)", true},
	{"glm-4.6:cloud", "", "GLM 4.6 (cloud)", true},
	{"kimi-k2:1t-cloud", "", "Kimi K2 1T (cloud)", true},
}
