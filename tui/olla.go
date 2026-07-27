package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllaClient talks to an Olla gateway over HTTP. All calls are safe to run from
// tea.Cmd goroutines; the client itself is stateless beyond its base URL.
type OllaClient struct {
	base string
	http *http.Client
}

func NewOllaClient(base string) *OllaClient {
	return &OllaClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 0}, // per-request contexts handle timeouts
	}
}

// ---- response shapes -------------------------------------------------------

type VersionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Edition string `json:"edition"`
}

type Endpoint struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	SuccessRate string `json:"success_rate"`
	AvgLatency  string `json:"avg_latency"`
	Connections int    `json:"connections"`
	Requests    int    `json:"requests"`
	Priority    int    `json:"priority"`
	Issues      string `json:"issues"`
	Models      struct {
		Count int `json:"count"`
	} `json:"models"`
}

type SystemStatus struct {
	EndpointsUp       any    `json:"endpoints_up"`
	ActiveConnections int    `json:"active_connections"`
	AvgLatency        string `json:"avg_latency"`
	SuccessRate       string `json:"success_rate"`
	Uptime            string `json:"uptime"`
	TotalRequests     int    `json:"total_requests"`
	TotalFailures     int    `json:"total_failures"`
	TotalTraffic      string `json:"total_traffic"`
}

type Status struct {
	System    SystemStatus `json:"system"`
	Endpoints []Endpoint   `json:"endpoints"`
}

type ModelDetails struct {
	Family            string `json:"family"`
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
}

type Model struct {
	Name    string       `json:"name"`
	Size    int64        `json:"size"`
	Details ModelDetails `json:"details"`
}

// ---- calls -----------------------------------------------------------------

func (c *OllaClient) ctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (c *OllaClient) Version() (VersionInfo, error) {
	ctx, cancel := c.ctx(10 * time.Second)
	defer cancel()
	var v VersionInfo
	err := c.getJSON(ctx, "/version", &v)
	return v, err
}

// Probe reports whether an Olla gateway answers on this base URL within d. It
// backs the startup "is a gateway already running here?" check, so the timeout
// is short: a machine with no gateway must not stall the UI.
func (c *OllaClient) Probe(d time.Duration) error {
	ctx, cancel := c.ctx(d)
	defer cancel()
	var v VersionInfo
	return c.getJSON(ctx, "/version", &v)
}

func (c *OllaClient) Status() (Status, error) {
	ctx, cancel := c.ctx(10 * time.Second)
	defer cancel()
	var s Status
	err := c.getJSON(ctx, "/internal/status", &s)
	return s, err
}

func (c *OllaClient) Models() ([]Model, error) {
	ctx, cancel := c.ctx(15 * time.Second)
	defer cancel()
	var out struct {
		Models []Model `json:"models"`
	}
	err := c.getJSON(ctx, "/olla/ollama/api/tags", &out)
	return out.Models, err
}

func (c *OllaClient) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// ---- streaming chat --------------------------------------------------------

// ChatEvent is one item emitted while streaming a completion. "note" carries
// out-of-band progress (e.g. the web-fetch phase) for the status line.
type ChatEvent struct {
	Kind    string // "first" | "delta" | "usage" | "note" | "done" | "error"
	Content string
	Usage   *ChatUsage
	Err     error
}

type ChatUsage struct {
	CompletionTokens int `json:"completion_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatStream POSTs a streaming chat completion and writes events to ch until the
// stream ends (a final "done" or "error" event is always sent). It is meant to
// be run in its own goroutine.
func (c *OllaClient) ChatStream(model string, messages []ChatMessage, ch chan<- ChatEvent) {
	defer close(ch)
	body := map[string]any{
		"model":          model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/olla/openai/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		ch <- ChatEvent{Kind: "error", Err: err}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		ch <- ChatEvent{Kind: "error", Err: err}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		ch <- ChatEvent{Kind: "error", Err: fmt.Errorf("chat %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))}
		return
	}

	first := true
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			ch <- ChatEvent{Kind: "done"}
			return
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *ChatUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue
		}
		if obj.Usage != nil {
			ch <- ChatEvent{Kind: "usage", Usage: obj.Usage}
		}
		if len(obj.Choices) > 0 {
			if d := obj.Choices[0].Delta.Content; d != "" {
				if first {
					first = false
					ch <- ChatEvent{Kind: "first", Content: d}
				} else {
					ch <- ChatEvent{Kind: "delta", Content: d}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		ch <- ChatEvent{Kind: "error", Err: err}
		return
	}
	ch <- ChatEvent{Kind: "done"}
}

// ---- model management (pull / delete) --------------------------------------

// PullEvent is one progress item while pulling a model. For a single (gateway)
// pull Worker is empty and Done marks the whole operation complete. For a
// parallel multi-worker pull, Worker identifies which worker the event is for
// and Done marks that worker finished; the overall completion is signalled by a
// final event with an empty Worker (the closed channel sentinel).
type PullEvent struct {
	Worker    string
	Status    string
	Completed int64
	Total     int64
	Done      bool
	Err       error
}

// Model management (pull/delete) is not done through the gateway: Olla is a load
// balancer and rejects those with 501 ("model management operations not
// supported by proxy"). They fan out to each worker's Ollama directly instead
// (see ollamaPullInto / startMultiPull and deleteModelAllCmd).
