package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// osNull is the platform's null device, used for ssh UserKnownHostsFile.
func osNull() string {
	if runtime.GOOS == "windows" {
		return "NUL"
	}
	return "/dev/null"
}

// osc8 wraps text in an OSC 8 terminal hyperlink so supporting terminals render
// it as a clickable link (click / ctrl+click opens the URL in a browser).
func osc8(url, text string) string {
	if url == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// shSingle escapes a value for safe interpolation inside a single-quoted POSIX
// shell string: close the quote, emit an escaped quote, then reopen.
func shSingle(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func msIf(v float64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f ms", v)
}

func tokIf(v float64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

// humanCount renders large token counts compactly (e.g. 12.3k, 4.1M).
func humanCount(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e9:
		return fmt.Sprintf("%.1fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fk", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
		return v
	}
	return def
}

func parseBytes(s string) float64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	units := map[string]float64{"B": 1, "KB": 1024, "MB": 1024 * 1024, "GB": 1024 * 1024 * 1024, "TB": 1024 * 1024 * 1024 * 1024}
	parts := strings.Fields(s)
	if len(parts) == 2 {
		if u, ok := units[parts[1]]; ok {
			v, _ := strconv.ParseFloat(parts[0], 64)
			return v * u
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func randToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "token"
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
