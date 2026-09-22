package controlplane

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// A sandbox pod's identity is its ServiceAccount and nothing else: the control
// plane must project it verbatim, and must not pin scheduling to any particular
// cluster, so this stays portable across Kubernetes distributions.
func TestPodSpecServiceAccount(t *testing.T) {
	tests := []struct {
		name   string
		spec   SandboxSpec
		wantSA string
	}{
		{
			name: "no service account leaves the pod on the cluster default",
			spec: SandboxSpec{Image: "ubuntu:22.04"},
		},
		{
			name:   "configured service account is projected onto the pod",
			spec:   SandboxSpec{Image: "ndx-sandbox-dev:latest", ServiceAccountName: "ndx-agent"},
			wantSA: "ndx-agent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestCP(t).podSpec(tt.spec.WithDefaults(), api.NewSandboxID(), "test-public-key")

			if got := pod.Spec.ServiceAccountName; got != tt.wantSA {
				t.Error(tt, got)
			}
			if pod.Spec.NodeSelector != nil {
				t.Error(tt, pod.Spec.NodeSelector)
			}
			if got := pod.Spec.Containers[0].Image; got != tt.spec.Image {
				t.Error(tt, got)
			}
		})
	}
}

// A spec sets only the resource fields it cares about and inherits the rest.
func TestPodSpecResourceMerge(t *testing.T) {
	tests := []struct {
		name          string
		resources     corev1.ResourceRequirements
		wantCPUReq    string
		wantCPULimit  string
		wantMemoryLim string
	}{
		{
			name:          "defaults when the spec sets nothing",
			wantCPUReq:    "10m",
			wantCPULimit:  "500m",
			wantMemoryLim: "256Mi",
		},
		{
			name: "spec overrides only what it sets",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
			wantCPUReq:    "10m",
			wantCPULimit:  "2",
			wantMemoryLim: "256Mi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := resourcesFor(SandboxSpec{Image: "img", Resources: tt.resources}.WithDefaults())

			if got := res.Requests.Cpu().String(); got != tt.wantCPUReq {
				t.Error(tt, got)
			}
			if got := res.Limits.Cpu().String(); got != tt.wantCPULimit {
				t.Error(tt, got)
			}
			if got := res.Limits.Memory().String(); got != tt.wantMemoryLim {
				t.Error(tt, got)
			}
		})
	}
}

// The writable budget is the /tmp emptyDir, and it stays capped for an image
// that is not configured.
func TestPodSpecTmpDiskLimit(t *testing.T) {
	pod := newTestCP(t).podSpec(SandboxSpec{Image: "alpine:3"}.WithDefaults(), api.NewSandboxID(), "")

	for _, v := range pod.Spec.Volumes {
		if v.Name != "tmp" {
			continue
		}
		if got := v.EmptyDir.SizeLimit.String(); got != "256Mi" {
			t.Error(got)
		}
		return
	}
	t.Fatal(pod.Spec.Volumes)
}

// Only configured images are served: an image the control plane has no spec for
// is refused, so no pod can exist whose limits and identity we never set.
func TestSandboxSpecLookup(t *testing.T) {
	config := Config{
		Sandboxes: []SandboxSpec{
			{Image: "ndx-sandbox-dev:latest", PoolSizeWarm: 2, ServiceAccountName: "ndx-agent"},
			{Image: "ndx-sandbox:latest", PoolSizeWarm: 5},
		},
	}.WithDefaults()

	tests := []struct {
		name     string
		image    string
		wantOK   bool
		wantSA   string
		wantWarm int
	}{
		{name: "configured image carries its identity", image: "ndx-sandbox-dev:latest", wantOK: true, wantSA: "ndx-agent", wantWarm: 2},
		{name: "configured user image has no identity", image: "ndx-sandbox:latest", wantOK: true, wantWarm: 5},
		{name: "unconfigured image is refused", image: "alpine:3"},
		{name: "empty image is refused", image: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := config.SandboxSpec(tt.image)

			if ok != tt.wantOK {
				t.Fatal(tt, ok)
			}
			if !ok {
				return
			}
			if got := spec.Image; got != tt.image {
				t.Error(tt, got)
			}
			if got := spec.ServiceAccountName; got != tt.wantSA {
				t.Error(tt, got)
			}
			if got := spec.PoolSizeWarm; got != tt.wantWarm {
				t.Error(tt, got)
			}
			if got := spec.DiskLimitMiB; got != 256 {
				t.Error(tt, got)
			}
		})
	}
}
