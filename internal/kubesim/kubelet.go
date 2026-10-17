package kubesim

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/rnapoles/kube-emulator/internal/events"
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/store"
)

type stage int

const (
	stNew         stage = iota // bound to a node, the kubelet starts creating containers
	stPulling                  // ContainerCreating: pulling the image
	stPullBackoff              // ErrImagePull <-> ImagePullBackOff
	stStarting                 // image pulled, creating the container
	stRunning                  // started, waiting for readiness / crash / completion
	stReady                    // steady state
	stBackoff                  // CrashLoopBackOff
	stDone                     // Succeeded or Failed
)

// track is the in-memory progress of one pod. It is rebuilt from the pod's
// status after a restart of the emulator.
type track struct {
	stage       stage
	deadline    time.Time
	restarts    int32
	cycles      int32 // image pull failures so far
	showBackOff bool  // currently ImagePullBackOff (as opposed to ErrImagePull)
	killing     bool
	orphanSince time.Time
}

// Kubelet moves pods bound to a node through their life. Nothing is executed:
// it only edits status and emits the events a real kubelet would.
type Kubelet struct {
	st     *store.Store
	ev     *events.Recorder
	ipam   *objects.IPAM
	d      Delays
	tracks map[types.UID]*track
	nodeIP map[string]string // node name -> internal IP, for existing nodes
}

func NewKubelet(st *store.Store, ev *events.Recorder, ipam *objects.IPAM, d Delays) *Kubelet {
	return &Kubelet{st: st, ev: ev, ipam: ipam, d: d, tracks: map[types.UID]*track{}, nodeIP: map[string]string{}}
}

func (k *Kubelet) Run(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safely("kubelet", k.once)
		}
	}
}

func (k *Kubelet) once() {
	k.nodeIP = map[string]string{}
	nodes, _ := k.st.List(kNodes, "")
	for _, nu := range nodes {
		var n corev1.Node
		if fromU(nu, &n) != nil {
			continue
		}
		ip := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ip = a.Address
			}
		}
		k.nodeIP[n.Name] = ip
	}
	pods, _ := k.st.List(kPods, "")
	live := map[types.UID]bool{}
	for _, pu := range pods {
		live[pu.GetUID()] = true
		k.syncPod(pu)
	}
	for uid := range k.tracks {
		if !live[uid] {
			delete(k.tracks, uid)
		}
	}
}

// ---- events and status helpers ----

func (k *Kubelet) event(obj *U, typ, reason, format string, args ...interface{}) {
	k.ev.Eventf(obj, typ, reason, "kubelet", gs(obj, "spec", "nodeName"), format, args...)
}

func (k *Kubelet) cevent(obj *U, container, typ, reason, format string, args ...interface{}) {
	k.ev.EventfField(obj, fmt.Sprintf("spec.containers{%s}", container), typ, reason, "kubelet", gs(obj, "spec", "nodeName"), format, args...)
}

// write stores a new pod status; it returns the updated pod, or nil on failure.
func (k *Kubelet) write(pu *U, st *corev1.PodStatus) *U {
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(st)
	if err != nil {
		return nil
	}
	updated, err := k.st.Mutate(kPods, pu.GetNamespace(), pu.GetName(), func(cur *U) (*U, error) {
		if cur.GetUID() != pu.GetUID() || cur.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("pod changed")
		}
		if equality.Semantic.DeepEqual(cur.Object["status"], m) {
			return nil, nil
		}
		cur.Object["status"] = m
		return cur, nil
	}, false)
	if err != nil {
		return nil
	}
	return updated
}

func containerID(pod *corev1.Pod, c *corev1.Container) string {
	return fmt.Sprintf("emulator://%s-%s", pod.UID, c.Name)
}

