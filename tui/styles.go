package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Charm-inspired "Tokyo Night" palette.
var (
	colBg      = lipgloss.Color("#1a1b26")
	colBorder  = lipgloss.Color("#3b4261")
	colSubtle  = lipgloss.Color("#565f89")
	colMuted   = lipgloss.Color("#787c99")
	colText    = lipgloss.Color("#c0caf5")
	colPrimary = lipgloss.Color("#7aa2f7")
	colAccent  = lipgloss.Color("#bb9af7")
	colGreen   = lipgloss.Color("#9ece6a")
	colYellow  = lipgloss.Color("#e0af68")
	colRed     = lipgloss.Color("#f7768e")
	colCyan    = lipgloss.Color("#7dcfff")
	colPink    = lipgloss.Color("#ff75b5")
	colOrange  = lipgloss.Color("#ff9e64")
)

var (
	// brand / header -------------------------------------------------------
	brandStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colBg).
			Background(colAccent).
			Padding(0, 1)

	brandSubStyle = lipgloss.NewStyle().Foreground(colMuted).Italic(true)

	badgeOn   = lipgloss.NewStyle().Bold(true).Foreground(colBg).Background(colGreen).Padding(0, 1)
	badgeOff  = lipgloss.NewStyle().Bold(true).Foreground(colBg).Background(colRed).Padding(0, 1)
	badgeInfo = lipgloss.NewStyle().Bold(true).Foreground(colBg).Background(colPrimary).Padding(0, 1)
	badgeDim  = lipgloss.NewStyle().Foreground(colMuted).Padding(0, 1)

	// sidebar --------------------------------------------------------------
	sidebarBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(1, 1)

	navTitle = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	navItem = lipgloss.NewStyle().Foreground(colMuted).Padding(0, 1)

	navItemCurrent = lipgloss.NewStyle().Foreground(colText).Bold(true).Padding(0, 1)

	navItemActive = lipgloss.NewStyle().
			Foreground(colBg).Background(colPrimary).Bold(true).Padding(0, 1)

	navHint = lipgloss.NewStyle().Foreground(colSubtle).Italic(true)

	// content --------------------------------------------------------------
	contentBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(1, 2)

	sectionTitle = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	// cards / panels -------------------------------------------------------
	statCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(0, 1).
			Width(20)

	statLabel = lipgloss.NewStyle().Foreground(colMuted)
	statBig   = lipgloss.NewStyle().Foreground(colCyan).Bold(true)

	// generic text ---------------------------------------------------------
	labelStyle = lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	dimStyle   = lipgloss.NewStyle().Foreground(colSubtle)
	goodStyle  = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	warnStyle  = lipgloss.NewStyle().Foreground(colYellow).Bold(true)
	badStyle   = lipgloss.NewStyle().Foreground(colRed).Bold(true)
	accentText = lipgloss.NewStyle().Foreground(colAccent)
	youStyle   = lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	botStyle   = lipgloss.NewStyle().Foreground(colGreen).Bold(true)

	// chrome ---------------------------------------------------------------
	noticeStyle = lipgloss.NewStyle().Bold(true).Foreground(colBg).
			Background(colAccent).Padding(0, 1)

	modalBox = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(colAccent).
			Padding(1, 3)

	modalTitle = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	codeStyle = lipgloss.NewStyle().Foreground(colGreen)

	statusGood = lipgloss.NewStyle().Foreground(colGreen)
	statusWarn = lipgloss.NewStyle().Foreground(colYellow)
	statusBad  = lipgloss.NewStyle().Foreground(colRed)
)

func statusStyle(s string) lipgloss.Style {
	switch strings.ToLower(s) {
	case "healthy", "online", "on", "active", "ready":
		return statusGood
	case "offline", "unhealthy", "off", "error", "failed":
		return statusBad
	default:
		return statusWarn
	}
}

func dot(s string) string {
	return statusStyle(s).Render("●")
}

func humanBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	for _, u := range units {
		if n < 1024 {
			return fmt.Sprintf("%.1f %s", n, u)
		}
		n /= 1024
	}
	return fmt.Sprintf("%.1f PB", n)
}

// barInline renders a fixed-width filled/empty bar in the given style.
func barInline(frac float64, width int, style lipgloss.Style) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	if filled > width {
		filled = width
	}
	return style.Render(strings.Repeat("█", filled)) +
		dimStyle.Render(strings.Repeat("░", width-filled))
}

// palette returns a stable accent color for index i (for per-endpoint bars).
func palette(i int) lipgloss.Color {
	cols := []lipgloss.Color{colGreen, colCyan, colAccent, colYellow, colPink, colPrimary, colOrange}
	return cols[i%len(cols)]
}
