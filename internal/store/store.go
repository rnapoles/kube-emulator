package store

import (
	"fmt"
	"sort"
	"strconv"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// U is a short alias used everywhere for unstructured objects.
type U = unstructured.Unstructured

const (
	historyCap = 4096 // watch events kept for resourceVersion replay
	subBuffer  = 4096 // per-watcher channel buffer
)

// ResourceKey identifies a storage bucket (all versions of a resource share it).
type ResourceKey struct{ Group, Resource string }

func (k ResourceKey) GR() schema.GroupResource {
	return schema.GroupResource{Group: k.Group, Resource: k.Resource}
}

// Record is one persisted object.
type Record struct {
	Key       ResourceKey
	Namespace string
	Name      string
	RV        uint64
	Obj       *U
}

// Persister is an optional write-through persistence layer (SQLite).
type Persister interface {
	Load() (recs []Record, lastRV uint64, err error)
	Save(rec Record, rv uint64) error
	Delete(key ResourceKey, ns, name string, rv uint64) error
	Close() error
}

// Event is a change notification.
type Event struct {
	Type watch.EventType
	Key  ResourceKey
	Obj  *U // immutable: never modify
	RV   uint64
}

type subscriber struct {
	key ResourceKey
	ns  string
	ch  chan Event
}

// Store keeps every object in memory (stored objects are immutable; every
// write replaces the entry with a fresh copy) and writes through to the
// Persister when one is configured.
type Store struct {
	mu      sync.Mutex
	data    map[ResourceKey]map[string]*U
	rv      uint64
	p       Persister
	history []Event
	floor   uint64
	subs    map[*subscriber]struct{}

	// Kick receives a signal after every committed write (used by controllers).
	Kick chan struct{}
}

func nsName(ns, name string) string { return ns + "/" + name }

func NewStore(p Persister) (*Store, error) {
	s := &Store{
		data: map[ResourceKey]map[string]*U{},
		p:    p,
		subs: map[*subscriber]struct{}{},
		Kick: make(chan struct{}, 1),
	}
	if p != nil {
		recs, last, err := p.Load()
		if err != nil {
			return nil, fmt.Errorf("loading persisted state: %w", err)
		}
		s.rv = last
		for _, r := range recs {
			m := s.data[r.Key]
			if m == nil {
				m = map[string]*U{}
				s.data[r.Key] = m
			}
			m[nsName(r.Namespace, r.Name)] = r.Obj
			if r.RV > s.rv {
				s.rv = r.RV
			}
		}
	}
	s.floor = s.rv // history before startup is not available
	return s, nil
}

// CurrentRV returns the latest resourceVersion.
func (s *Store) CurrentRV() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rv
}

func (s *Store) Close() error {
	if s.p != nil {
		return s.p.Close()
	}
	return nil
}

func (s *Store) emit(ev Event) {
	s.history = append(s.history, ev)
	if len(s.history) > 2*historyCap {
		drop := len(s.history) - historyCap
		s.floor = s.history[drop-1].RV
		s.history = append([]Event(nil), s.history[drop:]...)
	}
	for sub := range s.subs {
		if sub.key != ev.Key {
			continue
		}
		if sub.ns != "" && sub.ns != ev.Obj.GetNamespace() {
			continue
		}
		select {
		case sub.ch <- ev:
		default: // slow consumer: drop it, the client will re-watch
			close(sub.ch)
			delete(s.subs, sub)
		}
	}
	select {
	case s.Kick <- struct{}{}:
	default:
	}
}

func (s *Store) commitPut(key ResourceKey, obj *U, typ watch.EventType) error {
	rv := s.rv + 1
	obj.SetResourceVersion(strconv.FormatUint(rv, 10))
	if s.p != nil {
		rec := Record{Key: key, Namespace: obj.GetNamespace(), Name: obj.GetName(), RV: rv, Obj: obj}
		if err := s.p.Save(rec, rv); err != nil {
			return err
		}
	}
	s.rv = rv
	m := s.data[key]
	if m == nil {
		m = map[string]*U{}
		s.data[key] = m
	}
	m[nsName(obj.GetNamespace(), obj.GetName())] = obj
	s.emit(Event{Type: typ, Key: key, Obj: obj, RV: rv})
	return nil
}

func (s *Store) removeLocked(key ResourceKey, last *U) error {
	rv := s.rv + 1
	if s.p != nil {
		if err := s.p.Delete(key, last.GetNamespace(), last.GetName(), rv); err != nil {
			return apierrors.NewInternalError(err)
		}
	}
	s.rv = rv
	delete(s.data[key], nsName(last.GetNamespace(), last.GetName()))
	d := last.DeepCopy()
	d.SetResourceVersion(strconv.FormatUint(rv, 10))
	s.emit(Event{Type: watch.Deleted, Key: key, Obj: d, RV: rv})
	return nil
}

