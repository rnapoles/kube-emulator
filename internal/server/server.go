package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/openapi"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

// Server is the emulated kube-apiserver.
type Server struct {
	store   *Store
	reg     *Registry
	strict  serializer.CodecFactory
	lenient serializer.CodecFactory
	ipam    *objects.IPAM
	verbose bool
	oapi    *openapi.Spec // optional swagger.json definitions (kubectl explain)
}

// SetOpenAPI makes the definitions of a swagger.json available to kubectl.
func (s *Server) SetOpenAPI(sp *openapi.Spec) { s.oapi = sp }

// New creates the emulated API server.
func New(st *store.Store, ipam *objects.IPAM, verbose bool) *Server {
	s := &Server{
		store:   st,
		reg:     registry.New(),
		strict:  serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict),
		lenient: serializer.NewCodecFactory(scheme.Scheme),
		ipam:    ipam,
		verbose: verbose,
	}
	// Re-register CRDs restored from persistent storage.
	crds, _ := st.List(kCRDs, "")
	for _, c := range crds {
		_ = s.reg.RegisterCRD(c)
	}
	return s
}

// ---------------------------------------------------------------- responses

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, err error) {
	var st metav1.Status
	if se, ok := err.(apierrors.APIStatus); ok {
		st = se.Status()
	} else {
		st = apierrors.NewInternalError(err).Status()
	}
	st.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	if st.Code == 0 {
		st.Code = http.StatusInternalServerError
	}
	writeJSON(w, int(st.Code), &st)
}

func errNotFoundPath() error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound,
		Message: "the server could not find the requested resource",
	}}
}

func errUnsupportedMedia(ct string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 415, Reason: metav1.StatusReasonUnsupportedMediaType,
		Message: fmt.Sprintf("the body of the request was in an unknown format - accepted media types include: application/json-patch+json, application/merge-patch+json, application/strategic-merge-patch+json, application/apply-patch+yaml (got %q)", ct),
	}}
}

func errNotImplemented(msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 501, Reason: metav1.StatusReason("NotImplemented"), Message: msg,
	}}
}

func successStatus(res *Resource, obj *U) *metav1.Status {
	return &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusSuccess,
		Details:  &metav1.StatusDetails{Name: obj.GetName(), Group: res.Group, Kind: res.Name, UID: obj.GetUID()},
	}
}

// ---------------------------------------------------------------- routing

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.verbose {
		// DumpRequest(true) reads the body and puts an identical copy back,
		// so the handlers below still see it.
		if dump, err := httputil.DumpRequest(r, true); err != nil {
			log.Printf("dumping request: %v", err)
		} else {
			log.Printf("---- request from %s ----\n%s\n---- end request ----", r.RemoteAddr, dump)
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
			writeError(w, apierrors.NewInternalError(fmt.Errorf("%v", rec)))
		}
		if s.verbose {
			log.Printf("%s %s (%s)", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Microsecond))
		}
	}()
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case p == "":
		writeJSON(w, 200, map[string]interface{}{"paths": s.paths()})
	case p == "healthz" || p == "livez" || p == "readyz" ||
		strings.HasPrefix(p, "healthz/") || strings.HasPrefix(p, "livez/") || strings.HasPrefix(p, "readyz/"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	case p == "version":
		writeJSON(w, 200, &version.Info{
			Major: "1", Minor: "31", GitVersion: registry.ServerVersion, GitCommit: "emulator", GitTreeState: "clean",
			BuildDate: "1970-01-01T00:00:00Z", GoVersion: runtime.Version(), Compiler: runtime.Compiler,
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
		})
	case p == "openapi/v2":
		s.openAPIV2(w, r)
	case p == "openapi/v3":
		writeJSON(w, 200, s.reg.OpenAPIV3Index())
	case strings.HasPrefix(p, "openapi/v3/"):
		if doc := openapi.V3Doc(s.oapi, s.reg, strings.TrimPrefix(p, "openapi/v3/")); doc != nil {
			writeJSON(w, 200, doc)
		} else {
			writeError(w, errNotFoundPath())
		}
	case p == "api" || p == "apis" || strings.HasPrefix(p, "api/") || strings.HasPrefix(p, "apis/"):
		s.serveAPI(w, r, p)
	default:
		writeError(w, errNotFoundPath())
	}
}

func (s *Server) paths() []string {
	out := []string{"/api", "/api/v1", "/apis", "/healthz", "/livez", "/openapi/v2", "/openapi/v3", "/readyz", "/version"}
	for _, g := range s.reg.Groups() {
		out = append(out, "/apis/"+g)
	}
	return out
}

