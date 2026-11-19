/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

const (
	defaultLlamaCppImage = "ghcr.io/ggml-org/llama.cpp:server"
	modelFetcherImage    = "curlimages/curl:8.11.1"
	servingPort          = 8080
	modelsDir            = "/models"

	conditionAvailable = "Available"
)

// ModelDeploymentReconciler 把 ModelDeployment 變成 Deployment + Service。
type ModelDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// 這些 marker 會被 `make manifests` 產生成 config/rbac/role.yaml。
// 只宣告真的用到的資源與動詞 —— 這就是 controller 的最小權限。
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var md servingv1alpha1.ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		// 找不到 = 已經被刪了。子資源有 ownerReference，GC 會處理，這裡什麼都不用做。
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !md.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace}}
	depOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		mutateDeployment(&md, dep)
		return controllerutil.SetControllerReference(&md, dep, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: md.Name, Namespace: md.Namespace}}
	svcOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		mutateService(&md, svc)
		return controllerutil.SetControllerReference(&md, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	// 實驗 7 要看這行：每次 reconcile，子資源是 created / updated / unchanged？
	log.Info("reconciled children", "deployment", depOp, "service", svcOp)

	newStatus := md.Status.DeepCopy()
	newStatus.ObservedGeneration = md.Generation
	newStatus.ReadyReplicas = dep.Status.ReadyReplicas
	newStatus.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, servingPort)
	meta.SetStatusCondition(&newStatus.Conditions, availableCondition(&md, dep))

	// status 沒變就不寫：每次寫入都會改 resourceVersion，又觸發一次 watch 事件。
	if equality.Semantic.DeepEqual(&md.Status, newStatus) {
		return ctrl.Result{}, nil
	}
	md.Status = *newStatus
	if err := r.Status().Update(ctx, &md); err != nil {
		// 最常見的是 409 Conflict（cache 裡的版本舊了）。回傳 error → 退避後重試，下一輪會讀到新版本。
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{}, nil
}

func desiredReplicas(md *servingv1alpha1.ModelDeployment) int32 {
	if md.Spec.Replicas == nil {
		return 1
	}
	return *md.Spec.Replicas
}

func labelsFor(md *servingv1alpha1.ModelDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "model-server",
		"app.kubernetes.io/instance":   md.Name,
		"app.kubernetes.io/managed-by": "llm-serving-operator",
	}
}

func mutateDeployment(md *servingv1alpha1.ModelDeployment, dep *appsv1.Deployment) {
	labels := labelsFor(md)
	if dep.Labels == nil {
		dep.Labels = map[string]string{}
	}
	for k, v := range labels {
		dep.Labels[k] = v
	}

	// selector 建立後不可修改（API server 會拒絕）。只在建立時設定。
	if dep.CreationTimestamp.IsZero() {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	dep.Spec.Replicas = ptr.To(desiredReplicas(md))
	dep.Spec.Template.Labels = labels

	image := md.Spec.Image
	if image == "" {
		image = defaultLlamaCppImage
	}

	// ⚠️ 刻意整個覆寫 PodSpec。實驗 7 會看到這樣做的代價，Phase 2 再修。
	dep.Spec.Template.Spec = corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), // 推論 pod 不需要跟 API server 講話
		InitContainers: []corev1.Container{{
			Name:  "fetch-model",
			Image: modelFetcherImage,
			// URL 與檔名用環境變數傳進去，不直接拼進 shell 字串 —— 搭配 CRD 的 pattern，雙重避免注入。
			Env: []corev1.EnvVar{
				{Name: "MODEL_URL", Value: md.Spec.Model.URL},
				{Name: "MODEL_FILE", Value: md.Spec.Model.File},
			},
			Command: []string{"sh", "-c", `set -eu
if [ -f "/models/$MODEL_FILE" ]; then echo "model already present"; exit 0; fi
curl -fL --retry 3 --retry-delay 5 -o "/models/$MODEL_FILE.partial" "$MODEL_URL"
mv "/models/$MODEL_FILE.partial" "/models/$MODEL_FILE"
ls -lh /models`},
			VolumeMounts: []corev1.VolumeMount{{Name: "models", MountPath: modelsDir}},
		}},
		Containers: []corev1.Container{{
			Name:  "llama-server",
			Image: image,
			Args: []string{
				"--model", modelsDir + "/" + md.Spec.Model.File,
				"--host", "0.0.0.0",
				"--port", strconv.Itoa(servingPort),
				"--ctx-size", "2048",
				"--parallel", "2",
				"--metrics",
			},
			Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: servingPort}},
			StartupProbe:   httpProbe(5, 60), // 最多容忍 300 秒載入
			ReadinessProbe: httpProbe(5, 3),
			LivenessProbe:  httpProbe(30, 5), // 比 readiness 寬鬆：失敗就是重啟，重啟要重新載入模型
			Resources:      resourcesFor(md),
			VolumeMounts:   []corev1.VolumeMount{{Name: "models", MountPath: modelsDir}},
		}},
		// Phase 1 用 emptyDir：每個 pod 各自下載一次。Phase 2 改成共用的 PVC 快取。
		Volumes: []corev1.Volume{{
			Name:         "models",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}
}

func httpProbe(periodSeconds, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
		},
		PeriodSeconds:    periodSeconds,
		FailureThreshold: failureThreshold,
	}
}

func resourcesFor(md *servingv1alpha1.ModelDeployment) corev1.ResourceRequirements {
	if len(md.Spec.Resources.Requests) > 0 || len(md.Spec.Resources.Limits) > 0 {
		return md.Spec.Resources
	}
	// 預設值是給 135M 小模型的起點，實驗 2 之後依實測調整。
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

func mutateService(md *servingv1alpha1.ModelDeployment, svc *corev1.Service) {
	labels := labelsFor(md)
	if svc.Labels == nil {
		svc.Labels = map[string]string{}
	}
	for k, v := range labels {
		svc.Labels[k] = v
	}
	// 只改 selector 與 ports。clusterIP 由 API server 分配且不可改，整個覆寫 Spec 會把它清掉 → Update 被拒。
	svc.Spec.Selector = labels
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       "http",
		Port:       servingPort,
		TargetPort: intstr.FromString("http"),
		Protocol:   corev1.ProtocolTCP,
	}}
}

func availableCondition(md *servingv1alpha1.ModelDeployment, dep *appsv1.Deployment) metav1.Condition {
	desired := desiredReplicas(md)
	c := metav1.Condition{Type: conditionAvailable, ObservedGeneration: md.Generation}
	switch {
	case desired == 0:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0"
	case dep.Status.ObservedGeneration < dep.Generation:
		// deployment controller 還沒看到我們剛寫的 spec，它的 status 是舊的，不能拿來判斷。
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Progressing", "waiting for deployment controller to observe the latest spec"
	case dep.Status.UpdatedReplicas >= desired && dep.Status.ReadyReplicas >= desired:
		c.Status, c.Reason = metav1.ConditionTrue, "AllReplicasReady"
		c.Message = fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, desired)
	default:
		c.Status, c.Reason = metav1.ConditionFalse, "Progressing"
		c.Message = fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, desired)
	}
	return c
}

// SetupWithManager 註冊 controller 要 watch 的資源。
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ModelDeployment{}).
		Owns(&appsv1.Deployment{}). // 子資源被改或被刪 → reconcile 它的 owner
		Owns(&corev1.Service{}).
		Named("modeldeployment").
		Complete(r)
}
