package server

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	"github.com/rnapoles/kube-emulator/internal/objects"
)

func (s *Server) newObject(res *Resource) k8sruntime.Object {
	if !res.Typed {
		return nil
	}
	o, err := scheme.Scheme.New(res.GVK())
	if err != nil {
		return nil
	}
	return o
}

// decode parses a request body. Built-in kinds are decoded with the real
// Kubernetes serializers (strict mode rejects unknown and duplicate fields,
// like fieldValidation=Strict on a real API server); custom resources are
// decoded as unstructured data.
func (s *Server) decode(body []byte, res *Resource, fieldValidation string) (k8sruntime.Object, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, apierrors.NewBadRequest("the body of the request was empty")
	}
	if !res.Typed {
		jsonBytes, err := yaml.YAMLToJSON(body)
		if err != nil {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %v", err))
		}
		u := &U{}
		if err := u.UnmarshalJSON(jsonBytes); err != nil {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %v", err))
		}
		if err := checkGVK(u.GroupVersionKind(), res); err != nil {
			return nil, err
		}
		return u, nil
	}
	cf := s.lenient
	if fieldValidation == "Strict" {
		cf = s.strict
	}
	obj, gvk, err := cf.UniversalDeserializer().Decode(body, nil, nil)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if err := checkGVK(*gvk, res); err != nil {
		return nil, err
	}
	return obj, nil
}

func checkGVK(gvk schema.GroupVersionKind, res *Resource) error {
	if gvk.GroupVersion() != res.GV() {
		return apierrors.NewBadRequest(fmt.Sprintf("the API version in the data (%s) does not match the expected API version (%s)", gvk.GroupVersion(), res.GV()))
	}
	if gvk.Kind != res.Kind {
		return apierrors.NewBadRequest(fmt.Sprintf("the kind in the data (%s) does not match the expected kind (%s)", gvk.Kind, res.Kind))
	}
	return nil
}

// toUnstructured defaults and validates typed objects and converts them.
func (s *Server) toUnstructured(res *Resource, in k8sruntime.Object) (*U, field.ErrorList, error) {
	var errs field.ErrorList
	var u *U
	if res.Typed {
		normalize(in)
		errs = append(errs, validateTyped(in)...)
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(in)
		if err != nil {
			return nil, nil, apierrors.NewInternalError(err)
		}
		u = &U{Object: m}
	} else {
		u = in.(*U)
	}
	u.SetAPIVersion(res.APIVersion())
	u.SetKind(res.Kind)
	return u, errs, nil
}

func invalid(res *Resource, name string, errs field.ErrorList) error {
	return apierrors.NewInvalid(res.GVK().GroupKind(), name, errs)
}

// admitCreate prepares a new object for storage.
func (s *Server) admitCreate(res *Resource, ns string, in k8sruntime.Object) (*U, error) {
	u, errs, err := s.toUnstructured(res, in)
	if err != nil {
		return nil, err
	}
	if res.Namespaced {
		if u.GetNamespace() == "" {
			u.SetNamespace(ns)
		} else if u.GetNamespace() != ns {
			return nil, apierrors.NewBadRequest("the namespace of the provided object does not match the namespace sent on the request")
		}
	} else {
		u.SetNamespace("")
	}
	if u.GetName() == "" && u.GetGenerateName() != "" {
		u.SetName(u.GetGenerateName() + rand.String(5))
	}
	if u.GetName() == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "name"), "name or generateName is required"))
	}
	// Server-owned metadata is always reset on create.
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetCreationTimestamp(metav1.Time{})
	u.SetDeletionTimestamp(nil)
	u.SetGeneration(0)
	u.SetManagedFields(nil)

	errs = append(errs, validateMeta(res, u)...)
	if res.Key() == kCRDs {
		errs = append(errs, validateCRD(u)...)
	}
	if len(errs) > 0 {
		return nil, invalid(res, u.GetName(), errs)
	}
	if err := s.validateCR(res, u); err != nil {
		return nil, err
	}
	if res.HasStatus {
		delete(u.Object, "status")
	}
	initMeta(u)
	if err := s.afterCreate(res, u); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return u, nil
}

