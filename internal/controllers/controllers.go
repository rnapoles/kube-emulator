package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	"github.com/rnapoles/kube-emulator/internal/events"
	"github.com/rnapoles/kube-emulator/internal/kubesim"
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/store"
)

var (
	gvkPod        = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	gvkEndpoints  = schema.GroupVersionKind{Version: "v1", Kind: "Endpoints"}
	gvkSA         = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}
	gvkReplicaSet = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}
)

// ---------------------------------------------------------------- controller loop

// Controller runs the workload controllers (Deployment, ReplicaSet,
// ReplicationController, StatefulSet, DaemonSet, Job), the endpoints
// controller and garbage collection. Pods are only created here; the
// scheduler and kubelet simulation (package kubesim) move them forward.
type Controller struct {
	st *store.Store
	ev *events.Recorder
}

func New(st *store.Store, ev *events.Recorder) *Controller { return &Controller{st: st, ev: ev} }

func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		c.reconcileAll()
		select {
		case <-ctx.Done():
			return
		case <-c.st.Kick:
		case <-t.C:
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *Controller) reconcileAll() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("controller panic: %v", r)
		}
	}()
	c.ensureServiceAccounts()
	c.each(kDeployments, c.syncDeployment)
	c.each(kReplicaSets, c.syncReplicaSet)
	c.each(kReplicationControllers, c.syncReplicationController)
	c.each(kStatefulSet, c.syncStatefulSet)
	c.each(kDaemonSets, c.syncDaemonSet)
	c.each(kJobs, c.syncJob)
	c.syncEndpoints()
	c.gc()
}

func (c *Controller) each(key ResourceKey, fn func(*U)) {
	items, _ := c.st.List(key, "")
	for _, it := range items {
		if it.GetDeletionTimestamp() == nil {
			fn(it)
		}
	}
}

// ---------------------------------------------------------------- helpers

func (c *Controller) createTyped(key ResourceKey, obj k8sruntime.Object, gvk schema.GroupVersionKind) (*U, error) {
	u, err := toU(obj, gvk)
	if err != nil {
		return nil, err
	}
	initMeta(u)
	created, err := c.st.Create(key, u)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, nil
		}
		return nil, err
	}
	return created, nil
}

func (c *Controller) mutate(key ResourceKey, ns, name string, fn func(cur *U) bool) {
	_, _ = c.st.Mutate(key, ns, name, func(cur *U) (*U, error) {
		if !fn(cur) {
			return nil, nil
		}
		return cur, nil
	}, false)
}

func (c *Controller) setStatus(key ResourceKey, ns, name string, status map[string]interface{}) {
	if status == nil {
		return
	}
	c.mutate(key, ns, name, func(cur *U) bool {
		if equality.Semantic.DeepEqual(cur.Object["status"], status) {
			return false
		}
		cur.Object["status"] = status
		return true
	})
}

// setReplicas changes spec.replicas. It reports whether it changed anything
// and the previous value.
func (c *Controller) setReplicas(key ResourceKey, u *U, n int32) (changed bool, old int64) {
	c.mutate(key, u.GetNamespace(), u.GetName(), func(cur *U) bool {
		o, found, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas")
		if found && o == int64(n) {
			return false
		}
		old = o
		_ = unstructured.SetNestedField(cur.Object, int64(n), "spec", "replicas")
		cur.SetGeneration(cur.GetGeneration() + 1)
		changed = true
		return true
	})
	return
}

func specReplicas(u *U) int32 {
	v, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if !found {
		return 1
	}
	return int32(v)
}

func statusMap(v interface{}) map[string]interface{} {
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(v)
	if err != nil {
		return nil
	}
	return m
}

func controllerRef(owner *U) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(),
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

