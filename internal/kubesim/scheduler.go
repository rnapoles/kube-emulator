package kubesim

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rnapoles/kube-emulator/internal/events"
	"github.com/rnapoles/kube-emulator/internal/store"
)

type failure struct {
	msg string
	at  time.Time
}

// Scheduler binds pods without a node to a node, honouring nodeSelector,
// required and preferred node affinity, required pod affinity/anti-affinity,
// taints and tolerations, cordoning and node resources. Among the nodes that
// fit it prefers preferred-affinity matches, then the node with the fewest pods.
type Scheduler struct {
	st        *store.Store
	ev        *events.Recorder
	d         Delays
	firstSeen map[types.UID]time.Time
	lastFail  map[types.UID]failure
}

func NewScheduler(st *store.Store, ev *events.Recorder, d Delays) *Scheduler {
	return &Scheduler{st: st, ev: ev, d: d, firstSeen: map[types.UID]time.Time{}, lastFail: map[types.UID]failure{}}
}

func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safely("scheduler", s.once)
		}
	}
}

func terminal(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}

func (s *Scheduler) once() {
	pods, _ := s.st.List(kPods, "")
	var pending []*U
	live := map[types.UID]bool{}
	for _, p := range pods {
		live[p.GetUID()] = true
		if p.GetDeletionTimestamp() != nil || gs(p, "spec", "nodeName") != "" {
			continue
		}
		if sn := gs(p, "spec", "schedulerName"); sn != "" && sn != corev1.DefaultSchedulerName {
			continue // another scheduler's pod
		}
		pending = append(pending, p)
	}
	for uid := range s.firstSeen {
		if !live[uid] {
			delete(s.firstSeen, uid)
		}
	}
	for uid := range s.lastFail {
		if !live[uid] {
			delete(s.lastFail, uid)
		}
	}
	if len(pending) == 0 {
		return
	}
	cl := s.buildCluster(pods)
	sort.Slice(pending, func(i, j int) bool {
		a, b := pending[i].GetCreationTimestamp().Time, pending[j].GetCreationTimestamp().Time
		if !a.Equal(b) {
			return a.Before(b)
		}
		return pending[i].GetName() < pending[j].GetName()
	})
	now := time.Now()
	for _, pu := range pending {
		uid := pu.GetUID()
		if _, ok := s.firstSeen[uid]; !ok {
			s.firstSeen[uid] = now
		}
		var pod corev1.Pod
		if fromU(pu, &pod) != nil {
			continue
		}
		if now.Sub(s.firstSeen[uid]) < s.d.forPod(pod.Annotations).Schedule {
			continue
		}
		s.scheduleOne(pu, &pod, cl, now)
	}
}

func (s *Scheduler) buildCluster(pods []*U) *cluster {
	cl := &cluster{byName: map[string]*nodeInfo{}}
	nodes, _ := s.st.List(kNodes, "")
	for _, nu := range nodes {
		var n corev1.Node
		if fromU(nu, &n) != nil || nu.GetDeletionTimestamp() != nil {
			continue
		}
		ni := &nodeInfo{node: &n, used: corev1.ResourceList{}}
		cl.nodes = append(cl.nodes, ni)
		cl.byName[n.Name] = ni
	}
	for _, pu := range pods {
		node := gs(pu, "spec", "nodeName")
		if node == "" {
			continue
		}
		var p corev1.Pod
		if fromU(pu, &p) != nil || terminal(p.Status.Phase) {
			continue
		}
		cl.assigned = append(cl.assigned, &p)
		if ni := cl.byName[node]; ni != nil {
			ni.pods++
			addList(ni.used, podRequests(&p))
		}
	}
	return cl
}

func (s *Scheduler) scheduleOne(pu *U, pod *corev1.Pod, cl *cluster, now time.Time) {
	req := podRequests(pod)
	reasons := map[string]int{}
	var best *nodeInfo
	var bestScore int64
	for _, ni := range cl.nodes {
		ok, why := cl.filter(pod, ni, req)
		if !ok {
			reasons[why]++
			continue
		}
		score := preferredScore(pod, ni.node)
		if best == nil || score > bestScore || (score == bestScore && ni.pods < best.pods) {
			best, bestScore = ni, score
		}
	}
	if best == nil {
		s.unschedulable(pu, len(cl.nodes), reasons, now)
		return
	}
	node := best.node.Name
	updated, err := s.st.Mutate(kPods, pu.GetNamespace(), pu.GetName(), func(cur *U) (*U, error) {
		if cur.GetUID() != pu.GetUID() || cur.GetDeletionTimestamp() != nil || gs(cur, "spec", "nodeName") != "" {
			return nil, nil
		}
		var p corev1.Pod
		if err := fromU(cur, &p); err != nil {
			return nil, err
		}
		if err := unstructured.SetNestedField(cur.Object, node, "spec", "nodeName"); err != nil {
			return nil, err
		}
		setCond(&p.Status.Conditions, corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, metav1.Now())
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&p.Status)
		if err != nil {
			return nil, err
		}
		cur.Object["status"] = m
		return cur, nil
	}, false)
	if err != nil || gs(updated, "spec", "nodeName") != node {
		return
	}
	s.ev.Eventf(updated, events.Normal, "Scheduled", "default-scheduler", "",
		"Successfully assigned %s/%s to %s", pu.GetNamespace(), pu.GetName(), node)
	delete(s.firstSeen, pu.GetUID())
	delete(s.lastFail, pu.GetUID())

	// account for the pod so that the next pods of this round see it
	best.pods++
	addList(best.used, req)
	placed := pod.DeepCopy()
	placed.Spec.NodeName = node
	cl.assigned = append(cl.assigned, placed)
}

func failureMessage(total int, reasons map[string]int) string {
	if total == 0 {
		return "no nodes available to schedule pods"
	}
	type kv struct {
		reason string
		n      int
	}
	var list []kv
	for r, n := range reasons {
		list = append(list, kv{r, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].reason < list[j].reason
	})
	parts := make([]string, len(list))
	for i, e := range list {
		parts[i] = fmt.Sprintf("%d %s", e.n, e.reason)
	}
	return fmt.Sprintf("0/%d nodes are available: %s.", total, strings.Join(parts, ", "))
}

func (s *Scheduler) unschedulable(pu *U, total int, reasons map[string]int, now time.Time) {
	msg := failureMessage(total, reasons)
	uid := pu.GetUID()
	if f, ok := s.lastFail[uid]; ok && f.msg == msg && now.Sub(f.at) < 30*time.Second {
		return
	}
	s.lastFail[uid] = failure{msg: msg, at: now}
	updated, err := s.st.Mutate(kPods, pu.GetNamespace(), pu.GetName(), func(cur *U) (*U, error) {
		if cur.GetUID() != pu.GetUID() || gs(cur, "spec", "nodeName") != "" {
			return nil, nil
		}
		var p corev1.Pod
		if err := fromU(cur, &p); err != nil {
			return nil, err
		}
		setCond(&p.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: msg,
		}, metav1.Now())
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&p.Status)
		if err != nil {
			return nil, err
		}
		if equality.Semantic.DeepEqual(cur.Object["status"], m) {
			return nil, nil
		}
		cur.Object["status"] = m
		return cur, nil
	}, false)
	if err != nil {
		return
	}
	s.ev.Eventf(updated, events.Warning, "FailedScheduling", "default-scheduler", "", "%s", msg)
}
