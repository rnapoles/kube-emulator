// Package trace records the HTTP requests the emulator receives and the
// responses it sends (headers, bodies and watch streams) in a bounded ring
// buffer, so that the terminal UI can show them live.
package trace

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Exchange is one request and its response.
type Exchange struct {
	ID       int64
	Started  time.Time
	Duration time.Duration // set when Done
	Done     bool
	// Streaming is true for watch requests: the response stays open and its body keeps growing.
	Streaming bool

	Method, Path, RawQuery, Proto, Remote, Host string

	ReqHeader    http.Header
	ReqBody      []byte
	ReqTruncated bool
	ReqSize      int64

	Status        int // 0 until the response starts
	RespHeader    http.Header
	RespBody      []byte
	RespTruncated bool
	RespSize      int64

	// Derived from the URL, see classify.
	Verb, Resource, Namespace, Name, Subresource string
	Summary                                      string // for example "list pods · default"
	Noise                                        bool   // discovery, OpenAPI, health and version requests
}

// Recorder keeps the most recent exchanges.
type Recorder struct {
	mu       sync.Mutex
	max      int
	maxBody  int
	items    []*Exchange
	nextID   int64
	notify   chan struct{}
	received int64
}

// NewRecorder keeps up to maxItems exchanges and up to maxBody bytes of each body.
func NewRecorder(maxItems, maxBody int) *Recorder {
	if maxItems <= 0 {
		maxItems = 2000
	}
	if maxBody <= 0 {
		maxBody = 256 << 10
	}
	return &Recorder{max: maxItems, maxBody: maxBody, notify: make(chan struct{}, 1)}
}

// Changed signals (coalesced) that something was recorded or updated.
func (r *Recorder) Changed() <-chan struct{} { return r.notify }

func (r *Recorder) poke() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Snapshot returns copies of the recorded exchanges, oldest first. Bodies are
// shared (they only ever grow), everything else is copied.
func (r *Recorder) Snapshot() []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Exchange, len(r.items))
	for i, ex := range r.items {
		out[i] = *ex
	}
	return out
}

// Total is the number of requests seen since start (including dropped ones).
func (r *Recorder) Total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.received
}

// Clear forgets all recorded exchanges.
func (r *Recorder) Clear() {
	r.mu.Lock()
	r.items = nil
	r.mu.Unlock()
	r.poke()
}

func clip(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return append([]byte(nil), b...), false
	}
	return append([]byte(nil), b[:max]...), true
}

// Middleware records every exchange that passes through next.
func (r *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		if req.Body != nil && req.Body != http.NoBody {
			body, _ = io.ReadAll(io.LimitReader(req.Body, 32<<20))
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		ex := &Exchange{
			Started: time.Now(), Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery,
			Proto: req.Proto, Remote: req.RemoteAddr, Host: req.Host, ReqHeader: req.Header.Clone(),
			ReqSize: int64(len(body)),
		}
		ex.ReqBody, ex.ReqTruncated = clip(body, r.maxBody)
		classify(ex)

		r.mu.Lock()
		r.nextID++
		r.received++
		ex.ID = r.nextID
		r.items = append(r.items, ex)
		if len(r.items) > r.max {
			r.items = append([]*Exchange(nil), r.items[len(r.items)-r.max:]...)
		}
		r.mu.Unlock()
		r.poke()

		defer func() {
			r.mu.Lock()
			ex.Done = true
			ex.Duration = time.Since(ex.Started)
			if ex.Status == 0 {
				ex.Status = http.StatusOK
			}
			r.mu.Unlock()
			r.poke()
		}()
		next.ServeHTTP(&capture{ResponseWriter: w, r: r, ex: ex}, req)
	})
}

// capture is a ResponseWriter that copies what is written.
type capture struct {
	http.ResponseWriter
	r     *Recorder
	ex    *Exchange
	wrote bool
}