// Create stores a new object.
func (s *Store) Create(key ResourceKey, obj *U) (*U, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key][nsName(obj.GetNamespace(), obj.GetName())]; ok {
		return nil, apierrors.NewAlreadyExists(key.GR(), obj.GetName())
	}
	o := obj.DeepCopy()
	if err := s.commitPut(key, o, watch.Added); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return o.DeepCopy(), nil
}

// Get returns a copy of one object.
func (s *Store) Get(key ResourceKey, ns, name string) (*U, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.data[key][nsName(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(key.GR(), name)
	}
	return o.DeepCopy(), nil
}

func (s *Store) listLocked(key ResourceKey, ns string) []*U {
	m := s.data[key]
	out := make([]*U, 0, len(m))
	for _, o := range m {
		if ns == "" || o.GetNamespace() == ns {
			out = append(out, o.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetNamespace() != out[j].GetNamespace() {
			return out[i].GetNamespace() < out[j].GetNamespace()
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out
}

// List returns copies of the objects of a resource (all namespaces if ns == "")
// and the current resourceVersion.
func (s *Store) List(key ResourceKey, ns string) ([]*U, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(key, ns), s.rv
}

// Mutate performs an atomic read-modify-write. fn receives a private copy of
// the current object and returns the replacement (or nil for "no change").
// fn runs under the store lock and must not call back into the Store.
// With dry set nothing is committed.
func (s *Store) Mutate(key ResourceKey, ns, name string, fn func(cur *U) (*U, error), dry bool) (*U, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data[key][nsName(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(key.GR(), name)
	}
	next, err := fn(cur.DeepCopy())
	if err != nil {
		return nil, err
	}
	if next == nil {
		return cur.DeepCopy(), nil
	}
	if dry {
		return next, nil
	}
	if next.GetDeletionTimestamp() != nil && len(next.GetFinalizers()) == 0 {
		if err := s.removeLocked(key, next); err != nil {
			return nil, err
		}
		return next, nil
	}
	if err := s.commitPut(key, next, watch.Modified); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return next.DeepCopy(), nil
}

// Delete removes an object, honouring finalizers. The bool reports whether the
// object is really gone.
func (s *Store) Delete(key ResourceKey, ns, name string) (*U, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data[key][nsName(ns, name)]
	if !ok {
		return nil, false, apierrors.NewNotFound(key.GR(), name)
	}
	if len(cur.GetFinalizers()) > 0 {
		if cur.GetDeletionTimestamp() != nil {
			return cur.DeepCopy(), false, nil
		}
		n := cur.DeepCopy()
		now := metav1.Now()
		n.SetDeletionTimestamp(&now)
		if err := s.commitPut(key, n, watch.Modified); err != nil {
			return nil, false, apierrors.NewInternalError(err)
		}
		return n.DeepCopy(), false, nil
	}
	if err := s.removeLocked(key, cur); err != nil {
		return nil, false, err
	}
	return cur.DeepCopy(), true, nil
}

// DeleteAll removes every object of a resource (used when a CRD is deleted).
func (s *Store) DeleteAll(key ResourceKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.listLocked(key, "") {
		_ = s.removeLocked(key, o)
	}
}

// Snap is one entry of a full snapshot.
type Snap struct {
	Key ResourceKey
	Obj *U
}

// Snapshot returns copies of all objects (used by garbage collection).
func (s *Store) Snapshot() []Snap {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snap
	for k, m := range s.data {
		for _, o := range m {
			out = append(out, Snap{Key: k, Obj: o.DeepCopy()})
		}
	}
	return out
}

// Watcher streams events; Initial holds the replayed/synthetic events.
type Watcher struct {
	Initial []Event
	C       <-chan Event
	stop    func()
}

func (w *Watcher) Stop() { w.stop() }

// Watch starts a watch. An empty or "0" resourceVersion yields synthetic ADDED
// events for the current state; otherwise events newer than rv are replayed.
func (s *Store) Watch(key ResourceKey, ns, rvStr string) (*Watcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var initial []Event
	if rvStr == "" || rvStr == "0" {
		for _, o := range s.listLocked(key, ns) {
			initial = append(initial, Event{Type: watch.Added, Key: key, Obj: o, RV: s.rv})
		}
	} else {
		rv, err := strconv.ParseUint(rvStr, 10, 64)
		if err != nil {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid resourceVersion %q", rvStr))
		}
		if rv < s.floor {
			return nil, apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", rv, s.floor))
		}
		for _, ev := range s.history {
			if ev.RV > rv && ev.Key == key && (ns == "" || ev.Obj.GetNamespace() == ns) {
				initial = append(initial, ev)
			}
		}
	}
	sub := &subscriber{key: key, ns: ns, ch: make(chan Event, subBuffer)}
	s.subs[sub] = struct{}{}
	stop := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
	return &Watcher{Initial: initial, C: sub.ch, stop: stop}, nil
}
