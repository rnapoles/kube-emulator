package registry

import (
	"encoding/json"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"

	"github.com/rnapoles/kube-emulator/internal/store"
)

var (
	verbsAll = metav1.Verbs{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"}
	verbsRW  = metav1.Verbs{"get", "patch", "update"}
)

// ServerVersion is the Kubernetes version the emulator reports.
const ServerVersion = "v1.31.0"

type (
	ResourceKey = store.ResourceKey
	U           = store.U
)

func rk(group, resource string) ResourceKey { return ResourceKey{Group: group, Resource: resource} }

// Storage keys of the resources the emulator treats specially.
var (
	KNamespaces   = rk("", "namespaces")
	KNodes        = rk("", "nodes")
	KPods         = rk("", "pods")
	KServices     = rk("", "services")
	KEndpoints    = rk("", "endpoints")
	KEvents       = rk("", "events")
	KSAs          = rk("", "serviceaccounts")
	KPVCs         = rk("", "persistentvolumeclaims")
	KDeployments  = rk("apps", "deployments")
	KReplicaSets  = rk("apps", "replicasets")
	KStatefulSets = rk("apps", "statefulsets")
	KDaemonSets   = rk("apps", "daemonsets")
	KJobs         = rk("batch", "jobs")
	KReplicationControllers = rk("", "replicationcontrollers")
	KCRDs         = rk("apiextensions.k8s.io", "customresourcedefinitions")
)

// Resource describes one served group/version/resource.
type Resource struct {
	Group, Version, Name, Singular, Kind string
	Namespaced                           bool
	ShortNames, Categories               []string
	HasStatus, HasScale                  bool
	Typed                                bool // Go type registered in client-go's scheme
	Custom                               bool // defined by a CustomResourceDefinition
	Validator                            *validate.SchemaValidator
	Schema                               map[string]interface{} // raw openAPIV3Schema of a custom kind
}

func (r *Resource) Key() ResourceKey { return ResourceKey{Group: r.Group, Resource: r.Name} }
func (r *Resource) GV() schema.GroupVersion {
	return schema.GroupVersion{Group: r.Group, Version: r.Version}
}
func (r *Resource) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}
func (r *Resource) APIVersion() string { return r.GV().String() }

// Registry holds every served resource (built-in and CRD-defined).
type Registry struct {
	mu  sync.RWMutex
	res []*Resource
}

func New() *Registry {
	g := &Registry{}
	g.res = builtinResources()
	return g
}

