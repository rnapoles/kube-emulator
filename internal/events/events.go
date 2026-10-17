// Package events creates Kubernetes Event objects, so that `kubectl describe`
// and `kubectl get events` show what the simulated scheduler, kubelets and
// controllers did. Repeated events are aggregated (count / lastTimestamp)
// like the real event recorder does.
package events

import (
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

// Event types.
const (
	Normal  = "Normal"
	Warning = "Warning"
)

type U = store.U

var gvkEvent = schema.GroupVersionKind{Version: "v1", Kind: "Event"}

type recent struct {
	namespace, name string
	at              time.Time
}

// Recorder writes events into the store. A nil *Recorder is valid and records nothing.
type Recorder struct {
	st     *store.Store
	mu     sync.Mutex
	recent map[string]recent
}

// NewRecorder creates a recorder.
func NewRecorder(st *store.Store) *Recorder {
	return &Recorder{st: st, recent: map[string]recent{}}
}

// Eventf records an event about obj (a stored object).
func (r *Recorder) Eventf(obj *U, typ, reason, component, host, format string, args ...interface{}) {
	r.record(obj, "", typ, reason, component, host, fmt.Sprintf(format, args...))
}

// EventfField is like Eventf for an event about part of the object, for
// example fieldPath "spec.containers{nginx}".
func (r *Recorder) EventfField(obj *U, fieldPath, typ, reason, component, host, format string, args ...interface{}) {
	r.record(obj, fieldPath, typ, reason, component, host, fmt.Sprintf(format, args...))
}

func (r *Recorder) record(obj *U, fieldPath, typ, reason, component, host, message string) {
	if r == nil || obj == nil {
		return
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = "default" // events about cluster-scoped objects live in "default"
	}
	key := string(obj.GetUID()) + "|" + fieldPath + "|" + reason + "|" + message
	now := time.Now()

	r.mu.Lock()
	prev, seen := r.recent[key]
	if len(r.recent) > 4096 { // forget old entries
		for k, v := range r.recent {
			if now.Sub(v.at) > 10*time.Minute {
				delete(r.recent, k)
			}
		}
	}
	r.mu.Unlock()

	if seen {
		_, err := r.st.Mutate(registry.KEvents, prev.namespace, prev.name, func(cur *U) (*U, error) {
			count, _, _ := unstructured.NestedInt64(cur.Object, "count")
			_ = unstructured.SetNestedField(cur.Object, count+1, "count")
			_ = unstructured.SetNestedField(cur.Object, now.UTC().Format(time.RFC3339), "lastTimestamp")
			return cur, nil
		}, false)
		if err == nil {
			r.mu.Lock()
			r.recent[key] = recent{namespace: prev.namespace, name: prev.name, at: now}
			r.mu.Unlock()
			return
		}
		// the event was deleted (TTL or namespace): fall through and create a new one
	}

	ts := metav1.NewTime(now)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", obj.GetName(), now.UnixNano()),
			Namespace: ns,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(),
			APIVersion: obj.GetAPIVersion(), ResourceVersion: obj.GetResourceVersion(), FieldPath: fieldPath,
		},
		Reason:              reason,
		Message:             message,
		Source:              corev1.EventSource{Component: component, Host: host},
		FirstTimestamp:      ts,
		LastTimestamp:       ts,
		Count:               1,
		Type:                typ,
		ReportingController: component,
		ReportingInstance:   host,
	}
	u, err := objects.ToU(ev, gvkEvent)
	if err != nil {
		log.Printf("events: %v", err)
		return
	}
	objects.InitMeta(u)
	created, err := r.st.Create(registry.KEvents, u)
	if err != nil {
		log.Printf("events: creating %s/%s: %v", ns, ev.Name, err)
		return
	}
	r.mu.Lock()
	r.recent[key] = recent{namespace: created.GetNamespace(), name: created.GetName(), at: now}
	r.mu.Unlock()
}
