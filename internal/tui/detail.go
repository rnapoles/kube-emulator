package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/rnapoles/kube-emulator/internal/trace"
)

const (
	tabOverview = iota
	tabRequest
	tabResponse
	numTabs
)

var tabNames = [numTabs]string{"Overview", "Request", "Response"}

func section(title string) string {
	return "\n" + fg(cViolet).Bold(true).Render("▌ "+title)
}

func kv(label, value string) string {
	return "  " + stDim.Render(padRight(label, 15)) + value
}

func urlLine(ex *trace.Exchange) string {
	s := badge(ex.Method, methodColor(ex.Method)) + " " + stBold.Render(ex.Path)
	if ex.RawQuery != "" {
		s += stDim.Render("?") + stSubtle.Render(ex.RawQuery)
	}
	return s
}

func statusBadge(ex *trace.Exchange) string {
	if ex.Status == 0 {
		return badge("waiting…", cDim)
	}
	return badge(fmt.Sprintf("%d %s", ex.Status, http.StatusText(ex.Status)), statusColor(ex.Status))
}

func durText(ex *trace.Exchange) string {
	switch {
	case ex.Done:
		return fmtDur(ex.Duration)
	case ex.Streaming:
		return fg(cMagenta).Render("● streaming ") + fmtDur(time.Since(ex.Started))
	}
	return "… " + fmtDur(time.Since(ex.Started))
}

func fmtHeaders(h http.Header) string {
	if len(h) == 0 {
		return "  " + stDim.Render("(none)")
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("  " + stCyan.Render(k) + stDim.Render(": ") + stText.Render(strings.Join(h[k], ", ")))
	}
	return sb.String()
}

// resultSummary highlights what the API returned: kind, item counts, Status errors.
func resultSummary(ex *trace.Exchange) []string {
	if ex.Streaming || len(ex.RespBody) == 0 || ex.RespTruncated {
		return nil
	}
	var top map[string]interface{}
	if json.Unmarshal(ex.RespBody, &top) != nil {
		return nil
	}
	str := func(m map[string]interface{}, k string) string { s, _ := m[k].(string); return s }
	var out []string
	if k := str(top, "kind"); k != "" {
		out = append(out, kv("kind", stBold.Render(k)+stDim.Render("  "+str(top, "apiVersion"))))
	}
	if items, ok := top["items"].([]interface{}); ok {
		out = append(out, kv("items", stCyan.Render(strconv.Itoa(len(items)))))
	}
	if rows, ok := top["rows"].([]interface{}); ok {
		var cols []string
		if defs, ok := top["columnDefinitions"].([]interface{}); ok {
			for _, d := range defs {
				if dm, ok := d.(map[string]interface{}); ok {
					cols = append(cols, str(dm, "name"))
				}
			}
		}
		out = append(out, kv("table rows", stCyan.Render(strconv.Itoa(len(rows)))+stDim.Render("  "+strings.Join(cols, " · "))))
	}
	if md, ok := top["metadata"].(map[string]interface{}); ok {
		if n := str(md, "name"); n != "" {
			out = append(out, kv("name", stText.Render(n)))
		}
		if n := str(md, "namespace"); n != "" {
			out = append(out, kv("namespace", stText.Render(n)))
		}
		if n := str(md, "resourceVersion"); n != "" {
			out = append(out, kv("resourceVersion", stSubtle.Render(n)))
		}
	}
	if str(top, "kind") == "Status" {
		col := cGreen
		if str(top, "status") != "Success" {
			col = cRed
		}
		out = append(out, kv("status", fg(col).Bold(true).Render(str(top, "status"))))
		if r := str(top, "reason"); r != "" {
			out = append(out, kv("reason", fg(col).Render(r)))
		}
		if m := str(top, "message"); m != "" {
			out = append(out, kv("message", fg(col).Render(m)))
		}
	}
	return out
}

func watchSummary(ex *trace.Exchange) string {
	counts := map[string]int{}
	for _, line := range bytes.Split(ex.RespBody, []byte("\n")) {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &ev) == nil && ev.Type != "" {
			counts[ev.Type]++
		}
	}
	if len(counts) == 0 {
		return stDim.Render("no events yet")
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)
	parts := make([]string, len(types))
	for i, t := range types {
		parts[i] = fg(eventColor(t)).Render(fmt.Sprintf("%s %d", t, counts[t]))
	}
	return strings.Join(parts, stDim.Render("  ·  "))
}

