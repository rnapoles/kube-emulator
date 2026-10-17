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
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

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

type Controller struct {
	st   *store.Store
	ipam *objects.IPAM
}

func New(st *store.Store, ipam *objects.IPAM) *Controller { return &Controller{st: st, ipam: ipam} }

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

func (c *Controller) createTyped(key ResourceKey, obj k8sruntime.Object, gvk schema.GroupVersionKind) error {
	u, err := toU(obj, gvk)
	if err != nil {
		return err
	}
	initMeta(u)
	if _, err := c.st.Create(key, u); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
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

func (c *Controller) setReplicas(key ResourceKey, u *U, n int32) {
	c.mutate(key, u.GetNamespace(), u.GetName(), func(cur *U) bool {
		if old, found, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas"); found && old == int64(n) {
			return false
		}
		_ = unstructured.SetNestedField(cur.Object, int64(n), "spec", "replicas")
		cur.SetGeneration(cur.GetGeneration() + 1)
		return true
	})
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

func (c *Controller) createPod(owner *U, tmpl *corev1.PodTemplateSpec, name, node string, extra map[string]string, phase corev1.PodPhase) error {
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
	if err := objects.SimulatePod(c.ipam, u, phase); err != nil {
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
			_ = c.createTyped(kSAs, sa, gvkSA)
		}
	}
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

func (c *Controller) syncDeployment(du *U) {
	var d appsv1.Deployment
	if err := fromU(du, &d); err != nil || d.Spec.Selector == nil {
		return
	}
	ns := du.GetNamespace()
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	hash := templateHash(&d.Spec.Template)
	rsName := d.Name + "-" + hash
	rss := c.ownedBy(kReplicaSets, ns, du.GetUID())

	var maxRev int64
	var newRS *U
	for _, r := range rss {
		if rev := revisionOf(r); rev > maxRev {
			maxRev = rev
		}
		if r.GetName() == rsName {
			newRS = r
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
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: rsName, Namespace: ns, Labels: tmpl.Labels,
				Annotations:     map[string]string{revisionAnnotation: strconv.FormatInt(maxRev, 10)},
				OwnerReferences: []metav1.OwnerReference{controllerRef(du)},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: &replicas, Selector: sel, Template: *tmpl},
		}
		if err := c.createTyped(kReplicaSets, rs, gvkReplicaSet); err != nil {
			log.Printf("creating ReplicaSet %s/%s: %v", ns, rsName, err)
		}
	} else {
		c.setReplicas(kReplicaSets, newRS, replicas)
		if revisionOf(newRS) != maxRev { // rollback to an older template
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
	}
	var old []*U
	for _, r := range rss {
		if r.GetName() != rsName {
			c.setReplicas(kReplicaSets, r, 0)
			old = append(old, r)
		}
	}
	// keep at most revisionHistoryLimit old ReplicaSets
	limit := 10
	if d.Spec.RevisionHistoryLimit != nil {
		limit = int(*d.Spec.RevisionHistoryLimit)
	}
	sort.Slice(old, func(i, j int) bool { return revisionOf(old[i]) < revisionOf(old[j]) })
	for len(old) > limit {
		if gi(old[0], "status", "replicas") == 0 {
			_, _, _ = c.st.Delete(kReplicaSets, ns, old[0].GetName())
		}
		old = old[1:]
	}

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
	if total > avail {
		st.UnavailableReplicas = total - avail
	}
	if avail >= replicas {
		setDeployCond(&st.Conditions, appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "Deployment has minimum availability.")
	} else {
		setDeployCond(&st.Conditions, appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
	}
	if updated == replicas && total == replicas && avail == replicas {
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

// ---------------------------------------------------------------- replica sets

// reconcilePods keeps exactly `desired` active pods owned by owner, creating
// them from tmpl or deleting the newest ones. It returns the number of ready
// pods (simulated pods are Running as soon as they are created).
func (c *Controller) reconcilePods(owner *U, tmpl *corev1.PodTemplateSpec, namePrefix string, desired int) int {
	ns := owner.GetNamespace()
	pods := activePods(c.ownedBy(kPods, ns, owner.GetUID()))
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].GetCreationTimestamp().Time.After(pods[j].GetCreationTimestamp().Time)
	})
	for len(pods) > desired { // scale down: newest first
		_, _, _ = c.st.Delete(kPods, ns, pods[0].GetName())
		pods = pods[1:]
	}
	ready := 0
	for _, p := range pods {
		if podPhase(p) == "Running" {
			ready++
		}
	}
	for i := len(pods); i < desired; i++ {
		name := namePrefix + "-" + rand.String(5)
		if err := c.createPod(owner, tmpl, name, "", nil, corev1.PodRunning); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
			continue
		}
		ready++
	}
	return ready
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
	ready := c.reconcilePods(ru, &rs.Spec.Template, rs.Name, desired)
	status := appsv1.ReplicaSetStatus{
		Replicas: int32(ready), FullyLabeledReplicas: int32(ready), ReadyReplicas: int32(ready),
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
	ready := c.reconcilePods(ru, rc.Spec.Template, rc.Name, desired)
	status := corev1.ReplicationControllerStatus{
		Replicas: int32(ready), FullyLabeledReplicas: int32(ready), ReadyReplicas: int32(ready),
		AvailableReplicas: int32(ready), ObservedGeneration: ru.GetGeneration(),
	}
	c.setStatus(kReplicationControllers, ru.GetNamespace(), rc.Name, statusMap(&status))
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
	byName := map[string]*U{}
	for _, p := range activePods(c.ownedBy(kPods, ns, su.GetUID())) {
		ord := ordinalOf(p.GetName(), sts.Name)
		if ord < 0 || ord >= replicas || p.GetLabels()["controller-revision-hash"] != rev {
			_, _, _ = c.st.Delete(kPods, ns, p.GetName()) // scale down or rolling update
			continue
		}
		byName[p.GetName()] = p
	}
	for i := 0; i < replicas; i++ {
		name := fmt.Sprintf("%s-%d", sts.Name, i)
		if _, ok := byName[name]; ok {
			continue
		}
		extra := map[string]string{
			"controller-revision-hash":           rev,
			"statefulset.kubernetes.io/pod-name": name,
			"apps.kubernetes.io/pod-index":       strconv.Itoa(i),
		}
		if err := c.createPod(su, &sts.Spec.Template, name, "", extra, corev1.PodRunning); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
		}
	}
	status := appsv1.StatefulSetStatus{
		ObservedGeneration: su.GetGeneration(), Replicas: int32(replicas), ReadyReplicas: int32(replicas),
		AvailableReplicas: int32(replicas), CurrentReplicas: int32(replicas), UpdatedReplicas: int32(replicas),
		CurrentRevision: rev, UpdateRevision: rev,
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
	nodes, _ := c.st.List(kNodes, "")
	nodeSet := map[string]bool{}
	for _, n := range nodes {
		nodeSet[n.GetName()] = true
	}
	hash := templateHash(&ds.Spec.Template)
	byNode := map[string]*U{}
	for _, p := range activePods(c.ownedBy(kPods, ns, du.GetUID())) {
		node := gs(p, "spec", "nodeName")
		if !nodeSet[node] || byNode[node] != nil || p.GetLabels()["controller-revision-hash"] != hash {
			_, _, _ = c.st.Delete(kPods, ns, p.GetName())
			continue
		}
		byNode[node] = p
	}
	for _, n := range nodes {
		if byNode[n.GetName()] != nil {
			continue
		}
		extra := map[string]string{"controller-revision-hash": hash, "pod-template-generation": "1"}
		name := ds.Name + "-" + rand.String(5)
		if err := c.createPod(du, &ds.Spec.Template, name, n.GetName(), extra, corev1.PodRunning); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
		}
	}
	n := int32(len(nodes))
	status := appsv1.DaemonSetStatus{
		ObservedGeneration: du.GetGeneration(), DesiredNumberScheduled: n, CurrentNumberScheduled: n,
		NumberReady: n, UpdatedNumberScheduled: n, NumberAvailable: n,
	}
	c.setStatus(kDaemonSets, ns, ds.Name, statusMap(&status))
}