func builtinResources() []*Resource {
	type d struct {
		g, v, plural, singular, kind string
		ns                           bool
		short, cats                  []string
		status, scale                bool
	}
	all := []string{"all"}
	defs := []d{
		{"", "v1", "namespaces", "namespace", "Namespace", false, []string{"ns"}, nil, true, false},
		{"", "v1", "nodes", "node", "Node", false, []string{"no"}, nil, true, false},
		{"", "v1", "pods", "pod", "Pod", true, []string{"po"}, all, true, false},
		{"", "v1", "services", "service", "Service", true, []string{"svc"}, all, true, false},
		{"", "v1", "configmaps", "configmap", "ConfigMap", true, []string{"cm"}, nil, false, false},
		{"", "v1", "secrets", "secret", "Secret", true, nil, nil, false, false},
		{"", "v1", "serviceaccounts", "serviceaccount", "ServiceAccount", true, []string{"sa"}, nil, false, false},
		{"", "v1", "endpoints", "endpoints", "Endpoints", true, []string{"ep"}, nil, false, false},
		{"", "v1", "events", "event", "Event", true, []string{"ev"}, nil, false, false},
		{"", "v1", "persistentvolumes", "persistentvolume", "PersistentVolume", false, []string{"pv"}, nil, true, false},
		{"", "v1", "persistentvolumeclaims", "persistentvolumeclaim", "PersistentVolumeClaim", true, []string{"pvc"}, nil, true, false},
		{"", "v1", "resourcequotas", "resourcequota", "ResourceQuota", true, []string{"quota"}, nil, true, false},
		{"", "v1", "limitranges", "limitrange", "LimitRange", true, []string{"limits"}, nil, false, false},
		{"", "v1", "replicationcontrollers", "replicationcontroller", "ReplicationController", true, []string{"rc"}, all, true, true},
		{"", "v1", "podtemplates", "podtemplate", "PodTemplate", true, nil, nil, false, false},
		{"apps", "v1", "controllerrevisions", "controllerrevision", "ControllerRevision", true, nil, nil, false, false},
		{"discovery.k8s.io", "v1", "endpointslices", "endpointslice", "EndpointSlice", true, nil, nil, false, false},
		{"node.k8s.io", "v1", "runtimeclasses", "runtimeclass", "RuntimeClass", false, nil, nil, false, false},
		{"storage.k8s.io", "v1", "csidrivers", "csidriver", "CSIDriver", false, nil, nil, false, false},
		{"admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", "validatingwebhookconfiguration", "ValidatingWebhookConfiguration", false, nil, nil, false, false},
		{"admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations", "mutatingwebhookconfiguration", "MutatingWebhookConfiguration", false, nil, nil, false, false},
		{"apps", "v1", "deployments", "deployment", "Deployment", true, []string{"deploy"}, all, true, true},
		{"apps", "v1", "replicasets", "replicaset", "ReplicaSet", true, []string{"rs"}, all, true, true},
		{"apps", "v1", "statefulsets", "statefulset", "StatefulSet", true, []string{"sts"}, all, true, true},
		{"apps", "v1", "daemonsets", "daemonset", "DaemonSet", true, []string{"ds"}, all, true, false},
		{"batch", "v1", "jobs", "job", "Job", true, nil, all, true, false},
		{"batch", "v1", "cronjobs", "cronjob", "CronJob", true, []string{"cj"}, all, true, false},
		{"autoscaling", "v2", "horizontalpodautoscalers", "horizontalpodautoscaler", "HorizontalPodAutoscaler", true, []string{"hpa"}, all, true, false},
		{"networking.k8s.io", "v1", "ingresses", "ingress", "Ingress", true, []string{"ing"}, nil, true, false},
		{"networking.k8s.io", "v1", "networkpolicies", "networkpolicy", "NetworkPolicy", true, []string{"netpol"}, nil, false, false},
		{"networking.k8s.io", "v1", "ingressclasses", "ingressclass", "IngressClass", false, nil, nil, false, false},
		{"rbac.authorization.k8s.io", "v1", "roles", "role", "Role", true, nil, nil, false, false},
		{"rbac.authorization.k8s.io", "v1", "rolebindings", "rolebinding", "RoleBinding", true, nil, nil, false, false},
		{"rbac.authorization.k8s.io", "v1", "clusterroles", "clusterrole", "ClusterRole", false, nil, nil, false, false},
		{"rbac.authorization.k8s.io", "v1", "clusterrolebindings", "clusterrolebinding", "ClusterRoleBinding", false, nil, nil, false, false},
		{"storage.k8s.io", "v1", "storageclasses", "storageclass", "StorageClass", false, []string{"sc"}, nil, false, false},
		{"policy", "v1", "poddisruptionbudgets", "poddisruptionbudget", "PodDisruptionBudget", true, []string{"pdb"}, nil, true, false},
		{"scheduling.k8s.io", "v1", "priorityclasses", "priorityclass", "PriorityClass", false, []string{"pc"}, nil, false, false},
		{"coordination.k8s.io", "v1", "leases", "lease", "Lease", true, nil, nil, false, false},
		{"apiextensions.k8s.io", "v1", "customresourcedefinitions", "customresourcedefinition", "CustomResourceDefinition", false, []string{"crd", "crds"}, nil, true, false},
	}
	out := make([]*Resource, 0, len(defs))
	for _, x := range defs {
		r := &Resource{
			Group: x.g, Version: x.v, Name: x.plural, Singular: x.singular, Kind: x.kind,
			Namespaced: x.ns, ShortNames: x.short, Categories: x.cats,
			HasStatus: x.status, HasScale: x.scale,
		}
		r.Typed = scheme.Scheme.Recognizes(r.GVK())
		out = append(out, r)
	}
	return out
}

func (g *Registry) Lookup(group, version, name string) *Resource {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, r := range g.res {
		if r.Group == group && r.Version == version && r.Name == name {
			return r
		}
	}
	return nil
}

// LookupKind finds a served resource by its kind.
func (g *Registry) LookupKind(gvk schema.GroupVersionKind) *Resource {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, r := range g.res {
		if r.Group == gvk.Group && r.Version == gvk.Version && r.Kind == gvk.Kind {
			return r
		}
	}
	return nil
}

func (g *Registry) ForGV(group, version string) []*Resource {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Resource
	for _, r := range g.res {
		if r.Group == group && r.Version == version {
			out = append(out, r)
		}
	}
	return out
}

func (g *Registry) Groups() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	for _, r := range g.res {
		if r.Group != "" && !seen[r.Group] {
			seen[r.Group] = true
			out = append(out, r.Group)
		}
	}
	return out
}

// Group returns the discovery document of one API group (nil if unknown).
func (g *Registry) Group(name string) *metav1.APIGroup {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var versions []metav1.GroupVersionForDiscovery
	seen := map[string]bool{}
	for _, r := range g.res {
		if r.Group != name || seen[r.Version] {
			continue
		}
		seen[r.Version] = true
		versions = append(versions, metav1.GroupVersionForDiscovery{GroupVersion: r.GV().String(), Version: r.Version})
	}
	if len(versions) == 0 {
		return nil
	}
	return &metav1.APIGroup{
		TypeMeta:         metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
		Name:             name,
		Versions:         versions,
		PreferredVersion: versions[0],
	}
}