func (s *Server) openAPIV2(w http.ResponseWriter, r *http.Request) {
	// NOTE: the Accept header uses "@v1.0", but a response Content-Type must not
	// contain "@" (client-go would fail with "mime: unexpected content after media
	// subtype"), so the response uses "." like the real API server.
	if s.oapi != nil {
		if strings.Contains(r.Header.Get("Accept"), "protobuf") {
			w.Header().Set("Content-Type", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
			_, _ = w.Write(s.oapi.Protobuf())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.oapi.JSON())
		return
	}
	doc := []byte(`{"swagger":"2.0","info":{"title":"Kubernetes emulator","version":"` + registry.ServerVersion + `"},"paths":{},"definitions":{}}`)
	if strings.Contains(r.Header.Get("Accept"), "protobuf") {
		d, err := openapi_v2.ParseDocument(doc)
		if err == nil {
			if b, err := proto.Marshal(d); err == nil {
				w.Header().Set("Content-Type", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf")
				_, _ = w.Write(b)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}

type reqCtx struct {
	w    http.ResponseWriter
	r    *http.Request
	q    url.Values
	res  *Resource
	ns   string
	name string
	sub  string
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, p string) {
	parts := strings.Split(p, "/")
	var group, ver string
	var rest []string
	if parts[0] == "api" {
		if len(parts) == 1 {
			writeJSON(w, 200, &metav1.APIVersions{
				TypeMeta: metav1.TypeMeta{Kind: "APIVersions"},
				Versions: []string{"v1"},
				ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{
					{ClientCIDR: "0.0.0.0/0", ServerAddress: r.Host},
				},
			})
			return
		}
		ver, rest = parts[1], parts[2:]
	} else {
		if len(parts) == 1 {
			writeJSON(w, 200, s.reg.GroupList())
			return
		}
		group = parts[1]
		if len(parts) == 2 {
			if g := s.reg.Group(group); g != nil {
				writeJSON(w, 200, g)
			} else {
				writeError(w, errNotFoundPath())
			}
			return
		}
		ver, rest = parts[2], parts[3:]
	}
	if len(rest) == 0 {
		if rl := s.reg.ResourceList(group, ver); rl != nil {
			writeJSON(w, 200, rl)
		} else {
			writeError(w, errNotFoundPath())
		}
		return
	}

	c := &reqCtx{w: w, r: r, q: r.URL.Query()}
	var resName string
	nsPath := false
	isNsSub := len(rest) == 3 && (rest[2] == "status" || rest[2] == "finalize")
	if rest[0] == "namespaces" && len(rest) >= 3 && !isNsSub {
		nsPath = true
		c.ns, resName = rest[1], rest[2]
		if len(rest) > 3 {
			c.name = rest[3]
		}
		if len(rest) > 4 {
			c.sub = rest[4]
		}
	} else {
		resName = rest[0]
		if len(rest) > 1 {
			c.name = rest[1]
		}
		if len(rest) > 2 {
			c.sub = rest[2]
		}
	}
	if len(rest) > 5 {
		writeError(w, errNotFoundPath())
		return
	}
	c.res = s.reg.Lookup(group, ver, resName)
	if c.res == nil || (nsPath && !c.res.Namespaced) || (!nsPath && c.res.Namespaced && c.name != "") {
		writeError(w, errNotFoundPath())
		return
	}
	s.handle(c)
}

func subAllowed(res *Resource, method, sub string) bool {
	switch sub {
	case "status":
		return res.HasStatus
	case "scale":
		return res.HasScale
	case "log":
		return res.Name == "pods" && method == http.MethodGet
	}
	return false
}

func (s *Server) handle(c *reqCtx) {
	if c.sub != "" {
		switch c.sub {
		case "exec", "attach", "portforward", "proxy":
			writeError(c.w, errNotImplemented(fmt.Sprintf("%s/%s is not supported: the emulator does not run real containers", c.res.Name, c.sub)))
			return
		}
		if !subAllowed(c.res, c.r.Method, c.sub) {
			writeError(c.w, errNotFoundPath())
			return
		}
	}
	switch c.r.Method {
	case http.MethodGet:
		switch {
		case c.sub != "":
			s.getSub(c)
		case c.name == "":
			if w := c.q.Get("watch"); w == "true" || w == "1" {
				s.watch(c)
			} else {
				s.list(c)
			}
		default:
			s.get(c)
		}
	case http.MethodPost:
		if c.name != "" || c.sub != "" {
			writeError(c.w, apierrors.NewMethodNotSupported(c.res.Key().GR(), "post"))
			return
		}
		s.create(c)
	case http.MethodPut:
		if c.name == "" {
			writeError(c.w, apierrors.NewMethodNotSupported(c.res.Key().GR(), "put"))
			return
		}
		s.update(c)
	case http.MethodPatch:
		if c.name == "" {
			writeError(c.w, apierrors.NewMethodNotSupported(c.res.Key().GR(), "patch"))
			return
		}
		s.patch(c)
	case http.MethodDelete:
		if c.sub != "" {
			writeError(c.w, apierrors.NewMethodNotSupported(c.res.Key().GR(), "delete"))
			return
		}
		if c.name == "" {
			s.deleteCollection(c)
		} else {
			s.delete(c)
		}
	default:
		writeError(c.w, apierrors.NewMethodNotSupported(c.res.Key().GR(), strings.ToLower(c.r.Method)))
	}
}

// ---------------------------------------------------------------- selectors

type selectors struct {
	ls labels.Selector
	fs fields.Selector
}

func parseSelectors(q url.Values) (*selectors, error) {
	ls, err := labels.Parse(q.Get("labelSelector"))
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("unable to parse labelSelector: %v", err))
	}
	fs, err := fields.ParseSelector(q.Get("fieldSelector"))
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("unable to parse fieldSelector: %v", err))
	}
	return &selectors{ls: ls, fs: fs}, nil
}

func (s *selectors) matches(u *U) bool {
	if !s.ls.Empty() && !s.ls.Matches(labels.Set(u.GetLabels())) {
		return false
	}
	if !s.fs.Empty() {
		set := fields.Set{}
		for _, r := range s.fs.Requirements() {
			v, _, _ := unstructured.NestedString(u.Object, strings.Split(r.Field, ".")...)
			set[r.Field] = v
		}
		if !s.fs.Matches(set) {
			return false
		}
	}
	return true
}

func dryRun(q url.Values) bool { return q.Get("dryRun") == "All" }

func wantsTable(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "as=Table")
}

