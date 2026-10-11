package controlplane

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/ndx-technologies/lean-sandbox/api"
)

func (cp *ControlPlane) watchPods(ctx context.Context) {
	f := informers.NewSharedInformerFactoryWithOptions(cp.kube, cp.config.ReconcileEvery,
		informers.WithNamespace(cp.config.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = "app=lean-sandbox"
		}),
	)
	informer := f.Core().V1().Pods().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { cp.onPodEvent(ctx, obj.(*corev1.Pod)) },
		UpdateFunc: func(_, newObj any) { cp.onPodEvent(ctx, newObj.(*corev1.Pod)) },
		DeleteFunc: func(obj any) { cp.onPodDelete(obj) },
	}); err != nil {
		slog.ErrorContext(ctx, "watch pods", "error", err)
	}
	f.Start(ctx.Done())
	<-ctx.Done()
}

func (cp *ControlPlane) onPodEvent(ctx context.Context, pod *corev1.Pod) {
	if podReady(pod) {
		return
	}
	if id, ok := cp.sandboxIDByPodName(pod.Name); ok {
		slog.InfoContext(ctx, "pod not ready, dropping", "pod", pod.Name, "phase", pod.Status.Phase)
		cp.dropTracked(id)

		if err := cp.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			slog.ErrorContext(ctx, "delete not-ready pod", "pod", pod.Name, "error", err)
		}
	}
}

func (cp *ControlPlane) onPodDelete(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			pod, ok = tomb.Obj.(*corev1.Pod)
		}
	}
	if !ok || pod == nil {
		return
	}
	if id, ok := cp.sandboxIDByPodName(pod.Name); ok {
		cp.dropTracked(id)
	}
}

func (cp *ControlPlane) sandboxIDByPodName(name string) (api.SandboxID, bool) {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	for id, sb := range cp.byID {
		if sb.PodName == name {
			return id, true
		}
	}
	return api.SandboxID{}, false
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
