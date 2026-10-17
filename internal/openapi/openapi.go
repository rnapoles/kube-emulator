// Package openapi serves the schemas `kubectl explain` (and kubectl's client
// side validation) need.
//
// Built-in kinds get their schemas from a swagger.json file you provide (the
// `definitions` section of an OpenAPI v2 document, for example the output of
// `kubectl get --raw /openapi/v2` on a real cluster, or api/openapi-spec/swagger.json
// from the kubernetes repository). Custom kinds get theirs from the
// openAPIV3Schema of their CustomResourceDefinition. Everything is served as
// OpenAPI v2 (the raw file) and as per group/version OpenAPI v3 documents that
// are derived from the same definitions.
package openapi

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rnapoles/kube-emulator/internal/registry"
)

const (
	defsPrefix = "#/definitions/"
	compPrefix = "#/components/schemas/"
	metaDef    = "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"
)

// Spec holds a parsed swagger.json.
type Spec struct {
	raw   []byte
	pb    []byte
	defs  map[string]interface{}
	byGVK map[schema.GroupVersionKind]string
}

// Load reads and indexes a swagger.json (OpenAPI v2) file.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	defs, _ := doc["definitions"].(map[string]interface{})
	if len(defs) == 0 {
		return nil, fmt.Errorf("%s has no \"definitions\": expected an OpenAPI v2 (swagger) document", path)
	}
	sp := &Spec{raw: raw, defs: defs, byGVK: map[schema.GroupVersionKind]string{}}
	for name, d := range defs {
		dm, _ := d.(map[string]interface{})
		list, _ := dm["x-kubernetes-group-version-kind"].([]interface{})
		for _, e := range list {
			m, _ := e.(map[string]interface{})
			g, _ := m["group"].(string)
			v, _ := m["version"].(string)
			k, _ := m["kind"].(string)
			if v != "" && k != "" {
				sp.byGVK[schema.GroupVersionKind{Group: g, Version: v, Kind: k}] = name
			}
		}
	}
	// kubectl asks for the protobuf encoding; convert once.
	d, err := openapi_v2.ParseDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s as OpenAPI v2: %w", path, err)
	}
	if sp.pb, err = proto.Marshal(d); err != nil {
		return nil, err
	}
	return sp, nil
}

// NumDefinitions reports how many schemas were loaded.
func (s *Spec) NumDefinitions() int { return len(s.defs) }

// JSON is the OpenAPI v2 document as loaded.
func (s *Spec) JSON() []byte { return s.raw }

// Protobuf is the OpenAPI v2 document in the encoding kubectl requests.
func (s *Spec) Protobuf() []byte { return s.pb }

func (s *Spec) definitionFor(gvk schema.GroupVersionKind) (string, bool) {
	if s == nil {
		return "", false
	}
	n, ok := s.byGVK[gvk]
	return n, ok
}

// convert deep-copies a v2 schema into v3 form (only $ref targets differ) and
// records the definitions it refers to.
func convert(v interface{}, refs *[]string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, x := range t {
			if s, ok := x.(string); ok && k == "$ref" && strings.HasPrefix(s, defsPrefix) {
				name := strings.TrimPrefix(s, defsPrefix)
				*refs = append(*refs, name)
				out[k] = compPrefix + name
				continue
			}
			out[k] = convert(x, refs)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, x := range t {
			out[i] = convert(x, refs)
		}
		return out
	}
	return v
}

// components returns the v3 schemas for roots and everything they reference.
func (s *Spec) components(roots []string) map[string]interface{} {
	out := map[string]interface{}{}
	if s == nil {
		return out
	}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, done := out[name]; done {
			continue
		}
		def, ok := s.defs[name]
		if !ok {
			continue
		}
		var refs []string
		out[name] = convert(def, &refs)
		queue = append(queue, refs...)
	}
	return out
}

