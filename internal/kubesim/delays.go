package kubesim

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Delays controls how long each step of a pod's life takes.
type Delays struct {
	Schedule  time.Duration // Pending until the scheduler binds the pod
	Pull      time.Duration // image pull (ContainerCreating)
	Start     time.Duration // container creation after the pull
	Ready     time.Duration // Running until the readiness probe succeeds
	Job       time.Duration // how long run-to-completion pods (Jobs) run
	Terminate time.Duration // Terminating until the pod disappears (capped by its grace period)
	Backoff   time.Duration // first CrashLoopBackOff / ImagePullBackOff wait; doubles up to 5m
}

// DefaultDelays returns the built-in delays.
func DefaultDelays() Delays {
	return Delays{
		Schedule:  300 * time.Millisecond,
		Pull:      1 * time.Second,
		Start:     300 * time.Millisecond,
		Ready:     1 * time.Second,
		Job:       3 * time.Second,
		Terminate: 1 * time.Second,
		Backoff:   10 * time.Second,
	}
}

func (d *Delays) fields() map[string]*time.Duration {
	return map[string]*time.Duration{
		"schedule": &d.Schedule, "pull": &d.Pull, "start": &d.Start, "ready": &d.Ready,
		"job": &d.Job, "terminate": &d.Terminate, "backoff": &d.Backoff,
	}
}

// ParseDelays applies a setting such as "pull=3s,ready=5s" to base. The value
// "instant" (or "none") removes all waiting except the crash/pull back-off.
func ParseDelays(spec string, base Delays) (Delays, error) {
	spec = strings.TrimSpace(spec)
	switch strings.ToLower(spec) {
	case "":
		return base, nil
	case "instant", "none", "0":
		return Delays{Backoff: base.Backoff}, nil
	}
	d := base
	f := d.fields()
	for _, part := range strings.Split(spec, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return base, fmt.Errorf("invalid delay %q: expected name=duration", part)
		}
		p, ok := f[strings.ToLower(strings.TrimSpace(kv[0]))]
		if !ok {
			names := make([]string, 0, len(f))
			for n := range f {
				names = append(names, n)
			}
			sort.Strings(names)
			return base, fmt.Errorf("unknown delay %q (valid: %s)", kv[0], strings.Join(names, ", "))
		}
		v, err := time.ParseDuration(strings.TrimSpace(kv[1]))
		if err != nil || v < 0 {
			return base, fmt.Errorf("invalid duration %q for delay %q", kv[1], kv[0])
		}
		*p = v
	}
	return d, nil
}

// forPod applies the pod's emulator.kube/delay-<name> annotations.
func (d Delays) forPod(ann map[string]string) Delays {
	for name, p := range d.fields() {
		if v, ok := ann[AnnPrefix+"delay-"+name]; ok {
			if dur, err := time.ParseDuration(v); err == nil && dur >= 0 {
				*p = dur
			}
		}
	}
	return d
}

// backoffFor returns base doubled n-1 times, capped at five minutes.
func backoffFor(base time.Duration, n int32) time.Duration {
	d := base
	for i := int32(1); i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}
