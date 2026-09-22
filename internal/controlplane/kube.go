package controlplane

import (
	"maps"
	"os"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ndx-technologies/lean-sandbox/api"
)

const (
	agentMountPath   = "/opt/lean-sandbox"
	agentBinPath     = agentMountPath + "/agent"
	agentBinLimitMiB = 32
)

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

func (cp *ControlPlane) podSpec(spec SandboxSpec, id api.SandboxID, pubKeyB64 string) *corev1.Pod {
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
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: spec.ServiceAccountName,
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
					Image:   spec.Image,
					Command: []string{agentBinPath},
					Args:    agentArgs(cp.config.AgentPort, id.String(), pubKeyB64),
					Ports: []corev1.ContainerPort{
						{Name: "agent", ContainerPort: int32(cp.config.AgentPort), Protocol: corev1.ProtocolTCP},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt(cp.config.AgentPort),
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       5,
						TimeoutSeconds:      3,
						FailureThreshold:    5,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "agent-bin", MountPath: agentMountPath, ReadOnly: true},
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
					Resources: resourcesFor(spec),
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "agent-bin",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: miB(agentBinLimitMiB)},
					},
				},
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: miB(int64(spec.DiskLimitMiB))},
					},
				},
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

func miB(mib int64) *resource.Quantity {
	return new(*resource.NewQuantity(mib*1024*1024, resource.BinarySI))
}

func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("500m"),
			corev1.ResourceMemory:           resource.MustParse("256Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
		},
	}
}

func resourcesFor(spec SandboxSpec) corev1.ResourceRequirements {
	req := defaultResources()
	maps.Copy(req.Requests, spec.Resources.Requests)
	maps.Copy(req.Limits, spec.Resources.Limits)
	return req
}
