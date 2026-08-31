package controlplane

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// newTestCP builds a ControlPlane backed by an in-memory fake clientset.
func newTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	cp := &ControlPlane{
		opts: Options{
			Namespace:      "opensandbox",
			AgentPort:      9090,
			LeaseTTL:       15 * time.Minute,
			ReconcileEvery: time.Minute,
		},
		kube:    fake.NewSimpleClientset(),
		byID:    map[api.SandboxID]*Sandbox{},
		byImage: map[string][]*Sandbox{},
	}
	return cp
}

// registerClaimed adds a claimed sandbox directly (bypassing pod creation).
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

	// Simulate an old lease.
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

	// Deleting the pod removes it from tracking; the fake client returns
	// NotFound which is tolerated, then the lease must report gone.
	cp.mu.Lock()
	delete(cp.byID, id)
	cp.mu.Unlock()

	if _, err := cp.KeepAlive(context.Background(), id); err != ErrSandboxExpired {
		t.Fatalf("KeepAlive after delete: err=%v, want ErrSandboxExpired", err)
	}
}

// TestReconcileExpiresIdleSandbox verifies the janitor reclaims a claimed
// sandbox whose lease (LastSeen) has lapsed.
func TestReconcileExpiresIdleSandbox(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04")
	sb.LastSeen = time.Now().Add(-2 * cp.opts.LeaseTTL) // stale lease

	cp.reconcile(context.Background())

	cp.mu.RLock()
	_, ok := cp.byID[id]
	cp.mu.RUnlock()
	if ok {
		t.Fatal("sandbox with expired lease was not reclaimed")
	}
}

// TestReconcileKeepsActiveSandbox verifies a freshly-seen, Running sandbox
// survives reconcile (its pod exists and is Running, and its lease is live).
func TestReconcileKeepsActiveSandbox(t *testing.T) {
	cp := newTestCP(t)
	id := api.NewSandboxID()
	sb := registerClaimed(cp, id, "ubuntu:22.04") // LastSeen = now

	// Provide the matching Running pod so sweepNonRunning keeps it.
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