func readBody(c *reqCtx) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, 8<<20))
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	return b, nil
}

func listObject(res *Resource, items []*U, rv string) map[string]interface{} {
	arr := make([]interface{}, len(items))
	for i, it := range items {
		arr[i] = it.Object
	}
	return map[string]interface{}{
		"kind":       res.Kind + "List",
		"apiVersion": res.APIVersion(),
		"metadata":   map[string]interface{}{"resourceVersion": rv},
		"items":      arr,
	}
}

// ---------------------------------------------------------------- reads

func (s *Server) list(c *reqCtx) {
	sel, err := parseSelectors(c.q)
	if err != nil {
		writeError(c.w, err)
		return
	}
	items, rv := s.store.List(c.res.Key(), c.ns)
	out := items[:0]
	for _, it := range items {
		if sel.matches(it) {
			it.SetAPIVersion(c.res.APIVersion())
			out = append(out, it)
		}
	}
	rvs := strconv.FormatUint(rv, 10)
	if wantsTable(c.r) {
		writeJSON(c.w, 200, s.table(c.res, out, rvs))
		return
	}
	writeJSON(c.w, 200, listObject(c.res, out, rvs))
}

func (s *Server) get(c *reqCtx) {
	obj, err := s.store.Get(c.res.Key(), c.ns, c.name)
	if err != nil {
		writeError(c.w, err)
		return
	}
	obj.SetAPIVersion(c.res.APIVersion())
	if wantsTable(c.r) {
		writeJSON(c.w, 200, s.table(c.res, []*U{obj}, obj.GetResourceVersion()))
		return
	}
	writeJSON(c.w, 200, obj)
}

