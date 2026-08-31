// Package controlplane implements the in-cluster sandbox control plane:
// a warm pool of ready sandbox Pods per image, claim/release lifecycle,
// TTL-based cleanup, and the HTTP API consumed by the SDK.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Sandbox is the control plane's record of a sandbox pod.
type Sandbox struct {
	ID          api.SandboxID
	Image       string
	PodName     string
	Namespace   string
	Endpoint    string // podIP:agentPort
	AccessToken string // per-sandbox token required by the agent
	CreatedAt   time.Time
	LastSeen    time.Time // renewed by KeepAlive; drives the lease TTL
	Claimed     bool      // false = warm pool member, true = handed to a client
}

// Options configures the control plane.
type Options struct {
	Config         Config        // warm pool + pod resources per image, from CONFIG_PATH
	Namespace      string        // namespace where sandbox pods live (default "sandbox")
	AgentPort      int           // agent container port (default 9090)
	AgentImage     string        // image carrying the agent binary for injection
	LeaseTTL       time.Duration // sandbox lifetime without KeepAlive (activity-based)
	ReconcileEvery time.Duration
}

// ControlPlane manages sandbox pods.
type ControlPlane struct {
	opts    Options
	kube    kubernetes.Interface
	mu      sync.RWMutex
	byID    map[api.SandboxID]*Sandbox
	byImage map[string][]*Sandbox // warm (unclaimed) sandboxes per image
}

// New builds a ControlPlane from in-cluster or kubeconfig settings.
func New(opts Options) (*ControlPlane, error) {
	if opts.Namespace == "" {
		opts.Namespace = "sandbox"
	}
	if opts.AgentPort == 0 {
		opts.AgentPort = 9090
	}
	if opts.ReconcileEvery == 0 {
		opts.ReconcileEvery = 30 * time.Second
	}
	if opts.LeaseTTL == 0 {
		opts.LeaseTTL = 15 * time.Minute
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = buildOutOfClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes config: %w", err)
		}
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	return &ControlPlane{
		opts:    opts,
		kube:    kube,
		byID:    map[api.SandboxID]*Sandbox{},
		byImage: map[string][]*Sandbox{},
	}, nil
}

// Run starts background loops: warm-pool reconcile and TTL janitor.
func (cp *ControlPlane) Run(ctx context.Context) {
	ticker := time.NewTicker(cp.opts.ReconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cp.reconcile(ctx)
		}
	}
}

func (cp *ControlPlane) reconcile(ctx context.Context) {
	cp.deleteExpired(ctx)
	cp.sweepNonRunning(ctx)
	cp.reapOrphans(ctx)

	for _, s := range cp.opts.Config.Sandboxes {
		if err := cp.refillWarmPool(ctx, s.Image, s.PoolSizeWarm); err != nil {
			log.Printf("controlplane: warm pool %s: %v", s.Image, err)
		}
	}
}

// deleteExpired sandboxes whose lease expired: no KeepAlive since LastSeen.
// (crashed/leaked client, or a chat that went idle past the TTL.)
func (cp *ControlPlane) deleteExpired(ctx context.Context) {
	now := time.Now()

	cp.mu.Lock()
	var expired []*Sandbox
	for _, sb := range cp.byID {
		if sb.Claimed && now.Sub(sb.LastSeen) > cp.opts.LeaseTTL {
			expired = append(expired, sb)
		}
	}
	cp.mu.Unlock()

	for _, sb := range expired {
		log.Printf("controlplane: deleting expired sandbox %s (age %s)", sb.ID, now.Sub(sb.CreatedAt))
		if err := cp.DeleteSandbox(ctx, sb.ID); err != nil {
			log.Printf("controlplane: expire %s: %v", sb.ID, err)
		}
	}
}

// sweepNonRunning removes tracked sandboxes whose pod is not Running.
func (cp *ControlPlane) sweepNonRunning(ctx context.Context) {
	cp.mu.RLock()
	var check []*Sandbox
	for _, sb := range cp.byID {
		check = append(check, sb)
	}
	cp.mu.RUnlock()
	for _, sb := range check {
		pod, err := cp.kube.CoreV1().Pods(sb.Namespace).Get(ctx, sb.PodName, metav1.GetOptions{})
		if err != nil {
			if isNotFound(err) {
				cp.dropTracked(sb.ID)
				continue
			}
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			log.Printf("controlplane: sweeping sandbox %s (pod phase %s)", sb.ID, pod.Status.Phase)
			cp.dropTracked(sb.ID)
		}
	}
}

