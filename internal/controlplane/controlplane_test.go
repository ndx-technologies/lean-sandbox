package controlplane

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ndx-technologies/lean-sandbox/api"
)

func newTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	config := Config{
		Namespace:      "opensandbox",
		AgentPort:      9090,
		LeaseTTL:       15 * time.Minute,
		ReconcileEvery: time.Minute,
	}
	config = config.WithDefaults()
	cp := &ControlPlane{
		config:  config,
		kube:    fake.NewSimpleClientset(),
		byID:    map[api.SandboxID]*Sandbox{},
		byImage: map[string][]*Sandbox{},
	}
	return cp
}

func registerClaimed(cp *ControlPlane, id api.SandboxID, image string) *Sandbox {
	sb := &Sandbox{
		ID:        id,
		Image:     image,
		PodName:   "lean-sbx-" + id.String(),
		Namespace: "opensandbox",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
		Claimed:   true,
	}
	cp.byID[id] = sb
	return sb
}

func TestKeepAliveRenewsLease(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")

	sb.LastSeen = time.Now().Add(-10 * time.Minute)

	got, err := cp.KeepAlive(context.Background(), id)
	if err != nil {
		t.Fatalf("KeepAlive: %v", err)
	}
	if got == nil {
		t.Fatal("KeepAlive returned nil sandbox")
	}
	if time.Since(sb.LastSeen) > time.Second {
		t.Fatalf("LastSeen not renewed: %s old", time.Since(sb.LastSeen))
	}
}

func TestKeepAliveAfterDeleteReportsExpired(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	registerClaimed(cp, id, "ubuntu:22.04")

	cp.mu.Lock()
	delete(cp.byID, id)
	cp.mu.Unlock()

	if _, err := cp.KeepAlive(context.Background(), id); err != ErrSandboxExpired {
		t.Fatalf("KeepAlive after delete: err=%v, want ErrSandboxExpired", err)
	}
}

func TestReconcileExpiresIdleSandbox(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")
	sb.LastSeen = time.Now().Add(-2 * cp.config.LeaseTTL)

	cp.reconcile(context.Background())

	cp.mu.RLock()
	_, ok := cp.byID[id]
	cp.mu.RUnlock()
	if ok {
		t.Fatal("sandbox with expired lease was not reclaimed")
	}
}

func TestReconcileKeepsActiveSandbox(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sb.PodName, Namespace: sb.Namespace},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	if _, err := cp.kube.CoreV1().Pods(sb.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	cp.reconcile(context.Background())

	cp.mu.RLock()
	_, ok := cp.byID[id]
	cp.mu.RUnlock()
	if !ok {
		t.Fatal("active sandbox was incorrectly reclaimed")
	}
}

func TestNewSandboxRefusesUnconfiguredImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
	}{
		{name: "unknown image", image: "alpine:3"},
		{name: "empty image", image: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := newTestCP(t)

			if _, err := cp.NewSandbox(context.Background(), api.SandboxRequest{Image: tt.image}); err == nil {
				t.Fatal(tt)
			}

			pods, err := cp.kube.CoreV1().Pods(cp.config.Namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(pods.Items) != 0 {
				t.Error(tt, len(pods.Items))
			}
		})
	}
}

// seedPod creates a sandbox pod in the fake cluster with the given age and phase.
func seedPod(t *testing.T, cp *ControlPlane, name string, age time.Duration, phase corev1.PodPhase) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         cp.config.Namespace,
			Labels:            map[string]string{"app": "lean-sandbox"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if _, err := cp.kube.CoreV1().Pods(cp.config.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

func podExists(t *testing.T, cp *ControlPlane, name string) bool {
	t.Helper()
	_, err := cp.kube.CoreV1().Pods(cp.config.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	t.Fatalf("get pod: %v", err)
	return false
}

// A pod dropped from tracking (e.g. the watch saw it go not-ready) but whose API
// object is still around must be reclaimed once it ages past the provisioning
// grace period — otherwise the namespace never returns to PoolSizeWarm.
func TestReapOrphansReclaimsUntrackedPod(t *testing.T) {
	cp := newTestCP(t)
	seedPod(t, cp, "lean-sbx-orphan", 2*cp.config.PodProvisionGrace, corev1.PodRunning)

	cp.reapOrphans(context.Background())

	if podExists(t, cp, "lean-sbx-orphan") {
		t.Fatal("untracked pod older than the grace period was not reaped")
	}
}

// A pod this instance just created is untracked while waitPodReady runs; the
// reaper must not touch it.
func TestReapOrphansSparesFreshPod(t *testing.T) {
	cp := newTestCP(t)
	seedPod(t, cp, "lean-sbx-fresh", 0, corev1.PodPending)

	cp.reapOrphans(context.Background())

	if !podExists(t, cp, "lean-sbx-fresh") {
		t.Fatal("fresh untracked pod was incorrectly reaped")
	}
}

// A tracked pod is owned by the control plane regardless of age.
func TestReapOrphansSparesTrackedPod(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")
	seedPod(t, cp, sb.PodName, 2*cp.config.PodProvisionGrace, corev1.PodRunning)

	cp.reapOrphans(context.Background())

	if !podExists(t, cp, sb.PodName) {
		t.Fatal("tracked pod was incorrectly reaped")
	}
}

// A tracked pod that stops being Ready is dropped from tracking and its API
// object is deleted, so it cannot be handed out and cannot leak.
func TestOnPodEventDeletesNotReadyPod(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")
	seedPod(t, cp, sb.PodName, 0, corev1.PodRunning)

	pod, err := cp.kube.CoreV1().Pods(sb.Namespace).Get(context.Background(), sb.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	cp.onPodEvent(context.Background(), pod)

	cp.mu.RLock()
	_, ok := cp.byID[id]
	cp.mu.RUnlock()
	if ok {
		t.Fatal("not-ready pod was not dropped from tracking")
	}
	if podExists(t, cp, sb.PodName) {
		t.Fatal("not-ready pod was not deleted")
	}
}

// A Ready pod is left alone.
func TestOnPodEventKeepsReadyPod(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")
	seedPod(t, cp, sb.PodName, 0, corev1.PodRunning)

	pod, err := cp.kube.CoreV1().Pods(sb.Namespace).Get(context.Background(), sb.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	cp.onPodEvent(context.Background(), pod)

	cp.mu.RLock()
	_, ok := cp.byID[id]
	cp.mu.RUnlock()
	if !ok {
		t.Fatal("ready pod was incorrectly dropped")
	}
	if !podExists(t, cp, sb.PodName) {
		t.Fatal("ready pod was incorrectly deleted")
	}
}