func scaleFor(u *U) map[string]interface{} {
	replicas, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if !found {
		replicas = 1
	}
	status, _, _ := unstructured.NestedInt64(u.Object, "status", "replicas")
	selStr := ""
	if ml, ok, _ := unstructured.NestedMap(u.Object, "spec", "selector", "matchLabels"); ok {
		set := labels.Set{}
		for k, v := range ml {
			if sv, ok := v.(string); ok {
				set[k] = sv
			}
		}
		selStr = labels.SelectorFromSet(set).String()
	}
	if selStr == "" { // ReplicationController: spec.selector is a plain label map
		if m, ok, _ := unstructured.NestedStringMap(u.Object, "spec", "selector"); ok {
			selStr = labels.SelectorFromSet(labels.Set(m)).String()
		}
	}
	return map[string]interface{}{
		"kind":       "Scale",
		"apiVersion": "autoscaling/v1",
		"metadata": map[string]interface{}{
			"name": u.GetName(), "namespace": u.GetNamespace(), "uid": string(u.GetUID()),
			"resourceVersion":   u.GetResourceVersion(),
			"creationTimestamp": u.GetCreationTimestamp().UTC().Format(time.RFC3339),
		},
		"spec":   map[string]interface{}{"replicas": replicas},
		"status": map[string]interface{}{"replicas": status, "selector": selStr},
	}
}

func (s *Server) getSub(c *reqCtx) {
	obj, err := s.store.Get(c.res.Key(), c.ns, c.name)
	if err != nil {
		writeError(c.w, err)
		return
	}
	obj.SetAPIVersion(c.res.APIVersion())
	switch c.sub {
	case "scale":
		writeJSON(c.w, 200, scaleFor(obj))
	case "log":
		c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(c.w, "emulator: pod %s/%s has no real container, so there are no logs to show\n", c.ns, c.name)
	default:
		writeJSON(c.w, 200, obj)
	}
}

func (s *Server) watch(c *reqCtx) {
	sel, err := parseSelectors(c.q)
	if err != nil {
		writeError(c.w, err)
		return
	}
	wt, err := s.store.Watch(c.res.Key(), c.ns, c.q.Get("resourceVersion"))
	if err != nil {
		writeError(c.w, err)
		return
	}
	defer wt.Stop()
	flusher, ok := c.w.(http.Flusher)
	if !ok {
		writeError(c.w, apierrors.NewInternalError(fmt.Errorf("streaming is not supported")))
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	flusher.Flush()
	enc := json.NewEncoder(c.w)
	table := wantsTable(c.r)
	send := func(ev Event) bool {
		if !sel.matches(ev.Obj) {
			return true
		}
		obj := ev.Obj.DeepCopy()
		obj.SetAPIVersion(c.res.APIVersion())
		var payload interface{} = obj.Object
		if table {
			payload = s.table(c.res, []*U{obj}, obj.GetResourceVersion())
		}
		if err := enc.Encode(map[string]interface{}{"type": string(ev.Type), "object": payload}); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, ev := range wt.Initial {
		if !send(ev) {
			return
		}
	}
	timeout := 30 * time.Minute
	if t, err := strconv.Atoi(c.q.Get("timeoutSeconds")); err == nil && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-wt.C:
			if !ok || !send(ev) {
				return
			}
		case <-timer.C:
			return
		case <-c.r.Context().Done():
			return
		}
	}
}

// ---------------------------------------------------------------- writes

func (s *Server) ensureNamespace(ns string) error {
	_, err := s.store.Get(kNamespaces, "", ns)
	return err
}

func (s *Server) createFromBytes(c *reqCtx, body []byte) (*U, error) {
	if c.res.Namespaced {
		if c.ns == "" {
			return nil, apierrors.NewBadRequest("namespace is required to create " + c.res.Name)
		}
		if err := s.ensureNamespace(c.ns); err != nil {
			return nil, err
		}
	}
	in, err := s.decode(body, c.res, c.q.Get("fieldValidation"))
	if err != nil {
		return nil, err
	}
	u, err := s.admitCreate(c.res, c.ns, in)
	if err != nil {
		return nil, err
	}
	if dryRun(c.q) {
		if _, err := s.store.Get(c.res.Key(), u.GetNamespace(), u.GetName()); err == nil {
			return nil, apierrors.NewAlreadyExists(c.res.Key().GR(), u.GetName())
		}
		return u, nil
	}
	created, err := s.store.Create(c.res.Key(), u)
	if err != nil {
		return nil, err
	}
	s.afterWrite(c.res, created, false)
	return created, nil
}

func (s *Server) create(c *reqCtx) {
	body, err := readBody(c)
	if err != nil {
		writeError(c.w, err)
		return
	}
	created, err := s.createFromBytes(c, body)
	if err != nil {
		writeError(c.w, err)
		return
	}
	created.SetAPIVersion(c.res.APIVersion())
	writeJSON(c.w, http.StatusCreated, created)
}

