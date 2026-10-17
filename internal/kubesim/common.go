// Package kubesim simulates the parts of a cluster that make workloads move:
// a node controller (fake nodes with labels, taints and capacity), a
// scheduler that honours nodeSelector, affinity, taints and resources, and
// kubelets that walk pods through Pending -> ContainerCreating -> Running
// (or ImagePullBackOff, CrashLoopBackOff, not-ready, completed) with
// configurable delays, emitting events on the way. No container is ever run.
package kubesim

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rnapoles/kube-emulator/internal/events"
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

type U = store.U

var (
	kPods  = registry.KPods
	kNodes = registry.KNodes

	fromU = objects.FromU
	gs    = objects.GS
)

// Pod annotations that steer the simulation of one pod (put them on the pod
// template of a Deployment/Job/... to affect all of its pods).
const (
	AnnPrefix        = "emulator.kube/"
	AnnFailImagePull = AnnPrefix + "fail-image-pull" // "true": ErrImagePull / ImagePullBackOff forever
	AnnCrash         = AnnPrefix + "crash"           // "true": the container exits and restarts (CrashLoopBackOff)
	AnnCrashAfter    = AnnPrefix + "crash-after"     // how long it runs before crashing (default 1s)
	AnnCrashExitCode = AnnPrefix + "crash-exit-code" // exit code (default 1)
	AnnFailReadiness = AnnPrefix + "fail-readiness"  // "true": Running but never Ready
	// AnnPrefix+"delay-<name>" (schedule, pull, start, ready, job, terminate, backoff)
	// overrides a delay for this pod, see Delays.
)

func annBool(a map[string]string, key string) bool {
	v, err := strconv.ParseBool(a[key])
	return err == nil && v
}

func annDuration(a map[string]string, key string, def time.Duration) time.Duration {
	if v, ok := a[key]; ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func annInt(a map[string]string, key string, def int32) int32 {
	if v, ok := a[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return int32(n)
		}
	}
	return def
}

// setCond adds or updates a pod condition, keeping lastTransitionTime
// unchanged unless the status changes.
func setCond(conds *[]corev1.PodCondition, c corev1.PodCondition, now metav1.Time) {
	for i := range *conds {
		if (*conds)[i].Type != c.Type {
			continue
		}
		old := (*conds)[i]
		if old.Status == c.Status && old.Reason == c.Reason && old.Message == c.Message {
			return
		}
		if old.Status == c.Status {
			c.LastTransitionTime = old.LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		(*conds)[i] = c
		return
	}
	c.LastTransitionTime = now
	*conds = append(*conds, c)
}

// Sim runs the node controller, the scheduler and the kubelets.
type Sim struct {
	nodes     *nodeController
	scheduler *Scheduler
	kubelet   *Kubelet
}

// New creates the simulation.
func New(st *store.Store, ev *events.Recorder, ipam *objects.IPAM, d Delays, no NodeOptions) *Sim {
	return &Sim{
		nodes:     newNodeController(st, no),
		scheduler: NewScheduler(st, ev, d),
		kubelet:   NewKubelet(st, ev, ipam, d),
	}
}

// Run blocks until ctx is cancelled.
func (s *Sim) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, run := range []func(context.Context){s.nodes.Run, s.scheduler.Run, s.kubelet.Run} {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(run)
	}
	wg.Wait()
}

// safely runs fn and logs a panic instead of crashing the process.
func safely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: panic: %v", name, r)
		}
	}()
	fn()
}