func (c *Controller) ownedBy(key ResourceKey, ns string, uid types.UID) []*U {
	items, _ := c.st.List(key, ns)
	var out []*U
	for _, it := range items {
		for _, r := range it.GetOwnerReferences() {
			if r.UID == uid {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

func activePods(pods []*U) []*U {
	var out []*U
	for _, p := range pods {
		if p.GetDeletionTimestamp() == nil {
			out = append(out, p)
		}
	}
	return out
}

func podPhase(u *U) string { return gs(u, "status", "phase") }

// isReady reports whether the pod has the Ready condition.
func isReady(u *U) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, cd := range conds {
		if m, ok := cd.(map[string]interface{}); ok && m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

func templateHash(t *corev1.PodTemplateSpec) string {
	b, _ := json.Marshal(t)
	h := fnv.New32a()
	_, _ = h.Write(b)
	return rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
}

func revisionOf(u *U) int64 {
	n, _ := strconv.ParseInt(u.GetAnnotations()[revisionAnnotation], 10, 64)
	return n
}

// createPod creates a Pending pod from a template; the scheduler binds it
// (unless node is given) and the kubelet simulation starts it.
func (c *Controller) createPod(owner *U, tmpl *corev1.PodTemplateSpec, name, node string, extra map[string]string) error {
	pod := &corev1.Pod{ObjectMeta: *tmpl.ObjectMeta.DeepCopy(), Spec: *tmpl.Spec.DeepCopy()}
	pod.Name = name
	pod.GenerateName = ""
	pod.Namespace = owner.GetNamespace()
	pod.OwnerReferences = []metav1.OwnerReference{controllerRef(owner)}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	for k, v := range extra {
		pod.Labels[k] = v
	}
	if node != "" {
		pod.Spec.NodeName = node
	}
	normalize(pod)
	u, err := toU(pod, gvkPod)
	if err != nil {
		return err
	}
	initMeta(u)
	if err := objects.PreparePod(u); err != nil {
		return err
	}
	_, err = c.st.Create(kPods, u)
	return err
}

// ---------------------------------------------------------------- namespaces

func (c *Controller) ensureServiceAccounts() {
	nss, _ := c.st.List(kNamespaces, "")
	for _, ns := range nss {
		if ns.GetDeletionTimestamp() != nil {
			continue
		}
		if _, err := c.st.Get(kSAs, ns.GetName(), "default"); apierrors.IsNotFound(err) {
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns.GetName()}}
			_, _ = c.createTyped(kSAs, sa, gvkSA)
		}
	}
}

// ---------------------------------------------------------------- pod sets

// reconcilePods keeps `desired` active pods owned by owner: it creates pods
// from tmpl or deletes the least useful ones (unscheduled, then not ready,
// then newest). It returns how many pods exist and how many are ready.
func (c *Controller) reconcilePods(owner *U, tmpl *corev1.PodTemplateSpec, namePrefix, component string, desired int) (total, ready int) {
	ns := owner.GetNamespace()
	pods := activePods(c.ownedBy(kPods, ns, owner.GetUID()))
	rank := func(p *U) int {
		switch {
		case gs(p, "spec", "nodeName") == "":
			return 0
		case !isReady(p):
			return 1
		}
		return 2
	}
	sort.Slice(pods, func(i, j int) bool {
		if ri, rj := rank(pods[i]), rank(pods[j]); ri != rj {
			return ri < rj
		}
		return pods[i].GetCreationTimestamp().Time.After(pods[j].GetCreationTimestamp().Time)
	})
	for len(pods) > desired {
		victim := pods[0]
		pods = pods[1:]
		if _, _, err := c.st.Delete(kPods, ns, victim.GetName()); err == nil {
			c.ev.Eventf(owner, events.Normal, "SuccessfulDelete", component, "", "Deleted pod: %s", victim.GetName())
		}
	}
	for _, p := range pods {
		if isReady(p) {
			ready++
		}
	}
	total = len(pods)
	for i := len(pods); i < desired; i++ {
		name := namePrefix + "-" + rand.String(5)
		if err := c.createPod(owner, tmpl, name, "", nil); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
			continue
		}
		c.ev.Eventf(owner, events.Normal, "SuccessfulCreate", component, "", "Created pod: %s", name)
		total++
	}
	return total, ready
}

func (c *Controller) syncReplicaSet(ru *U) {
	var rs appsv1.ReplicaSet
	if err := fromU(ru, &rs); err != nil {
		return
	}
	desired := 1
	if rs.Spec.Replicas != nil {
		desired = int(*rs.Spec.Replicas)
	}
	total, ready := c.reconcilePods(ru, &rs.Spec.Template, rs.Name, "replicaset-controller", desired)
	status := appsv1.ReplicaSetStatus{
		Replicas: int32(total), FullyLabeledReplicas: int32(total), ReadyReplicas: int32(ready),
		AvailableReplicas: int32(ready), ObservedGeneration: ru.GetGeneration(),
	}
	c.setStatus(kReplicaSets, ru.GetNamespace(), rs.Name, statusMap(&status))
}

func (c *Controller) syncReplicationController(ru *U) {
	var rc corev1.ReplicationController
	if err := fromU(ru, &rc); err != nil || rc.Spec.Template == nil {
		return
	}
	desired := 1
	if rc.Spec.Replicas != nil {
		desired = int(*rc.Spec.Replicas)
	}
	total, ready := c.reconcilePods(ru, rc.Spec.Template, rc.Name, "replication-controller", desired)
	status := corev1.ReplicationControllerStatus{
		Replicas: int32(total), FullyLabeledReplicas: int32(total), ReadyReplicas: int32(ready),
		AvailableReplicas: int32(ready), ObservedGeneration: ru.GetGeneration(),
	}
	c.setStatus(kReplicationControllers, ru.GetNamespace(), rc.Name, statusMap(&status))
}

// ---------------------------------------------------------------- deployments

func setDeployCond(conds *[]appsv1.DeploymentCondition, t appsv1.DeploymentConditionType, status corev1.ConditionStatus, reason, msg string) {
	now := metav1.Now()
	for i := range *conds {
		if (*conds)[i].Type == t {
			if (*conds)[i].Status == status && (*conds)[i].Reason == reason {
				return
			}
			(*conds)[i] = appsv1.DeploymentCondition{Type: t, Status: status, Reason: reason, Message: msg, LastUpdateTime: now, LastTransitionTime: now}
			return
		}
	}
	*conds = append(*conds, appsv1.DeploymentCondition{Type: t, Status: status, Reason: reason, Message: msg, LastUpdateTime: now, LastTransitionTime: now})
}

func rollingParams(d *appsv1.Deployment, desired int32) (surge, unavailable int32) {
	ms, mu := intstr.FromString("25%"), intstr.FromString("25%")
	if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
		if ru.MaxSurge != nil {
			ms = *ru.MaxSurge
		}
		if ru.MaxUnavailable != nil {
			mu = *ru.MaxUnavailable
		}
	}
	s, _ := intstr.GetScaledValueFromIntOrPercent(&ms, int(desired), true)
	u, _ := intstr.GetScaledValueFromIntOrPercent(&mu, int(desired), false)
	if s == 0 && u == 0 {
		u = 1
	}
	return int32(s), int32(u)
}

func (c *Controller) scaleRS(du, rs *U, n int32) {
	changed, old := c.setReplicas(kReplicaSets, rs, n)
	if !changed {
		return
	}
	verb := "up"
	if int64(n) < old {
		verb = "down"
	}
	c.ev.Eventf(du, events.Normal, "ScalingReplicaSet", "deployment-controller", "",
		"Scaled %s replica set %s to %d from %d", verb, rs.GetName(), n, old)
}

type rsView struct {
	u           *U
	spec, avail int32
}

func (c *Controller) syncDeployment(du *U) {
	var d appsv1.Deployment
	if err := fromU(du, &d); err != nil || d.Spec.Selector == nil {
		return
	}
	ns := du.GetNamespace()
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	hash := templateHash(&d.Spec.Template)
	rsName := d.Name + "-" + hash
	rss := c.ownedBy(kReplicaSets, ns, du.GetUID())

	var maxRev int64
	var newRS *U
	var old []*U
	for _, r := range rss {
		if rev := revisionOf(r); rev > maxRev {
			maxRev = rev
		}
		if r.GetName() == rsName {
			newRS = r
		} else {
			old = append(old, r)
		}
	}
	if newRS == nil {
		tmpl := d.Spec.Template.DeepCopy()
		if tmpl.Labels == nil {
			tmpl.Labels = map[string]string{}
		}
		tmpl.Labels["pod-template-hash"] = hash
		sel := d.Spec.Selector.DeepCopy()
		if sel.MatchLabels == nil {
			sel.MatchLabels = map[string]string{}
		}
		sel.MatchLabels["pod-template-hash"] = hash
		maxRev++
		zero := int32(0) // scaled up by the rollout logic below
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: rsName, Namespace: ns, Labels: tmpl.Labels,
				Annotations:     map[string]string{revisionAnnotation: strconv.FormatInt(maxRev, 10)},
				OwnerReferences: []metav1.OwnerReference{controllerRef(du)},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: &zero, Selector: sel, Template: *tmpl},
		}
		created, err := c.createTyped(kReplicaSets, rs, gvkReplicaSet)
		if err != nil {
			log.Printf("creating ReplicaSet %s/%s: %v", ns, rsName, err)
			return
		}
		if created == nil {
			return
		}
		newRS = created
		rss = append(rss, created)
	} else if revisionOf(newRS) != maxRev { // rolled back to an older template
		maxRev++
		want := strconv.FormatInt(maxRev, 10)
		c.mutate(kReplicaSets, ns, rsName, func(cur *U) bool {
			a := cur.GetAnnotations()
			if a == nil {
				a = map[string]string{}
			}
			a[revisionAnnotation] = want
			cur.SetAnnotations(a)
			return true
		})
	}

	// ---- rollout: surge the new ReplicaSet up and the old ones down, step by step
	sort.Slice(old, func(i, j int) bool { return revisionOf(old[i]) < revisionOf(old[j]) })
	minAvail := desired
	var newTarget int32
	if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		minAvail = 0
		var oldPods int64
		for _, r := range old {
			oldPods += gi(r, "status", "replicas") + int64(specReplicas(r))
			c.scaleRS(du, r, 0)
		}
		if oldPods == 0 {
			newTarget = desired
		}
	} else {
		surge, unavail := rollingParams(&d, desired)
		minAvail = max(desired-unavail, 0)
		views := make([]rsView, len(old))
		var oldSpec int32
		for i, r := range old {
			v := rsView{u: r, spec: specReplicas(r), avail: int32(gi(r, "status", "availableReplicas"))}
			if v.spec > v.avail { // replicas that never became available go first
				c.scaleRS(du, r, v.avail)
				v.spec = v.avail
			}
			views[i] = v
			oldSpec += v.spec
		}
		newSpec := specReplicas(newRS)
		if newSpec > desired {
			newTarget = desired
		} else {
			allowed := max(desired+surge-(newSpec+oldSpec), 0)
			newTarget = min(desired, newSpec+allowed)
		}
		newAvail := min(int32(gi(newRS, "status", "availableReplicas")), newTarget)
		maxDown := newTarget + oldSpec - minAvail - (newTarget - newAvail)
		for _, v := range views {
			if maxDown <= 0 {
				break
			}
			if dec := min(v.spec, maxDown); dec > 0 {
				c.scaleRS(du, v.u, v.spec-dec)
				maxDown -= dec
			}
		}
	}
	c.scaleRS(du, newRS, newTarget)

	// keep at most revisionHistoryLimit old ReplicaSets
	limit := 10
	if d.Spec.RevisionHistoryLimit != nil {
		limit = int(*d.Spec.RevisionHistoryLimit)
	}
	for len(old) > limit {
		if specReplicas(old[0]) == 0 && gi(old[0], "status", "replicas") == 0 {
			_, _, _ = c.st.Delete(kReplicaSets, ns, old[0].GetName())
		}
		old = old[1:]
	}

	// ---- status
	var total, ready, avail, updated int32
	for _, r := range rss {
		n := int32(gi(r, "status", "replicas"))
		total += n
		ready += int32(gi(r, "status", "readyReplicas"))
		avail += int32(gi(r, "status", "availableReplicas"))
		if r.GetName() == rsName {
			updated = n
		}
	}
	st := d.Status.DeepCopy()
	st.ObservedGeneration = du.GetGeneration()
	st.Replicas, st.UpdatedReplicas, st.ReadyReplicas, st.AvailableReplicas = total, updated, ready, avail
	st.UnavailableReplicas = 0
	if desired > avail {
		st.UnavailableReplicas = desired - avail
	}
	if avail >= minAvail {
		setDeployCond(&st.Conditions, appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "Deployment has minimum availability.")
	} else {
		setDeployCond(&st.Conditions, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
	}
	if updated == desired && total == desired && avail == desired {
		setDeployCond(&st.Conditions, appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable", fmt.Sprintf("ReplicaSet %q has successfully progressed.", rsName))
	} else {
		setDeployCond(&st.Conditions, appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", fmt.Sprintf("ReplicaSet %q is progressing.", rsName))
	}
	c.setStatus(kDeployments, ns, d.Name, statusMap(st))

	if want := strconv.FormatInt(maxRev, 10); maxRev > 0 && du.GetAnnotations()[revisionAnnotation] != want {
		c.mutate(kDeployments, ns, d.Name, func(cur *U) bool {
			a := cur.GetAnnotations()
			if a == nil {
				a = map[string]string{}
			}
			a[revisionAnnotation] = want
			cur.SetAnnotations(a)
			return true
		})
	}
}

// ---------------------------------------------------------------- stateful sets

func ordinalOf(podName, stsName string) int {
	if !strings.HasPrefix(podName, stsName+"-") {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimPrefix(podName, stsName+"-"))
	if err != nil {
		return -1
	}
	return n
}

func (c *Controller) syncStatefulSet(su *U) {
	var sts appsv1.StatefulSet
	if err := fromU(su, &sts); err != nil {
		return
	}
	ns := su.GetNamespace()
	replicas := 1
	if sts.Spec.Replicas != nil {
		replicas = int(*sts.Spec.Replicas)
	}
	rev := sts.Name + "-" + templateHash(&sts.Spec.Template)
	ordered := sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement
	const component = "statefulset-controller"

	byOrd := map[int]*U{}
	outdated := -1
	for _, p := range activePods(c.ownedBy(kPods, ns, su.GetUID())) {
		ord := ordinalOf(p.GetName(), sts.Name)
		if ord < 0 || ord >= replicas { // scale down
			if _, _, err := c.st.Delete(kPods, ns, p.GetName()); err == nil {
				c.ev.Eventf(su, events.Normal, "SuccessfulDelete", component, "", "delete Pod %s in StatefulSet %s successful", p.GetName(), sts.Name)
			}
			continue
		}
		byOrd[ord] = p
		if p.GetLabels()["controller-revision-hash"] != rev && ord > outdated {
			outdated = ord
		}
	}
	// rolling update: replace the highest outdated pod once everything else is ready
	if outdated >= 0 {
		allReady := true
		for _, p := range byOrd {
			if !isReady(p) {
				allReady = false
			}
		}
		if allReady {
			victim := byOrd[outdated]
			if _, _, err := c.st.Delete(kPods, ns, victim.GetName()); err == nil {
				c.ev.Eventf(su, events.Normal, "SuccessfulDelete", component, "", "delete Pod %s in StatefulSet %s successful", victim.GetName(), sts.Name)
				delete(byOrd, outdated)
			}
		}
	}
	created := 0
	for i := 0; i < replicas; i++ {
		if p, ok := byOrd[i]; ok {
			if ordered && !isReady(p) {
				break // OrderedReady: wait for this pod before creating the next
			}
			continue
		}
		name := fmt.Sprintf("%s-%d", sts.Name, i)
		extra := map[string]string{
			"controller-revision-hash":           rev,
			"statefulset.kubernetes.io/pod-name": name,
			"apps.kubernetes.io/pod-index":       strconv.Itoa(i),
		}
		if err := c.createPod(su, &sts.Spec.Template, name, "", extra); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
			break
		}
		c.ev.Eventf(su, events.Normal, "SuccessfulCreate", component, "", "create Pod %s in StatefulSet %s successful", name, sts.Name)
		created++
		if ordered {
			break
		}
	}
	var ready, updated int32
	for _, p := range byOrd {
		if isReady(p) {
			ready++
		}
		if p.GetLabels()["controller-revision-hash"] == rev {
			updated++
		}
	}
	current := sts.Status.CurrentRevision
	if current == "" || (int(updated) == replicas && int(ready) == replicas) {
		current = rev
	}
	status := appsv1.StatefulSetStatus{
		ObservedGeneration: su.GetGeneration(), Replicas: int32(len(byOrd) + created), ReadyReplicas: ready,
		AvailableReplicas: ready, CurrentReplicas: updated, UpdatedReplicas: updated,
		CurrentRevision: current, UpdateRevision: rev,
	}
	c.setStatus(kStatefulSet, ns, sts.Name, statusMap(&status))
}

// ---------------------------------------------------------------- daemon sets

func (c *Controller) syncDaemonSet(du *U) {
	var ds appsv1.DaemonSet
	if err := fromU(du, &ds); err != nil {
		return
	}
	ns := du.GetNamespace()
	tmplPod := &corev1.Pod{ObjectMeta: *ds.Spec.Template.ObjectMeta.DeepCopy(), Spec: *ds.Spec.Template.Spec.DeepCopy()}
	nodes, _ := c.st.List(kNodes, "")
	eligible := map[string]bool{}
	var names []string
	for _, nu := range nodes { // nodes that match nodeSelector/affinity and whose taints are tolerated
		var n corev1.Node
		if fromU(nu, &n) != nil || nu.GetDeletionTimestamp() != nil {
			continue
		}
		if ok, _ := kubesim.NodeFits(&n, tmplPod); ok {
			eligible[n.Name] = true
			names = append(names, n.Name)
		}
	}
	hash := templateHash(&ds.Spec.Template)
	byNode := map[string]*U{}
	for _, p := range activePods(c.ownedBy(kPods, ns, du.GetUID())) {
		node := gs(p, "spec", "nodeName")
		if !eligible[node] || byNode[node] != nil || p.GetLabels()["controller-revision-hash"] != hash {
			_, _, _ = c.st.Delete(kPods, ns, p.GetName())
			continue
		}
		byNode[node] = p
	}
	created := 0
	for _, node := range names {
		if byNode[node] != nil {
			continue
		}
		extra := map[string]string{"controller-revision-hash": hash, "pod-template-generation": "1"}
		name := ds.Name + "-" + rand.String(5)
		if err := c.createPod(du, &ds.Spec.Template, name, node, extra); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
			continue
		}
		c.ev.Eventf(du, events.Normal, "SuccessfulCreate", "daemonset-controller", "", "Created pod: %s", name)
		created++
	}
	var ready int32
	for _, p := range byNode {
		if isReady(p) {
			ready++
		}
	}
	current := int32(len(byNode) + created)
	status := appsv1.DaemonSetStatus{
		ObservedGeneration: du.GetGeneration(), DesiredNumberScheduled: int32(len(names)), CurrentNumberScheduled: current,
		NumberReady: ready, UpdatedNumberScheduled: current, NumberAvailable: ready,
		NumberMisscheduled: 0, NumberUnavailable: int32(len(names)) - ready,
	}
	c.setStatus(kDaemonSets, ns, ds.Name, statusMap(&status))
}

