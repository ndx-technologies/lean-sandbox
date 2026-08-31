package controlplane

import (
	"encoding/json"
	"os"

	corev1 "k8s.io/api/core/v1"
)

type Config struct {
	Sandboxes []SandboxSpec `json:"sandboxes"`
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
