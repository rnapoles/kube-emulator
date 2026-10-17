package controllers

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/rnapoles/kube-emulator/internal/store"
)

var (
	gvkNamespace = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	gvkService   = schema.GroupVersionKind{Version: "v1", Kind: "Service"}
)

// apiServerIP is the address advertised by the "kubernetes" endpoints.
const apiServerIP = "192.168.0.1"

// Bootstrap creates the objects every cluster has: the default namespaces and
// the "kubernetes" service. (Nodes are created by kubesim.EnsureNodes.) Objects
// that already exist (for example when reusing an SQLite database) are left untouched.
func Bootstrap(st *store.Store, port int) error {
	c := New(st, nil)

	for _, name := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name}},
			Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{corev1.FinalizerKubernetes}},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
		}
		if _, err := c.createTyped(kNamespaces, ns, gvkNamespace); err != nil {
			return err
		}
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubernetes", Namespace: "default",
			Labels: map[string]string{"component": "apiserver", "provider": "kubernetes"},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.1", ClusterIPs: []string{"10.96.0.1"},
			SessionAffinity: corev1.ServiceAffinityNone,
			Ports: []corev1.ServicePort{{
				Name: "https", Protocol: corev1.ProtocolTCP, Port: 443,
				TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 443},
			}},
		},
	}
	if _, err := c.createTyped(kServices, svc, gvkService); err != nil {
		return err
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: apiServerIP}},
			Ports:     []corev1.EndpointPort{{Name: "https", Port: int32(port), Protocol: corev1.ProtocolTCP}},
		}},
	}
	_, err := c.createTyped(kEndpoints, ep, gvkEndpoints)
	return err
}
