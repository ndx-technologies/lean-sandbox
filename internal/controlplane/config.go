package controlplane

import (
	"encoding/json"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type Config struct {
	Sandboxes      []SandboxSpec `json:"sandboxes"`
	Namespace      string        `json:"namespace"`   // namespace where sandbox pods live (default "sandbox")
	AgentPort      int           `json:"agent_port"`  // agent container port (default 9090)
	AgentImage     string        `json:"agent_image"` // image carrying the agent binary for injection
	LeaseTTL       time.Duration `json:"lease_ttl"`   // sandbox lifetime without KeepAlive (activity-based)
	ReconcileEvery time.Duration `json:"reconcile_every"`
	TokenTTL       time.Duration `json:"token_ttl"`
}

func (s Config) WithDefaults() Config {
	if s.Namespace == "" {
		s.Namespace = "sandbox"
	}
	if s.AgentPort == 0 {
		s.AgentPort = 9090
	}
	if s.ReconcileEvery == 0 {
		s.ReconcileEvery = 30 * time.Second
	}
	if s.LeaseTTL == 0 {
		s.LeaseTTL = 15 * time.Minute
	}
	if s.TokenTTL == 0 {
		s.TokenTTL = 15 * time.Minute
	}
	return s
}

// Merge overlays the non-zero fields of other onto c, so a config file wins
// per field it sets and flag-provided values fill the rest.
func (c Config) Merge(other Config) Config {
	if other.Namespace != "" {
		c.Namespace = other.Namespace
	}
	if other.AgentPort != 0 {
		c.AgentPort = other.AgentPort
	}
	if other.AgentImage != "" {
		c.AgentImage = other.AgentImage
	}
	if other.LeaseTTL != 0 {
		c.LeaseTTL = other.LeaseTTL
	}
	if other.ReconcileEvery != 0 {
		c.ReconcileEvery = other.ReconcileEvery
	}
	if other.TokenTTL != 0 {
		c.TokenTTL = other.TokenTTL
	}
	if len(other.Sandboxes) > 0 {
		c.Sandboxes = other.Sandboxes
	}
	return c
}

type SandboxSpec struct {
	Image        string                      `json:"image"`
	PoolSizeWarm int                         `json:"pool_size_warm"`
	Resources    corev1.ResourceRequirements `json:"resources,omitzero"`
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}
