package objects

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
)

// The single fake node every pod is bound to.
const (
	EmulatorNode   = "emulator-node"
	EmulatorNodeIP = "192.168.0.10"
)

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

func qosClass(pod *corev1.Pod) corev1.PodQOSClass {
	for _, c := range pod.Spec.Containers {
		if len(c.Resources.Requests) > 0 || len(c.Resources.Limits) > 0 {
			return corev1.PodQOSBurstable
		}
	}
	return corev1.PodQOSBestEffort
}

// PodStatus builds the status a kubelet would report for the pod.
func PodStatus(ipam *IPAM, pod *corev1.Pod, phase corev1.PodPhase) corev1.PodStatus {
	now := metav1.Now()
	running := phase == corev1.PodRunning
	ready := corev1.ConditionFalse
	if running {
		ready = corev1.ConditionTrue
	}
	ip := ipam.PodIP()
	st := corev1.PodStatus{
		Phase:     phase,
		HostIP:    EmulatorNodeIP,
		HostIPs:   []corev1.HostIP{{IP: EmulatorNodeIP}},
		PodIP:     ip,
		PodIPs:    []corev1.PodIP{{IP: ip}},
		StartTime: &now,
		QOSClass:  qosClass(pod),
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now},
			{Type: corev1.PodReady, Status: ready, LastTransitionTime: now},
		},
	}
	for _, c := range pod.Spec.Containers {
		id := "emulator://" + string(uuid.NewUUID())
		cs := corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, ImageID: "emulator://" + c.Image,
			ContainerID: id, Ready: running, Started: ptr.To(running),
		}
		if running {
			cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}}
		} else {
			cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed", StartedAt: now, FinishedAt: now, ContainerID: id,
			}}
		}
		st.ContainerStatuses = append(st.ContainerStatuses, cs)
	}
	return st
}

// SimulatePod plays the scheduler and kubelet: it binds the pod to the fake
// node and reports it Running (or Succeeded) immediately. No container exists.
func SimulatePod(ipam *IPAM, u *U, phase corev1.PodPhase) error {
	var pod corev1.Pod
	if err := FromU(u, &pod); err != nil {
		return err
	}
	if pod.Spec.NodeName == "" {
		pod.Spec.NodeName = EmulatorNode
	}
	pod.Status = PodStatus(ipam, &pod, phase)
	m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&pod)
	if err != nil {
		return err
	}
	u.Object["spec"] = m["spec"]
	u.Object["status"] = m["status"]
	return nil
}
