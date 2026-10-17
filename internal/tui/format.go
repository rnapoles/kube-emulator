package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const highlightLimit = 120 << 10 // above this, bodies are shown without colours

var (
	stJSONKey  = fg(cCyan)
	stJSONStr  = fg(cGreen)
	stJSONNum  = fg(cAmber)
	stJSONBool = fg(cMagenta)
	stJSONNull = fg(cDim).Italic(true)
	stJSONSym  = fg(cDim)
)

func isNumByte(c byte) bool {
	return c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E'
}

func isLetterByte(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// highlightJSON colours JSON text. It tolerates invalid or truncated JSON.
func highlightJSON(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '"' {
					j++
					break
				}
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if k < len(s) && s[k] == ':' {
				b.WriteString(stJSONKey.Render(s[i:j]))
			} else {
				b.WriteString(stJSONStr.Render(s[i:j]))
			}
			i = j
		case c == '-' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(s) && isNumByte(s[j]) {
				j++
			}
			b.WriteString(stJSONNum.Render(s[i:j]))
			i = j
		case isLetterByte(c):
			j := i
			for j < len(s) && isLetterByte(s[j]) {
				j++
			}
			switch w := s[i:j]; w {
			case "true", "false":
				b.WriteString(stJSONBool.Render(w))
			case "null":
				b.WriteString(stJSONNull.Render(w))
			default:
				b.WriteString(w)
			}
			i = j
		case strings.IndexByte("{}[],:", c) >= 0:
			b.WriteString(stJSONSym.Render(string(c)))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func colorize(s string) string {
	if len(s) > highlightLimit {
		return s
	}
	return highlightJSON(s)
}

// prettyJSON indents valid JSON; invalid JSON is returned unchanged.
func prettyJSON(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return string(raw), false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw), false
	}
	return buf.String(), true
}

func isBinary(b []byte) bool {
	if len(b) > 4096 {
		b = b[:4096]
	}
	return bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b)
}

func hexDump(b []byte, truncated bool) []string {
	total := len(b)
	if len(b) > 512 {
		b = b[:512]
		truncated = true
	}
	out := []string{stAmber.Render(fmt.Sprintf("binary data, %s", fmtSize(int64(total))))}
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		var hex, asc strings.Builder
		for i := off; i < off+16; i++ {
			if i < end {
				fmt.Fprintf(&hex, "%02x ", b[i])
				if b[i] >= 32 && b[i] < 127 {
					asc.WriteByte(b[i])
				} else {
					asc.WriteByte('.')
				}
			} else {
				hex.WriteString("   ")
			}
		}
		out = append(out, stDim.Render(fmt.Sprintf("%08x  ", off))+stText.Render(hex.String())+stDim.Render("│")+stSubtle.Render(asc.String())+stDim.Render("│"))
	}
	if truncated {
		out = append(out, stDim.Render("…"))
	}
	return out
}

func eventColor(t string) string {
	switch t {
	case "ADDED":
		return cGreen
	case "MODIFIED":
		return cAmber
	case "DELETED":
		return cRed
	}
	return cMagenta
}

// formatStream formats a watch response: one JSON event per line.
func formatStream(body []byte) string {
	var sb strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(line, &ev)
		if ev.Type != "" {
			sb.WriteString(fg(eventColor(ev.Type)).Bold(true).Render("━━ " + ev.Type + " "))
			sb.WriteString("\n")
		}
		pretty, _ := prettyJSON(line)
		sb.WriteString(colorize(pretty))
		sb.WriteString("\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatBody renders a request or response body as wrapped, coloured lines.
func formatBody(body []byte, truncated, stream bool, width int) []string {
	if len(body) == 0 {
		return []string{stDim.Render("(empty)")}
	}
	if isBinary(body) {
		return hexDump(body, truncated)
	}
	var out string
	switch {
	case stream:
		out = formatStream(body)
	default:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			pretty, _ := prettyJSON(trimmed)
			out = colorize(pretty)
		} else {
			out = string(body)
		}
	}
	if len(body) > highlightLimit {
		out += "\n" + stDim.Render("(syntax highlighting is off for large bodies)")
	}
	if truncated {
		out += "\n" + stAmber.Render("… body truncated, only the first part was kept")
	}
	return strings.Split(ansi.Hardwrap(out, width, true), "\n")
}

func fmtSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
