package controlplane

import (
	"os"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ndx-technologies/lean-sandbox/api"
)

// Agent binary mount path inside the sandbox container.
const (
	agentMountPath = "/opt/lean-sandbox"
	agentBinPath   = agentMountPath + "/agent"
)

// buildOutOfClusterConfig loads kubeconfig from KUBECONFIG or ~/.kube/config.
func buildOutOfClusterConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// podSpec builds the sandbox pod: user image runs the injected agent binary.
// An init container copies the static agent binary from AgentImage into a
// shared emptyDir; the sandbox container then starts it as its entrypoint.
func (cp *ControlPlane) podSpec(image string, id api.SandboxID, accessToken string) *corev1.Pod {
	podName := "lean-sbx-" + id.String()
	labels := map[string]string{
		"app":                        "lean-sandbox",
		"lean-sandbox.ndx.one/id":    id.String(),
		"lean-sandbox.ndx.one/image": sanitizeLabelValue(image),
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cp.opts.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{
				{
					Name:    "agent-inject",
					Image:   cp.opts.AgentImage,
					Command: []string{"/agent", "-install-to", agentBinPath},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "agent-bin", MountPath: agentMountPath},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "sandbox",
					Image:   image,
					Command: []string{agentBinPath},
					Args:    agentArgs(cp.opts.AgentPort, accessToken),
					Ports: []corev1.ContainerPort{
						{Name: "agent", ContainerPort: int32(cp.opts.AgentPort), Protocol: corev1.ProtocolTCP},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "agent-bin", MountPath: agentMountPath},
						{Name: "tmp", MountPath: "/tmp"},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: new(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             new(true),
						RunAsUser:                new(int64(1000)),
						RunAsGroup:               new(int64(1000)),
						ReadOnlyRootFilesystem:   new(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Resources: cp.resourcesFor(image),
				},
			},
			Volumes: []corev1.Volume{
				{Name: "agent-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

// agentArgs builds the sandbox container args, forwarding the access token so
// agent pods require the same token the control plane hands out to clients.
func agentArgs(port int, accessToken string) []string {
	args := []string{"-listen", ":" + strconv.Itoa(port)}
	if accessToken != "" {
		args = append(args, "-access-token", accessToken)
	}
	return args
}

// resourcesFor returns the pod resources for image, using its warm-pool spec
// when configured, otherwise lean defaults so the warm pool is cheap while idle.
func (cp *ControlPlane) resourcesFor(image string) corev1.ResourceRequirements {
	for _, s := range cp.opts.Config.Sandboxes {
		if s.Image == image && (len(s.Resources.Requests) > 0 || len(s.Resources.Limits) > 0) {
			return s.Resources
		}
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// sanitizeLabelValue keeps a label value within DNS-1123 safe chars for
// common image strings like "ubuntu:22.04".
func sanitizeLabelValue(v string) string {
	out := make([]rune, 0, len(v))
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
