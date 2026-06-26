package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// usageLedger persists per-day, per-model token counts so the dashboard can
// report a rolling 30-day aggregate. Only chats issued through this TUI are
// recorded — Olla Community does not expose per-model token history through its
// API, so this is the one usage signal we can attribute reliably.
type usageLedger struct {
	// Days maps a YYYY-MM-DD bucket to per-model token totals for that day.
	Days map[string]map[string]int64 `json:"days"`
}

const usageWindowDays = 30

func usagePath(tokFile string) string {
	return filepath.Join(filepath.Dir(tokFile), "usage.json")
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func loadUsage(path string) usageLedger {
	led := usageLedger{Days: map[string]map[string]int64{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &led)
	}
	if led.Days == nil {
		led.Days = map[string]map[string]int64{}
	}
	return led
}

func saveUsage(path string, led usageLedger) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(led, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

// pruneUsage drops day buckets older than the reporting window.
func pruneUsage(led usageLedger) {
	cutoff := time.Now().AddDate(0, 0, -usageWindowDays)
	for day := range led.Days {
		if t, err := time.Parse("2006-01-02", day); err == nil && t.Before(cutoff) {
			delete(led.Days, day)
		}
	}
}

// recordUsage adds tokens for a model to today's bucket, prunes stale days, and
// persists. It returns the updated ledger so the caller can recompute the
// dashboard aggregate without re-reading the file.
func recordUsage(path, model string, tokens int) usageLedger {
	led := loadUsage(path)
	pruneUsage(led)
	if model != "" && tokens > 0 {
		d := dayKey(time.Now())
		if led.Days[d] == nil {
			led.Days[d] = map[string]int64{}
		}
		led.Days[d][model] += int64(tokens)
		_ = saveUsage(path, led)
	}
	return led
}

// usage30 aggregates tokens per model across the reporting window.
func usage30(led usageLedger) map[string]int64 {
	cutoff := time.Now().AddDate(0, 0, -usageWindowDays)
	out := map[string]int64{}
	for day, models := range led.Days {
		if t, err := time.Parse("2006-01-02", day); err != nil || t.Before(cutoff) {
			continue
		}
		for model, n := range models {
			out[model] += n
		}
	}
	return out
}

type modelTokens struct {
	model  string
	tokens int64
}

// usage30Sorted returns per-model totals ordered by tokens (desc).
func usage30Sorted(agg map[string]int64) []modelTokens {
	out := make([]modelTokens, 0, len(agg))
	for k, v := range agg {
		out = append(out, modelTokens{model: k, tokens: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].tokens != out[j].tokens {
			return out[i].tokens > out[j].tokens
		}
		return out[i].model < out[j].model
	})
	return out
}