// reapOrphans deletes sandbox pods that are not tracked by this control plane
// instance and are no longer Running — e.g. broken or stuck pods left behind
// by a previous instance after a restart. Running pods are left alone: after a
// restart they may be active sessions this instance can no longer account for.
func (cp *ControlPlane) reapOrphans(ctx context.Context) {
	pods, err := cp.kube.CoreV1().Pods(cp.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=lean-sandbox",
	})
	if err != nil {
		log.Printf("controlplane: list sandbox pods: %v", err)
		return
	}
	cp.mu.RLock()
	tracked := make(map[string]bool, len(cp.byID))
	for _, sb := range cp.byID {
		tracked[sb.PodName] = true
	}
	cp.mu.RUnlock()
	for _, pod := range pods.Items {
		if tracked[pod.Name] {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning {
			continue
		}
		if time.Since(pod.CreationTimestamp.Time) < cp.opts.Config.OrphanReapGrace {
			continue
		}
		log.Printf("controlplane: reaping orphan sandbox pod %s (phase %s)", pod.Name, pod.Status.Phase)
		if err := cp.kube.CoreV1().Pods(cp.opts.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
			log.Printf("controlplane: reap %s: %v", pod.Name, err)
		}
	}
}

// CreateSandbox claims a warm pod for image, or cold-creates one.
func (cp *ControlPlane) CreateSandbox(ctx context.Context, req api.CreateSandboxRequest) (*Sandbox, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("image required")
	}
	cp.mu.Lock()
	warm := cp.byImage[req.Image]
	if len(warm) > 0 {
		sb := warm[len(warm)-1]
		cp.byImage[req.Image] = warm[:len(warm)-1]
		sb.Claimed = true
		sb.LastSeen = time.Now()
		cp.mu.Unlock()
		log.Printf("controlplane: claimed warm sandbox %s for %s", sb.ID, req.Image)
		return sb, nil
	}
	cp.mu.Unlock()

	sb, err := cp.createPod(ctx, req.Image, req.Env)
	if err != nil {
		return nil, err
	}
	if err := cp.waitAgentReady(ctx, sb); err != nil {
		_ = cp.deleteSandbox(ctx, sb)
		return nil, err
	}
	sb.Claimed = true
	sb.LastSeen = time.Now()
	cp.register(sb)
	return sb, nil
}

// GetSandbox returns a sandbox record without renewing its lease.
func (cp *ControlPlane) GetSandbox(ctx context.Context, id api.SandboxID) (*Sandbox, error) {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	sb, ok := cp.byID[id]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", id)
	}
	return sb, nil
}

// KeepAlive renews a sandbox lease. It returns ErrSandboxExpired if the
// sandbox is no longer tracked (e.g. already reclaimed by the janitor), so
// the caller knows to allocate a fresh one.
var ErrSandboxExpired = fmt.Errorf("sandbox lease expired")

func (cp *ControlPlane) KeepAlive(ctx context.Context, id api.SandboxID) (*Sandbox, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	sb, ok := cp.byID[id]
	if !ok {
		return nil, ErrSandboxExpired
	}
	sb.LastSeen = time.Now()
	return sb, nil
}

// DeleteSandbox removes a sandbox pod (explicit teardown) and immediately
// refills the warm pool for that image so the next CreateSandbox stays fast.
func (cp *ControlPlane) DeleteSandbox(ctx context.Context, id api.SandboxID) error {
	cp.mu.Lock()
	sb, ok := cp.byID[id]
	if ok {
		delete(cp.byID, id)
		cp.removeWarmRef(sb)
	}
	cp.mu.Unlock()
	if !ok {
		return fmt.Errorf("sandbox %s not found", id)
	}
	if err := cp.deleteSandbox(ctx, sb); err != nil {
		return err
	}
	cp.refillAfterDelete(sb.Image)
	return nil
}