func (g *Registry) GroupList() *metav1.APIGroupList {
	list := &metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}}
	for _, name := range g.Groups() {
		if grp := g.Group(name); grp != nil {
			list.Groups = append(list.Groups, *grp)
		}
	}
	return list
}

// ResourceList returns the legacy discovery document of one group/version.
func (g *Registry) ResourceList(group, version string) *metav1.APIResourceList {
	rs := g.ForGV(group, version)
	if len(rs) == 0 {
		return nil
	}
	list := &metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
		GroupVersion: schema.GroupVersion{Group: group, Version: version}.String(),
	}
	for _, r := range rs {
		list.APIResources = append(list.APIResources, metav1.APIResource{
			Name: r.Name, SingularName: r.Singular, Namespaced: r.Namespaced, Kind: r.Kind,
			Verbs: verbsAll, ShortNames: r.ShortNames, Categories: r.Categories,
		})
		if r.HasStatus {
			list.APIResources = append(list.APIResources, metav1.APIResource{
				Name: r.Name + "/status", Namespaced: r.Namespaced, Kind: r.Kind, Verbs: verbsRW,
			})
		}
		if r.HasScale {
			list.APIResources = append(list.APIResources, metav1.APIResource{
				Name: r.Name + "/scale", Namespaced: r.Namespaced, Group: "autoscaling", Version: "v1", Kind: "Scale", Verbs: verbsRW,
			})
		}
		if r.Name == "pods" {
			list.APIResources = append(list.APIResources,
				metav1.APIResource{Name: "pods/log", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"get"}},
				metav1.APIResource{Name: "pods/eviction", Namespaced: true, Group: "policy", Version: "v1", Kind: "Eviction", Verbs: metav1.Verbs{"create"}},
			)
		}
	}
	return list
}

// ---- CustomResourceDefinitions ----

func strSlice(v interface{}) []string {
	var out []string
	if arr, ok := v.([]interface{}); ok {
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// RegisterCRD (re)registers every served version of a CRD.
func (g *Registry) RegisterCRD(u *U) error {
	sp, _, _ := unstructured.NestedMap(u.Object, "spec")
	group, _ := sp["group"].(string)
	names, _ := sp["names"].(map[string]interface{})
	plural, _ := names["plural"].(string)
	kind, _ := names["kind"].(string)
	singular, _ := names["singular"].(string)
	if singular == "" {
		singular = strings.ToLower(kind)
	}
	scope, _ := sp["scope"].(string)
	versions, _ := sp["versions"].([]interface{})
	g.UnregisterCRD(group, plural)

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, v := range versions {
		vm, _ := v.(map[string]interface{})
		name, _ := vm["name"].(string)
		if served, ok := vm["served"].(bool); ok && !served {
			continue
		}
		r := &Resource{
			Group: group, Version: name, Name: plural, Singular: singular, Kind: kind,
			Namespaced: scope == "Namespaced",
			ShortNames: strSlice(names["shortNames"]), Categories: strSlice(names["categories"]),
			Custom: true,
		}
		if sub, ok := vm["subresources"].(map[string]interface{}); ok {
			_, r.HasStatus = sub["status"]
		}
		if sch, ok := vm["schema"].(map[string]interface{}); ok {
			if oas, ok := sch["openAPIV3Schema"]; ok {
				r.Validator = buildValidator(oas)
				r.Schema, _ = oas.(map[string]interface{})
			}
		}
		g.res = append(g.res, r)
	}
	return nil
}

func (g *Registry) UnregisterCRD(group, plural string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.res[:0]
	for _, r := range g.res {
		if !(r.Custom && r.Group == group && r.Name == plural) {
			kept = append(kept, r)
		}
	}
	g.res = kept
}

// buildValidator turns the CRD's openAPIV3Schema into a kube-openapi schema
// validator, the same component the real apiextensions-apiserver uses.
func buildValidator(raw interface{}) *validate.SchemaValidator {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var sc spec.Schema
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil
	}
	return validate.NewSchemaValidator(&sc, nil, "", strfmt.Default)
}

// ---- OpenAPI v3 index (the documents themselves are built by the openapi package) ----

// GVPath is the OpenAPI v3 path key of a group/version ("api/v1", "apis/apps/v1").
func GVPath(group, version string) string {
	if group == "" {
		return "api/" + version
	}
	return "apis/" + group + "/" + version
}

func (g *Registry) OpenAPIV3Index() map[string]interface{} {
	g.mu.RLock()
	defer g.mu.RUnlock()
	paths := map[string]interface{}{}
	for _, r := range g.res {
		key := GVPath(r.Group, r.Version)
		paths[key] = map[string]interface{}{"serverRelativeURL": "/openapi/v3/" + key}
	}
	return map[string]interface{}{"paths": paths}
}

// ByPath returns the resources served under an OpenAPI v3 path key.
func (g *Registry) ByPath(key string) []*Resource {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Resource
	for _, r := range g.res {
		if GVPath(r.Group, r.Version) == key {
			out = append(out, r)
		}
	}
	return out
}