// admitUpdate computes the new stored object for PUT/PATCH. It runs under the
// store lock, so it must not touch the store. It returns nil if nothing changed.
func (s *Server) admitUpdate(res *Resource, ns, name, sub string, in k8sruntime.Object, cur *U) (*U, error) {
	u, errs, err := s.toUnstructured(res, in)
	if err != nil {
		return nil, err
	}
	if u.GetName() == "" {
		u.SetName(name)
	} else if u.GetName() != name {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", u.GetName(), name))
	}
	if res.Namespaced {
		if u.GetNamespace() == "" {
			u.SetNamespace(ns)
		} else if u.GetNamespace() != ns {
			return nil, apierrors.NewBadRequest("the namespace of the provided object does not match the namespace sent on the request")
		}
	} else {
		u.SetNamespace("")
	}
	if rv := u.GetResourceVersion(); rv != "" && rv != cur.GetResourceVersion() {
		return nil, apierrors.NewConflict(res.Key().GR(), name,
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}

	if sub == "status" {
		n := cur.DeepCopy()
		if st, ok := u.Object["status"]; ok {
			n.Object["status"] = st
		} else {
			delete(n.Object, "status")
		}
		if equality.Semantic.DeepEqual(n.Object, cur.Object) {
			return nil, nil
		}
		return n, nil
	}

	// Immutable, server-owned metadata always comes from the stored object.
	u.SetUID(cur.GetUID())
	u.SetCreationTimestamp(cur.GetCreationTimestamp())
	u.SetDeletionTimestamp(cur.GetDeletionTimestamp())
	u.SetDeletionGracePeriodSeconds(cur.GetDeletionGracePeriodSeconds())
	u.SetResourceVersion(cur.GetResourceVersion())
	u.SetGeneration(cur.GetGeneration())
	u.SetManagedFields(nil)
	if res.HasStatus {
		if st, ok := cur.Object["status"]; ok {
			u.Object["status"] = st
		} else {
			delete(u.Object, "status")
		}
	}

	errs = append(errs, validateMeta(res, u)...)
	errs = append(errs, apimachineryvalidation.ValidateObjectMetaAccessorUpdate(u, cur, field.NewPath("metadata"))...)
	errs = append(errs, reconcileImmutable(res, cur, u)...)
	if res.Key() == kCRDs {
		errs = append(errs, validateCRD(u)...)
	}
	if len(errs) > 0 {
		return nil, invalid(res, name, errs)
	}
	if err := s.validateCR(res, u); err != nil {
		return nil, err
	}
	if res.Key() == kServices {
		if err := s.allocateService(u); err != nil {
			return nil, apierrors.NewInternalError(err)
		}
	}
	if !equality.Semantic.DeepEqual(cur.Object["spec"], u.Object["spec"]) {
		u.SetGeneration(cur.GetGeneration() + 1)
	}
	if equality.Semantic.DeepEqual(cur.Object, u.Object) {
		return nil, nil
	}
	return u, nil
}

// reconcileImmutable enforces (and, where the real server would, carries over)
// fields that cannot change after creation.
func reconcileImmutable(res *Resource, cur, u *U) field.ErrorList {
	var errs field.ErrorList
	switch res.Key() {
	case kPods:
		var op, np corev1.Pod
		if fromU(cur, &op) != nil || fromU(u, &np) != nil {
			return nil
		}
		masked := np.Spec.DeepCopy()
		if len(masked.Containers) == len(op.Spec.Containers) {
			for i := range masked.Containers {
				masked.Containers[i].Image = op.Spec.Containers[i].Image
			}
		}
		if len(masked.InitContainers) == len(op.Spec.InitContainers) {
			for i := range masked.InitContainers {
				masked.InitContainers[i].Image = op.Spec.InitContainers[i].Image
			}
		}
		masked.ActiveDeadlineSeconds = op.Spec.ActiveDeadlineSeconds
		masked.Tolerations = op.Spec.Tolerations
		if !equality.Semantic.DeepEqual(masked, &op.Spec) {
			errs = append(errs, field.Forbidden(field.NewPath("spec"),
				"pod updates may not change fields other than `spec.containers[*].image`, `spec.initContainers[*].image`, `spec.activeDeadlineSeconds` or `spec.tolerations`"))
		}
	case kServices:
		var os, nw corev1.Service
		if fromU(cur, &os) != nil || fromU(u, &nw) != nil {
			return nil
		}
		if nw.Spec.ClusterIP == "" {
			nw.Spec.ClusterIP = os.Spec.ClusterIP
			nw.Spec.ClusterIPs = os.Spec.ClusterIPs
		} else if os.Spec.ClusterIP != "" && nw.Spec.ClusterIP != os.Spec.ClusterIP {
			errs = append(errs, field.Invalid(field.NewPath("spec", "clusterIP"), nw.Spec.ClusterIP, "field is immutable"))
		}
		for i := range nw.Spec.Ports {
			if nw.Spec.Ports[i].NodePort != 0 {
				continue
			}
			for _, op := range os.Spec.Ports {
				if op.Port == nw.Spec.Ports[i].Port && op.Protocol == nw.Spec.Ports[i].Protocol {
					nw.Spec.Ports[i].NodePort = op.NodePort
				}
			}
		}
		if m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&nw); err == nil {
			u.Object["spec"] = m["spec"]
		}
	case kDeployments, kReplicaSets, kStatefulSet, kDaemonSets:
		a, _, _ := unstructured.NestedFieldNoCopy(cur.Object, "spec", "selector")
		b, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "selector")
		if !equality.Semantic.DeepEqual(a, b) {
			errs = append(errs, field.Invalid(field.NewPath("spec", "selector"), b, "field is immutable"))
		}
	}
	return errs
}