// refillAfterDelete tops up the warm pool for image without blocking the
// delete response. Only refills images that are configured as warm.
func (cp *ControlPlane) refillAfterDelete(image string) {
	for _, s := range cp.opts.Config.Sandboxes {
		if s.Image != image {
			continue
		}
		go func(warm int) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := cp.refillWarmPool(ctx, image, warm); err != nil {
				log.Printf("controlplane: warm refill %s after delete: %v", image, err)
			}
		}(s.PoolSizeWarm)
		return
	}
}

// ListSandboxes returns all tracked sandboxes (useful for admin/debug).
func (cp *ControlPlane) ListSandboxes(ctx context.Context) []*Sandbox {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	out := make([]*Sandbox, 0, len(cp.byID))
	for _, sb := range cp.byID {
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (cp *ControlPlane) register(sb *Sandbox) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.byID[sb.ID] = sb
}

func (cp *ControlPlane) dropTracked(id api.SandboxID) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if sb, ok := cp.byID[id]; ok {
		delete(cp.byID, id)
		cp.removeWarmRef(sb)
	}
}

func (cp *ControlPlane) removeWarmRef(sb *Sandbox) {
	if sb.Claimed {
		return
	}
	warm := cp.byImage[sb.Image]
	for i, s := range warm {
		if s.ID == sb.ID {
			cp.byImage[sb.Image] = append(warm[:i], warm[i+1:]...)
			return
		}
	}
}

// refillWarmPool ensures at least min ready pods exist for image.
func (cp *ControlPlane) refillWarmPool(ctx context.Context, image string, min int) error {
	cp.mu.RLock()
	have := len(cp.byImage[image])
	cp.mu.RUnlock()
	if have >= min {
		return nil
	}
	for i := have; i < min; i++ {
		sb, err := cp.createPod(ctx, image, nil)
		if err != nil {
			return err
		}
		if err := cp.waitAgentReady(ctx, sb); err != nil {
			_ = cp.deleteSandbox(ctx, sb)
			return err
		}
		sb.Claimed = false
		cp.register(sb)
		cp.mu.Lock()
		cp.byImage[image] = append(cp.byImage[image], sb)
		cp.mu.Unlock()
		log.Printf("controlplane: warmed sandbox %s for %s", sb.ID, image)
	}
	return nil
}

// createPod builds and creates the sandbox pod (user image + injected agent).
func (cp *ControlPlane) createPod(ctx context.Context, image string, env []string) (*Sandbox, error) {
	id := api.NewSandboxID()
	token := randomToken()
	pod := cp.podSpec(image, env, id, token)
	created, err := cp.kube.CoreV1().Pods(cp.opts.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return &Sandbox{
		ID:          id,
		Image:       image,
		PodName:     created.Name,
		Namespace:   created.Namespace,
		CreatedAt:   time.Now(),
		LastSeen:    time.Now(),
		AccessToken: token,
	}, nil
}

// randomToken returns a cryptographically random hex string used as the
// per-sandbox agent token. Each sandbox gets its own, so one client's token
// cannot authenticate against another client's sandbox.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("controlplane: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// waitAgentReady polls the pod until Running and the agent /healthz responds.
func (cp *ControlPlane) waitAgentReady(ctx context.Context, sb *Sandbox) error {
	timeout := time.After(3 * time.Minute)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("sandbox %s agent not ready in time", sb.ID)
		case <-tick.C:
			pod, err := cp.kube.CoreV1().Pods(sb.Namespace).Get(ctx, sb.PodName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get pod: %w", err)
			}
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			ip := pod.Status.PodIP
			if ip == "" {
				continue
			}
			sb.Endpoint = "http://" + ip + ":" + strconv.Itoa(cp.opts.AgentPort)
			if healthyAgent(ctx, sb.Endpoint, sb.AccessToken) {
				return nil
			}
		}
	}
}

// deleteSandbox removes the pod (best-effort; ignore NotFound).
func (cp *ControlPlane) deleteSandbox(ctx context.Context, sb *Sandbox) error {
	err := cp.kube.CoreV1().Pods(sb.Namespace).Delete(ctx, sb.PodName, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", sb.PodName, err)
	}
	return nil
}

// WarmStatus reports warm pool sizes per image (for admin/debug).
func (cp *ControlPlane) WarmStatus() map[string]int {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	out := map[string]int{}
	for img, list := range cp.byImage {
		out[img] = len(list)
	}
	return out
}

func healthyAgent(ctx context.Context, endpoint, token string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set(api.AccessTokenHeader, token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
