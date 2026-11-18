# LLM Serving Operator 實驗室 — 02：Phase 1，CRD 與 reconcile

> 成本：**$0**。
> 產出：一個能把 `ModelDeployment` 變成真推論服務的 controller、會擋錯誤欄位的 CRD、envtest 測試，以及八個在 kind 上量到的行為。
> 先讀：`00-concepts.md` Part 2～6、Part 9。
> 預估時間：約 3 小時。

---

## 這一階段的目的

把 operator 最核心的迴圈做出來，並**親眼驗證**概念文件裡的每一個說法：

- 驗證是 API server 在擋，不是 controller（Part 1、實驗 1）
- 子資源被刪、被手改，controller 會拉回（實驗 3、4）
- 刪 CR 時，子資源是 GC 刪的，不是 controller（實驗 6）
- 每次無條件寫入的代價（實驗 7，Phase 2 修）

**六個部分：**

1. 定義 CRD 型別與驗證規則
2. 寫 Reconcile
3. envtest 測試
4. 在 kind 上跑起來
5. 八個實驗
6. 記錄與收工

---

## Part 1：CRD 型別

### 1.1 設計

```yaml
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: smollm
spec:
  runtime: llamacpp          # 目前只接受 llamacpp；預設值也是它
  model:
    url: https://...gguf     # 只接受 https
    file: SmolLM2-135M-Instruct-Q8_0.gguf   # 只接受安全的檔名
  replicas: 1                # 預設 1，可以是 0
  image: ""                  # 選填，覆蓋預設的 llama.cpp image
  resources: {}              # 選填，覆蓋預設的 requests/limits
status:
  observedGeneration: 3
  readyReplicas: 1
  endpoint: http://smollm.default.svc:8080
  conditions: [ { type: Available, status: "True", reason: AllReplicasReady, ... } ]
```

**設計取捨**：

- **`runtime` 現在只有一個值也要留欄位**：之後加 vLLM 時，舊的 CR 不用改就是 `llamacpp`。這是 API 相容性的基本功 —— CRD 一旦有人用，欄位就很難改名或刪除。
- **`file` 用 pattern 限制**：這個值會變成容器內的路徑。不限制的話，`file: ../../etc/x` 這種值會被原封不動拼進指令。**在 API 層擋，比在 controller 裡檢查好** —— 錯的物件根本寫不進 etcd，`kubectl apply` 當下就失敗。
- **`replicas` 用 `*int32`**：指標才能分辨「沒填」和「填了 0」。`int32` 的零值是 0，配上 `omitempty` 會讓「填 0」在序列化時消失。

### 1.2 改 `api/v1alpha1/modeldeployment_types.go`

**只替換** `ModelDeploymentSpec` 與 `ModelDeploymentStatus` 兩個 struct，並在 `ModelDeployment` 上方補 marker。
檔案裡其他 scaffold 的部分（`ModelDeploymentList`、`init()` 裡的 `SchemeBuilder.Register`）保持不動。

```go
import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelSource 描述模型檔從哪裡來、存成什麼名字。
type ModelSource struct {
	// URL 是 GGUF 檔的下載位址。只接受 https。
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`

	// File 是存進容器 /models 底下的檔名。
	// 只允許英數、點、底線、連字號，且必須以 .gguf 結尾 —— 擋掉路徑穿越。
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+\.gguf$`
	File string `json:"file"`
}

// ModelDeploymentSpec 是使用者宣告的期望狀態。
type ModelDeploymentSpec struct {
	// Runtime 是推論引擎。目前只支援 llamacpp。
	// +kubebuilder:validation:Enum=llamacpp
	// +kubebuilder:default=llamacpp
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// Model 是要載入的模型。
	Model ModelSource `json:"model"`

	// Replicas 是推論 pod 數量。可以是 0（停用但保留設定）。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image 覆蓋預設的推論 image。
	// +optional
	Image string `json:"image,omitempty"`

	// Resources 覆蓋推論容器預設的 requests/limits。
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ModelDeploymentStatus 是 controller 觀察到的實際狀態。
type ModelDeploymentStatus struct {
	// ObservedGeneration 是 controller 最後處理過的 metadata.generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas 是通過 readiness probe 的 pod 數量。
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint 是叢集內呼叫推論服務的網址。
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Conditions 依 Kubernetes 慣例表達狀態，目前只有 Available。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

在 `type ModelDeployment struct` **上方**的 marker 區塊，保留 scaffold 已有的 `+kubebuilder:object:root=true` 與 `+kubebuilder:subresource:status`，補上：

```go
// +kubebuilder:resource:shortName=md
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