func (c *capture) WriteHeader(code int) {
	if !c.wrote {
		c.wrote = true
		c.r.mu.Lock()
		c.ex.Status = code
		c.ex.RespHeader = c.ResponseWriter.Header().Clone()
		c.r.mu.Unlock()
		c.r.poke()
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	n, err := c.ResponseWriter.Write(b)
	c.r.mu.Lock()
	c.ex.RespSize += int64(n)
	if room := c.r.maxBody - len(c.ex.RespBody); room > 0 {
		part := b[:n]
		if len(part) > room {
			part = part[:room]
			c.ex.RespTruncated = true
		}
		c.ex.RespBody = append(c.ex.RespBody, part...)
	} else if n > 0 {
		c.ex.RespTruncated = true
	}
	c.r.mu.Unlock()
	c.r.poke()
	return n, err
}

// Flush keeps watch streams working through the wrapper.
func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// classify derives verb, resource, namespace and a one-line summary from the URL.
func classify(ex *Exchange) {
	q, _ := url.ParseQuery(ex.RawQuery)
	if w := q.Get("watch"); w == "true" || w == "1" {
		ex.Streaming = true
	}
	p := strings.Trim(ex.Path, "/")
	noise := func(summary string) {
		ex.Summary, ex.Noise = summary, true
	}
	switch {
	case p == "":
		noise("index /")
		return
	case p == "version":
		noise("version")
		return
	case p == "healthz" || p == "livez" || p == "readyz" || strings.HasPrefix(p, "healthz/") ||
		strings.HasPrefix(p, "livez/") || strings.HasPrefix(p, "readyz/"):
		noise("health /" + p)
		return
	case strings.HasPrefix(p, "openapi"):
		noise("openapi /" + p)
		return
	case p == "api" || p == "apis":
		noise("discovery /" + p)
		return
	case !strings.HasPrefix(p, "api/") && !strings.HasPrefix(p, "apis/"):
		ex.Summary = ex.Method + " /" + p
		return
	}
	parts := strings.Split(p, "/")
	var rest []string
	if parts[0] == "api" {
		rest = parts[2:]
	} else {
		if len(parts) < 3 {
			noise("discovery /" + p)
			return
		}
		rest = parts[3:]
	}
	if len(rest) == 0 {
		noise("discovery /" + p)
		return
	}
	isNsSub := len(rest) == 3 && (rest[2] == "status" || rest[2] == "finalize")
	if rest[0] == "namespaces" && len(rest) >= 3 && !isNsSub {
		ex.Namespace, ex.Resource = rest[1], rest[2]
		if len(rest) > 3 {
			ex.Name = rest[3]
		}
		if len(rest) > 4 {
			ex.Subresource = rest[4]
		}
	} else {
		ex.Resource = rest[0]
		if len(rest) > 1 {
			ex.Name = rest[1]
		}
		if len(rest) > 2 {
			ex.Subresource = rest[2]
		}
	}
	switch ex.Method {
	case http.MethodGet:
		switch {
		case ex.Streaming:
			ex.Verb = "watch"
		case ex.Name == "":
			ex.Verb = "list"
		default:
			ex.Verb = "get"
		}
	case http.MethodPost:
		ex.Verb = "create"
		if ex.Subresource == "eviction" {
			ex.Verb = "evict"
		}
	case http.MethodPut:
		ex.Verb = "update"
	case http.MethodPatch:
		ex.Verb = "patch"
	case http.MethodDelete:
		ex.Verb = "delete"
		if ex.Name == "" {
			ex.Verb = "deletecollection"
		}
	default:
		ex.Verb = strings.ToLower(ex.Method)
	}
	s := ex.Verb + " " + ex.Resource
	if ex.Name != "" {
		s += "/" + ex.Name
	}
	if ex.Subresource != "" {
		s += "/" + ex.Subresource
	}
	if ex.Namespace != "" {
		s += " · " + ex.Namespace
	}
	ex.Summary = s
}