// buildStatus creates the pod status for a phase. fn sets the state of every
// container; it receives the previous status, so restart counts survive.
func (k *Kubelet) buildStatus(pod *corev1.Pod, phase corev1.PodPhase, ready bool, notReadyReason string,
	fn func(c *corev1.Container, cs *corev1.ContainerStatus)) corev1.PodStatus {
	now := metav1.Now()
	old := &pod.Status
	st := corev1.PodStatus{
		Phase: phase, QOSClass: old.QOSClass, PodIP: old.PodIP, PodIPs: old.PodIPs, StartTime: old.StartTime,
		HostIP:     k.nodeIP[pod.Spec.NodeName],
		Conditions: append([]corev1.PodCondition(nil), old.Conditions...),
	}
	if st.HostIP != "" {
		st.HostIPs = []corev1.HostIP{{IP: st.HostIP}}
	}
	if st.StartTime == nil {
		st.StartTime = &now
	}
	if st.QOSClass == "" {
		st.QOSClass = objects.QOSClass(pod)
	}
	var names []string
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		cs := corev1.ContainerStatus{Name: c.Name}
		for j := range old.ContainerStatuses {
			if old.ContainerStatuses[j].Name == c.Name {
				cs = old.ContainerStatuses[j]
			}
		}
		cs.Image, cs.ImageID = c.Image, "emulator://"+c.Image
		fn(c, &cs)
		st.ContainerStatuses = append(st.ContainerStatuses, cs)
		names = append(names, c.Name)
	}
	// terminal pods: containers are not "unready", the pod is completed
	condReason, condMsg := notReadyReason, ""
	if notReadyReason == "ContainersNotReady" {
		condMsg = fmt.Sprintf("containers with unready status: [%s]", strings.Join(names, " "))
	}
	setCond(&st.Conditions, corev1.PodCondition{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue}, now)
	setCond(&st.Conditions, corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}, now)
	setCond(&st.Conditions, corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, now)
	status := corev1.ConditionFalse
	if ready {
		status, condReason, condMsg = corev1.ConditionTrue, "", ""
	}
	setCond(&st.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: status, Reason: condReason, Message: condMsg}, now)
	setCond(&st.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: status, Reason: condReason, Message: condMsg}, now)
	return st
}

func waiting(reason, msg string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
}

func startedAt(cs *corev1.ContainerStatus, def metav1.Time) metav1.Time {
	if cs.State.Running != nil {
		return cs.State.Running.StartedAt
	}
	return def
}

func isReadyPod(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ---- the state machine ----

func (k *Kubelet) initTrack(pod *corev1.Pod, now time.Time) *track {
	t := &track{stage: stNew, deadline: now}
	if pod.Status.Phase == corev1.PodRunning {
		t.stage = stRunning
		if isReadyPod(pod) {
			t.stage = stReady
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.RestartCount > t.restarts {
				t.restarts = cs.RestartCount
			}
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				t.stage = stBackoff
			}
		}
	}
	return t
}

func (k *Kubelet) syncPod(pu *U) {
	uid := pu.GetUID()
	t := k.tracks[uid]
	// fast path: pods in a steady state need no work (and no conversion)
	if t != nil && pu.GetDeletionTimestamp() == nil && (t.stage == stReady || t.stage == stDone) {
		if _, ok := k.nodeIP[gs(pu, "spec", "nodeName")]; ok {
			return
		}
	}
	var pod corev1.Pod
	if err := fromU(pu, &pod); err != nil {
		return
	}
	now := time.Now()
	if pu.GetDeletionTimestamp() != nil {
		k.terminate(pu, &pod, t, now)
		return
	}
	node := pod.Spec.NodeName
	if node == "" || terminal(pod.Status.Phase) {
		return
	}
	if _, ok := k.nodeIP[node]; !ok { // the node was deleted: garbage collect the pod after a while
		if t == nil {
			t = &track{stage: stNew}
			k.tracks[uid] = t
		}
		if t.orphanSince.IsZero() {
			t.orphanSince = now
		} else if now.Sub(t.orphanSince) > 10*time.Second {
			k.forceDelete(pu)
		}
		return
	}
	if t == nil {
		t = k.initTrack(&pod, now)
		k.tracks[uid] = t
	}
	t.orphanSince = time.Time{}
	if now.Before(t.deadline) {
		return
	}
	d := k.d.forPod(pod.Annotations)

	switch t.stage {
	case stNew:
		st := k.buildStatus(&pod, corev1.PodPending, false, "ContainersNotReady", func(c *corev1.Container, cs *corev1.ContainerStatus) {
			cs.State, cs.Ready, cs.Started = waiting("ContainerCreating", ""), false, ptr.To(false)
		})
		up := k.write(pu, &st)
		if up == nil {
			return
		}
		for _, c := range pod.Spec.Containers {
			k.cevent(up, c.Name, events.Normal, "Pulling", "Pulling image %q", c.Image)
		}
		t.stage, t.deadline = stPulling, now.Add(d.Pull)

	case stPulling:
		if annBool(pod.Annotations, AnnFailImagePull) {
			k.failPull(pu, &pod, t, now, "ErrImagePull")
			return
		}
		took := d.Pull.Round(time.Millisecond)
		for _, c := range pod.Spec.Containers {
			k.cevent(pu, c.Name, events.Normal, "Pulled", "Successfully pulled image %q in %s (%s including waiting). Image size: 0 bytes.", c.Image, took, took)
		}
		t.stage, t.deadline = stStarting, now.Add(d.Start)

	case stPullBackoff:
		if !t.showBackOff { // ErrImagePull is over: back off
			k.failPull(pu, &pod, t, now, "ImagePullBackOff")
			t.deadline = now.Add(backoffFor(d.Backoff, t.cycles))
			return
		}
		for _, c := range pod.Spec.Containers { // back-off is over: try again
			k.cevent(pu, c.Name, events.Normal, "Pulling", "Pulling image %q", c.Image)
		}
		t.stage, t.deadline = stPulling, now.Add(d.Pull)

	case stStarting:
		k.startContainers(pu, &pod, t, d, now, false)

	case stBackoff:
		k.startContainers(pu, &pod, t, d, now, true)

	case stRunning:
		k.running(pu, &pod, t, d, now)
	}
}