// afterCreate applies kind-specific behaviour on creation.
func (s *Server) afterCreate(res *Resource, u *U) error {
	switch res.Key() {
	case kPods:
		return objects.SimulatePod(s.ipam, u, corev1.PodRunning)
	case kServices:
		return s.allocateService(u)
	case kNamespaces:
		u.Object["status"] = map[string]interface{}{"phase": "Active"}
		l := u.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l["kubernetes.io/metadata.name"] = u.GetName()
		u.SetLabels(l)
	case kPVCs:
		var pvc corev1.PersistentVolumeClaim
		if err := fromU(u, &pvc); err != nil {
			return err
		}
		pvc.Status.Phase = corev1.ClaimBound
		pvc.Status.AccessModes = pvc.Spec.AccessModes
		pvc.Status.Capacity = pvc.Spec.Resources.Requests
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&pvc)
		if err != nil {
			return err
		}
		u.Object["status"] = m["status"]
	case kCRDs:
		setCRDStatus(u)
	}
	return nil
}

// allocateService fills in cluster IPs and node ports that were left empty.
func (s *Server) allocateService(u *U) error {
	var svc corev1.Service
	if err := fromU(u, &svc); err != nil {
		return err
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName && svc.Spec.ClusterIP == "" {
		svc.Spec.ClusterIP = s.ipam.ClusterIP()
		svc.Spec.ClusterIPs = []string{svc.Spec.ClusterIP}
	}
	if svc.Spec.ClusterIP == corev1.ClusterIPNone && len(svc.Spec.ClusterIPs) == 0 {
		svc.Spec.ClusterIPs = []string{corev1.ClusterIPNone}
	}
	if svc.Spec.Type == corev1.ServiceTypeNodePort || svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].NodePort == 0 {
				svc.Spec.Ports[i].NodePort = s.ipam.NodePort()
			}
		}
	}
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&svc)
	if err != nil {
		return err
	}
	u.Object["spec"] = m["spec"]
	return nil
}

// ---------------------------------------------------------------- CRDs

func validateCRD(u *U) field.ErrorList {
	var errs field.ErrorList
	sp := field.NewPath("spec")
	group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(u.Object, "spec", "names", "plural")
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "names", "kind")
	scope, _, _ := unstructured.NestedString(u.Object, "spec", "scope")
	versions, _, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
	if group == "" {
		errs = append(errs, field.Required(sp.Child("group"), ""))
	}
	if plural == "" {
		errs = append(errs, field.Required(sp.Child("names", "plural"), ""))
	}
	if kind == "" {
		errs = append(errs, field.Required(sp.Child("names", "kind"), ""))
	}
	if scope != "Namespaced" && scope != "Cluster" {
		errs = append(errs, field.NotSupported(sp.Child("scope"), scope, []string{"Namespaced", "Cluster"}))
	}
	if len(versions) == 0 {
		errs = append(errs, field.Required(sp.Child("versions"), ""))
	}
	for i, v := range versions {
		if vm, ok := v.(map[string]interface{}); !ok || vm["name"] == nil || vm["name"] == "" {
			errs = append(errs, field.Required(sp.Child("versions").Index(i).Child("name"), ""))
		}
	}
	if group != "" && plural != "" && u.GetName() != plural+"."+group {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), u.GetName(), "must be spec.names.plural+\".\"+spec.group"))
	}
	return errs
}

func condMap(t, reason, msg, now string) map[string]interface{} {
	return map[string]interface{}{"type": t, "status": "True", "reason": reason, "message": msg, "lastTransitionTime": now}
}

func setCRDStatus(u *U) {
	names, _, _ := unstructured.NestedMap(u.Object, "spec", "names")
	versions, _, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
	stored := ""
	for i, v := range versions {
		vm, _ := v.(map[string]interface{})
		n, _ := vm["name"].(string)
		if i == 0 {
			stored = n
		}
		if b, _ := vm["storage"].(bool); b {
			stored = n
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	u.Object["status"] = map[string]interface{}{
		"acceptedNames":  names,
		"storedVersions": []interface{}{stored},
		"conditions": []interface{}{
			condMap("NamesAccepted", "NoConflicts", "no conflicts found", now),
			condMap("Established", "InitialNamesAccepted", "the initial names have been accepted", now),
		},
	}
}

// validateCR validates a custom resource against its CRD's openAPIV3Schema
// using the kube-openapi validator (the one apiextensions-apiserver uses).
func (s *Server) validateCR(res *Resource, u *U) error {
	if !res.Custom || res.Validator == nil {
		return nil
	}
	result := res.Validator.Validate(u.Object)
	if result == nil || result.IsValid() {
		return nil
	}
	var msgs []string
	var causes []metav1.StatusCause
	for _, e := range result.Errors {
		msgs = append(msgs, e.Error())
		causes = append(causes, metav1.StatusCause{Type: metav1.CauseTypeFieldValueInvalid, Message: e.Error()})
	}
	gk := res.GVK().GroupKind()
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    422,
		Reason:  metav1.StatusReasonInvalid,
		Message: fmt.Sprintf("%s %q is invalid: %s", gk.String(), u.GetName(), strings.Join(msgs, ", ")),
		Details: &metav1.StatusDetails{Group: gk.Group, Kind: gk.Kind, Name: u.GetName(), Causes: causes},
	}}
}