> **scaffold 可能不同的地方**：較新版 kubebuilder 的 scaffold 在 Status 裡**已經**有 `Conditions` 欄位，`ModelDeployment` 的 `Status` 欄位 json tag 是 `omitzero`（v4.16.0 實測如此）。
> 以上面的 struct 整個取代即可；`ModelDeployment` struct 本身（`Spec`、`Status` 兩個欄位的宣告）照 scaffold 的寫法保留。

`priority=1` 的欄位只在 `kubectl get md -o wide` 才顯示 —— endpoint 太長，平常不想佔版面。

### 1.3 產生並檢查

```bash
make generate manifests
```

打開 `config/crd/bases/serving.llmlab.dev_modeldeployments.yaml`，找到這三樣東西，確認 marker 真的變成了 schema：

- `pattern: ^[A-Za-z0-9._-]+\.gguf$`
- `default: 1`（在 replicas 底下）
- `subresources: status: {}`

**這是面試會被問的點**：驗證規則存在 CRD 的 OpenAPI schema 裡，由 API server 執行。controller 根本不需要在線上，錯的物件就寫不進去。

---

## Part 2：Reconcile

### 2.1 整體流程

```
Reconcile(ns/name)
  ├─ Get ModelDeployment ── 不存在 → 結束（已被刪，子資源交給 GC）
  ├─ CreateOrUpdate Deployment ── mutate 函式把它改成「應該的樣子」+ 設 ownerReference
  ├─ CreateOrUpdate Service    ── 同上
  └─ 計算 status ── 跟目前 status 一樣 → 不寫；不一樣 → Status().Update()
```

**`controllerutil.CreateOrUpdate` 在做什麼**：Get 物件 → 不存在就跑 mutate 再 Create；存在就備份一份、跑 mutate、**跟備份比較**，不一樣才 Update。
回傳值告訴你結果是 `created`、`updated` 還是 `unchanged` —— 我們會把它印進 log，實驗 7 要用。

### 2.2 改 `internal/controller/modeldeployment_controller.go`

整個檔案換成下面這份（保留 scaffold 檔頭的 license 註解）：

```go
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
```

```bash
go mod tidy          # k8s.io/utils/ptr 等套件已是間接相依，這步把它們變成直接相依
make manifests       # RBAC marker → config/rbac/role.yaml
go build ./...
```

打開 `config/rbac/role.yaml`，確認裡面**只有**上面 marker 宣告的五組權限。

### 2.3 讀懂四個關鍵判斷

1. **為什麼 selector 只在建立時設**：Deployment 的 `spec.selector` 建立後不可變。每次都設的話，只要 labels 有任何調整，Update 就會被 API server 拒絕，controller 進入無限重試。
2. **為什麼 Service 不整個覆寫 spec**：`clusterIP` 是 API server 分配的。整個換掉 `svc.Spec`，等於送一個 `clusterIP: ""` 的 Update，會被拒絕（欄位不可變）。
3. **為什麼要比較 status 才寫**：status 寫入會改 resourceVersion → informer 收到事件 → 又觸發一次 Reconcile。無條件寫入會讓每一輪都多觸發一輪。
4. **為什麼 Available 要先看 `dep.Status.ObservedGeneration`**：剛改完 Deployment 的那一刻，它的 status 還是舊 spec 的觀察結果。直接拿 `ReadyReplicas` 判斷，會在滾動更新還沒開始時就回報 Available=True。