func reverseGroup(group string) string {
	parts := strings.Split(group, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

func customName(r *registry.Resource) string {
	if r.Group == "" {
		return r.Version + "." + r.Kind
	}
	return reverseGroup(r.Group) + "." + r.Version + "." + r.Kind
}

// customSchema turns a CRD's openAPIV3Schema into a resource schema, adding
// the apiVersion/kind/metadata properties the real API server adds.
func customSchema(r *registry.Resource, sp *Spec, roots *[]string) map[string]interface{} {
	var sch map[string]interface{}
	if b, err := json.Marshal(r.Schema); err == nil {
		_ = json.Unmarshal(b, &sch)
	}
	if sch == nil {
		sch = map[string]interface{}{}
	}
	sch["type"] = "object"
	props, _ := sch["properties"].(map[string]interface{})
	if props == nil {
		props = map[string]interface{}{}
		sch["properties"] = props
	}
	if _, ok := props["apiVersion"]; !ok {
		props["apiVersion"] = map[string]interface{}{"type": "string",
			"description": "APIVersion defines the versioned schema of this representation of an object."}
	}
	if _, ok := props["kind"]; !ok {
		props["kind"] = map[string]interface{}{"type": "string",
			"description": "Kind is a string value representing the REST resource this object represents."}
	}
	if _, ok := props["metadata"]; !ok && sp != nil {
		if _, has := sp.defs[metaDef]; has {
			props["metadata"] = map[string]interface{}{"$ref": compPrefix + metaDef,
				"description": "Standard object's metadata."}
			*roots = append(*roots, metaDef)
		}
	}
	sch["x-kubernetes-group-version-kind"] = []interface{}{
		map[string]interface{}{"group": r.Group, "kind": r.Kind, "version": r.Version},
	}
	return sch
}

func operation(r *registry.Resource, action, ref string) map[string]interface{} {
	resp := map[string]interface{}{"description": "OK"}
	if ref != "" {
		resp["content"] = map[string]interface{}{
			"application/json": map[string]interface{}{"schema": map[string]interface{}{"$ref": ref}},
		}
	}
	op := map[string]interface{}{
		"x-kubernetes-action":             action,
		"x-kubernetes-group-version-kind": map[string]interface{}{"group": r.Group, "version": r.Version, "kind": r.Kind},
		"responses":                       map[string]interface{}{"200": resp},
	}
	switch action {
	case "post", "put", "patch":
		// lets kubectl detect that server-side fieldValidation is supported
		op["parameters"] = []interface{}{map[string]interface{}{
			"name": "fieldValidation", "in": "query", "schema": map[string]interface{}{"type": "string"},
		}}
	}
	return op
}

// V3Doc builds the OpenAPI v3 document of one group/version (key is
// "api/v1" or "apis/<group>/<version>"). sp may be nil: custom kinds are then
// described from their CRDs only and built-in kinds have no schemas.
func V3Doc(sp *Spec, reg *registry.Registry, key string) map[string]interface{} {
	rs := reg.ByPath(key)
	if len(rs) == 0 {
		return nil
	}
	paths := map[string]interface{}{}
	schemas := map[string]interface{}{}
	var roots []string
	base := "/" + key
	for _, r := range rs {
		var ref, listRef string
		if r.Custom {
			if r.Schema != nil {
				name := customName(r)
				schemas[name] = customSchema(r, sp, &roots)
				ref = compPrefix + name
			}
		} else {
			if n, ok := sp.definitionFor(r.GVK()); ok {
				roots = append(roots, n)
				ref = compPrefix + n
			}
			listGVK := schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind + "List"}
			if n, ok := sp.definitionFor(listGVK); ok {
				roots = append(roots, n)
				listRef = compPrefix + n
			}
		}
		coll := base + "/" + r.Name
		if r.Namespaced {
			paths[coll] = map[string]interface{}{"get": operation(r, "list", listRef)} // all namespaces
			coll = base + "/namespaces/{namespace}/" + r.Name
		}
		paths[coll] = map[string]interface{}{
			"get":  operation(r, "list", listRef),
			"post": operation(r, "post", ref),
		}
		paths[coll+"/{name}"] = map[string]interface{}{
			"get":    operation(r, "get", ref),
			"put":    operation(r, "put", ref),
			"patch":  operation(r, "patch", ref),
			"delete": operation(r, "delete", ""),
		}
	}
	for name, c := range sp.components(roots) {
		if _, exists := schemas[name]; !exists {
			schemas[name] = c
		}
	}
	return map[string]interface{}{
		"openapi":    "3.0.0",
		"info":       map[string]interface{}{"title": "Kubernetes emulator", "version": registry.ServerVersion},
		"paths":      paths,
		"components": map[string]interface{}{"schemas": schemas},
	}
}
