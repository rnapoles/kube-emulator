// Package objects holds helpers shared by the API server and the controllers:
// conversion between typed and unstructured objects, defaulting, validation
// and the pod/IP simulation.
package objects

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/rnapoles/kube-emulator/internal/store"
)

// U is a short alias for unstructured objects.
type U = store.U

// FromU converts an unstructured object into a typed one (out must be a pointer).
func FromU(u *U, out interface{}) error {
	return k8sruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

// ToU converts a typed object (pointer) into an unstructured one.
func ToU(obj interface{}, gvk schema.GroupVersionKind) (*U, error) {
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &U{Object: m}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

// InitMeta sets the server-owned metadata of a newly created object.
func InitMeta(u *U) {
	if u.GetUID() == "" {
		u.SetUID(uuid.NewUUID())
	}
	if u.GetCreationTimestamp().Time.IsZero() {
		u.SetCreationTimestamp(metav1.Now())
	}
	if _, ok := u.Object["spec"]; ok && u.GetGeneration() == 0 {
		u.SetGeneration(1)
	}
}

// GS reads a nested string field ("" if missing).
func GS(u *U, f ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, f...)
	return v
}

// GI reads a nested integer field (0 if missing).
func GI(u *U, f ...string) int64 {
	v, _, _ := unstructured.NestedInt64(u.Object, f...)
	return v
}
