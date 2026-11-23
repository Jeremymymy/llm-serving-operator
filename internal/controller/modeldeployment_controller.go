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
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

const (
	defaultLlamaCppImage = "ghcr.io/ggml-org/llama.cpp:server"
	modelFetcherImage    = "curlimages/curl:8.11.1"
	servingPort          = 8080
	modelsDir            = "/models"
	modelCacheSize       = "1Gi"
	fetchContainerName   = "fetch-model"
	fetchAdapterName     = "fetch-adapter"

	// fieldOwner 是 SSA 的 field manager 名稱。managedFields 裡會看到它。
	fieldOwner = client.FieldOwner("llm-serving-operator")

	conditionAvailable = "Available"
	conditionDegraded  = "Degraded"
)

// ModelDeploymentReconciler 把 ModelDeployment 變成 PVC + Deployment + Service。
type ModelDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// 最小權限（Phase 3）：每個 verb 都對應到一個實際的 API 呼叫，見 docs/04 Part 3.1。
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/status,verbs=patch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var md servingv1alpha1.ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !md.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	owner := ownerRef(&md)
	pvc := desiredPVC(&md, owner)
	if err := r.Apply(ctx, pvc, fieldOwner, client.ForceOwnership); err != nil {
		return r.fail(&md, "apply PersistentVolumeClaim", err)
	}
	depAC := desiredDeployment(&md, owner)
	if err := r.Apply(ctx, depAC, fieldOwner, client.ForceOwnership); err != nil {
		return r.fail(&md, "apply Deployment", err)
	}
	svc := desiredService(&md, owner)
	if err := r.Apply(ctx, svc, fieldOwner, client.ForceOwnership); err != nil {
		return r.fail(&md, "apply Service", err)
	}

	// resourceVersion 沒變 = 這次 apply 沒有實際寫入。實驗 7 對照這一行。
	log.Info("Applied children",
		"deploymentRV", *depAC.ResourceVersion, "serviceRV", *svc.ResourceVersion, "pvcRV", *pvc.ResourceVersion)

	dep, err := deploymentFromApply(depAC)
	if err != nil {
		return ctrl.Result{}, err
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(md.Namespace), client.MatchingLabels(labelsFor(&md))); err != nil {
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}

	oldStatus := md.Status.DeepCopy()
	newStatus := md.Status.DeepCopy()
	newStatus.ObservedGeneration = md.Generation
	newStatus.ReadyReplicas = dep.Status.ReadyReplicas
	newStatus.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d", md.Name, md.Namespace, servingPort)

	degraded := degradedCondition(&md, pods.Items)
	available := availableCondition(&md, dep, meta.FindStatusCondition(md.Status.Conditions, conditionAvailable))
	if degraded.Status == metav1.ConditionTrue && available.Status == metav1.ConditionFalse {
		// 「Progressing」會讓人以為等一下就好；下載失敗不會自己好，直接講原因。
		available.Reason, available.Message = degraded.Reason, degraded.Message
	}
	meta.SetStatusCondition(&newStatus.Conditions, available)
	meta.SetStatusCondition(&newStatus.Conditions, degraded)

	if equality.Semantic.DeepEqual(oldStatus, newStatus) {
		return ctrl.Result{}, nil
	}
	// 帶 resourceVersion 的 merge patch（樂觀鎖）：md 是從 cache 讀的，cache 舊時這次寫入會 409。
	// 這是「有意義的 409」—— 這一輪的判斷是根據舊的 status 做的（例如把已經發過的 False→True 又算一次），
	// 擋下來、退避重試，下一輪讀到新的 status，就不會重複寫入、重複發 Event。
	patch := client.MergeFromWithOptions(md.DeepCopy(), client.MergeFromWithOptimisticLock{})
	md.Status = *newStatus
	if err := r.Status().Patch(ctx, &md, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	r.emitTransitionEvents(&md, oldStatus, newStatus)
	return ctrl.Result{}, nil
}