// ---------------------------------------------------------------- jobs

func hasJobCond(conds []batchv1.JobCondition, t batchv1.JobConditionType) bool {
	for _, cd := range conds {
		if cd.Type == t && cd.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *Controller) syncJob(ju *U) {
	var job batchv1.Job
	if err := fromU(ju, &job); err != nil {
		return
	}
	ns := ju.GetNamespace()
	completions, parallelism, backoffLimit := int32(1), int32(1), int32(6)
	if job.Spec.Completions != nil {
		completions = *job.Spec.Completions
	}
	if job.Spec.Parallelism != nil {
		parallelism = *job.Spec.Parallelism
	}
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}
	var succeeded, failed, active, ready int32
	var running []*U
	for _, p := range c.ownedBy(kPods, ns, ju.GetUID()) {
		switch podPhase(p) {
		case "Succeeded":
			succeeded++
		case "Failed":
			failed++
		default:
			if p.GetDeletionTimestamp() == nil {
				active++
				running = append(running, p)
				if isReady(p) {
					ready++
				}
			}
		}
	}
	status := job.Status.DeepCopy()
	now := metav1.Now()
	if status.StartTime == nil {
		status.StartTime = &now
	}
	finished := hasJobCond(status.Conditions, batchv1.JobComplete) || hasJobCond(status.Conditions, batchv1.JobFailed)
	if !finished {
		switch {
		case failed > backoffLimit:
			for _, p := range running {
				_, _, _ = c.st.Delete(kPods, ns, p.GetName())
			}
			active, ready = 0, 0
			status.Conditions = append(status.Conditions, batchv1.JobCondition{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
				Message: "Job has reached the specified backoff limit", LastProbeTime: now, LastTransitionTime: now,
			})
			c.ev.Eventf(ju, events.Warning, "BackoffLimitExceeded", "job-controller", "", "Job has reached the specified backoff limit")
		case succeeded >= completions:
			status.CompletionTime = &now
			status.Conditions = append(status.Conditions, batchv1.JobCondition{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now,
			})
			c.ev.Eventf(ju, events.Normal, "Completed", "job-controller", "", "Job completed")
		default:
			extra := map[string]string{
				"batch.kubernetes.io/job-name": job.Name, "job-name": job.Name,
				"batch.kubernetes.io/controller-uid": string(ju.GetUID()), "controller-uid": string(ju.GetUID()),
			}
			n := min(completions-succeeded-active, parallelism-active)
			for i := int32(0); i < n; i++ {
				name := job.Name + "-" + rand.String(5)
				if err := c.createPod(ju, &job.Spec.Template, name, "", extra); err != nil {
					log.Printf("creating pod %s/%s: %v", ns, name, err)
					break
				}
				c.ev.Eventf(ju, events.Normal, "SuccessfulCreate", "job-controller", "", "Created pod: %s", name)
				active++
			}
		}
	}
	status.Succeeded, status.Failed, status.Active = succeeded, failed, active
	status.Ready, status.Terminating = ptr.To(ready), ptr.To(int32(0))
	c.setStatus(kJobs, ns, job.Name, statusMap(status))
}