// failPull puts the containers into ErrImagePull or ImagePullBackOff.
func (k *Kubelet) failPull(pu *U, pod *corev1.Pod, t *track, now time.Time, reason string) {
	backoff := reason == "ImagePullBackOff"
	st := k.buildStatus(pod, corev1.PodPending, false, "ContainersNotReady", func(c *corev1.Container, cs *corev1.ContainerStatus) {
		msg := fmt.Sprintf("rpc error: code = NotFound desc = failed to pull and unpack image %q: not found", c.Image)
		if backoff {
			msg = fmt.Sprintf("Back-off pulling image %q", c.Image)
		}
		cs.State, cs.Ready, cs.Started = waiting(reason, msg), false, ptr.To(false)
	})
	up := k.write(pu, &st)
	if up == nil {
		return
	}
	for _, c := range pod.Spec.Containers {
		if backoff {
			k.cevent(up, c.Name, events.Warning, "BackOff", "Back-off pulling image %q", c.Image)
			k.cevent(up, c.Name, events.Warning, "Failed", "Error: ImagePullBackOff")
		} else {
			k.cevent(up, c.Name, events.Warning, "Failed", "Failed to pull image %q: rpc error: code = NotFound desc = failed to pull and unpack image %q: not found", c.Image, c.Image)
			k.cevent(up, c.Name, events.Warning, "Failed", "Error: ErrImagePull")
		}
	}
	t.stage = stPullBackoff
	t.showBackOff = backoff
	if !backoff {
		t.cycles++
		t.deadline = now.Add(time.Second) // ErrImagePull is shown briefly before the back-off
	}
}

// startContainers creates and starts the containers (the first time, or after a crash back-off).
func (k *Kubelet) startContainers(pu *U, pod *corev1.Pod, t *track, d Delays, now time.Time, restart bool) {
	if pod.Status.PodIP == "" {
		ip := k.ipam.PodIP()
		pod.Status.PodIP, pod.Status.PodIPs = ip, []corev1.PodIP{{IP: ip}}
	}
	started := metav1.Now()
	st := k.buildStatus(pod, corev1.PodRunning, false, "ContainersNotReady", func(c *corev1.Container, cs *corev1.ContainerStatus) {
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}
		cs.Ready, cs.Started, cs.ContainerID = false, ptr.To(true), containerID(pod, c)
		cs.RestartCount = t.restarts
	})
	up := k.write(pu, &st)
	if up == nil {
		return
	}
	for _, c := range pod.Spec.Containers {
		if restart {
			k.cevent(up, c.Name, events.Normal, "Pulled", "Container image %q already present on machine", c.Image)
		}
		k.cevent(up, c.Name, events.Normal, "Created", "Created container %s", c.Name)
		k.cevent(up, c.Name, events.Normal, "Started", "Started container %s", c.Name)
	}
	t.stage = stRunning
	switch {
	case annBool(pod.Annotations, AnnCrash):
		t.deadline = now.Add(annDuration(pod.Annotations, AnnCrashAfter, time.Second))
	case pod.Spec.RestartPolicy != corev1.RestartPolicyAlways: // run to completion (Jobs)
		t.deadline = now.Add(d.Job)
	default:
		t.deadline = now.Add(d.Ready)
	}
}

