package controlplane

import (
	"context"
	"crypto/rsa"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/jwt"
)

// Sandbox is the control plane's record of a sandbox pod.
type Sandbox struct {
	ID        api.SandboxID
	Image     string
	PodName   string
	Namespace string
	Endpoint  string // podIP:agentPort
	CreatedAt time.Time
	LastSeen  time.Time // renewed by KeepAlive; drives the lease TTL
	Claimed   bool      // false = warm pool member, true = handed to a client
}

// ControlPlane manages sandbox pods.
// Controlplane implements the in-cluster sandbox control plane:
// a warm pool of ready sandbox Pods per image, claim/release lifecycle,
// TTL-based cleanup, and the HTTP API consumed by the SDK.
type ControlPlane struct {
	config     Config
	kube       kubernetes.Interface
	mu         sync.RWMutex
	byID       map[api.SandboxID]*Sandbox
	byImage    map[string][]*Sandbox // warm (unclaimed) sandboxes per image
	startedAt  time.Time             // this instance's start; older pods are orphans
	signingKey *rsa.PrivateKey       // signs per-sandbox JWTs; private key never leaves this process
	pubKeyB64  string                // base64 SPKI public key, injected into agent pods
}

func New(config Config) (*ControlPlane, error) {
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

	signingKey, pubKeyB64, err := jwt.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}

	return &ControlPlane{
		config:     config,
		kube:       kube,
		byID:       map[api.SandboxID]*Sandbox{},
		byImage:    map[string][]*Sandbox{},
		startedAt:  time.Now(),
		signingKey: signingKey,
		pubKeyB64:  pubKeyB64,
	}, nil
}

// Run starts background loops: warm-pool reconcile and TTL janitor.
func (cp *ControlPlane) Run(ctx context.Context) {
	ticker := time.NewTicker(cp.config.ReconcileEvery)
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

	for _, s := range cp.config.Sandboxes {
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
		if sb.Claimed && now.Sub(sb.LastSeen) > cp.config.LeaseTTL {
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
			if apierrors.IsNotFound(err) {
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
// instance and were created before this instance started — i.e. everything left
// over from a previous instance after a restart, warm or active. Pods created
// by this instance (registered, or still provisioning) are never touched.
// Clients must recreate a sandbox if it is gone (NewSandbox), since a restarted
// control plane cannot account for it.
func (cp *ControlPlane) reapOrphans(ctx context.Context) {
	pods, err := cp.kube.CoreV1().Pods(cp.config.Namespace).List(ctx, metav1.ListOptions{
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
		if !pod.CreationTimestamp.Time.Before(cp.startedAt) {
			continue
		}
		log.Printf("controlplane: reaping orphan sandbox pod %s (phase %s)", pod.Name, pod.Status.Phase)
		if err := cp.kube.CoreV1().Pods(cp.config.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("controlplane: reap %s: %v", pod.Name, err)
		}
	}
}

// NewSandbox claims a warm pod for image, or cold-creates one. The returned
// sandbox is ready to use: it has an id and a token to talk to its agent.
func (cp *ControlPlane) NewSandbox(ctx context.Context, req api.SandboxRequest) (*Sandbox, error) {
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

	sb, err := cp.createPod(ctx, req.Image)
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
// refills the warm pool for that image so the next Sandbox stays fast.
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
	for _, s := range cp.config.Sandboxes {
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
		sb, err := cp.createPod(ctx, image)
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
func (cp *ControlPlane) createPod(ctx context.Context, image string) (*Sandbox, error) {
	id := api.NewSandboxID()
	pod := cp.podSpec(image, id, cp.pubKeyB64)
	created, err := cp.kube.CoreV1().Pods(cp.config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return &Sandbox{
		ID:        id,
		Image:     image,
		PodName:   created.Name,
		Namespace: created.Namespace,
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}, nil
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
			sb.Endpoint = "http://" + ip + ":" + strconv.Itoa(cp.config.AgentPort)
			if healthyAgent(ctx, sb.Endpoint, cp.mintJWT(sb.ID)) {
				return nil
			}
		}
	}
}

// deleteSandbox removes the pod (best-effort; ignore NotFound).
func (cp *ControlPlane) deleteSandbox(ctx context.Context, sb *Sandbox) error {
	err := cp.kube.CoreV1().Pods(sb.Namespace).Delete(ctx, sb.PodName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
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
