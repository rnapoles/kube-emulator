package server

import (
	"io/fs"
	"log"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// seedPriority orders manifests so that dependencies come first.
func seedPriority(u *U) int {
	switch u.GetKind() {
	case "CustomResourceDefinition":
		return 0
	case "Namespace":
		return 1
	case "Node":
		return 2
	}
	return 3
}

// SeedDir creates the objects found in dir (any kind the emulator serves;
// YAML or JSON, multi-document files and `kind: List` are fine, sub-folders
// are searched). Objects that already exist are left alone, so it is safe to
// use together with SQLite storage. Missing namespaces are created.
func (s *Server) SeedDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var docs []*U
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		ds, err := readDocs(p)
		if err != nil {
			log.Printf("seed: skipping %s: %v", p, err)
			return nil
		}
		docs = append(docs, ds...)
		return nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(docs, func(i, j int) bool { return seedPriority(docs[i]) < seedPriority(docs[j]) })
	for _, u := range docs {
		s.seedOne(u)
	}
	return nil
}

func (s *Server) seedNamespace(name string) {
	if _, err := s.store.Get(kNamespaces, "", name); err == nil {
		return
	}
	res := s.reg.Lookup("", "v1", "namespaces")
	obj, err := s.admitCreate(res, "", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil {
		return
	}
	_, _ = s.store.Create(kNamespaces, obj)
}

func (s *Server) seedOne(u *U) {
	gvk := u.GroupVersionKind()
	res := s.reg.LookupKind(gvk)
	if res == nil {
		log.Printf("seed: %s %q: kind %s is not served (create its CRD first, for example with --kinds-dir)", gvk.Kind, u.GetName(), gvk.GroupVersion())
		return
	}
	ns := ""
	if res.Namespaced {
		if ns = u.GetNamespace(); ns == "" {
			ns = "default"
		}
		s.seedNamespace(ns)
	}
	body, err := u.MarshalJSON()
	if err != nil {
		return
	}
	in, err := s.decode(body, res, "Strict")
	if err != nil {
		log.Printf("seed: %s %q: %v", gvk.Kind, u.GetName(), err)
		return
	}
	obj, err := s.admitCreate(res, ns, in)
	if err != nil {
		log.Printf("seed: %s %q: %v", gvk.Kind, u.GetName(), err)
		return
	}
	created, err := s.store.Create(res.Key(), obj)
	switch {
	case apierrors.IsAlreadyExists(err):
		log.Printf("seed: %s %q already exists, left as is", gvk.Kind, u.GetName())
	case err != nil:
		log.Printf("seed: %s %q: %v", gvk.Kind, u.GetName(), err)
	default:
		s.afterWrite(res, created, false)
		log.Printf("seed: created %s %q", gvk.Kind, u.GetName())
	}
}