---

## Part 3：envtest 測試

### 3.1 這組測試驗證什麼

envtest 沒有 deployment controller、沒有 GC（`00-concepts.md` Part 9），所以只測 **「controller 產生了什麼」** 與 **「API server 擋了什麼」**：

| 測試 | 驗證 |
|---|---|
| 建立後產生子資源 | Deployment 與 Service 存在、ownerReference 指向 CR、預設值（replicas=1、runtime=llamacpp、image）生效 |
| status | endpoint 寫入；因為沒有 deployment controller，Available 必為 False/Progressing |
| spec 變更會傳遞 | 改 replicas → reconcile → Deployment replicas 跟著變 |
| 縮到零 | Available=False、reason=ScaledToZero |
| 欄位驗證 | `file: ../x.gguf`、`url: http://...` 都被 API server 拒絕（`IsInvalid`） |

### 3.2 改 `internal/controller/modeldeployment_controller_test.go`

整個換掉（`suite_test.go` 不動，它負責啟動 envtest 並建立全域的 `k8sClient`）：

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Controller", func() {
	const name = "smollm"
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: "default"}

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{
					URL:  "https://example.com/model.gguf",
					File: "model.gguf",
				},
			},
		}
	}

	reconcileOnce := func() {
		r := &ModelDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	AfterEach(func() {
		md := &servingv1alpha1.ModelDeployment{}
		if err := k8sClient.Get(ctx, key, md); err == nil {
			Expect(k8sClient.Delete(ctx, md)).To(Succeed())
		}
		// envtest 沒有 garbage collector：刪了 owner，子資源不會跟著消失，要手動清。
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	})

	It("creates an owned Deployment and Service with defaults applied", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Spec.Runtime).To(Equal("llamacpp"))        // CRD default
		Expect(md.Spec.Replicas).To(Equal(ptr.To[int32](1))) // CRD default

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultLlamaCppImage))
		Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElement("/models/model.gguf"))

		owner := metav1.GetControllerOf(dep)
		Expect(owner).NotTo(BeNil())
		Expect(owner.Kind).To(Equal("ModelDeployment"))
		Expect(owner.UID).To(Equal(md.UID))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(servingPort)))
		Expect(metav1.GetControllerOf(svc).UID).To(Equal(md.UID))
	})

	It("writes status, and reports Progressing because envtest has no deployment controller", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Status.Endpoint).To(Equal("http://smollm.default.svc:8080"))
		Expect(md.Status.ObservedGeneration).To(Equal(md.Generation))

		c := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Progressing"))
	})

	It("propagates spec.replicas changes to the Deployment", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		md.Spec.Replicas = ptr.To[int32](3)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
	})

	It("reports ScaledToZero when replicas is 0", func() {
		md := newMD()
		md.Spec.Replicas = ptr.To[int32](0)
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		c := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("ScaledToZero"))
	})

	DescribeTable("rejects invalid specs at the API server",
		func(mutate func(*servingv1alpha1.ModelDeployment)) {
			md := newMD()
			mutate(md)
			err := k8sClient.Create(ctx, md)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got: %v", err)
		},
		Entry("path traversal in file", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.File = "../x.gguf" }),
		Entry("non-gguf file", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.File = "model.bin" }),
		Entry("plain http url", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.URL = "http://example.com/m.gguf" }),
		Entry("unknown runtime", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Runtime = "vllm" }),
		Entry("negative replicas", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Replicas = ptr.To[int32](-1) }),
	)
})
```

```bash
make test
```

**完成條件**：全部通過（Ginkgo 會顯示 `Ran 9 of 9 Specs`：4 個 `It` 加上 `DescribeTable` 的 5 個 `Entry`）。記下測試數量與 `internal/controller` 的覆蓋率。

> 想看每個 spec 名稱：`go test ./internal/controller/ -v -count=1`（`make test` 會先產生 envtest 執行檔，第一次要先跑過 `make test`）。

> 失敗時先看是哪一類：
> - 驗證測試失敗 → 多半是 marker 沒產生進 CRD，重跑 `make manifests` 再看 CRD YAML
> - 「unknown runtime」通過了建立 → `Enum` marker 寫錯位置（要在欄位正上方）
> - 找不到 `k8sClient` → `suite_test.go` 被動到了

---

## Part 4：在 kind 上跑起來

### 4.1 範例 CR

把 `config/samples/serving_v1alpha1_modeldeployment.yaml` 換成：

```yaml
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: smollm
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q8_0.gguf
    file: SmolLM2-135M-Instruct-Q8_0.gguf
  replicas: 1
