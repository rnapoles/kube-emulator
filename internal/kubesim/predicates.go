package kubesim

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// nodeInfo is a node with what is already placed on it.
type nodeInfo struct {
	node *corev1.Node
	used corev1.ResourceList
	pods int
}

// cluster is the scheduler's view of the nodes and the pods bound to them.
type cluster struct {
	nodes    []*nodeInfo
	byName   map[string]*nodeInfo
	assigned []*corev1.Pod
}

// ---- resources ----

func effectiveRequests(c *corev1.Container) corev1.ResourceList {
	out := corev1.ResourceList{}
	for name, q := range c.Resources.Requests {
		out[name] = q
	}
	for name, q := range c.Resources.Limits { // limits imply requests
		if _, ok := out[name]; !ok {
			out[name] = q
		}
	}
	return out
}

func addList(dst, src corev1.ResourceList) {
	for name, q := range src {
		cur, ok := dst[name]
		if !ok {
			dst[name] = q.DeepCopy()
			continue
		}
		cur.Add(q)
		dst[name] = cur
	}
}

func maxList(dst, src corev1.ResourceList) {
	for name, q := range src {
		cur, ok := dst[name]
		if !ok || q.Cmp(cur) > 0 {
			dst[name] = q.DeepCopy()
		}
	}
}

// podRequests is what the pod asks for: the sum of its containers, or the
// largest init container if that is more.
func podRequests(pod *corev1.Pod) corev1.ResourceList {
	sum := corev1.ResourceList{}
	for i := range pod.Spec.Containers {
		addList(sum, effectiveRequests(&pod.Spec.Containers[i]))
	}
	init := corev1.ResourceList{}
	for i := range pod.Spec.InitContainers {
		maxList(init, effectiveRequests(&pod.Spec.InitContainers[i]))
	}
	maxList(sum, init)
	return sum
}

func (ni *nodeInfo) fitsResources(req corev1.ResourceList) (bool, string) {
	alloc := ni.node.Status.Allocatable
	if len(alloc) == 0 {
		alloc = ni.node.Status.Capacity
	}
	if maxPods, ok := alloc[corev1.ResourcePods]; ok && int64(ni.pods)+1 > maxPods.Value() {
		return false, "Too many pods"
	}
	for name, q := range req {
		if q.IsZero() {
			continue
		}
		a, ok := alloc[name]
		if !ok {
			return false, fmt.Sprintf("Insufficient %s", name)
		}
		avail := a.DeepCopy()
		if u, ok := ni.used[name]; ok {
			avail.Sub(u)
		}
		if q.Cmp(avail) > 0 {
			return false, fmt.Sprintf("Insufficient %s", name)
		}
	}
	return true, ""
}

// ---- node selector, affinity, taints ----

func untoleratedTaint(tols []corev1.Toleration, taints []corev1.Taint) (corev1.Taint, bool) {
	for i := range taints {
		t := taints[i]
		if t.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for j := range tols {
			if tols[j].ToleratesTaint(&t) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return t, true
		}
	}
	return corev1.Taint{}, false
}

func nodeSelectorFits(pod *corev1.Pod, node *corev1.Node) bool {
	ok, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(node)
	return err == nil && ok
}

// NodeFits checks only what a DaemonSet controller checks: nodeSelector,
// required node affinity and taints (not resources, not cordoning).
func NodeFits(node *corev1.Node, pod *corev1.Pod) (bool, string) {
	if !nodeSelectorFits(pod, node) {
		return false, "node(s) didn't match Pod's node affinity/selector"
	}
	if t, bad := untoleratedTaint(pod.Spec.Tolerations, node.Spec.Taints); bad {
		return false, fmt.Sprintf("node(s) had untolerated taint {%s: %s}", t.Key, t.Value)
	}
	return true, ""
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return true
}

func preferredScore(pod *corev1.Pod, node *corev1.Node) int64 {
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		return 0
	}
	terms, err := nodeaffinity.NewPreferredSchedulingTerms(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	if err != nil {
		return 0
	}
	return terms.Score(node)
}

// ---- inter-pod affinity ----

type termMatch struct {
	matching  int
	topoVals  map[string]bool
	selfMatch bool
}

func (cl *cluster) matchTerm(pod *corev1.Pod, term corev1.PodAffinityTerm) (termMatch, error) {
	sel, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
	if err != nil {
		return termMatch{}, err
	}
	namespaces := term.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{pod.Namespace}
	}
	inNS := func(ns string) bool {
		for _, n := range namespaces {
			if n == ns {
				return true
			}
		}
		return false
	}
	m := termMatch{topoVals: map[string]bool{}, selfMatch: sel.Matches(labels.Set(pod.Labels)) && inNS(pod.Namespace)}
	for _, p := range cl.assigned {
		if p.UID == pod.UID || !inNS(p.Namespace) || !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		m.matching++
		if ni := cl.byName[p.Spec.NodeName]; ni != nil {
			if v, ok := ni.node.Labels[term.TopologyKey]; ok {
				m.topoVals[v] = true
			}
		}
	}
	return m, nil
}

// podAffinityFits evaluates requiredDuringScheduling pod affinity and
// anti-affinity for placing pod on node.
func (cl *cluster) podAffinityFits(pod *corev1.Pod, node *corev1.Node) (bool, string) {
	aff := pod.Spec.Affinity
	if aff == nil {
		return true, ""
	}
	if aff.PodAffinity != nil {
		for _, term := range aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			m, err := cl.matchTerm(pod, term)
			val, has := node.Labels[term.TopologyKey]
			if err != nil || !has {
				return false, "node(s) didn't match pod affinity rules"
			}
			if m.matching == 0 {
				if !m.selfMatch { // the first pod of a group may satisfy its own affinity
					return false, "node(s) didn't match pod affinity rules"
				}
				continue
			}
			if !m.topoVals[val] {
				return false, "node(s) didn't match pod affinity rules"
			}
		}
	}
	if aff.PodAntiAffinity != nil {
		for _, term := range aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			m, err := cl.matchTerm(pod, term)
			if err != nil {
				return false, "node(s) didn't match pod anti-affinity rules"
			}
			if val, has := node.Labels[term.TopologyKey]; has && m.topoVals[val] {
				return false, "node(s) didn't match pod anti-affinity rules"
			}
		}
	}
	return true, ""
}

// filter reports whether pod can run on the node, or the reason why not.
func (cl *cluster) filter(pod *corev1.Pod, ni *nodeInfo, req corev1.ResourceList) (bool, string) {
	n := ni.node
	switch {
	case n.Spec.Unschedulable:
		return false, "node(s) were unschedulable"
	case !nodeReady(n):
		return false, "node(s) were not ready"
	}
	if ok, why := NodeFits(n, pod); !ok {
		return false, why
	}
	if ok, why := ni.fitsResources(req); !ok {
		return false, why
	}
	return cl.podAffinityFits(pod, n)
}
