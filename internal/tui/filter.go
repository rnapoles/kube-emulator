package tui

import (
	"strconv"
	"strings"

	"github.com/rnapoles/kube-emulator/internal/trace"
)

// term is one word of the search box. Words are AND-ed. A leading "-" negates.
// Supported keys: method, status, verb, res(ource), ns, name, path, body.
type term struct {
	neg      bool
	key, val string
}

func parseTerms(s string) []term {
	var out []term
	for _, f := range strings.Fields(s) {
		t := term{}
		if strings.HasPrefix(f, "-") && len(f) > 1 {
			t.neg = true
			f = f[1:]
		}
		if i := strings.IndexByte(f, ':'); i > 0 {
			switch k := strings.ToLower(f[:i]); k {
			case "method", "status", "verb", "res", "resource", "ns", "namespace", "name", "path", "body":
				t.key, t.val = k, strings.ToLower(f[i+1:])
				out = append(out, t)
				continue
			}
		}
		t.val = strings.ToLower(f)
		out = append(out, t)
	}
	return out
}

// statusMatches matches "404", "4xx", "4" or "pending".
func statusMatches(status int, val string) bool {
	s := "pending"
	if status != 0 {
		s = strconv.Itoa(status)
	}
	return strings.HasPrefix(s, strings.TrimRight(val, "x"))
}

func containsFold(b []byte, lower string) bool {
	return len(b) > 0 && strings.Contains(strings.ToLower(string(b)), lower)
}

func (t term) match(ex *trace.Exchange) bool {
	var ok bool
	switch t.key {
	case "method":
		ok = strings.EqualFold(ex.Method, t.val)
	case "status":
		ok = statusMatches(ex.Status, t.val)
	case "verb":
		ok = strings.Contains(ex.Verb, t.val)
	case "res", "resource":
		ok = strings.Contains(strings.ToLower(ex.Resource), t.val)
	case "ns", "namespace":
		ok = strings.Contains(strings.ToLower(ex.Namespace), t.val)
	case "name":
		ok = strings.Contains(strings.ToLower(ex.Name), t.val)
	case "path":
		ok = strings.Contains(strings.ToLower(ex.Path), t.val)
	case "body":
		ok = containsFold(ex.ReqBody, t.val) || containsFold(ex.RespBody, t.val)
	default:
		hay := strings.ToLower(ex.Method + " " + ex.Path + "?" + ex.RawQuery + " " + ex.Summary + " " + strconv.Itoa(ex.Status))
		ok = strings.Contains(hay, t.val)
	}
	if t.neg {
		return !ok
	}
	return ok
}