```

### 4.2 裝 CRD、跑 controller

```bash
kubectl config use-context kind-operator-lab
make install                      # 只裝 CRD
kubectl get crd modeldeployments.serving.llmlab.dev
```

**終端機 A**（controller 在本機前景跑，log 直接看）：

```bash
make run
```

**終端機 B**：

```bash
kubectl apply -f config/samples/serving_v1alpha1_modeldeployment.yaml
kubectl get md -w                 # 看 Ready 從空白 → 1、Available False → True
```

另一個視窗看 pod：

```bash
kubectl get pods -l app.kubernetes.io/instance=smollm -w
kubectl logs -l app.kubernetes.io/instance=smollm -c fetch-model   # 下載過程
```

### 4.3 打一個推論請求

```bash
kubectl port-forward svc/smollm 8080:8080
# 另一個終端機
curl -s localhost:8080/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Kubernetes is","max_tokens":16}' | python3 -m json.tool
```

回得來 `choices[0].text` 就是成功。135M 的模型回答品質很差，**這不重要** —— 這個 lab 驗證的是 controller，不是模型。

---

## Part 5：八個實驗

每個實驗都記進 `RESULTS.md`：**你下的指令、看到的輸出（貼關鍵行）、花了多久**。預期寫在這裡，實際結果以你看到的為準 —— 不一樣的話，那正是最有價值的紀錄。

### 實驗 1：驗證在 API server 層

**先停掉終端機 A 的 controller（Ctrl-C）**，再套一個錯的 CR：

```bash
kubectl apply -f - <<'EOF'
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata: { name: bad }
spec:
  model: { url: "http://example.com/x.gguf", file: "../x.gguf" }
