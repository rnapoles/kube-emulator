package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

type colDef struct{ Name, Type, Format, Description string }

type tableDef struct {
	cols []colDef
	row  func(u *U) []interface{}
}

var (
	cName = colDef{"Name", "string", "name", "Name must be unique within a namespace."}
	cAge  = colDef{"Age", "string", "", "CreationTimestamp is a timestamp representing the server time when this object was created."}
)

// gs / gi (nested field accessors) come from the objects package, see aliases.go.

func humanAge(u *U) string {
	return duration.HumanDuration(time.Since(u.GetCreationTimestamp().Time))
}

func partialMeta(u *U) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: u.GetName(), Namespace: u.GetNamespace(), UID: u.GetUID(),
			ResourceVersion: u.GetResourceVersion(), Generation: u.GetGeneration(),
			CreationTimestamp: u.GetCreationTimestamp(), DeletionTimestamp: u.GetDeletionTimestamp(),
			Labels: u.GetLabels(), Annotations: u.GetAnnotations(),
			OwnerReferences: u.GetOwnerReferences(), Finalizers: u.GetFinalizers(),
		},
	}
}

func (s *Server) table(res *Resource, items []*U, rv string) *metav1.Table {
	def := tableDefFor(res)
	t := &metav1.Table{
		TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"},
		ListMeta: metav1.ListMeta{ResourceVersion: rv},
	}
	for _, c := range def.cols {
		t.ColumnDefinitions = append(t.ColumnDefinitions, metav1.TableColumnDefinition{
			Name: c.Name, Type: c.Type, Format: c.Format, Description: c.Description,
		})
	}
	for _, it := range items {
		raw, _ := json.Marshal(partialMeta(it))
		t.Rows = append(t.Rows, metav1.TableRow{Cells: def.row(it), Object: k8sruntime.RawExtension{Raw: raw}})
	}
	return t
}

func readyOf(u *U, specField, statusField string) string {
	desired := int64(1)
	if v, found, _ := unstructured.NestedInt64(u.Object, "spec", specField); found {
		desired = v
	}
	return fmt.Sprintf("%d/%d", gi(u, "status", statusField), desired)
}