// ---------------------------------------------------------------- jobs

func (c *Controller) syncJob(ju *U) {
	var job batchv1.Job
	if err := fromU(ju, &job); err != nil {
		return
	}
	ns := ju.GetNamespace()
	completions := int32(1)
	if job.Spec.Completions != nil {
		completions = *job.Spec.Completions
	}
	pods := activePods(c.ownedBy(kPods, ns, ju.GetUID()))
	extra := map[string]string{
		"batch.kubernetes.io/job-name": job.Name, "job-name": job.Name,
		"batch.kubernetes.io/controller-uid": string(ju.GetUID()), "controller-uid": string(ju.GetUID()),
	}
	// Pods "run" instantly and always succeed.
	succeeded := int32(len(pods))
	for i := len(pods); i < int(completions); i++ {
		name := job.Name + "-" + rand.String(5)
		if err := c.createPod(ju, &job.Spec.Template, name, "", extra, corev1.PodSucceeded); err != nil {
			log.Printf("creating pod %s/%s: %v", ns, name, err)
			continue
		}
		succeeded++
	}
	if succeeded > completions {
		succeeded = completions
	}
	status := job.Status.DeepCopy()
	now := metav1.Now()
	if status.StartTime == nil {
		status.StartTime = &now
	}
	status.Succeeded, status.Active, status.Ready, status.Terminating = succeeded, 0, ptr.To(int32(0)), ptr.To(int32(0))
	if succeeded >= completions {
		if status.CompletionTime == nil {
			status.CompletionTime = &now
		}
		found := false
		for _, cd := range status.Conditions {
			if cd.Type == batchv1.JobComplete {
				found = true
			}
		}
		if !found {
			status.Conditions = append(status.Conditions, batchv1.JobCondition{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now,
			})
		}
	}
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
		var addrs []corev1.EndpointAddress
		var first *corev1.Pod
		for _, pu := range pods {
			if pu.GetDeletionTimestamp() != nil || podPhase(pu) != "Running" || !sel.Matches(labels.Set(pu.GetLabels())) {
				continue
			}
			var p corev1.Pod
			if fromU(pu, &p) != nil || p.Status.PodIP == "" {
				continue
			}
			if first == nil {
				first = &p
			}
			addrs = append(addrs, corev1.EndpointAddress{
				IP: p.Status.PodIP, NodeName: ptr.To(p.Spec.NodeName),
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID},
			})
		}
		var subsets []corev1.EndpointSubset
		if len(addrs) > 0 {
			var ports []corev1.EndpointPort
			for _, sp := range svc.Spec.Ports {
				ports = append(ports, corev1.EndpointPort{Name: sp.Name, Port: resolveTargetPort(sp, first), Protocol: sp.Protocol})
			}
			subsets = []corev1.EndpointSubset{{Addresses: addrs, Ports: ports}}
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

// gc removes objects whose namespace is gone and dependents whose owners are
// all gone (the cascading delete a real cluster performs).
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
