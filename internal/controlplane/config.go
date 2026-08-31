package controlplane

import (
	"encoding/json"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type Config struct {
	Sandboxes []SandboxSpec `json:"sandboxes"`

	// OrphanReapGrace is how long an untracked, non-Running sandbox pod may
	// linger before the controller deletes it. Must exceed the ~3m provisioning
	// timeout so pods still being warmed are never reaped.
	OrphanReapGrace time.Duration `json:"orphan_reap_grace"`
}

type SandboxSpec struct {
	Image        string                      `json:"image"`
	PoolSizeWarm int                         `json:"pool_size_warm"`
	Resources    corev1.ResourceRequirements `json:"resources,omitzero"`
}

// WithDefaults fills zero-valued fields with their defaults.
func (c Config) WithDefaults() Config {
	if c.OrphanReapGrace <= 0 {
		c.OrphanReapGrace = 5 * time.Minute
	}
	return c
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