// afterWrite keeps the registry in sync with CustomResourceDefinitions.
func (s *Server) afterWrite(res *Resource, obj *U, deleted bool) {
	if res.Key() != kCRDs {
		return
	}
	group, _, _ := unstructured.NestedString(obj.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "plural")
	if deleted {
		s.reg.UnregisterCRD(group, plural)
		s.store.DeleteAll(ResourceKey{Group: group, Resource: plural})
		return
	}
	_ = s.reg.RegisterCRD(obj)
}

func (s *Server) update(c *reqCtx) {
	body, err := readBody(c)
	if err != nil {
		writeError(c.w, err)
		return
	}
	if c.sub == "scale" {
		var sc struct {
			Spec struct {
				Replicas *int64 `json:"replicas"`
			} `json:"spec"`
		}
		jb, err := yaml.YAMLToJSON(body)
		if err == nil {
			err = json.Unmarshal(jb, &sc)
		}
		if err != nil || sc.Spec.Replicas == nil {
			writeError(c.w, apierrors.NewBadRequest("invalid Scale object: spec.replicas is required"))
			return
		}
		s.applyScale(c, *sc.Spec.Replicas)
		return
	}
	in, err := s.decode(body, c.res, c.q.Get("fieldValidation"))
	if err != nil {
		writeError(c.w, err)
		return
	}
	updated, err := s.store.Mutate(c.res.Key(), c.ns, c.name, func(cur *U) (*U, error) {
		return s.admitUpdate(c.res, c.ns, c.name, c.sub, in, cur)
	}, dryRun(c.q))
	if err != nil {
		writeError(c.w, err)
		return
	}
	s.afterWrite(c.res, updated, false)
	updated.SetAPIVersion(c.res.APIVersion())
	writeJSON(c.w, http.StatusOK, updated)
}

func (s *Server) applyScale(c *reqCtx, replicas int64) {
	if replicas < 0 {
		writeError(c.w, apierrors.NewBadRequest("spec.replicas must be greater than or equal to 0"))
		return
	}
	updated, err := s.store.Mutate(c.res.Key(), c.ns, c.name, func(cur *U) (*U, error) {
		if old, found, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas"); found && old == replicas {
			return nil, nil
		}
		if err := unstructured.SetNestedField(cur.Object, replicas, "spec", "replicas"); err != nil {
			return nil, apierrors.NewInternalError(err)
		}
		cur.SetGeneration(cur.GetGeneration() + 1)
		return cur, nil
	}, dryRun(c.q))
	if err != nil {
		writeError(c.w, err)
		return
	}
	writeJSON(c.w, http.StatusOK, scaleFor(updated))
}

func patchBytes(ct string, cur, patch []byte, typed k8sruntime.Object) ([]byte, error) {
	switch ct {
	case "application/json-patch+json":
		p, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		out, err := p.Apply(cur)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		return out, nil
	case "application/merge-patch+json":
		out, err := jsonpatch.MergePatch(cur, patch)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		return out, nil
	case "application/strategic-merge-patch+json":
		if typed == nil { // used for Scale
			out, err := jsonpatch.MergePatch(cur, patch)
			if err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
			return out, nil
		}
		out, err := strategicpatch.StrategicMergePatch(cur, patch, typed)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		return out, nil
	}
	return nil, errUnsupportedMedia(ct)
}

func (s *Server) patch(c *reqCtx) {
	body, err := readBody(c)
	if err != nil {
		writeError(c.w, err)
		return
	}
	ct, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	fv := c.q.Get("fieldValidation")

	if c.sub == "scale" {
		cur, err := s.store.Get(c.res.Key(), c.ns, c.name)
		if err != nil {
			writeError(c.w, err)
			return
		}
		b, _ := json.Marshal(scaleFor(cur))
		out, err := patchBytes(ct, b, body, nil)
		if err != nil {
			writeError(c.w, err)
			return
		}
		var sc struct {
			Spec struct {
				Replicas *int64 `json:"replicas"`
			} `json:"spec"`
		}
		if json.Unmarshal(out, &sc) != nil || sc.Spec.Replicas == nil {
			writeError(c.w, apierrors.NewBadRequest("invalid Scale patch"))
			return
		}
		s.applyScale(c, *sc.Spec.Replicas)
		return
	}

	if ct == "application/apply-patch+yaml" {
		s.serverSideApply(c, body)
		return
	}
	if ct == "application/strategic-merge-patch+json" && !c.res.Typed {
		writeError(c.w, errUnsupportedMedia(ct))
		return
	}
	updated, err := s.store.Mutate(c.res.Key(), c.ns, c.name, func(cur *U) (*U, error) {
		curJSON, err := cur.MarshalJSON()
		if err != nil {
			return nil, apierrors.NewInternalError(err)
		}
		patched, err := patchBytes(ct, curJSON, body, s.newObject(c.res))
		if err != nil {
			return nil, err
		}
		in, err := s.decode(patched, c.res, fv)
		if err != nil {
			return nil, err
		}
		return s.admitUpdate(c.res, c.ns, c.name, c.sub, in, cur)
	}, dryRun(c.q))
	if err != nil {
		writeError(c.w, err)
		return
	}
	s.afterWrite(c.res, updated, false)
	updated.SetAPIVersion(c.res.APIVersion())
	writeJSON(c.w, http.StatusOK, updated)
}

