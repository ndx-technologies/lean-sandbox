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
func (cp *ControlPlane) podSpec(image string, id api.SandboxID, pubKeyB64 string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lean-sbx-" + id.String(),
			Namespace: cp.config.Namespace,
			Labels: map[string]string{
				"app":                     "lean-sandbox",
				"lean-sandbox.ndx.one/id": id.String(),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{
				{
					Name:    "agent-inject",
					Image:   cp.config.AgentImage,
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
					Args:    agentArgs(cp.config.AgentPort, id.String(), pubKeyB64),
					Ports: []corev1.ContainerPort{
						{Name: "agent", ContainerPort: int32(cp.config.AgentPort), Protocol: corev1.ProtocolTCP},
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

func agentArgs(port int, sandboxID, pubKeyB64 string) []string {
	args := []string{"-listen", ":" + strconv.Itoa(port)}
	if sandboxID != "" {
		args = append(args, "-sandbox-id", sandboxID)
	}
	if pubKeyB64 != "" {
		args = append(args, "-controlplane-public-key", pubKeyB64)
	}
	return args
}

func (cp *ControlPlane) resourcesFor(image string) corev1.ResourceRequirements {
	for _, s := range cp.config.Sandboxes {
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