// fail 記一筆 Warning Event 再回傳 error（交給 workqueue 退避重試）。
func (r *ModelDeploymentReconciler) fail(md *servingv1alpha1.ModelDeployment, what string, err error) (ctrl.Result, error) {
	r.Recorder.Eventf(md, nil, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile", "%s: %v", what, err)
	return ctrl.Result{}, fmt.Errorf("%s: %w", what, err)
}

// emitTransitionEvents 只在 condition 狀態「轉變」時發 Event，不是每輪都發。
func (r *ModelDeploymentReconciler) emitTransitionEvents(md *servingv1alpha1.ModelDeployment, oldS, newS *servingv1alpha1.ModelDeploymentStatus) {
	oldA := meta.FindStatusCondition(oldS.Conditions, conditionAvailable)
	newA := meta.FindStatusCondition(newS.Conditions, conditionAvailable)
	switch {
	case newA.Status == metav1.ConditionTrue && (oldA == nil || oldA.Status != metav1.ConditionTrue):
		r.Recorder.Eventf(md, nil, corev1.EventTypeNormal, "Available", "Reconcile", "%s", newA.Message)
	case newA.Status != metav1.ConditionTrue && oldA != nil && oldA.Status == metav1.ConditionTrue:
		r.Recorder.Eventf(md, nil, corev1.EventTypeWarning, "Unavailable", "Reconcile", "%s: %s", newA.Reason, newA.Message)
	}

	oldD := meta.FindStatusCondition(oldS.Conditions, conditionDegraded)
	newD := meta.FindStatusCondition(newS.Conditions, conditionDegraded)
	if newD.Status == metav1.ConditionTrue && (oldD == nil || oldD.Status != metav1.ConditionTrue) {
		r.Recorder.Eventf(md, nil, corev1.EventTypeWarning, newD.Reason, "Reconcile", "%s", newD.Message)
	}
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

func pvcName(md *servingv1alpha1.ModelDeployment) string { return md.Name + "-models" }

// ownerRef 等同 SetControllerReference 寫進去的內容。SSA 要自己組，因為 apply configuration 不是 client.Object。
func ownerRef(md *servingv1alpha1.ModelDeployment) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(servingv1alpha1.GroupVersion.String()).
		WithKind("ModelDeployment").
		WithName(md.Name).
		WithUID(md.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

func desiredPVC(md *servingv1alpha1.ModelDeployment, owner *metav1ac.OwnerReferenceApplyConfiguration) *corev1ac.PersistentVolumeClaimApplyConfiguration {
	// 不宣告 storageClassName：交給叢集預設值。沒宣告的欄位不歸我們管，SSA 不會去動它。
	return corev1ac.PersistentVolumeClaim(pvcName(md), md.Namespace).
		WithLabels(labelsFor(md)).
		WithOwnerReferences(owner).
		WithSpec(corev1ac.PersistentVolumeClaimSpec().
			WithAccessModes(corev1.ReadWriteOnce).
			WithResources(corev1ac.VolumeResourceRequirements().
				WithRequests(corev1.ResourceList{corev1.ResourceStorage: cacheSize(md)})))
}

// cacheSize：CRD 的 default 會在寫入時補上 1Gi，這裡的備案只給沒經過 API server 的物件（單元測試）用。
func cacheSize(md *servingv1alpha1.ModelDeployment) resource.Quantity {
	if md.Spec.CacheSize == nil {
		return resource.MustParse(modelCacheSize)
	}
	return *md.Spec.CacheSize
}

// fetchScript：先下載到 pod 專屬的暫存檔，再 mv（同一個檔案系統內是原子操作）。
// 多個 pod 共用同一個 PVC 時，同時下載也不會互相寫壞對方的檔案。
const fetchScript = `set -eu
if [ -f "/models/$MODEL_FILE" ]; then echo "model already present"; exit 0; fi
tmp="/models/$MODEL_FILE.partial.$POD_NAME"
trap 'rm -f "$tmp"' EXIT
curl -fL --retry 3 --retry-delay 5 -o "$tmp" "$MODEL_URL"
mv "$tmp" "/models/$MODEL_FILE"
ls -lh /models`

// fetchAdapterScript 跟 fetchScript 一樣先下載到暫存檔再 mv，多了選用的 bearer token。
// token 從 stdin 交給 curl（-K -），不放在命令列：命令列參數在節點上用 ps 看得到。
const fetchAdapterScript = `set -eu
if [ -f "/models/$ADAPTER_FILE" ]; then echo "adapter already present"; exit 0; fi
tmp="/models/$ADAPTER_FILE.partial.$POD_NAME"
trap 'rm -f "$tmp"' EXIT
if [ -n "${TOKEN:-}" ]; then
  printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | curl -fL --retry 3 --retry-delay 5 -K - -o "$tmp" "$ADAPTER_URL"
else
  curl -fL --retry 3 --retry-delay 5 -o "$tmp" "$ADAPTER_URL"
fi
mv "$tmp" "/models/$ADAPTER_FILE"
ls -lh /models`

func desiredDeployment(md *servingv1alpha1.ModelDeployment, owner *metav1ac.OwnerReferenceApplyConfiguration) *appsv1ac.DeploymentApplyConfiguration {
	labels := labelsFor(md)
	image := md.Spec.Image
	if image == "" {
		image = defaultLlamaCppImage
	}
	modelsMount := corev1ac.VolumeMount().WithName("models").WithMountPath(modelsDir)

	podName := corev1ac.EnvVar().WithName("POD_NAME").WithValueFrom(corev1ac.EnvVarSource().
		WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath("metadata.name")))

	initContainers := []*corev1ac.ContainerApplyConfiguration{corev1ac.Container().
		WithName(fetchContainerName).
		WithImage(modelFetcherImage).
		WithEnv(
			corev1ac.EnvVar().WithName("MODEL_URL").WithValue(md.Spec.Model.URL),
			corev1ac.EnvVar().WithName("MODEL_FILE").WithValue(md.Spec.Model.File),
			podName,
		).
		WithCommand("sh", "-c", fetchScript).
		WithVolumeMounts(modelsMount)}
	args := []string{
		"--model", modelsDir + "/" + md.Spec.Model.File,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(servingPort),
		"--ctx-size", "2048",
		// 4 個 slot 平分 2048：每個 512 token，估價的 prompt 約 150。跟 k8s-lab 一樣，MAE 才能直接比。
		"--parallel", "4",
		"--metrics",
		// prompt cache 預設最多 8 GiB、不會釋放；估價的 prompt 幾乎不重複，開著只會一路漲到 OOMKilled（k8s-lab 實測過，docs/06 4.2）。
		"--cache-ram", "0",
	}
	if a := md.Spec.Adapter; a != nil {
		env := []*corev1ac.EnvVarApplyConfiguration{
			corev1ac.EnvVar().WithName("ADAPTER_URL").WithValue(a.URL),
			corev1ac.EnvVar().WithName("ADAPTER_FILE").WithValue(a.File),
			podName,
		}
		if ref := a.TokenSecretRef; ref != nil {
			sel := corev1ac.SecretKeySelector().WithName(ref.Name).WithKey(ref.Key)
			if ref.Optional != nil {
				sel.WithOptional(*ref.Optional)
			}
			env = append(env, corev1ac.EnvVar().WithName("TOKEN").WithValueFrom(corev1ac.EnvVarSource().WithSecretKeyRef(sel)))
		}
		initContainers = append(initContainers, corev1ac.Container().
			WithName(fetchAdapterName).
			WithImage(modelFetcherImage).
			WithEnv(env...).
			WithCommand("sh", "-c", fetchAdapterScript).
			WithVolumeMounts(modelsMount))
		args = append(args, "--lora", modelsDir+"/"+a.File)
	}

	return appsv1ac.Deployment(md.Name, md.Namespace).
		WithLabels(labels).
		WithOwnerReferences(owner).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(desiredReplicas(md)).
			// SSA 每次都送 selector 沒問題：值沒變就不算修改，不可變欄位不會被拒。
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(labels)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				WithSpec(corev1ac.PodSpec().
					WithAutomountServiceAccountToken(false).
					WithInitContainers(initContainers...).
					WithContainers(corev1ac.Container().
						WithName("llama-server").
						WithImage(image).
						WithArgs(args...).
						WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(servingPort)).
						WithStartupProbe(httpProbe(5, 60)).
						WithReadinessProbe(httpProbe(5, 3)).
						WithLivenessProbe(httpProbe(30, 5)).
						WithResources(resourcesFor(md)).
						WithVolumeMounts(modelsMount)).
					WithVolumes(corev1ac.Volume().
						WithName("models").
						WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(pvcName(md)))))))
}