func overview(ex *trace.Exchange) string {
	var sb strings.Builder
	add := func(s string) { sb.WriteString(s + "\n") }
	add(urlLine(ex))
	add("")
	add(statusBadge(ex) + "  " + stDim.Render("⏱ ") + stText.Render(durText(ex)) +
		"  " + stDim.Render("↑ ") + stText.Render(fmtSize(ex.ReqSize)) +
		"  " + stDim.Render("↓ ") + stText.Render(fmtSize(ex.RespSize)) +
		"  " + stDim.Render(ex.Started.Format("15:04:05.000")))

	add(section("KUBERNETES"))
	if ex.Resource != "" {
		add(kv("verb", fg(methodColor(ex.Method)).Bold(true).Render(ex.Verb)))
		add(kv("resource", stText.Render(ex.Resource)))
		if ex.Namespace != "" {
			add(kv("namespace", stText.Render(ex.Namespace)))
		}
		if ex.Name != "" {
			add(kv("name", stText.Render(ex.Name)))
		}
		if ex.Subresource != "" {
			add(kv("subresource", stText.Render(ex.Subresource)))
		}
	} else {
		add(kv("request", stText.Render(ex.Summary)))
	}

	if res := resultSummary(ex); len(res) > 0 {
		add(section("RESULT"))
		for _, l := range res {
			add(l)
		}
	}
	if ex.Streaming {
		add(section("WATCH"))
		add(kv("events", watchSummary(ex)))
	}

	if q, err := url.ParseQuery(ex.RawQuery); err == nil && len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		add(section("QUERY"))
		for _, k := range keys {
			add(kv(k, stText.Render(strings.Join(q[k], ","))))
		}
	}

	add(section("CLIENT"))
	add(kv("remote", stText.Render(ex.Remote)))
	for _, h := range []string{"User-Agent", "Accept", "Content-Type"} {
		if v := ex.ReqHeader.Get(h); v != "" {
			add(kv(strings.ToLower(h), stSubtle.Render(v)))
		}
	}
	if v := ex.RespHeader.Get("Content-Type"); v != "" {
		add(kv("responded", stSubtle.Render(v)))
	}
	add("")
	add(stDim.Render("  2  request details    3  response details    y  copy as curl"))
	return sb.String()
}

// detailLines renders one tab for an exchange, wrapped to width.
func detailLines(ex *trace.Exchange, tab, width int) []string {
	wrap := func(s string) []string { return strings.Split(ansi.Hardwrap(s, width, true), "\n") }
	switch tab {
	case tabRequest:
		var sb strings.Builder
		sb.WriteString(urlLine(ex) + "\n")
		sb.WriteString(stDim.Render("  "+ex.Proto+" · from "+ex.Remote+" · "+ex.Host) + "\n")
		sb.WriteString(section("HEADERS") + "\n")
		sb.WriteString(fmtHeaders(ex.ReqHeader) + "\n")
		sb.WriteString(section("BODY  " + stDim.Render(fmtSize(ex.ReqSize))))
		return append(wrap(sb.String()+"\n"), formatBody(ex.ReqBody, ex.ReqTruncated, false, width)...)
	case tabResponse:
		var sb strings.Builder
		sb.WriteString(statusBadge(ex) + "  " + stDim.Render("⏱ ") + stText.Render(durText(ex)) + "\n")
		sb.WriteString(section("HEADERS") + "\n")
		sb.WriteString(fmtHeaders(ex.RespHeader) + "\n")
		title := "BODY  " + stDim.Render(fmtSize(ex.RespSize))
		if ex.Streaming && !ex.Done {
			title += "  " + fg(cMagenta).Render("● live")
		}
		sb.WriteString(section(title))
		return append(wrap(sb.String()+"\n"), formatBody(ex.RespBody, ex.RespTruncated, ex.Streaming, width)...)
	}
	return wrap(overview(ex))
}