// serverSideApply is an approximation of server-side apply: create when
// missing, otherwise merge the applied configuration over the live object
// (strategic merge for built-ins, JSON merge for custom resources). Field
// ownership (managedFields) is not tracked.
func (s *Server) serverSideApply(c *reqCtx, body []byte) {
	jsonBody, err := yaml.YAMLToJSON(body)
	if err != nil {
		writeError(c.w, apierrors.NewBadRequest(err.Error()))
		return
	}
	nameFix := []byte(fmt.Sprintf(`{"metadata":{"name":%q}}`, c.name))
	if merged, err := jsonpatch.MergePatch(jsonBody, nameFix); err == nil {
		jsonBody = merged
	}
	fv := c.q.Get("fieldValidation")
	if _, err := s.store.Get(c.res.Key(), c.ns, c.name); apierrors.IsNotFound(err) {
		created, err := s.createFromBytes(c, jsonBody)
		if err != nil {
			writeError(c.w, err)
			return
		}
		created.SetAPIVersion(c.res.APIVersion())
		writeJSON(c.w, http.StatusCreated, created)
		return
	}
	updated, err := s.store.Mutate(c.res.Key(), c.ns, c.name, func(cur *U) (*U, error) {
		curJSON, err := cur.MarshalJSON()
		if err != nil {
			return nil, apierrors.NewInternalError(err)
		}
		ct := "application/merge-patch+json"
		if c.res.Typed {
			ct = "application/strategic-merge-patch+json"
		}
		patched, err := patchBytes(ct, curJSON, jsonBody, s.newObject(c.res))
		if err != nil {
			return nil, err
		}
		in, err := s.decode(patched, c.res, fv)
		if err != nil {
			return nil, err
		}
		return s.admitUpdate(c.res, c.ns, c.name, "", in, cur)
	}, dryRun(c.q))
	if err != nil {
		writeError(c.w, err)
		return
	}
	s.afterWrite(c.res, updated, false)
	updated.SetAPIVersion(c.res.APIVersion())
	writeJSON(c.w, http.StatusOK, updated)
}

func (s *Server) delete(c *reqCtx) {
	if dryRun(c.q) {
		obj, err := s.store.Get(c.res.Key(), c.ns, c.name)
		if err != nil {
			writeError(c.w, err)
			return
		}
		writeJSON(c.w, http.StatusOK, successStatus(c.res, obj))
		return
	}
	obj, gone, err := s.store.Delete(c.res.Key(), c.ns, c.name)
	if err != nil {
		writeError(c.w, err)
		return
	}
	if !gone { // finalizers pending: return the object with deletionTimestamp
		obj.SetAPIVersion(c.res.APIVersion())
		writeJSON(c.w, http.StatusOK, obj)
		return
	}
	s.afterWrite(c.res, obj, true)
	writeJSON(c.w, http.StatusOK, successStatus(c.res, obj))
}

func (s *Server) deleteCollection(c *reqCtx) {
	sel, err := parseSelectors(c.q)
	if err != nil {
		writeError(c.w, err)
		return
	}
	items, rv := s.store.List(c.res.Key(), c.ns)
	var deleted []*U
	for _, it := range items {
		if !sel.matches(it) {
			continue
		}
		if !dryRun(c.q) {
			if _, gone, err := s.store.Delete(c.res.Key(), it.GetNamespace(), it.GetName()); err != nil {
				continue
			} else if gone {
				s.afterWrite(c.res, it, true)
			}
		}
		it.SetAPIVersion(c.res.APIVersion())
		deleted = append(deleted, it)
	}
	writeJSON(c.w, http.StatusOK, listObject(c.res, deleted, strconv.FormatUint(rv, 10)))
}