// ---------------------------------------------------------------- endpoints

func resolveTargetPort(sp corev1.ServicePort, pod *corev1.Pod) int32 {
	if sp.TargetPort.IntVal != 0 {
		return sp.TargetPort.IntVal
	}
	if sp.TargetPort.StrVal != "" && pod != nil {
		for _, ct := range pod.Spec.Containers {
			for _, p := range ct.Ports {
				if p.Name == sp.TargetPort.StrVal {
					return p.ContainerPort
				}
			}
		}
	}
	return sp.Port
}

func (c *Controller) syncEndpoints() {
	svcs, _ := c.st.List(kServices, "")
	have := map[string]bool{}
	for _, su := range svcs {
		have[su.GetNamespace()+"/"+su.GetName()] = true
		if su.GetDeletionTimestamp() != nil {
			continue
		}
		var svc corev1.Service
		if fromU(su, &svc) != nil || len(svc.Spec.Selector) == 0 || svc.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		pods, _ := c.st.List(kPods, su.GetNamespace())
		var ready, notReady []corev1.EndpointAddress
		var first *corev1.Pod
		for _, pu := range pods {
			if pu.GetDeletionTimestamp() != nil || !sel.Matches(labels.Set(pu.GetLabels())) {
				continue
			}
			var p corev1.Pod
			if fromU(pu, &p) != nil || p.Status.PodIP == "" || terminalPhase(p.Status.Phase) {
				continue
			}
			addr := corev1.EndpointAddress{
				IP: p.Status.PodIP, NodeName: ptr.To(p.Spec.NodeName),
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID},
			}
			if isReady(pu) {
				if first == nil {
					first = &p
				}
				ready = append(ready, addr)
			} else {
				notReady = append(notReady, addr)
			}
		}
		var subsets []corev1.EndpointSubset
		if len(ready) > 0 || len(notReady) > 0 {
			var ports []corev1.EndpointPort
			for _, sp := range svc.Spec.Ports {
				ports = append(ports, corev1.EndpointPort{Name: sp.Name, Port: resolveTargetPort(sp, first), Protocol: sp.Protocol})
			}
			subsets = []corev1.EndpointSubset{{Addresses: ready, NotReadyAddresses: notReady, Ports: ports}}
		}
		c.upsertEndpoints(&svc, subsets)
	}
	eps, _ := c.st.List(kEndpoints, "")
	for _, ep := range eps {
		if ep.GetLabels()[managedLabel] == "true" && !have[ep.GetNamespace()+"/"+ep.GetName()] {
			_, _, _ = c.st.Delete(kEndpoints, ep.GetNamespace(), ep.GetName())
		}
	}
}

