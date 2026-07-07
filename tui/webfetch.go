package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Chat web fetch: when a chat prompt contains URLs, the TUI downloads each
// page, strips it to readable text, and hands it to the model as system
// context — giving the (otherwise offline) pool models basic web access
// straight from the Chat tab.

const (
	webFetchMaxURLs  = 3
	webFetchMaxBytes = 1 << 20 // per-page read cap
	webFetchMaxChars = 8000    // per-page extracted-text cap (keeps prompts sane)
	webFetchTimeout  = 20 * time.Second
)

var urlRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// extractURLs returns up to webFetchMaxURLs distinct URLs found in a chat
// message, with common trailing punctuation stripped.
func extractURLs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range urlRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:!?)]}")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		if len(out) == webFetchMaxURLs {
			break
		}
	}
	return out
}

var (
	dropBlockRe = regexp.MustCompile(`(?is)<(script|style|noscript|head|svg)[^>]*>.*?</\s*(script|style|noscript|head|svg)\s*>`)
	tagRe       = regexp.MustCompile(`(?s)<[^>]*>`)
	runSpaceRe  = regexp.MustCompile(`[ \t]+`)
	blankRunRe  = regexp.MustCompile(`\n{3,}`)
	entityRepl  = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&ndash;", "–", "&mdash;", "—",
	)
)

// htmlToText reduces an HTML document to readable plain text: script/style/head
// blocks are dropped, remaining tags stripped, common entities decoded, and
// whitespace collapsed.
func htmlToText(s string) string {
	s = dropBlockRe.ReplaceAllString(s, " ")
	// Keep paragraph structure: block-level closers become newlines before all
	// remaining tags are flattened to spaces.
	s = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article)>|<br\s*/?>`).ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = entityRepl.Replace(s)
	s = runSpaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = blankRunRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// fetchPageText downloads one URL and returns its readable text (HTML stripped,
// size-capped). Plain text and JSON pass through as-is (also capped).
func fetchPageText(url string) (string, error) {
	client := &http.Client{Timeout: webFetchTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "oilsand-ai-gateway-tui/1.0 (chat web fetch)")
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))
	if err != nil {
		return "", err
	}
	text := string(raw)
	ct := resp.Header.Get("Content-Type")
	head := strings.ToLower(text[:minInt(len(text), 512)])
	if strings.Contains(ct, "html") || strings.Contains(head, "<html") || strings.Contains(head, "<!doctype") {
		text = htmlToText(text)
	} else {
		text = strings.TrimSpace(text)
	}
	if r := []rune(text); len(r) > webFetchMaxChars {
		text = string(r[:webFetchMaxChars]) + " …[truncated]"
	}
	if text == "" {
		return "", fmt.Errorf("no readable text")
	}
	return text, nil
}

// webContext fetches every URL and builds the system-context block handed to
// the model, plus a short status note for the UI. ctx is "" when nothing could
// be fetched.
func webContext(urls []string) (ctx, note string) {
	var b strings.Builder
	var oks, fails []string
	for _, u := range urls {
		text, err := fetchPageText(u)
		if err != nil {
			fails = append(fails, hostFromURL(u)+" ("+err.Error()+")")
			continue
		}
		fmt.Fprintf(&b, "--- content of %s ---\n%s\n\n", u, text)
		oks = append(oks, hostFromURL(u))
	}
	if b.Len() > 0 {
		ctx = "The user's message references web pages. Their fetched content is below — use it to answer.\n\n" + b.String()
	}
	var parts []string
	if len(oks) > 0 {
		parts = append(parts, "fetched "+strings.Join(oks, ", "))
	}
	if len(fails) > 0 {
		parts = append(parts, "failed "+strings.Join(fails, ", "))
	}
	if len(parts) > 0 {
		note = "web: " + strings.Join(parts, " · ")
	}
	return ctx, note
}