// running handles the end of the "running, not ready yet" period.
func (k *Kubelet) running(pu *U, pod *corev1.Pod, t *track, d Delays, now time.Time) {
	finished := metav1.Now()
	switch {
	case annBool(pod.Annotations, AnnCrash):
		exit := annInt(pod.Annotations, AnnCrashExitCode, 1)
		if pod.Spec.RestartPolicy == corev1.RestartPolicyNever {
			st := k.buildStatus(pod, corev1.PodFailed, false, "PodFailed", func(c *corev1.Container, cs *corev1.ContainerStatus) {
				cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: "Error", StartedAt: startedAt(cs, finished), FinishedAt: finished, ContainerID: containerID(pod, c),
				}}
				cs.Ready, cs.Started = false, ptr.To(false)
			})
			k.write(pu, &st)
			t.stage = stDone
			return
		}
		t.restarts++
		wait := backoffFor(d.Backoff, t.restarts)
		st := k.buildStatus(pod, corev1.PodRunning, false, "ContainersNotReady", func(c *corev1.Container, cs *corev1.ContainerStatus) {
			cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: exit, Reason: "Error", StartedAt: startedAt(cs, finished), FinishedAt: finished, ContainerID: containerID(pod, c),
			}}
			cs.State = waiting("CrashLoopBackOff", fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)",
				wait, c.Name, pod.Name, pod.Namespace, pod.UID))
			cs.Ready, cs.Started, cs.RestartCount = false, ptr.To(false), t.restarts
		})
		up := k.write(pu, &st)
		if up == nil {
			return
		}
		for _, c := range pod.Spec.Containers {
			k.cevent(up, c.Name, events.Warning, "BackOff", "Back-off restarting failed container %s in pod %s_%s(%s)", c.Name, pod.Name, pod.Namespace, pod.UID)
		}
		t.stage, t.deadline = stBackoff, now.Add(wait)

	case pod.Spec.RestartPolicy != corev1.RestartPolicyAlways: // finished successfully
		st := k.buildStatus(pod, corev1.PodSucceeded, false, "PodCompleted", func(c *corev1.Container, cs *corev1.ContainerStatus) {
			cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed", StartedAt: startedAt(cs, finished), FinishedAt: finished, ContainerID: containerID(pod, c),
			}}
			cs.Ready, cs.Started = false, ptr.To(false)
		})
		k.write(pu, &st)
		t.stage = stDone

	case annBool(pod.Annotations, AnnFailReadiness): // Running, but never Ready
		k.event(pu, events.Warning, "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503")
		t.deadline = now.Add(10 * time.Second)

	default: // readiness probe succeeds
		st := k.buildStatus(pod, corev1.PodRunning, true, "", func(c *corev1.Container, cs *corev1.ContainerStatus) {
			cs.Ready, cs.Started = true, ptr.To(true)
		})
		if k.write(pu, &st) != nil {
			t.stage = stReady
		}
	}
}

// terminate handles a pod with a deletionTimestamp: Killing events, then the
// kubelet finalizer is removed so that the pod disappears.
func (k *Kubelet) terminate(pu *U, pod *corev1.Pod, t *track, now time.Time) {
	if t == nil {
		t = &track{}
		k.tracks[pu.GetUID()] = t
	}
	if !t.killing {
		t.killing = true
		wait := time.Duration(0)
		if pod.Spec.NodeName != "" {
			if pod.Status.Phase == corev1.PodRunning {
				for _, c := range pod.Spec.Containers {
					k.cevent(pu, c.Name, events.Normal, "Killing", "Stopping container %s", c.Name)
				}
			}
			wait = k.d.forPod(pod.Annotations).Terminate
			if g := pod.Spec.TerminationGracePeriodSeconds; g != nil && time.Duration(*g)*time.Second < wait {
				wait = time.Duration(*g) * time.Second
			}
		}
		t.deadline = now.Add(wait)
	}
	if now.Before(t.deadline) {
		return
	}
	k.dropFinalizer(pu, false)
}

func (k *Kubelet) forceDelete(pu *U) { k.dropFinalizer(pu, true) }

// dropFinalizer removes the kubelet finalizer (and marks the pod as deleted if
// requested); the store removes the pod once no finalizer is left.
func (k *Kubelet) dropFinalizer(pu *U, markDeleted bool) {
	_, _ = k.st.Mutate(kPods, pu.GetNamespace(), pu.GetName(), func(cur *U) (*U, error) {
		if cur.GetUID() != pu.GetUID() {
			return nil, nil
		}
		var rest []string
		for _, f := range cur.GetFinalizers() {
			if f != objects.KubeletFinalizer {
				rest = append(rest, f)
			}
		}
		changed := len(rest) != len(cur.GetFinalizers())
		if markDeleted && cur.GetDeletionTimestamp() == nil {
			now := metav1.Now()
			cur.SetDeletionTimestamp(&now)
			changed = true
		}
		if !changed {
			return nil, nil
		}
		cur.SetFinalizers(rest)
		return cur, nil
	}, false)
}