func httpProbe(periodSeconds, failureThreshold int32) *corev1ac.ProbeApplyConfiguration {
	return corev1ac.Probe().
		WithHTTPGet(corev1ac.HTTPGetAction().WithPath("/health").WithPort(intstr.FromString("http"))).
		WithPeriodSeconds(periodSeconds).
		WithFailureThreshold(failureThreshold)
}

func resourcesFor(md *servingv1alpha1.ModelDeployment) *corev1ac.ResourceRequirementsApplyConfiguration {
	rr := md.Spec.Resources
	if len(rr.Requests) == 0 && len(rr.Limits) == 0 {
		rr = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
	}
	ac := corev1ac.ResourceRequirements()
	// 只送有值的部分。送一個空的 requests 等於宣告「我擁有 requests，而且它是空的」。
	if len(rr.Requests) > 0 {
		ac.WithRequests(rr.Requests)
	}
	if len(rr.Limits) > 0 {
		ac.WithLimits(rr.Limits)
	}
	return ac
}

func desiredService(md *servingv1alpha1.ModelDeployment, owner *metav1ac.OwnerReferenceApplyConfiguration) *corev1ac.ServiceApplyConfiguration {
	// 不宣告 clusterIP：它不歸我們管。Phase 1 要小心「不能整個覆寫 spec」，SSA 天生沒有這個問題。
	return corev1ac.Service(md.Name, md.Namespace).
		WithLabels(labelsFor(md)).
		WithOwnerReferences(owner).
		WithSpec(corev1ac.ServiceSpec().
			WithSelector(labelsFor(md)).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(servingPort).
				WithTargetPort(intstr.FromString("http")).
				WithProtocol(corev1.ProtocolTCP)))
}