EOF
```

**預期**：controller 沒在跑，apply 仍然被拒，錯誤訊息同時列出兩個欄位的 pattern。
**證明了什麼**：驗證跟 controller 無關，是 CRD schema 在 API server 執行。記下完整錯誤訊息。

做完重新 `make run`。

### 實驗 2：子資源與 ownerReference

```bash
kubectl get deploy,svc -l app.kubernetes.io/instance=smollm
kubectl get deploy smollm -o jsonpath='{.metadata.ownerReferences}' | python3 -m json.tool
kubectl top pod -l app.kubernetes.io/instance=smollm 2>/dev/null || echo "kind 預設沒有 metrics-server，略過"
kubectl get md smollm -o wide
```

記下 ownerReference 的 `controller`、`blockOwnerDeletion` 值，以及 `-o wide` 是否多出 Endpoint 欄。

### 實驗 3：誤刪 Deployment

```bash
kubectl get deploy -w &           # 背景觀察
date +%T; kubectl delete deploy smollm
```

**預期**：幾乎立刻出現新的 `smollm` Deployment（`Owns()` 的 watch 觸發）。新 pod 要重新下載模型（emptyDir），所以 md 會短暫變 Available=False。
記下：Deployment 重建的時間差、md 從 False 回到 True 花多久。做完 `kill %1`。

### 實驗 4：有人手動擴容

```bash
kubectl scale deploy smollm --replicas=3
kubectl get deploy smollm -w
```

**預期**：replicas 立刻被拉回 1。controller log 出現一次 `deployment: updated`。
**證明了什麼**：CR 是唯一的真實來源；想改 replicas，要改 CR。這就是「drift 拉回」。

### 實驗 5：改 CR 觸發變更

```bash
kubectl patch md smollm --type=merge -p '{"spec":{"replicas":2}}'
kubectl get md smollm -w
kubectl get md smollm -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
```

記下：generation 與 observedGeneration 的變化、Available 經過哪些 reason、Ready 從 1 到 2 花多久。最後改回 1。

### 實驗 6：刪 CR，子資源由 GC 清掉

```bash
kubectl delete md smollm
kubectl get deploy,svc,pods -l app.kubernetes.io/instance=smollm
```

**預期**：子資源全部消失，而 controller log **沒有任何刪除動作** —— 只有一行 Get 找不到就結束。
**證明了什麼**：刪除是 kube-controller-manager 的 garbage collector 依 ownerReference 做的。不需要 finalizer。

做完重新 `kubectl apply` 範例 CR，等它回到 Available=True。

### 實驗 7：每次 reconcile 都在寫入嗎？

服務穩定（Available=True）之後，觸發幾次不改任何東西的 reconcile：

```bash
RV1=$(kubectl get deploy smollm -o jsonpath='{.metadata.resourceVersion}')
kubectl annotate md smollm lab/poke="$(date +%s)" --overwrite    # annotation 會觸發 reconcile，但不改 spec
sleep 2
RV2=$(kubectl get deploy smollm -o jsonpath='{.metadata.resourceVersion}')
echo "$RV1 -> $RV2"
```

同時看終端機 A 的 log：`deployment` 是 `unchanged` 還是 `updated`？

**預期**：log 顯示 `updated`，但 Deployment 的 resourceVersion **可能沒變**。
原因推測：mutate 整個覆寫 PodSpec，把 API server 填的預設值（`imagePullPolicy`、`terminationMessagePath`、probe 的 `timeoutSeconds` 等）都清掉了 →
CreateOrUpdate 比較時認為不一樣 → 送出 Update → API server 補回預設值後發現跟原本一樣，可能不實際寫入。
**記下你看到的實際結果**（log 是什麼、resourceVersion 變了沒）。這是 Phase 2 的起點：不管 resourceVersion 有沒有變，每一輪都多打一次 Update 請求。

### 實驗 8：controller 停機期間的變更

```bash
# 終端機 A：Ctrl-C 停掉 controller
kubectl patch md smollm --type=merge -p '{"spec":{"replicas":2}}'
kubectl get deploy smollm -o jsonpath='{.spec.replicas}{"\n"}'    # 預期還是 1
# 終端機 A：make run
kubectl get deploy smollm -w
```

**預期**：controller 重啟後，初次 list 就看到差距，把 Deployment 改成 2。
**證明了什麼**：level-triggered —— 停機期間的「事件」沒有被收到，但狀態差距不會消失。最後改回 1。

---

## Part 6：記錄與收工

1. 把八個實驗與測試結果填進 `RESULTS.md` 的 Phase 1 段落
2. 實驗 7 的結果特別寫清楚，Phase 2 要對照
3. commit：

```bash
git add -A
git commit -m "feat: ModelDeployment CRD and reconcile for Deployment/Service"
```

4. 叢集可以留著給 Phase 2；要釋放記憶體就 `kind delete cluster --name operator-lab`

## 完成條件

- [ ] `make test` 全過，測試數量與覆蓋率已記錄
- [ ] `kubectl get md` 看到 Available=True，curl 推論有回應
- [ ] 八個實驗都有紀錄（指令、關鍵輸出、時間）
- [ ] 實驗 1 的錯誤訊息原文已貼進 RESULTS
- [ ] 實驗 7 的 log 與 resourceVersion 結果已記錄
- [ ] 有任何跟預期不同的地方，寫進 RESULTS 的「與預期不同」