func terminalPhase(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

func (c *Controller) upsertEndpoints(svc *corev1.Service, subsets []corev1.EndpointSubset) {
	lbls := map[string]string{managedLabel: "true"}
	for k, v := range svc.Labels {
		lbls[k] = v
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace, Labels: lbls},
		Subsets:    subsets,
	}
	u, err := toU(ep, gvkEndpoints)
	if err != nil {
		return
	}
	if _, err := c.st.Get(kEndpoints, svc.Namespace, svc.Name); apierrors.IsNotFound(err) {
		initMeta(u)
		_, _ = c.st.Create(kEndpoints, u)
		return
	}
	c.mutate(kEndpoints, svc.Namespace, svc.Name, func(cur *U) bool {
		want, ok := u.Object["subsets"]
		if equality.Semantic.DeepEqual(cur.Object["subsets"], want) {
			return false
		}
		if ok {
			cur.Object["subsets"] = want
		} else {
			delete(cur.Object, "subsets")
		}
		return true
	})
}

// ---------------------------------------------------------------- garbage collection

// gc removes objects whose namespace is gone, dependents whose owners are all
// gone (the cascading delete a real cluster performs) and events older than an hour.
func (c *Controller) gc() {
	all := c.st.Snapshot()
	uids := map[types.UID]bool{}
	namespaces := map[string]bool{}
	for _, sn := range all {
		uids[sn.Obj.GetUID()] = true
		if sn.Key == kNamespaces {
			namespaces[sn.Obj.GetName()] = true
		}
	}
	for _, sn := range all {
		o := sn.Obj
		if ns := o.GetNamespace(); ns != "" && !namespaces[ns] {
			_, _, _ = c.st.Delete(sn.Key, ns, o.GetName())
			continue
		}
		if sn.Key == kEvents {
			if ts, err := time.Parse(time.RFC3339, gs(o, "lastTimestamp")); err == nil && time.Since(ts) > time.Hour {
				_, _, _ = c.st.Delete(sn.Key, o.GetNamespace(), o.GetName())
			}
			continue
		}
		refs := o.GetOwnerReferences()
		if len(refs) == 0 {
			continue
		}
		orphan := true
		for _, r := range refs {
			if uids[r.UID] {
				orphan = false
				break
			}
		}
		if orphan {
			_, _, _ = c.st.Delete(sn.Key, o.GetNamespace(), o.GetName())
		}
	}
}
