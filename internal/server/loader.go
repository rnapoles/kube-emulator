package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// loadedFromAnnotation marks CRDs that were created from a file, so that they
// can be removed again when the file disappears (even across restarts).
const loadedFromAnnotation = "emulator.kube/loaded-from"

// KindsLoader makes new kinds available without touching the code: it reads
// CustomResourceDefinition manifests (YAML or JSON, multi-document files and
// `kind: List` are fine, sub-folders are searched too) from a folder and keeps
// the cluster in sync with it:
//
//   - a new or changed file creates or updates the CRD, so the kind is served
//     immediately and its resources are validated against the CRD's schema;
//   - a removed file (or a CRD removed from a file) deletes the CRD and all of
//     its resources.
//
// Only CRDs that were loaded from this folder are ever pruned; CRDs created
// with kubectl are left alone.
type KindsLoader struct {
	s      *Server
	dir    string
	hashes map[string]string // CRD name -> hash of the last version applied
}

// NewKindsLoader creates a loader for dir.
func (s *Server) NewKindsLoader(dir string) *KindsLoader {
	return &KindsLoader{s: s, dir: dir, hashes: map[string]string{}}
}

type fileCRD struct {
	crd  *U
	file string
}

func isCRD(u *U) bool {
	return u.GetKind() == "CustomResourceDefinition" && strings.HasPrefix(u.GetAPIVersion(), "apiextensions.k8s.io/")
}

// readDocs splits a file into its YAML/JSON documents.
func readDocs(path string) ([]*U, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(f))
	var out []*U
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		jb, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(jb)) == "null" { // comment-only document
			continue
		}
		u := &U{}
		if err := u.UnmarshalJSON(jb); err != nil {
			return nil, err
		}
		if items, ok := u.Object["items"].([]interface{}); ok && strings.HasSuffix(u.GetKind(), "List") {
			for _, it := range items {
				if m, ok := it.(map[string]interface{}); ok {
					out = append(out, &U{Object: m})
				}
			}
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// readCRDs collects every CRD below dir. complete is false if some file could
// not be read or parsed (then nothing may be pruned).
func readCRDs(dir string) (found []fileCRD, complete bool, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, false, err
	}
	if st, err := os.Stat(abs); err != nil {
		return nil, false, err
	} else if !st.IsDir() {
		return nil, false, errors.New(abs + " is not a directory")
	}
	complete = true
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			log.Printf("kinds: %v", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		docs, err := readDocs(p)
		if err != nil {
			complete = false
			log.Printf("kinds: skipping %s: %v", p, err)
			return nil
		}
		for _, u := range docs {
			if !isCRD(u) {
				log.Printf("kinds: %s: ignoring %s %q (only CustomResourceDefinitions are loaded)", p, u.GetKind(), u.GetName())
				continue
			}
			found = append(found, fileCRD{crd: u, file: p})
		}
		return nil
	})
	return found, complete, err
}

func hashOf(u *U) string {
	b, _ := u.MarshalJSON()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// apply creates or updates one CRD through the normal admission pipeline, so
// it gets the same defaults and validation as a CRD sent by kubectl.
func (l *KindsLoader) apply(res *Resource, crd *U, file string) error {
	s := l.s
	ann := crd.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[loadedFromAnnotation] = file
	crd.SetAnnotations(ann)
	name := crd.GetName()

	existing, getErr := s.store.Get(kCRDs, "", name)
	var out *U
	switch {
	case apierrors.IsNotFound(getErr):
		u, err := s.admitCreate(res, "", crd)
		if err != nil {
			return err
		}
		out, err = s.store.Create(kCRDs, u)
		if err != nil {
			return err
		}
		log.Printf("kinds: created CRD %s from %s", name, file)
	case getErr != nil:
		return getErr
	default:
		var err error
		out, err = s.store.Mutate(kCRDs, "", name, func(cur *U) (*U, error) {
			return s.admitUpdate(res, "", name, "", crd, cur)
		}, false)
		if err != nil {
			return err
		}
		if out.GetResourceVersion() != existing.GetResourceVersion() {
			log.Printf("kinds: updated CRD %s from %s", name, file)
		}
	}
	s.afterWrite(res, out, false) // registers the kind: it is served from now on
	return nil
}

// Sync applies the folder's current content once.
func (l *KindsLoader) Sync() error {
	found, complete, err := readCRDs(l.dir)
	if err != nil {
		return err
	}
	s := l.s
	res := s.reg.Lookup("apiextensions.k8s.io", "v1", "customresourcedefinitions")
	seen := map[string]bool{}
	for _, f := range found {
		name := f.crd.GetName()
		if seen[name] {
			log.Printf("kinds: CRD %s is defined more than once, ignoring the copy in %s", name, f.file)
			continue
		}
		seen[name] = true
		h := hashOf(f.crd)
		if l.hashes[name] == h {
			if _, err := s.store.Get(kCRDs, "", name); err == nil {
				continue // unchanged
			}
		}
		if err := l.apply(res, f.crd, f.file); err != nil {
			log.Printf("kinds: cannot load CRD %s from %s: %v", name, f.file, err)
		}
		l.hashes[name] = h // also remember failures, so they are logged once per change
	}
	if !complete {
		return nil // do not prune on a partial read
	}
	abs, _ := filepath.Abs(l.dir)
	existing, _ := s.store.List(kCRDs, "")
	for _, e := range existing {
		from := e.GetAnnotations()[loadedFromAnnotation]
		if from == "" || seen[e.GetName()] || !strings.HasPrefix(from, abs+string(filepath.Separator)) {
			continue
		}
		if _, gone, err := s.store.Delete(kCRDs, "", e.GetName()); err == nil && gone {
			s.afterWrite(res, e, true)
			delete(l.hashes, e.GetName())
			log.Printf("kinds: deleted CRD %s (no longer defined in %s)", e.GetName(), abs)
		}
	}
	return nil
}

// Watch re-syncs the folder every interval until ctx is cancelled.
func (l *KindsLoader) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Sync(); err != nil {
				log.Printf("kinds: %v", err)
			}
		}
	}
}