// deploymentFromApply 把 apply 的回應（整個物件，含 status）轉回 appsv1.Deployment。
// 用回應而不是 cache：剛 apply 完，cache 裡可能還是舊 generation，會讓 Available 判斷錯。
func deploymentFromApply(ac *appsv1ac.DeploymentApplyConfiguration) (*appsv1.Deployment, error) {
	b, err := json.Marshal(ac)
	if err != nil {
		return nil, err
	}
	dep := &appsv1.Deployment{}
	return dep, json.Unmarshal(b, dep)
}

// current 是 md 目前的 Available condition，第一次 reconcile 時是 nil。
func availableCondition(md *servingv1alpha1.ModelDeployment, dep *appsv1.Deployment, current *metav1.Condition) metav1.Condition {
	desired := desiredReplicas(md)
	c := metav1.Condition{Type: conditionAvailable, ObservedGeneration: md.Generation}
	switch {
	case desired == 0:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0"
	case dep.Status.ObservedGeneration < dep.Generation:
		// deployment controller 還沒看到最新的 Deployment，UpdatedReplicas 還不能信。
		// 落後的原因可能是我們剛改了 spec，也可能只是有人改了 annotation（Deployment 的 annotation 變動也會讓 generation +1）。
		// ReadyReplicas 仍然是事實（現在有幾個 pod 在服務）。pod 還夠，就保留上一次的觀察，不要因為「還不知道」就報中斷；
		// pod 不夠（例如 Deployment 剛被刪掉重建，status 是空的），就老實回報 Progressing。
		if current != nil && dep.Status.ReadyReplicas >= desired {
			return *current
		}
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Progressing", "waiting for deployment controller to observe the latest spec"
	// Replicas == UpdatedReplicas：舊 ReplicaSet 的 pod 都走了。少了這個條件，滾動更新卡住時
	// 「新 pod 算在 UpdatedReplicas、舊 pod 算在 ReadyReplicas」會湊成 True（docs/06 插曲）。
	case dep.Status.UpdatedReplicas >= desired && dep.Status.Replicas == dep.Status.UpdatedReplicas &&
		dep.Status.ReadyReplicas >= desired:
		c.Status, c.Reason = metav1.ConditionTrue, "AllReplicasReady"
		c.Message = fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, desired)
	default:
		c.Status, c.Reason = metav1.ConditionFalse, "Progressing"
		c.Message = fmt.Sprintf("%d/%d replicas ready, %d/%d on the latest spec",
			dep.Status.ReadyReplicas, desired, dep.Status.UpdatedReplicas, dep.Status.Replicas)
	}
	return c
}

