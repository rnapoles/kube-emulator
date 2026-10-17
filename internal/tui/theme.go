// Package tui implements the terminal user interface (--tui): a live,
// filterable view of every request the emulator receives and every response
// it sends.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Palette: a dark, neon-on-slate look.
const (
	cCyan    = "#22D3EE"
	cBlue    = "#60A5FA"
	cViolet  = "#A78BFA"
	cMagenta = "#E879F9"
	cGreen   = "#34D399"
	cAmber   = "#FBBF24"
	cRed     = "#FB7185"
	cText    = "#E5E7EB"
	cSubtle  = "#9CA3AF"
	cDim     = "#6B7280"
	cFaint   = "#334155"
	cSelBg   = "#1E293B"
	cChipBg  = "#0F172A"
	cTabBg   = "#5B21B6"
	cBadgeFg = "#0B1020"
)

func fg(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }

var (
	stDim    = fg(cDim)
	stSubtle = fg(cSubtle)
	stText   = fg(cText)
	stCyan   = fg(cCyan)
	stGreen  = fg(cGreen)
	stAmber  = fg(cAmber)
	stRed    = fg(cRed)
	stViolet = fg(cViolet)
	stBold   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cText))
)

type rgb [3]int

var (
	gradFrom = rgb{34, 211, 238}  // cyan
	gradTo   = rgb{232, 121, 249} // magenta
)

func lerpColor(a, b rgb, t float64) string {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	c := func(i int) int { return a[i] + int(float64(b[i]-a[i])*t) }
	return fmt.Sprintf("#%02x%02x%02x", c(0), c(1), c(2))
}

// gradientText colours s from one end of the gradient to the other.
func gradientText(s string, a, b rgb, bold bool) string {
	rs := []rune(s)
	var sb strings.Builder
	for i, r := range rs {
		t := 0.0
		if len(rs) > 1 {
			t = float64(i) / float64(len(rs)-1)
		}
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(lerpColor(a, b, t))).Bold(bold)
		sb.WriteString(st.Render(string(r)))
	}
	return sb.String()
}

func methodColor(m string) string {
	switch m {
	case "GET":
		return cBlue
	case "POST":
		return cGreen
	case "PUT":
		return cAmber
	case "PATCH":
		return cViolet
	case "DELETE":
		return cRed
	}
	return cSubtle
}

func statusColor(code int) string {
	switch {
	case code == 0:
		return cDim
	case code < 300:
		return cGreen
	case code < 400:
		return cCyan
	case code < 500:
		return cAmber
	}
	return cRed
}

// badge is a solid, rounded-looking label.
func badge(text, bg string) string {
	return lipgloss.NewStyle().Background(lipgloss.Color(bg)).Foreground(lipgloss.Color(cBadgeFg)).Bold(true).Render(" " + text + " ")
}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func padLeft(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return padRight(ansi.Truncate(s, w, "…"), w)
}

// box draws a rounded panel of exactly w x h cells. A focused panel gets a
// gradient top edge and violet sides. title and footer may contain styling.
func box(title, footer string, lines []string, w, h int, focused bool) string {
	if w < 6 || h < 3 {
		return ""
	}
	iw, ih := w-2, h-2
	side := fg(cFaint)
	if focused {
		side = fg(cViolet)
	}
	edge := func(i int) lipgloss.Style {
		if focused {
			return fg(lerpColor(gradFrom, gradTo, float64(i)/float64(w-1)))
		}
		return fg(cFaint)
	}
	var top strings.Builder
	top.WriteString(edge(0).Render("╭"))
	top.WriteString(edge(1).Render("─"))
	tt := " " + title + " "
	if lipgloss.Width(tt) > w-5 {
		tt = ansi.Truncate(tt, w-5, "…")
	}
	tw := lipgloss.Width(tt)
	top.WriteString(tt)
	for i := 2 + tw; i < w-1; i++ {
		top.WriteString(edge(i).Render("─"))
	}
	top.WriteString(edge(w - 1).Render("╮"))

	out := make([]string, 0, h)
	out = append(out, top.String())
	for i := 0; i < ih; i++ {
		ln := ""
		if i < len(lines) {
			ln = lines[i]
		}
		ln = ansi.Truncate(ln, iw, "")
		if pad := iw - lipgloss.Width(ln); pad > 0 {
			ln += strings.Repeat(" ", pad)
		}
		out = append(out, side.Render("│")+ln+"\x1b[0m"+side.Render("│"))
	}
	ft := ""
	if footer != "" {
		ft = " " + footer + " "
	}
	n := w - 3 - lipgloss.Width(ft)
	if n < 0 {
		ft, n = "", w-3
	}
	out = append(out, side.Render("╰"+strings.Repeat("─", n))+ft+side.Render("─╯"))
	return strings.Join(out, "\n")
}

func sparkline(vals []int) string {
	const bars = "▁▂▃▄▅▆▇█"
	rs := []rune(bars)
	max := 1
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var sb strings.Builder
	for i, v := range vals {
		idx := 0
		if v > 0 {
			idx = 1 + (v-1)*(len(rs)-2)/max
			if idx >= len(rs) {
				idx = len(rs) - 1
			}
		}
		col := lerpColor(gradFrom, gradTo, float64(i)/float64(len(vals)-1))
		sb.WriteString(fg(col).Render(string(rs[idx])))
	}
	return sb.String()
}
