package controllers

import (
	"runtime"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

var (
	gvkNamespace = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	gvkNode      = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	gvkService   = schema.GroupVersionKind{Version: "v1", Kind: "Service"}
)

// Bootstrap creates the objects every cluster has. Objects that already exist
// (for example when reusing an SQLite database) are left untouched.
func Bootstrap(st *store.Store, ipam *objects.IPAM, port int) error {
	c := New(st, ipam)

	for _, name := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name}},
			Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{corev1.FinalizerKubernetes}},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
		}
		if err := c.createTyped(kNamespaces, ns, gvkNamespace); err != nil {
			return err
		}
	}

	now := metav1.Now()
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("8"),
		corev1.ResourceMemory:           resource.MustParse("16Gi"),
		corev1.ResourcePods:             resource.MustParse("110"),
		corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: emulatorNode,
			Labels: map[string]string{
				"kubernetes.io/hostname":                emulatorNode,
				"kubernetes.io/os":                      "linux",
				"kubernetes.io/arch":                    runtime.GOARCH,
				"node-role.kubernetes.io/control-plane": "",
			},
		},
		Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/16"},
		Status: corev1.NodeStatus{
			Capacity:    capacity,
			Allocatable: capacity.DeepCopy(),
			Phase:       corev1.NodeRunning,
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
				Message: "emulated kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now,
			}},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: emulatorNodeIP},
				{Type: corev1.NodeHostName, Address: emulatorNode},
			},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: registry.ServerVersion, KubeProxyVersion: registry.ServerVersion,
				OSImage: "Emulated Linux", OperatingSystem: "linux", Architecture: runtime.GOARCH,
				ContainerRuntimeVersion: "emulator://1.0", KernelVersion: "emulated",
			},
		},
	}
	if err := c.createTyped(kNodes, node, gvkNode); err != nil {
		return err
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
	if err := c.createTyped(kServices, svc, gvkService); err != nil {
		return err
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: emulatorNodeIP}},
			Ports:     []corev1.EndpointPort{{Name: "https", Port: int32(port), Protocol: corev1.ProtocolTCP}},
		}},
	}
	return c.createTyped(kEndpoints, ep, gvkEndpoints)
}