func tableDefFor(res *Resource) tableDef {
	switch res.Key() {
	case kPods:
		return tableDef{
			cols: []colDef{cName,
				{"Ready", "string", "", "The aggregate readiness state of this pod for accepting traffic."},
				{"Status", "string", "", "The aggregate status of the containers in this pod."},
				{"Restarts", "string", "", "The number of times the containers in this pod have been restarted."},
				cAge},
			row: func(u *U) []interface{} {
				cons, _, _ := unstructured.NestedSlice(u.Object, "spec", "containers")
				css, _, _ := unstructured.NestedSlice(u.Object, "status", "containerStatuses")
				ready, restarts := 0, int64(0)
				for _, c := range css {
					if m, ok := c.(map[string]interface{}); ok {
						if b, _ := m["ready"].(bool); b {
							ready++
						}
						if n, ok := m["restartCount"].(int64); ok {
							restarts += n
						}
					}
				}
				status := gs(u, "status", "phase")
				if u.GetDeletionTimestamp() != nil {
					status = "Terminating"
				}
				return []interface{}{u.GetName(), fmt.Sprintf("%d/%d", ready, len(cons)), status, fmt.Sprint(restarts), humanAge(u)}
			},
		}
	case kDeployments:
		return tableDef{
			cols: []colDef{cName,
				{"Ready", "string", "", "Number of the pod with ready state"},
				{"Up-to-date", "string", "", "Total number of non-terminated pods targeted by this deployment that have the desired template spec."},
				{"Available", "string", "", "Total number of available pods (ready for at least minReadySeconds) targeted by this deployment."},
				cAge},
			row: func(u *U) []interface{} {
				return []interface{}{u.GetName(), readyOf(u, "replicas", "readyReplicas"),
					fmt.Sprint(gi(u, "status", "updatedReplicas")), fmt.Sprint(gi(u, "status", "availableReplicas")), humanAge(u)}
			},
		}
	case kReplicaSets:
		return tableDef{
			cols: []colDef{cName,
				{"Desired", "integer", "", "Number of desired pods."},
				{"Current", "integer", "", "Number of currently running pods."},
				{"Ready", "integer", "", "Number of ready pods."},
				cAge},
			row: func(u *U) []interface{} {
				return []interface{}{u.GetName(), gi(u, "spec", "replicas"), gi(u, "status", "replicas"), gi(u, "status", "readyReplicas"), humanAge(u)}
			},
		}
	case kStatefulSet:
		return tableDef{
			cols: []colDef{cName, {"Ready", "string", "", "Number of ready pods."}, cAge},
			row: func(u *U) []interface{} {
				return []interface{}{u.GetName(), readyOf(u, "replicas", "readyReplicas"), humanAge(u)}
			},
		}
	case kDaemonSets:
		return tableDef{
			cols: []colDef{cName,
				{"Desired", "integer", "", ""}, {"Current", "integer", "", ""}, {"Ready", "integer", "", ""},
				{"Up-to-date", "integer", "", ""}, {"Available", "integer", "", ""}, cAge},
			row: func(u *U) []interface{} {
				return []interface{}{u.GetName(), gi(u, "status", "desiredNumberScheduled"), gi(u, "status", "currentNumberScheduled"),
					gi(u, "status", "numberReady"), gi(u, "status", "updatedNumberScheduled"), gi(u, "status", "numberAvailable"), humanAge(u)}
			},
		}
	case kJobs:
		return tableDef{
			cols: []colDef{cName, {"Completions", "string", "", "Number of completions."}, cAge},
			row: func(u *U) []interface{} {
				want := int64(1)
				if v, found, _ := unstructured.NestedInt64(u.Object, "spec", "completions"); found {
					want = v
				}
				return []interface{}{u.GetName(), fmt.Sprintf("%d/%d", gi(u, "status", "succeeded"), want), humanAge(u)}
			},
		}
	case kServices:
		return tableDef{
			cols: []colDef{cName,
				{"Type", "string", "", ""}, {"Cluster-IP", "string", "", ""}, {"External-IP", "string", "", ""},
				{"Port(s)", "string", "", ""}, cAge},
			row: func(u *U) []interface{} {
				ports, _, _ := unstructured.NestedSlice(u.Object, "spec", "ports")
				var ps []string
				for _, p := range ports {
					m, _ := p.(map[string]interface{})
					port, _ := m["port"].(int64)
					proto, _ := m["protocol"].(string)
					if np, ok := m["nodePort"].(int64); ok && np != 0 {
						ps = append(ps, fmt.Sprintf("%d:%d/%s", port, np, proto))
					} else {
						ps = append(ps, fmt.Sprintf("%d/%s", port, proto))
					}
				}
				portStr := strings.Join(ps, ",")
				if portStr == "" {
					portStr = "<none>"
				}
				ext := "<none>"
				if gs(u, "spec", "type") == "ExternalName" {
					ext = gs(u, "spec", "externalName")
				}
				return []interface{}{u.GetName(), gs(u, "spec", "type"), gs(u, "spec", "clusterIP"), ext, portStr, humanAge(u)}
			},
		}
	case kNamespaces:
		return tableDef{
			cols: []colDef{cName, {"Status", "string", "", "The status of the namespace"}, cAge},
			row: func(u *U) []interface{} {
				return []interface{}{u.GetName(), gs(u, "status", "phase"), humanAge(u)}
			},
		}
	case kNodes:
		return tableDef{
			cols: []colDef{cName, {"Status", "string", "", ""}, {"Roles", "string", "", ""}, cAge, {"Version", "string", "", ""}},
			row: func(u *U) []interface{} {
				var roles []string
				for k := range u.GetLabels() {
					if strings.HasPrefix(k, "node-role.kubernetes.io/") {
						roles = append(roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
					}
				}
				sort.Strings(roles)
				r := strings.Join(roles, ",")
				if r == "" {
					r = "<none>"
				}
				return []interface{}{u.GetName(), "Ready", r, humanAge(u), gs(u, "status", "nodeInfo", "kubeletVersion")}
			},
		}
	case ResourceKey{"", "configmaps"}:
		return tableDef{
			cols: []colDef{cName, {"Data", "integer", "", "The number of keys in the ConfigMap."}, cAge},
			row: func(u *U) []interface{} {
				d, _, _ := unstructured.NestedMap(u.Object, "data")
				b, _, _ := unstructured.NestedMap(u.Object, "binaryData")
				return []interface{}{u.GetName(), int64(len(d) + len(b)), humanAge(u)}
			},
		}
	case ResourceKey{"", "secrets"}:
		return tableDef{
			cols: []colDef{cName, {"Type", "string", "", ""}, {"Data", "integer", "", ""}, cAge},
			row: func(u *U) []interface{} {
				d, _, _ := unstructured.NestedMap(u.Object, "data")
				return []interface{}{u.GetName(), gs(u, "type"), int64(len(d)), humanAge(u)}
			},
		}
	case kSAs:
		return tableDef{
			cols: []colDef{cName, {"Secrets", "integer", "", ""}, cAge},
			row: func(u *U) []interface{} {
				s, _, _ := unstructured.NestedSlice(u.Object, "secrets")
				return []interface{}{u.GetName(), int64(len(s)), humanAge(u)}
			},
		}
	case kEndpoints:
		return tableDef{
			cols: []colDef{cName, {"Endpoints", "string", "", ""}, cAge},
			row: func(u *U) []interface{} {
				subsets, _, _ := unstructured.NestedSlice(u.Object, "subsets")
				var eps []string
				for _, ss := range subsets {
					m, _ := ss.(map[string]interface{})
					addrs, _ := m["addresses"].([]interface{})
					ports, _ := m["ports"].([]interface{})
					for _, a := range addrs {
						am, _ := a.(map[string]interface{})
						ip, _ := am["ip"].(string)
						for _, p := range ports {
							pm, _ := p.(map[string]interface{})
							port, _ := pm["port"].(int64)
							eps = append(eps, fmt.Sprintf("%s:%d", ip, port))
						}
					}
				}
				out := strings.Join(eps, ",")
				if out == "" {
					out = "<none>"
				}
				return []interface{}{u.GetName(), out, humanAge(u)}
			},
		}
	}
	// Same fallback the real server uses for resources without a printer.
	return tableDef{
		cols: []colDef{cName, {"Created At", "date", "", "CreationTimestamp is a timestamp representing the server time when this object was created."}},
		row: func(u *U) []interface{} {
			return []interface{}{u.GetName(), u.GetCreationTimestamp().UTC().Format(time.RFC3339)}
		},
	}
}
