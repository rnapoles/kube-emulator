package objects

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// KubeletFinalizer is put on every pod so that deleting it is graceful: the
// pod becomes Terminating, the simulated kubelet emits a Killing event and
// removes the finalizer after the termination delay.
const KubeletFinalizer = "emulator.kube/kubelet"

// IPAM hands out fake pod IPs, cluster IPs and node ports.
type IPAM struct {
	mu            sync.Mutex
	pod, svc, npt int
}

// NewIPAM seeds the counters (use the store's resourceVersion so that IPs are
// not reused after a restart with persistent storage).
func NewIPAM(seed uint64) *IPAM {
	n := int(seed % 40000)
	return &IPAM{pod: n, svc: n, npt: n}
}

func (i *IPAM) PodIP() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := i.pod
	i.pod++
	return fmt.Sprintf("10.244.%d.%d", (n/250)%256, n%250+2)
}

func (i *IPAM) ClusterIP() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := i.svc
	i.svc++
	return fmt.Sprintf("10.96.%d.%d", (n/250)%256, n%250+10)
}

func (i *IPAM) NodePort() int32 {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := i.npt
	i.npt++
	return int32(30000 + n%2768)
}

// QOSClass derives the pod's quality of service class from its resources.
func QOSClass(pod *corev1.Pod) corev1.PodQOSClass {
	for _, c := range pod.Spec.Containers {
		if len(c.Resources.Requests) > 0 || len(c.Resources.Limits) > 0 {
			return corev1.PodQOSBurstable
		}
	}
	return corev1.PodQOSBestEffort
}

// PreparePod gives a newly created pod its initial state: Pending, not bound
// to a node and carrying the kubelet finalizer.
func PreparePod(u *U) error {
	var pod corev1.Pod
	if err := FromU(u, &pod); err != nil {
		return err
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, QOSClass: QOSClass(&pod)}
	has := false
	for _, f := range pod.Finalizers {
		if f == KubeletFinalizer {
			has = true
		}
	}
	if !has {
		pod.Finalizers = append(pod.Finalizers, KubeletFinalizer)
	}
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&pod.Status)
	if err != nil {
		return err
	}
	u.Object["status"] = m
	u.SetFinalizers(pod.Finalizers)
	return nil
}
