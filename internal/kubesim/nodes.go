package kubesim

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/store"
)

var gvkNode = schema.GroupVersionKind{Version: "v1", Kind: "Node"}

// NodeOptions describes the generated nodes and the capacity given to nodes
// that were created without any (for example by `kubectl apply`).
type NodeOptions struct {
	Count  int    // number of generated nodes emulator-node-1..N
	CPU    string // capacity, for example "8"
	Memory string // for example "16Gi"
	Pods   int    // max pods per node
}

func (o NodeOptions) withDefaults() NodeOptions {
	if o.CPU == "" {
		o.CPU = "8"
	}
	if o.Memory == "" {
		o.Memory = "16Gi"
	}
	if o.Pods <= 0 {
		o.Pods = 110
	}
	return o
}

// Validate checks the capacity settings.
func (o NodeOptions) Validate() error {
	o = o.withDefaults()
	if _, err := resource.ParseQuantity(o.CPU); err != nil {
		return fmt.Errorf("invalid node cpu %q: %w", o.CPU, err)
	}
	if _, err := resource.ParseQuantity(o.Memory); err != nil {
		return fmt.Errorf("invalid node memory %q: %w", o.Memory, err)
	}
	if o.Count < 0 {
		return fmt.Errorf("the number of nodes cannot be negative")
	}
	return nil
}

// NodeName is the name of generated node i (1-based).
func NodeName(i int) string { return fmt.Sprintf("emulator-node-%d", i) }

// NodeIP is the fake internal IP of generated node i.
func NodeIP(i int) string { return fmt.Sprintf("192.168.0.%d", 10+i) }

func hashIP(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("192.168.200.%d", h.Sum32()%250+1)
}

// fillNode completes a node's metadata and status with what a real kubelet
// would have reported. It returns whether anything changed. Values the user
// set (capacity, labels, addresses, conditions) are never overwritten.
func fillNode(n *corev1.Node, o NodeOptions, ip string) bool {
	o = o.withDefaults()
	changed := false
	now := metav1.Now()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	for k, v := range map[string]string{
		"kubernetes.io/hostname": n.Name, "kubernetes.io/os": "linux", "kubernetes.io/arch": runtime.GOARCH,
	} {
		if _, ok := n.Labels[k]; !ok {
			n.Labels[k] = v
			changed = true
		}
	}
	if len(n.Status.Capacity) == 0 {
		n.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse(o.CPU),
			corev1.ResourceMemory:           resource.MustParse(o.Memory),
			corev1.ResourcePods:             resource.MustParse(fmt.Sprint(o.Pods)),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		changed = true
	}
	if len(n.Status.Allocatable) == 0 {
		n.Status.Allocatable = n.Status.Capacity.DeepCopy()
		changed = true
	}
	hasReady := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			hasReady = true
		}
	}
	if !hasReady {
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
			Message: "emulated kubelet is posting ready status", LastHeartbeatTime: now, LastTransitionTime: now,
		})
		changed = true
	}
	if len(n.Status.Addresses) == 0 {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: ip},
			{Type: corev1.NodeHostName, Address: n.Name},
		}
		changed = true
	}
	if n.Status.NodeInfo.KubeletVersion == "" {
		n.Status.NodeInfo = corev1.NodeSystemInfo{
			KubeletVersion: "v1.31.0", KubeProxyVersion: "v1.31.0", OSImage: "Emulated Linux",
			OperatingSystem: "linux", Architecture: runtime.GOARCH,
			ContainerRuntimeVersion: "emulator://1.0", KernelVersion: "emulated",
		}
		changed = true
	}
	if n.Status.Phase == "" {
		n.Status.Phase = corev1.NodeRunning
		changed = true
	}
	return changed
}

func generatedNode(i int, o NodeOptions) *corev1.Node {
	role := "node-role.kubernetes.io/worker"
	if i == 1 {
		role = "node-role.kubernetes.io/control-plane"
	}
	zones := []string{"zone-a", "zone-b", "zone-c"}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: NodeName(i),
			Labels: map[string]string{
				role:                            "",
				"topology.kubernetes.io/zone":   zones[(i-1)%len(zones)],
				"topology.kubernetes.io/region": "emulator",
			},
		},
		Spec: corev1.NodeSpec{PodCIDR: fmt.Sprintf("10.244.%d.0/24", i)},
	}
	fillNode(n, o, NodeIP(i))
	return n
}

// EnsureNodes creates the missing generated nodes emulator-node-1..N. Nodes
// that already exist (for example from an SQLite database) are left alone.
func EnsureNodes(st *store.Store, o NodeOptions) error {
	for i := 1; i <= o.Count; i++ {
		u, err := objects.ToU(generatedNode(i, o), gvkNode)
		if err != nil {
			return err
		}
		objects.InitMeta(u)
		if _, err := st.Create(kNodes, u); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// nodeController gives nodes that were created without status (kubectl apply,
// seed files) the defaults a registering kubelet would report.
type nodeController struct {
	st *store.Store
	o  NodeOptions
}

func newNodeController(st *store.Store, o NodeOptions) *nodeController {
	return &nodeController{st: st, o: o}
}

func (c *nodeController) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safely("node controller", c.once)
		}
	}
}

func (c *nodeController) once() {
	nodes, _ := c.st.List(kNodes, "")
	for _, nu := range nodes {
		var n corev1.Node
		if fromU(nu, &n) != nil || nu.GetDeletionTimestamp() != nil {
			continue
		}
		if !fillNode(&n, c.o, hashIP(n.Name)) {
			continue
		}
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&n.Status)
		if err != nil {
			continue
		}
		labels := n.Labels
		_, _ = c.st.Mutate(kNodes, "", n.Name, func(cur *U) (*U, error) {
			if equality.Semantic.DeepEqual(cur.Object["status"], m) && equality.Semantic.DeepEqual(cur.GetLabels(), labels) {
				return nil, nil
			}
			cur.Object["status"] = m
			cur.SetLabels(labels)
			return cur, nil
		}, false)
	}
}