// downloadFailureReasons：哪個 init container 失敗，對應到哪個 Degraded reason。
var downloadFailureReasons = map[string]string{
	fetchContainerName: "ModelDownloadFailed",
	fetchAdapterName:   "AdapterDownloadFailed",
}

// degradedCondition 看 pod 的 init container：下載失敗是 Deployment status 看不出來的領域知識。
func degradedCondition(md *servingv1alpha1.ModelDeployment, pods []corev1.Pod) metav1.Condition {
	c := metav1.Condition{
		Type: conditionDegraded, ObservedGeneration: md.Generation,
		Status: metav1.ConditionFalse, Reason: "NoFailure", Message: "no failure detected",
	}
	for _, p := range pods {
		for _, s := range p.Status.InitContainerStatuses {
			reason, ok := downloadFailureReasons[s.Name]
			if !ok {
				continue
			}
			// kubelet 解析不了 env（例如 tokenSecretRef 指向的 Secret 不存在）：container 根本沒啟動。
			// controller 不讀 Secret，只能從這裡知道。Secret 補上後 kubelet 會自己重試。
			if w := s.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
				c.Status, c.Reason = metav1.ConditionTrue, w.Reason
				c.Message = fmt.Sprintf("pod %s: %s: %s", p.Name, s.Name, w.Message)
				return c
			}
			// 先看目前狀態；正在重試（Running/Waiting）時才回頭看上一次的結束狀態。
			t := s.State.Terminated
			if t == nil {
				t = s.LastTerminationState.Terminated
			}
			if t != nil && t.ExitCode != 0 {
				c.Status, c.Reason = metav1.ConditionTrue, reason
				c.Message = fmt.Sprintf("pod %s: %s exited with code %d (restarts: %d)", p.Name, s.Name, t.ExitCode, s.RestartCount)
				return c
			}
		}
	}
	return c
}

// podToModelDeployment：pod 不是我們直接擁有的（owner 是 ReplicaSet），所以不能用 Owns()，改用 label 對回 CR。
func podToModelDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l["app.kubernetes.io/managed-by"] != "llm-serving-operator" || l["app.kubernetes.io/instance"] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: l["app.kubernetes.io/instance"]}}}
}

// SetupWithManager 註冊 controller 要 watch 的資源。
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// 只有 spec 變（generation +1）才觸發：自己寫 status、別人加 annotation 都不會再跑一輪。
		For(&servingv1alpha1.ModelDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToModelDeployment)).
		Named("modeldeployment").
		Complete(r)
}
