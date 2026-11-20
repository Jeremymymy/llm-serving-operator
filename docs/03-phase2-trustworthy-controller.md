# LLM Serving Operator 實驗室 — 03：Phase 2，讓 controller 值得信任

> 成本：**$0**。
> 產出：改用 Server-Side Apply 的 controller、PVC 模型快取、`Degraded` condition 與 Events、16 個 envtest 測試（13 個在 Part 4，3 個在 Part 6 的三次插曲），以及八個跟 Phase 1 對照的實驗。
> 先讀：`RESULTS.md` 的 Phase 1（尤其實驗 3、4、7、8 與「與預期不同」）、`00-concepts.md` Part 3、5、6。
> 預估時間：約 3.5 小時。

---

## 這一階段的目的

Phase 1 的 controller「會動」，但 RESULTS 裡量到幾個問題。這一階段逐一修掉，並**用同樣的實驗量一次修完的數字**：

| Phase 1 觀察到的 | 原因 | Phase 2 的修法 |
|---|---|---|
| 每輪 reconcile 都是 `deployment: updated`（實驗 7） | mutate 整個覆寫 PodSpec，跟 API server 補上的預設值比較永遠不一樣 | **Server-Side Apply**：只宣告自己在乎的欄位 |
| 409 Conflict（`make run` 啟動時、實驗 4、8） | `Update` 帶著 resourceVersion（樂觀鎖），cache 一舊就衝突 | SSA 不帶 resourceVersion；status 改用 merge patch |
| 1 秒內 9 輪 reconcile（實驗 4、5） | 自己寫 status 也會觸發自己再跑一輪 | `GenerationChangedPredicate` |
| Deployment 被刪後要 61 秒才恢復（實驗 3） | emptyDir：每個新 pod 都重新下載模型 | **PVC 模型快取** |
| 模型下載失敗時，status 只會說 `Progressing` | controller 只看 Deployment 的 status | 看 init container 的結束碼 → **`Degraded` condition + Warning Event** |

**關於 finalizer**：README 原本把「finalizer 的正確用途」排在這一階段。這個 operator 管的東西全部在同一個 namespace，GC 就處理得了（Phase 1 實驗 6 已證明），**沒有真正需要 finalizer 的地方** —— 硬做一個，反而是在示範錯誤用法。
所以這一階段改用實驗 8：看 Kubernetes 自己怎麼正確使用 finalizer（PVC 的 `pvc-protection`），以及亂加 finalizer 的後果。

**七個部分：**

1. 概念：SSA、status patch、predicate
2. 設計：PVC 快取、Degraded、Events
3. 程式碼
4. envtest 測試
5. 在 kind 上跑起來
6. 八個實驗
7. 記錄與收工

---

## Part 1：概念

### 1.1 Server-Side Apply（SSA）在解決什麼

Phase 1 的 `CreateOrUpdate` 是**在 client 端比較**：拿 cache 裡的物件、跑 mutate、兩份比一比，不一樣就送整個物件的 `Update`。
問題出在「cache 裡的物件」包含了 API server 補上的預設值（`imagePullPolicy`、`terminationMessagePath`、probe 的 `timeoutSeconds`…），而 mutate 產生的版本沒有 —— 所以永遠不一樣（RESULTS 實驗 7 列了完整清單）。

SSA 換一個思路：**controller 只送「我宣告擁有的欄位，以及它們的值」，由 API server 合併。**

```
你送的（apply configuration）        API server 上的物件
spec.replicas: 1                     spec.replicas: 1                  ← 你擁有
spec.template...image: llama...      ...image: llama...                ← 你擁有
（沒送）                              ...imagePullPolicy: IfNotPresent  ← 預設值，沒人擁有，你不送也不會被清掉
（沒送）                              metadata.annotations[...]         ← 別人擁有，你不會碰到
```

API server 用 `metadata.managedFields` 記錄**每個欄位是哪個 field manager 擁有的**。這帶來四個性質，每一個都對應 Phase 1 的一個問題：

| 性質 | 對應 Phase 1 的問題 |
|---|---|
| 合併後跟現狀一樣 → **不寫入**，resourceVersion 不變 | 實驗 7 的無效 Update |
| apply 請求**不帶 resourceVersion** → 不做樂觀鎖檢查，cache 舊也不會 409 | 啟動時、實驗 4、8 的 409 |
| 沒宣告的欄位不歸你管 → 不用再小心「selector 只在建立時設」「Service 不能整個覆寫 spec」 | 02 Part 2.3 的第 1、2 點 |
| 別人改了**你擁有的**欄位 → 你下次 apply 會衝突；加上 `ForceOwnership` 就搶回來 | 實驗 4 的 drift 拉回 |

**用 SSA 一定要知道的兩個坑**：

1. **每次都要送完整的期望狀態。** apply 的語意是「我擁有的欄位就是這些」。這次少送一個欄位，等於宣告「我不再擁有它」—— 如果沒有別人擁有，API server 會**把它刪掉**。所以不能只送「這次想改的部分」。
2. **想讓別人管的欄位，就不要送。** Phase 5 如果加 HPA，HPA 會改 `spec.replicas`。controller 若還在送 replicas 又加了 `ForceOwnership`，兩邊會一直互相搶。那時就要把 replicas 從 apply 裡拿掉。

**為什麼不用 `controllerutil.CreateOrPatch`**：它一樣在 client 端比較，預設值的問題還在，只是把 Update 換成 Patch。

**程式寫法的差別**（controller-runtime v0.25）：

```go
// Phase 1：拿 typed 物件，mutate 後比較
controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error { ... })

// Phase 2：組一個 apply configuration（k8s.io/client-go/applyconfigurations），直接送
depAC := appsv1ac.Deployment(name, ns).WithSpec(...)
r.Apply(ctx, depAC, client.FieldOwner("llm-serving-operator"), client.ForceOwnership)
```

apply configuration 的每個欄位都是指標，所以「沒設」和「設成零值」分得開 —— 這正是 SSA 需要的（跟 Phase 1 `replicas` 用 `*int32` 同一個道理）。
`r.Apply` 回傳後，API server 的回應（**整個物件，含 status**）會寫回 `depAC`。

> **ownerReference 要自己組**：`SetControllerReference` 吃的是 `client.Object`，apply configuration 不是。程式裡的 `ownerRef()` 寫出跟它一樣的內容（`controller: true`、`blockOwnerDeletion: true`）。

### 1.2 status：從 Update 改成 merge patch

SSA 修掉了子資源的 409，但 Phase 1 實驗 4 的那次 409 是 **md 的 status** 衝突。`Status().Update()` 也帶 resourceVersion。

改成：

```go
patch := client.MergeFrom(md.DeepCopy())
md.Status = *newStatus
r.Status().Patch(ctx, &md, patch)
```

`MergeFrom` 產生的 JSON merge patch **不帶 resourceVersion**，所以不做樂觀鎖檢查。

**這樣安全嗎？** 樂觀鎖是為了防止「兩個寫入者互相蓋掉」。status 只有這個 controller 在寫，而且 leader election 保證同一時間只有一個 controller 在 reconcile（00-concepts Part 3）—— 沒有第二個寫入者，鎖就沒有保護的對象。
反過來說，**如果 status 有別的寫入者，就不能這樣做**。這是面試可以講的取捨。

**拿掉鎖還有一個代價：cache 舊的時候，會重複寫入、重複發 Event。** 寫完 status 之後，informer cache 要一點時間才收到新版本。這段期間如果又有事件觸發 reconcile（pod、Deployment 的變化很常見），這一輪從 cache 讀到的還是舊 status，就會再算一次「False → True 轉變」，再 patch 一次、再發一次 Event。
用 `Update` 的話，這一輪會 409、退避重試，等 cache 跟上就不會重複；用 merge patch，這一輪會成功，只是寫入的內容跟上一輪一樣（冪等，結果正確）。
**兩種做法都在處理同一件事 —— cache 一定會有舊的時候 —— 只是選擇不同的代價**：409 加重試，或是重複但無害的寫入。這個 lab **一開始**選後者，實驗 4、5 會看到重複的 Event，然後在插曲 3 改成前者 —— 照順序做，兩種代價都會親眼看到。
想兩邊都要，可以用 `client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})`：patch 會帶 resourceVersion，cache 舊時 409，等於只在 status 上把鎖加回來。

> envtest 的測試抓不到這類問題：測試用的 `k8sClient` 是直接打 API server、**沒有 cache** 的 client，永遠讀到最新版本。cache 造成的行為只有在 kind 上跑真的 manager 才看得到。

> 另一個做法是 status 也用 SSA（`r.Status().Apply`）。那需要替 `ModelDeployment` 產生 apply configuration 型別（controller-gen 的 `applyconfiguration` 產生器），kubebuilder 預設沒開。這個 lab 不做。

### 1.3 predicate：自己寫 status，不該再觸發自己

Phase 1 實驗 4 的 9 輪 reconcile 裡，有一部分是這樣來的：寫 md status → md 的 resourceVersion 變了 → informer 收到 Update 事件 → 又 reconcile 一次。

```go
For(&servingv1alpha1.ModelDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
```

`GenerationChangedPredicate` 只放行 `metadata.generation` 有變的 Update 事件。generation 只在 **spec** 變動時 +1（00-concepts Part 5），所以：

| md 上發生的事 | Phase 1 | Phase 2 |
|---|---|---|
| 改 spec | 觸發 | 觸發 |
| controller 寫 status | 觸發 | **不觸發** |
| 有人加 annotation／label | 觸發 | **不觸發**（所以 Phase 1 實驗 7 的 poke 方式要換） |
| 建立、刪除 | 觸發 | 觸發（predicate 只過濾 Update 事件） |

**level-triggered 的安全網還在嗎？** 還在。predicate 只加在 `For()` 上，子資源（`Owns()`）的事件照樣觸發。md 本身定期 resync 產生的事件會被濾掉，但 Deployment 的 resync 一樣會把 md 送進 workqueue。

---

## Part 2：設計

### 2.1 PVC 模型快取

```
Phase 1：每個 pod 一個 emptyDir → 每個新 pod 都下載一次（138 MB，約 56 秒）
Phase 2：每個 ModelDeployment 一個 PVC（<name>-models）→ 第一個 pod 下載，之後的 pod 直接用
```

- **PVC 也由 controller 用 SSA 建立**，帶 ownerReference → 刪 md 時 GC 一起刪，`Owns()` 也會 watch 它（誤刪會重建）
- **不宣告 `storageClassName`**：交給叢集預設（kind 是 `standard`，local-path provisioner）。沒宣告的欄位不歸我們管，API server 填了預設值也不會被我們清掉 —— 換成 Phase 1 的寫法，這裡又是一個「每輪都 updated」的來源
- **ReadWriteOnce**：只能被**同一個節點**上的 pod 掛載。kind 是單節點，所以 2 個 replica 能共用；多節點叢集要換 RWX 儲存（NFS、EFS…），列進 KNOWN-GAPS
- **容量寫死 1Gi**：夠放 135M 模型。正式做法是加一個 spec 欄位，這個 lab 不做

**兩個 pod 同時下載會怎樣？** 共用 PVC 之後，replicas=2 的兩個 init container 可能同時發現「檔案不在」而一起下載。所以下載腳本改成：

```sh
tmp="/models/$MODEL_FILE.partial.$POD_NAME"    # 每個 pod 自己的暫存檔，不會互相寫壞
trap 'rm -f "$tmp"' EXIT                       # 失敗時清掉暫存檔
curl ... -o "$tmp" "$MODEL_URL"
mv "$tmp" "/models/$MODEL_FILE"                # 同一個檔案系統內，rename 是原子操作
```

最壞情況是下載兩次、後完成的 `mv` 蓋掉先完成的 —— 兩份內容一樣，所以沒有問題。`POD_NAME` 用 downward API（`fieldRef: metadata.name`）傳進去。

### 2.2 Degraded condition：把領域知識寫進 controller

URL 打錯時，Phase 1 的 controller 會怎麼回報？init container 裡 `curl -f` 遇到 404 以 exit code 22 結束 → kubelet 重啟它 → `CrashLoopBackOff`。
但 Deployment 的 status 只看得到「0 個 ready」，所以 md 一直顯示 `Available=False / Progressing` —— **看起來像是「再等一下就好」，其實永遠不會好。**

「init container `fetch-model` 非零結束 = 模型下載失敗」是**只有這個 operator 知道的事**。這正是 operator 比 Helm 多出來的價值（00-concepts Part 1）。

```
conditions:
- type: Available
  status: "False"
  reason: ModelDownloadFailed        # 下載失敗時，用失敗原因取代 Progressing
- type: Degraded
  status: "True"
  reason: ModelDownloadFailed
  message: "pod smollm-xxx: fetch-model exited with code 22 (restarts: 3)"
```

實作上要多做兩件事：

1. **watch pod**。pod 的 owner 是 ReplicaSet，不是 md，所以不能用 `Owns()`。改用 `Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(...))`，從 pod 的 `app.kubernetes.io/instance` label 對回 md 的名字。
2. **RBAC 加 `pods: get;list;watch`**。只讀，不需要寫。

> **代價**：`Watches(&corev1.Pod{})` 會讓 informer **cache 整個叢集的所有 pod**。在大叢集上這是可觀的記憶體。解法是在 `cmd/main.go` 用 `cache.Options.ByObject` 只 cache 帶 `managed-by=llm-serving-operator` label 的 pod。這個 lab 不做，列進 KNOWN-GAPS（面試被問「你的 operator 在一萬個 pod 的叢集上會怎樣」時，這是答案）。

### 2.3 Events：只在「轉變」時發

`kubectl describe md smollm` 底部的 Events，是給人在出事時第一眼看的時間軸。規則：

| 時機 | type | reason |
|---|---|---|
| Available 變成 True | Normal | `Available` |
| Available 從 True 變成別的 | Warning | `Unavailable` |
| Degraded 變成 True | Warning | `ModelDownloadFailed` |
| apply 子資源失敗 | Warning | `ReconcileFailed` |

**只在狀態轉變時發，不是每輪 reconcile 都發** —— 一個 md 一天被 reconcile 上千次，每次都發 Event，等於對 etcd 做 DoS，有用的訊息也會被洗掉。
Event 在 status patch **成功之後**才發：patch 失敗的話，下一輪會再看到同樣的轉變，那時再發，不會重複。

**用新版 Events API**：controller-runtime v0.25 的 `mgr.GetEventRecorder(name)` 寫的是 `events.k8s.io/v1`，簽名是

```go
Eventf(regarding, related runtime.Object, eventtype, reason, action, note string, args ...interface{})
```

舊的 `GetEventRecorderFor`（`core/v1` Events）已經標成 deprecated。RBAC 要給 `events.k8s.io` 的 `events: create;patch`。

---

## Part 3：程式碼

### 3.1 先記錄 Phase 1 的 managedFields

改程式之前，先看一眼 Phase 1 用 `Update` 建出來的 Deployment，等一下實驗 1 要對照：

```bash
kubectl get deploy smollm --show-managed-fields \
  -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\t"}{.operation}{"\t"}{.subresource}{"\n"}{end}'
```

把輸出貼進 RESULTS 的 Phase 2 段落。

### 3.2 改 `internal/controller/modeldeployment_controller.go`

整個檔案換成下面這份（保留檔頭的 license 註解）。跟 Phase 1 比，**沒變的**是 `labelsFor`、`desiredReplicas`、`availableCondition`、probe 與資源預設值；其他都是本階段的內容。

```go
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

// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
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
	available := availableCondition(&md, dep)
	if degraded.Status == metav1.ConditionTrue && available.Status == metav1.ConditionFalse {
		// 「Progressing」會讓人以為等一下就好；下載失敗不會自己好，直接講原因。
		available.Reason, available.Message = degraded.Reason, degraded.Message
	}
	meta.SetStatusCondition(&newStatus.Conditions, available)
	meta.SetStatusCondition(&newStatus.Conditions, degraded)

	if equality.Semantic.DeepEqual(oldStatus, newStatus) {
		return ctrl.Result{}, nil
	}
	// Merge patch 不帶 resourceVersion，不做樂觀鎖檢查 → 不會因為 cache 舊而 409。
	// 能這樣做的前提：status 只有這個 controller 在寫（leader election 保證同時只有一個）。
	patch := client.MergeFrom(md.DeepCopy())
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
				WithRequests(corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(modelCacheSize)})))
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

func desiredDeployment(md *servingv1alpha1.ModelDeployment, owner *metav1ac.OwnerReferenceApplyConfiguration) *appsv1ac.DeploymentApplyConfiguration {
	labels := labelsFor(md)
	image := md.Spec.Image
	if image == "" {
		image = defaultLlamaCppImage
	}
	modelsMount := corev1ac.VolumeMount().WithName("models").WithMountPath(modelsDir)

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
					WithInitContainers(corev1ac.Container().
						WithName(fetchContainerName).
						WithImage(modelFetcherImage).
						WithEnv(
							corev1ac.EnvVar().WithName("MODEL_URL").WithValue(md.Spec.Model.URL),
							corev1ac.EnvVar().WithName("MODEL_FILE").WithValue(md.Spec.Model.File),
							corev1ac.EnvVar().WithName("POD_NAME").WithValueFrom(corev1ac.EnvVarSource().
								WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath("metadata.name"))),
						).
						WithCommand("sh", "-c", fetchScript).
						WithVolumeMounts(modelsMount)).
					WithContainers(corev1ac.Container().
						WithName("llama-server").
						WithImage(image).
						WithArgs(
							"--model", modelsDir+"/"+md.Spec.Model.File,
							"--host", "0.0.0.0",
							"--port", strconv.Itoa(servingPort),
							"--ctx-size", "2048",
							"--parallel", "2",
							"--metrics",
						).
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

func availableCondition(md *servingv1alpha1.ModelDeployment, dep *appsv1.Deployment) metav1.Condition {
	desired := desiredReplicas(md)
	c := metav1.Condition{Type: conditionAvailable, ObservedGeneration: md.Generation}
	switch {
	case desired == 0:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0"
	case dep.Status.ObservedGeneration < dep.Generation:
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

// degradedCondition 看 pod 的 init container：下載失敗是 Deployment status 看不出來的領域知識。
func degradedCondition(md *servingv1alpha1.ModelDeployment, pods []corev1.Pod) metav1.Condition {
	c := metav1.Condition{
		Type: conditionDegraded, ObservedGeneration: md.Generation,
		Status: metav1.ConditionFalse, Reason: "NoFailure", Message: "no failure detected",
	}
	for _, p := range pods {
		for _, s := range p.Status.InitContainerStatuses {
			if s.Name != fetchContainerName {
				continue
			}
			// 先看目前狀態；正在重試（Running/Waiting）時才回頭看上一次的結束狀態。
			t := s.State.Terminated
			if t == nil {
				t = s.LastTerminationState.Terminated
			}
			if t != nil && t.ExitCode != 0 {
				c.Status, c.Reason = metav1.ConditionTrue, "ModelDownloadFailed"
				c.Message = fmt.Sprintf("pod %s: %s exited with code %d (restarts: %d)", p.Name, fetchContainerName, t.ExitCode, s.RestartCount)
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
```

讀的時候注意這幾個地方：

1. **`r.Apply` 之後，用回應判斷 Available**（`deploymentFromApply`）。剛 apply 完，cache 裡的 Deployment 可能還是舊的 generation —— 這就是 02 Part 2.3 第 4 點的情境：拿舊的 status 判斷，會在滾動更新還沒開始時就回報 Available=True。apply 的回應是 API server 上最新的物件，generation 是新的、observedGeneration 還是舊的，`availableCondition` 才判斷得對。
2. **log 改印 resourceVersion**：SSA 不會告訴你「這次有沒有改」。連續兩行的 RV 一樣，就確定這次 apply 沒有寫入；RV 不一樣，則可能是我們寫的，也可能是別人（例如 deployment controller 寫 status）在兩輪之間寫的 —— 要分辨，看 managedFields 裡 `llm-serving-operator` 那一筆的 `time`（Part 5.2）。
3. **`resourcesFor` 只送有值的部分**：送一個空的 `requests`，等於宣告「我擁有 requests，而且它是空的」（Part 1.1 的坑 1）。

### 3.3 改 `cmd/main.go`

只改註冊 controller 的那一段，加上 `Recorder`：

```go
	if err := (&controller.ModelDeploymentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("modeldeployment-controller"),
	}).SetupWithManager(mgr); err != nil {
```

```bash
make manifests generate
go build ./...
```

打開 `config/rbac/role.yaml`，確認多了三樣東西：core group 的 rule 多了 `persistentvolumeclaims`、新的 `pods`（只有 `get`、`list`、`watch`）、新的 `events.k8s.io` `events`（`create`、`patch`）。

> CRD 沒有改（`*_types.go` 沒動），所以 `config/crd/bases/` 不會有 diff。

---

## Part 4：envtest 測試

### 4.1 新增的測試

| 測試 | 驗證 | 對應 |
|---|---|---|
| 建立子資源 | 多了 PVC：存在、RWO、ownerReference 指向 CR；Deployment 掛的是這個 PVC | Part 2.1 |
| 沒有變更時不寫入 | 連續 reconcile，Deployment／Service／PVC 的 resourceVersion 都不變 | Phase 1 實驗 7 的回歸測試 |
| 搶回自己的欄位、不碰別人的 | 別人改 replicas → 被拉回 1；別人加的 annotation → 保留；`Apply` 類型的 manager 只有我們 | Part 1.1 |
| Available + Normal Event | 扮演 deployment controller 寫入「全部就緒」的 status → Available=True、發一次 Event；再跑一次不再發 | Part 2.3 |
| Degraded + Warning Event | 扮演 kubelet 做一個 init container exit 22 的 pod → Degraded=True、Available 的 reason 變成 `ModelDownloadFailed` | Part 2.2 |

測試裡的 `Recorder` 用 client-go 的 `events.FakeRecorder`：它把每個 Event 格式化成 `"<type> <reason> <note>"` 丟進一個 channel，用 Gomega 的 `Receive()` 讀。

envtest 沒有 deployment controller 和 kubelet（00-concepts Part 9），所以後兩個測試**自己扮演它們**：直接用 `Status().Update()` 寫 Deployment 和 Pod 的 status。這是 envtest 測「controller 對某種狀態的反應」的標準做法。

### 4.2 改 `internal/controller/modeldeployment_controller_test.go`

整個換掉（保留 license 檔頭；`suite_test.go` 不動）：

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
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Controller", func() {
	const (
		name = "smollm"
		ns   = "default"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: ns}
	pvcKey := types.NamespacedName{Name: name + "-models", Namespace: ns}

	var recorder *events.FakeRecorder

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{
					URL:  "https://example.com/model.gguf",
					File: "model.gguf",
				},
			},
		}
	}

	reconcileOnce := func() {
		r := &ModelDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
	})

	AfterEach(func() {
		md := &servingv1alpha1.ModelDeployment{}
		if err := k8sClient.Get(ctx, key, md); err == nil {
			Expect(k8sClient.Delete(ctx, md)).To(Succeed())
		}
		// envtest 沒有 garbage collector：刪了 owner，子資源不會跟著消失，要手動清。
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(ns), client.GracePeriodSeconds(0))
		// API server 會替 PVC 加上 kubernetes.io/pvc-protection finalizer，平常由 kube-controller-manager 拿掉。
		// envtest 沒有它，不手動拿掉的話 PVC 會永遠卡在 Terminating。
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, pvcKey, pvc); err == nil {
			pvc.Finalizers = nil
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
			_ = k8sClient.Delete(ctx, pvc)
		}
	})

	It("creates owned PVC, Deployment and Service with defaults applied", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Spec.Runtime).To(Equal("llamacpp"))
		Expect(md.Spec.Replicas).To(Equal(ptr.To[int32](1)))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultLlamaCppImage))
		Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElement("/models/model.gguf"))
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvcKey.Name))
		Expect(metav1.GetControllerOf(dep).UID).To(Equal(md.UID))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(servingPort)))
		Expect(metav1.GetControllerOf(svc).UID).To(Equal(md.UID))

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
		Expect(metav1.GetControllerOf(pvc).UID).To(Equal(md.UID))
	})

	It("writes status, and reports Progressing because envtest has no deployment controller", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Status.Endpoint).To(Equal("http://smollm.default.svc:8080"))
		Expect(md.Status.ObservedGeneration).To(Equal(md.Generation))

		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a).NotTo(BeNil())
		Expect(a.Status).To(Equal(metav1.ConditionFalse))
		Expect(a.Reason).To(Equal("Progressing"))
		d := meta.FindStatusCondition(md.Status.Conditions, conditionDegraded)
		Expect(d).NotTo(BeNil())
		Expect(d.Status).To(Equal(metav1.ConditionFalse))
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

	// Phase 1 實驗 7 的回歸測試：沒有變更時，子資源不應該被寫入。
	It("does not write children when nothing changed", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()
		// 實測：PVC 建立後的第一次 re-apply 會多寫一次（內容完全相同、只有 resourceVersion 變），之後就穩定。
		// 所以先多跑一輪讓它穩定，再量「沒有變更時有沒有寫入」。
		reconcileOnce()

		rv := func(obj client.Object, k types.NamespacedName) string {
			Expect(k8sClient.Get(ctx, k, obj)).To(Succeed())
			return obj.GetResourceVersion()
		}
		depRV, svcRV, pvcRV := rv(&appsv1.Deployment{}, key), rv(&corev1.Service{}, key), rv(&corev1.PersistentVolumeClaim{}, pvcKey)

		reconcileOnce()

		Expect(rv(&appsv1.Deployment{}, key)).To(Equal(depRV))
		Expect(rv(&corev1.Service{}, key)).To(Equal(svcRV))
		Expect(rv(&corev1.PersistentVolumeClaim{}, pvcKey)).To(Equal(pvcRV))
	})

	It("takes back fields it owns, and leaves other managers' fields alone", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		// 模擬另一個人：改 replicas（我們擁有的欄位）、加 annotation（我們沒宣告的欄位）。
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Spec.Replicas = ptr.To[int32](3)
		dep.Annotations = map[string]string{"team.example.com/owner": "alice"}
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Annotations).To(HaveKeyWithValue("team.example.com/owner", "alice"))

		var managers []string
		for _, mf := range dep.ManagedFields {
			if mf.Operation == metav1.ManagedFieldsOperationApply {
				managers = append(managers, mf.Manager)
			}
		}
		Expect(managers).To(ConsistOf(string(fieldOwner)))
	})

	It("reports Available and emits a Normal event once replicas are ready", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		// envtest 沒有 deployment controller，自己扮演它：把 Deployment status 寫成「全部就緒」。
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(md.Status.Conditions, conditionAvailable)).To(BeTrue())
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 再跑一次，狀態沒有轉變 → 不應該再發 Event。
		reconcileOnce()
		Expect(recorder.Events).NotTo(Receive())
	})

	It("reports Degraded when the model download fails", func() {
		md := newMD()
		Expect(k8sClient.Create(ctx, md)).To(Succeed())

		// 扮演 kubelet：做一個 init container 以 exit 22（curl -f 遇到 HTTP 4xx）結束的 pod。
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: ns, Labels: labelsFor(md)},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: fetchContainerName, Image: modelFetcherImage}},
				Containers:     []corev1.Container{{Name: "llama-server", Image: defaultLlamaCppImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:         fetchContainerName,
			Image:        modelFetcherImage,
			RestartCount: 2,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 22, Reason: "Error"},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		d := meta.FindStatusCondition(md.Status.Conditions, conditionDegraded)
		Expect(d).NotTo(BeNil())
		Expect(d.Status).To(Equal(metav1.ConditionTrue))
		Expect(d.Reason).To(Equal("ModelDownloadFailed"))
		Expect(d.Message).To(ContainSubstring("exited with code 22"))
		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a.Reason).To(Equal("ModelDownloadFailed"))
		Expect(recorder.Events).To(Receive(HavePrefix("Warning ModelDownloadFailed")))
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
go test ./internal/controller/ -v -count=1    # 應該看到 Ran 13 of 13 Specs：8 個 It 加上 DescribeTable 的 5 個 Entry
make lint
```

**完成條件**：13 個全部通過、`make lint` 是 `0 issues`，記下 `internal/controller` 的覆蓋率（Phase 1 是 81.2%）。

> **為什麼「沒有變更時不寫入」那個測試要先多 reconcile 一次**：寫這份文件時在 envtest 上實測，**PVC 建立後的第一次 re-apply 會多寫一次** —— 前後兩份物件逐欄比較完全相同，只有 resourceVersion 變了；之後再 apply 就不會再變。Deployment 和 Service 沒有這個現象。原因沒有查明（推測跟 PVC 建立時 admission 加上 `pvc-protection` finalizer 有關），但它只發生一次，不會造成迴圈。
> 如果你把那一行拿掉，測試會失敗在 PVC 的 resourceVersion 比較。可以自己試一次，把觀察寫進 RESULTS。

> 失敗時先看是哪一類：
> - 測試卡在 AfterEach 或下一個測試出現 `object is being deleted` → PVC 的 finalizer 沒拿掉（AfterEach 那段註解）
> - `Receive(Equal("Normal Available ..."))` 失敗 → 先印 `recorder.Events` 裡實際收到什麼；FakeRecorder 的格式是 `type reason note`，沒有 action
> - `managers` 多了一個 → 有東西用 apply 寫了 Deployment，檢查是不是 field owner 名稱打錯成兩個

---

## Part 5：在 kind 上跑起來

### 5.1 清掉 Phase 1 建的資源

Phase 1 的 Deployment 欄位，在 managedFields 裡屬於 Phase 1 controller 的 `Update` manager。直接換成 SSA 也能跑，但那些欄位會**同時被兩個 manager 擁有**：之後就算 apply 不再送某個欄位，它也不會被刪（另一個 manager 還擁有它）。
這個 lab 直接刪掉重建，最乾淨：

```bash
# 終端機 A：如果 Phase 1 的 controller 還在跑，先 Ctrl-C
kubectl delete md smollm
kubectl get deploy,svc,pods -l app.kubernetes.io/instance=smollm    # 等到 No resources found
```

> 正式環境不能刪重建時，client-go 有 `k8s.io/client-go/util/csaupgrade` 可以把舊 manager 的欄位轉移給 SSA manager。知道有這個工具就好。

### 5.2 跑新的 controller

```bash
make install        # CRD 沒變，跑了也無妨；確認叢集上是最新的
```

**終端機 A**（這次把 log 同時存檔，實驗 3、6 要數行數）：

```bash
make run 2>&1 | tee /tmp/operator-phase2.log
```

**終端機 B**：

```bash
kubectl apply -f config/samples/serving_v1alpha1_modeldeployment.yaml
kubectl get md -w
```

另一個視窗看 PVC（`kubectl get -w` 一次只能 watch 一種資源，寫成 `md,pvc` 會報 `you may only specify a single resource type`）：

```bash
kubectl get pvc -w
```

**預期**：

- PVC `smollm-models` 先是 `Pending`（kind 的 `standard` StorageClass 是 `WaitForFirstConsumer`，要等 pod 被排程），之後 `Bound`
- 第一次一樣要下載模型（約 1 分鐘）。`llama.cpp:server` image 在 Phase 1 已經拉過，這次不用再等 8 分鐘
- log 每一輪是一行 `Applied children`，帶三個 RV。**RV 變了不一定是我們寫的**：deployment controller 寫 Deployment status、scheduler 與 PV controller 綁定 PVC，都會讓 RV 變。所以 `deploymentRV` 和 `pvcRV` 在啟動期間會變好幾次，`serviceRV` 從頭到尾不變（沒有別人會寫 Service）
- 要證明「我們的 apply 沒有寫入」，看 managedFields 裡我們那一筆的時間：

  ```bash
  kubectl get deploy smollm --show-managed-fields \
    -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\t"}{.operation}{"\t"}{.time}{"\n"}{end}'
  ```

  `llm-serving-operator  Apply` 的時間只有在我們的 apply 真的改了東西時才會更新。啟動後 log 跑了十幾輪，這個時間應該還停在建立那一秒
- `kubectl describe md smollm` 最後出現 `Normal  Available  ...  1/1 replicas ready`

記下：從 apply 到 Available=True 花多久、log 總共幾輪、有沒有 409（`grep -c "the object has been modified" /tmp/operator-phase2.log`）。Phase 1 同一個階段是 5 輪（1 輪 `created`、4 輪 `updated`）、1 次 409。

推論請求照 02 Part 4.3 再打一次，確認服務正常。

---

## Part 6：八個實驗

格式跟 Phase 1 一樣：**指令、關鍵輸出、時間**記進 RESULTS 的 Phase 2 段落。這一階段多一欄「Phase 1 對照」—— 能直接比較數字，才看得出修法有沒有用。

### 實驗 1：managedFields 換人了

```bash
kubectl get deploy smollm --show-managed-fields \
  -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\t"}{.operation}{"\t"}{.subresource}{"\n"}{end}'
```

**預期**：一行 `llm-serving-operator  Apply`（我們，spec 與 metadata），一行 `kube-controller-manager  Update  status`（deployment controller 寫 status）。
跟 Part 3.1 記下的 Phase 1 輸出對照：Phase 1 的 manager 是誰、operation 是什麼？

想看更細：`kubectl get deploy smollm --show-managed-fields -o yaml`，找 `manager: llm-serving-operator` 底下的 `fieldsV1`，確認裡面**沒有** `imagePullPolicy`、`terminationMessagePath` —— 那些是預設值，沒有人擁有。

### 實驗 2：沒有變更的 reconcile 還會寫入嗎？（Phase 1 實驗 7）

**先試 Phase 1 的做法**：

```bash
kubectl annotate md smollm lab/poke="$(date +%s)" --overwrite
```

**預期**：終端機 A **沒有任何新的 log**。annotation 不改 generation，被 predicate 擋掉了（Part 1.3）。

**改成戳 Deployment**（`Owns()` 不受 predicate 影響）：

```bash
RV1=$(kubectl get deploy smollm -o jsonpath='{.metadata.resourceVersion}')
kubectl annotate deploy smollm lab/poke="$(date +%s)" --overwrite
sleep 2
RV2=$(kubectl get deploy smollm -o jsonpath='{.metadata.resourceVersion}')
echo "$RV1 -> $RV2"
kubectl get deploy smollm -o jsonpath='{.metadata.annotations.lab/poke}{"\n"}'
```

**預期**：

- annotation 還在：它屬於 `kubectl-annotate` 這個 manager，我們的 apply 不會碰它
- controller 的 apply **沒有寫入**：用 Part 5.2 的指令看 managedFields，`llm-serving-operator` 的時間沒有變，而且多了一個 `kubectl-annotate  Update`
- RV1 → RV2 會變**兩次**：一次是 `kubectl annotate` 自己寫的，一次是 deployment controller 更新 `status.observedGeneration`（原因見下面的插曲）。都不是我們寫的

> 寫這份文件時原本預期「RV 只變一次、不會有 Event」。實測時 md 在同一秒內發出 `Warning Unavailable` 和 `Normal Available` —— controller 有一個缺陷，下一節修。

Phase 1 對照：log 是 `deployment: updated`，resourceVersion 也沒變。**兩邊其實都發了一個寫入請求**：Phase 1 是 client 端誤判「不一樣」才送 Update；Phase 2 是每輪都送 apply，由 API server 合併後判斷沒變。
所以 SSA 的改進**不是少一個請求**，而是：不再誤判、不帶 resourceVersion 所以不會 409、不會清掉別人的欄位。這一點寫進 RESULTS 時要講精確。

### 插曲 1：Available 為什麼閃了一下（修正 `availableCondition`）

**做到實驗 2 時實際看到的**（RESULTS 2026-11-20 14:39:52）：

```
Applied children  deploymentRV: 92628
Event  Warning  Unavailable  Progressing: waiting for deployment controller to observe the latest spec
Applied children  deploymentRV: 92630
Event  Normal   Available    1/1 replicas ready
```

pod 沒有動，服務沒有中斷，但 md 的 status 和 Events 都說「中斷了一下」。

**查證**：

```bash
kubectl get deploy smollm -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
# 建立時是 1 1，annotate 之後變成 2 2
```

**Deployment 的 annotation 變動，也會讓 `metadata.generation` +1。** annotation 會被複製到 ReplicaSet，所以 deployment controller 要重新處理一次。
00-concepts Part 5 說「只有 spec 變動會讓 generation +1」，這對我們的 CR 成立（CRD 的規則），但**每種內建資源可以有自己的規則**，Deployment 就是例外。

所以事情的順序是：

1. `kubectl annotate` 寫入 → generation 1→2（RV 92628）
2. `Owns()` 觸發 reconcile → apply 回應裡 generation=2、observedGeneration=1 → `availableCondition` 判定「deployment controller 還沒看到最新 spec」→ 回報 False → 發 `Unavailable`
3. deployment controller 處理完，observedGeneration=2（RV 92630）→ 再 reconcile → True → 發 `Available`

**缺陷在哪裡**：02 Part 2.3 第 4 點的設計是「generation 落後時不能拿 Deployment 的 status 判斷」—— 這是對的。錯在接下來的動作：**不能判斷，不代表要回報 False**。
落後的原因可能是「我們剛改了 spec」，也可能是「別人碰了 Deployment」，controller 分不出來。Phase 1 實驗 4 的 Available 在同一秒內 True→False→True，很可能也是同一個原因（`kubectl scale` 也會讓 generation +1）。

**修法：不知道結果，就保留上一次的觀察。**

| 情境 | 修正前 | 修正後 |
|---|---|---|
| 第一次建立，還沒有 Available condition | False / Progressing | False / Progressing（不變） |
| 別人改了 Deployment 的 annotation | True → **False** → True，發兩筆 Event | 一直是 True，不發 Event |
| md 改了 spec（例如 replicas 1→2） | 立刻 False / Progressing | 保留原本的 True **約一秒**，deployment controller 處理完後變成 False / Progressing |

最後一列是代價：改 spec 之後的那一瞬間，Available 還是 True。但被保留的 condition 身上帶著**舊的 `observedGeneration`**（例如 1，而 md 的 generation 已經是 2）—— 讀的人比對一下就知道這是舊 spec 的觀察結果。這正是 condition 上 `observedGeneration` 欄位存在的理由（00-concepts Part 5）。
依賴 Available 的工具都應該做這個比對；只看 `status` 不看 `observedGeneration`，在任何 controller 上都可能讀到舊結果。

**先寫測試**。在 `modeldeployment_controller_test.go` 的 `It("reports Degraded when the model download fails", ...)` **前面**加上：

```go
	// Phase 2 實驗 2 的回歸測試：別人改了 Deployment 的 annotation，Available 不應該閃一下。
	It("keeps Available when only the Deployment's annotations change", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce()
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 kubectl annotate deploy：Deployment 的 annotation 一變，generation 也會 +1。
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		genBefore := dep.Generation
		dep.Annotations = map[string]string{"lab/poke": "1"}
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(dep.Generation).To(Equal(genBefore + 1))

		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(md.Status.Conditions, conditionAvailable)).To(BeTrue())
		Expect(recorder.Events).NotTo(Receive())
	})
```

跑一次，**確認它會失敗**：

```bash
go test ./internal/controller/ -count=1
# 預期：14 個跑 13 個過，失敗在 IsStatusConditionTrue 那行
```

注意 `Expect(dep.Generation).To(Equal(genBefore + 1))` 那行是**通過**的 —— envtest 的 API server 也證實了「Deployment 改 annotation 會讓 generation +1」。

**再改程式**。`modeldeployment_controller.go` 兩個地方：

1. `Reconcile` 裡呼叫 `availableCondition` 的那一行，多傳目前的 condition：

```go
	available := availableCondition(&md, dep, meta.FindStatusCondition(md.Status.Conditions, conditionAvailable))
```

2. `availableCondition` 的開頭到 `ObservedGeneration < Generation` 那個 case，換成：

```go
// current 是 md 目前的 Available condition，第一次 reconcile 時是 nil。
func availableCondition(md *servingv1alpha1.ModelDeployment, dep *appsv1.Deployment, current *metav1.Condition) metav1.Condition {
	desired := desiredReplicas(md)
	c := metav1.Condition{Type: conditionAvailable, ObservedGeneration: md.Generation}
	switch {
	case desired == 0:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0"
	case dep.Status.ObservedGeneration < dep.Generation:
		// deployment controller 還沒看到最新的 Deployment，它的 status 不能拿來下結論。
		// 落後的原因可能是我們剛改了 spec，也可能只是有人改了 annotation（Deployment 的 annotation 變動也會讓 generation +1）。
		// 不知道結果就不要改：保留上一次的觀察。它身上的 observedGeneration 會告訴讀者「這是舊 spec 的結果」。
		if current != nil {
			return *current
		}
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Progressing", "waiting for deployment controller to observe the latest spec"
```

（其他 case 不動。）

```bash
make test
go test ./internal/controller/ -v -count=1    # Ran 14 of 14 Specs
make lint
```

寫這份文件時實測：14／14 通過、覆蓋率 81.2%、`make lint` 0 issues。

**在 kind 上驗證**：終端機 A 重啟 controller（Ctrl-C → `make run 2>&1 | tee /tmp/operator-phase2.log`），等 md 是 Available=True 之後，再做一次實驗 2 的 Deployment annotate。

**預期**：log 一樣有 2 輪 `Applied children`（generation 變化與 observedGeneration 更新各觸發一次），但**沒有任何 `Event occurred`**；`kubectl describe md smollm` 的 Events 沒有新的 `Unavailable`。

> ⚠️ 這個修正有漏洞，實驗 4 會踩到，修法在插曲 2。照順序做，兩次修正都會經歷到。

> 這個缺陷是 Phase 1 就有的，到 Phase 2 才看到，因為 Phase 1 沒有 Events —— Available 閃一下只會反映在 `lastTransitionTime` 上，很難發現。**可觀測性做得越好，越容易抓到原本就存在的 bug。**

### 實驗 3：有人手動擴容（Phase 1 實驗 4）

```bash
date +%T; kubectl scale deploy smollm --replicas=3
kubectl get deploy smollm -w
```

**預期**：replicas 一樣在 1 秒內被拉回 1（`ForceOwnership` 把 `spec.replicas` 從 kubectl 手上搶回來）。
`kubectl scale` 也會讓 Deployment 的 generation +1，做過插曲的修正之後，md 的 Available 應該**一直是 True、沒有 `Unavailable` Event**（Phase 1 這裡是 True→False→True）。

數這一秒內的 reconcile 輪數與 409：

```bash
# 每一秒各有幾輪（log 每行開頭是 2026-11-20T15:39:38+08:00，第 12～19 個字元就是時:分:秒）
grep "Applied children" /tmp/operator-phase2.log | cut -c12-19 | uniq -c | tail -5
grep -c "the object has been modified" /tmp/operator-phase2.log
grep -c "Event occurred" /tmp/operator-phase2.log
```

Phase 1 對照：9 輪、1 次 409（status）。
**預期 409 是 0**。輪數**不一定變少** —— 這一階段多 watch 了 pod，多出來的 pod 被建立又刪除，每個 pod 事件都會觸發 reconcile。但這些輪次大多是 no-op（RV 不變）。**輪數多不是問題，每輪都寫入、都衝突才是問題**。實際數字以你看到的為準。

### 實驗 4：誤刪 Deployment，這次模型還在（Phase 1 實驗 3）

```bash
date +%T; kubectl delete deploy smollm
kubectl get md smollm -w
```

另一個視窗：

```bash
kubectl logs -l app.kubernetes.io/instance=smollm -c fetch-model -f
```

**預期**：

- Deployment 一樣立刻重建
- init container 印 `model already present`，直接結束
- md 回到 Available=True 的時間**從 61 秒降到十幾秒以內**（剩下的是排程、啟動 llama-server、等 startup／readiness probe，probe 週期是 5 秒）
- `kubectl describe md smollm` 的 Events 多了 `Warning Unavailable` 接著 `Normal Available`（`Normal Available` 可能是 `(x2)`，原因見 Part 1.2 最後一段）

記下：實際恢復時間、Events 原文。

恢復時間用 log 裡的 Event 時間算最準（`kubectl get md -w` 沒有時間戳）：

```bash
grep "Event occurred" /tmp/operator-phase2.log | tail -3
```

### 插曲 2：Deployment 被刪掉時，Available 還是 True

**做到實驗 4 時實際看到的**（2026-11-20 15:45:52 刪除 Deployment）：

```
NAME     RUNTIME    READY   AVAILABLE   AGE
smollm   llamacpp           True        95m     ← 一個 pod 都沒有，卻是 Available
smollm   llamacpp           False       95m
smollm   llamacpp   1       True        95m
```

**原因是插曲 1 的修正太寬**：Deployment 被刪掉後，controller 用 apply 重建一個新的。新 Deployment 的 generation=1、observedGeneration=0（deployment controller 還沒處理過），落進「generation 落後 → 保留上一次的觀察」那個分支，於是沿用了舊的 True。
插曲 1 說「被保留的 condition 帶著舊的 observedGeneration，讀的人看得出它是舊的」—— **這只在 md 的 spec 改了的時候成立**。這次 md 一個字都沒改，condition 的 observedGeneration 跟 md 的 generation 一樣，沒有人看得出它是錯的。

**修法：generation 落後時，`UpdatedReplicas` 不能信，但 `ReadyReplicas` 仍然是事實**（現在有幾個 pod 在服務）。只有在 pod 還夠的時候才保留上一次的觀察：

| 情境 | 插曲 1 | 插曲 2 |
|---|---|---|
| 別人改 Deployment 的 annotation（ready 1，要 1） | 保留 True | 保留 True（不變） |
| `kubectl scale`（ready 1，要 1） | 保留 True | 保留 True（不變） |
| **Deployment 被刪掉重建**（status 是空的，ready 0） | **錯誤地保留 True** | False / Progressing |
| md 的 replicas 1→2（ready 1，要 2） | 保留 True 約一秒 | **立刻** False / Progressing（比插曲 1 更準） |
| md 換 image、replicas 不變（ready 1，要 1） | 保留 True 約一秒 | 保留 True 約一秒（不變，帶舊的 observedGeneration） |

**先寫測試**。在 `It("reports Degraded when the model download fails", ...)` **前面**再加一個：

```go
	// Phase 2 實驗 4 的回歸測試：Deployment 被刪掉重建，不能沿用舊的 Available=True。
	It("reports Progressing when the Deployment is recreated", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce()
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 kubectl delete deploy（envtest 沒有 GC，刪除會立刻生效）。
		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a.Status).To(Equal(metav1.ConditionFalse))
		Expect(a.Reason).To(Equal("Progressing"))
		Expect(recorder.Events).To(Receive(HavePrefix("Warning Unavailable")))
	})
```

```bash
go test ./internal/controller/ -count=1
# 預期：15 個跑 14 個過，失敗在 a.Status 那行（Expected True to equal False）
```

**再改程式**。`availableCondition` 裡插曲 1 加的那段，換成：

```go
		// deployment controller 還沒看到最新的 Deployment，UpdatedReplicas 還不能信。
		// 落後的原因可能是我們剛改了 spec，也可能只是有人改了 annotation（Deployment 的 annotation 變動也會讓 generation +1）。
		// ReadyReplicas 仍然是事實（現在有幾個 pod 在服務）。pod 還夠，就保留上一次的觀察，不要因為「還不知道」就報中斷；
		// pod 不夠（例如 Deployment 剛被刪掉重建，status 是空的），就老實回報 Progressing。
		if current != nil && dep.Status.ReadyReplicas >= desired {
			return *current
		}
```

```bash
make test
go test ./internal/controller/ -v -count=1    # Ran 15 of 15 Specs
make lint
```

寫這份文件時實測：15／15 通過、覆蓋率 82.1%、`make lint` 0 issues。插曲 1 的測試（annotation 不閃）一樣通過。

**在 kind 上驗證**：重啟 controller，再做一次實驗 4。**預期**：`kubectl get md -w` 刪除後的第一行就是 `False`，不會再出現「READY 空白、AVAILABLE True」；Events 有 `Warning Unavailable` 接著 `Normal Available`。

> **兩次插曲的教訓**：插曲 1 修好了看到的問題，卻在沒測到的情境製造了新問題。測試只證明「測到的情境」是對的 —— 插曲 1 只加了 annotation 的測試，沒有想到「Deployment 整個不見」也會落進同一個分支。修 condition 判斷這種邏輯時，把所有會進到那個分支的情境列成表（像上面那張），一列一列檢查，比只修眼前那一個情境可靠。

### 實驗 5：兩個 replica 共用快取（Phase 1 實驗 5）

```bash
date +%T; kubectl patch md smollm --type=merge -p '{"spec":{"replicas":2}}'
kubectl get md smollm -w
kubectl logs -l app.kubernetes.io/instance=smollm -c fetch-model --prefix
```

**預期**：新 pod 的 init container 印 `model already present`；Ready 1→2 遠少於 Phase 1 的 61 秒。
順便確認 RWO 在單節點上可以被兩個 pod 同時掛載：`kubectl get pods -l app.kubernetes.io/instance=smollm -o wide`（NODE 欄都是 `operator-lab-control-plane`）。

做完改回 1。

### 插曲 3：重複的 Event —— 把 status 的樂觀鎖加回來

**做到實驗 4、5 時實際看到的**：

```
16:11:15  Normal   Available    1/1 replicas ready
16:11:15  Normal   Available    1/1 replicas ready                                   ← 實驗 4，describe 顯示 (x2)
16:18:18  Warning  Unavailable  Progressing: waiting for deployment controller to observe the latest spec
16:18:18  Warning  Unavailable  Progressing: 1/2 replicas ready                      ← 實驗 5，同一個轉變發了兩次
```

實驗 5 的兩筆 note 不一樣，這是關鍵證據：**它們是兩輪 reconcile 各自算出來的**，不是同一輪發了兩次。

**原因就是 Part 1.2 最後那段說的代價**：

1. 第一輪：從 cache 讀到 md（Available=True）→ 算出 False → patch status → 發 `Unavailable`
2. 緊接著 pod／Deployment 有事件 → 第二輪開始。這時 informer cache **還沒收到**第一輪寫的 status，讀到的 md 還是 True
3. 第二輪又算出「True → False」→ merge patch 不帶 resourceVersion，**寫入成功** → 又發一次 `Unavailable`

status 的最終內容是對的，但 Events 是給人看的時間軸，兩筆互相矛盾的 note 比偶爾一次 409 更糟。而且實驗 4、5 都出現 —— 狀態轉變之後幾乎一定緊接著 pod 事件，**這是常態，不是偶發**。

**修法：status patch 帶上 resourceVersion。** 第二輪的 md 是舊的，resourceVersion 也是舊的 → API server 回 409 → 回傳 error、退避重試 → 下一輪 cache 已經跟上，讀到 False，沒有轉變，不寫也不發 Event。

**這個 409 跟 Phase 1 的 409 不一樣**：

| | Phase 1 的 409 | 插曲 3 之後的 409 |
|---|---|---|
| 發生在 | 子資源的 `Update`、status 的 `Update` | 只有 status patch |
| 起因 | 整個覆寫 PodSpec，每輪都送一個無效的寫入，剛好碰上 cache 舊 | 這一輪的**判斷**是根據舊的 status 做的 |
| 擋下來的是 | 一個本來就不需要的寫入 | 一個**錯誤的**寫入（重複的轉變） |
| 頻率 | 每次有事件都可能 | 只在狀態轉變後、cache 跟上之前 |

**為什麼子資源不用加鎖，status 要？** 看寫入的內容是不是根據「讀到的舊值」算出來的：

| | Deployment／Service／PVC（SSA apply） | md status（merge patch） |
|---|---|---|
| 送出的內容 | 我要的欄位與值，**不帶** resourceVersion | 改了什麼，**帶**讀到的 resourceVersion |
| 內容從哪裡算出來 | 只看 md 的 spec —— 跟子資源現在長什麼樣無關 | 要拿**舊的 status** 比較，才知道有沒有轉變、要不要發 Event |
| 讀到舊資料會怎樣 | 沒影響，要送的內容一樣 | 算錯：把已經發過的轉變再算一次 |

**寫入內容取決於你讀到的舊值（讀—改—寫），就要加鎖；只是宣告「應該長這樣」，就不用。** 實驗 6 的結果正好對應：子資源的 409 是 0，status 的 409 是 1。
（SSA 也有另一種 409：欄位擁有權衝突。我們用 `ForceOwnership` 直接搶回，所以也不會出現 —— 實驗 3 的 `kubectl scale` 就是這種情況。）

**樂觀鎖的價值不在「防止兩個人互相覆蓋」而已，也在防止「同一個人根據舊資料做決定」。** Part 1.2 只考慮了前者，所以得出「只有一個寫入者，可以不加鎖」—— 這個結論對資料正確性成立，但對 Event 這種副作用不成立。這是面試可以講的一個點。

**先寫測試。** envtest 的 `k8sClient` 沒有 cache，重現不了「讀到舊資料」。但可以在測試裡包一個 client，**Get md 時回傳一份事先存好的舊快照**，其他操作照常 —— 等於手動製造 cache 落後的狀態。

在測試檔**最底部**（`var _ = Describe(...)` 的外面）加上：

```go
// staleReader 模擬 informer cache 落後：Get ModelDeployment 時回傳一份舊的快照，其他照常。
type staleReader struct {
	client.Client
	stale *servingv1alpha1.ModelDeployment
}

func (s *staleReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if md, ok := obj.(*servingv1alpha1.ModelDeployment); ok {
		s.stale.DeepCopyInto(md)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}
```

在 `It("reports Degraded when the model download fails", ...)` **前面**加上：

```go
	// Phase 2 實驗 4、5 的回歸測試：cache 舊的時候，不能把同一個轉變再寫一次、再發一次 Event。
	It("does not repeat a transition when it reads a stale ModelDeployment", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce() // Available=False（Progressing）

		// 先留一份「舊的」md：status 還是 False，resourceVersion 也是舊的。
		stale := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, stale)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce() // False → True，發一次 Event
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 informer cache 還沒收到剛才的 status 寫入：Get md 時回傳舊快照。
		r := &ModelDeploymentReconciler{
			Client:   &staleReader{Client: k8sClient, stale: stale},
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got: %v", err) // 樂觀鎖擋下根據舊資料做的寫入
		Expect(recorder.Events).NotTo(Receive())                       // 沒有重複的 Event
	})
```

跑一次，確認它會失敗：

```bash
go test ./internal/controller/ -count=1
# 預期：16 個跑 15 個過，失敗在 IsConflict 那行，訊息是 got: <nil> —— 舊程式讓這次寫入成功了
```

**再改程式。** `Reconcile` 裡 status patch 那段（原本的兩行註解加 `patch := client.MergeFrom(...)`）換成：

```go
	// 帶 resourceVersion 的 merge patch（樂觀鎖）：md 是從 cache 讀的，cache 舊時這次寫入會 409。
	// 這是「有意義的 409」—— 這一輪的判斷是根據舊的 status 做的（例如把已經發過的 False→True 又算一次），
	// 擋下來、退避重試，下一輪讀到新的 status，就不會重複寫入、重複發 Event。
	patch := client.MergeFromWithOptions(md.DeepCopy(), client.MergeFromWithOptimisticLock{})
```

```bash
make test
go test ./internal/controller/ -v -count=1    # Ran 16 of 16 Specs
make lint
```

寫這份文件時實測：16／16 通過、覆蓋率 83.0%、`make lint` 0 issues；把這一行改回 `client.MergeFrom(md.DeepCopy())`，新測試就失敗在 `IsConflict`。

**在 kind 上驗證**：重啟 controller，重做一次實驗 5（replicas 改成 2，再改回 1）。

**預期**：

- 每個轉變只有**一筆** Event（`grep "Event occurred" /tmp/operator-phase2.log | tail -4`）
- log 裡可能出現 `patch status: ... the object has been modified` 的 409 —— 那就是被擋下的第二輪。數一下有幾次：`grep "the object has been modified" /tmp/operator-phase2.log | grep -c "patch status"`

> 409 會以 `ERROR Reconciler error` 的形式出現在 log 裡，看起來很嚇人，但這裡是預期中的行為。如果覺得太吵，可以在 `Reconcile` 裡用 `apierrors.IsConflict(err)` 判斷，改成 `return ctrl.Result{RequeueAfter: time.Second}, nil` 安靜地重試。這個 lab 保留 error，讓它在 log 裡看得到。

### 實驗 6：controller 停機期間的變更（Phase 1 實驗 8）

```bash
# 終端機 A：Ctrl-C，然後
make run 2>&1 | tee /tmp/operator-phase2-restart.log
```

順序跟 Phase 1 一樣：停 controller → `kubectl patch md smollm --type=merge -p '{"spec":{"replicas":2}}'` → 確認 Deployment 還是 1 → `make run` → 看 Deployment 變成 2。

數啟動那一秒的輪數與 409：

```bash
grep -c "Applied children" /tmp/operator-phase2-restart.log
grep -c "the object has been modified" /tmp/operator-phase2-restart.log
```

Phase 1 對照：啟動那一秒 7 輪、**4 次 409**（1 次 status、3 次 deployment）。
**預期**：子資源（apply）的 409 是 0。status 的 409 可能有幾次（插曲 3 加回的樂觀鎖，擋下根據舊資料做的轉變），分開數：

```bash
grep "the object has been modified" /tmp/operator-phase2-restart.log | grep -c "patch status"
grep "the object has been modified" /tmp/operator-phase2-restart.log | grep -vc "patch status"    # 預期 0
```

做完改回 1。

### 實驗 7：模型下載失敗

```bash
kubectl apply -f - <<'EOF'
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata: { name: broken }
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/does-not-exist.gguf
    file: does-not-exist.gguf
EOF
kubectl get pods -l app.kubernetes.io/instance=broken -w
```

等 pod 變成 `Init:Error` 或 `Init:CrashLoopBackOff`，然後：

```bash
kubectl get md broken -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
kubectl describe md broken | tail -5
kubectl logs -l app.kubernetes.io/instance=broken -c fetch-model    # 不要加 --previous：Init:Error 時最近一次失敗的就是目前的 container
```

**預期**：

- `Available  False  ModelDownloadFailed`、`Degraded  True  ModelDownloadFailed  pod broken-xxx: fetch-model exited with code 22 ...`
- Events 有一筆 `Warning  ModelDownloadFailed`，**只有一筆** —— 之後 CrashLoopBackOff 每重啟一次，restarts 會增加、message 會變，但 Degraded 一直是 True，沒有「轉變」，所以不再發
- log 裡看得到 curl 的 `The requested URL returned error: 404`（如果剛好碰上 container 正在重跑，等幾秒再讀一次）

> 如果 Hugging Face 回的不是 404（例如 401），exit code 一樣是 22（`curl -f` 對所有 ≥400 的回應都是 22），結果不受影響。把實際的 HTTP 狀態碼記下來。

**證明了什麼**：Phase 1 的 controller 碰到同樣情況，只會說 `Progressing`。這一行判斷，就是「把維運知識寫成程式碼」。

做完 `kubectl delete md broken`。

### 實驗 8：finalizer —— 正確的用法與錯誤的後果

**8a：Kubernetes 自己的 finalizer**

```bash
kubectl get pvc smollm-models -o jsonpath='{.metadata.finalizers}{"\n"}'    # 預期 ["kubernetes.io/pvc-protection"]
date +%T; kubectl delete pvc smollm-models --wait=false
kubectl get pvc smollm-models                                               # 預期 STATUS: Terminating
```

此時用 02 Part 4.3 的 curl 打一次推論：**服務照常回應**。
`pvc-protection` 擋住了刪除：還有 pod 在用這個 PVC，真的刪掉的話，pod 的資料就沒了。這是 finalizer 的教科書用法 —— **保護「GC 不知道還在被使用」的東西**。

接著把正在用它的 pod 拿掉：

```bash
date +%T; kubectl delete deploy smollm
kubectl get pvc -w                                              # 一次只能 watch 一種資源
kubectl get pods -l app.kubernetes.io/instance=smollm -w        # 另一個視窗
```

**預期**（這段是推論，以實際看到的為準）：pod 終止後，PV controller 拿掉 finalizer → PVC 真的被刪除 → `Owns()` 收到事件 → controller 重建 PVC（空的）和 Deployment → 新 pod **重新下載模型**。
記下整個過程的時間軸，以及 md 回到 Available=True 花多久（應該接近 Phase 1 的 61 秒，因為快取沒了）。

**8b：亂加 finalizer 的後果**

```bash
kubectl patch md smollm --type=merge -p '{"metadata":{"finalizers":["llmlab.dev/demo"]}}'
date +%T; kubectl delete md smollm --wait=false
kubectl get md smollm -o jsonpath='{.metadata.deletionTimestamp}{"  "}{.metadata.finalizers}{"\n"}'
kubectl get deploy,svc,pvc -l app.kubernetes.io/instance=smollm
```

**預期**：md 有了 `deletionTimestamp`，但**一直不會消失**；子資源也全部還在（owner 還沒真的被刪，GC 不會動手）。controller 看到 `deletionTimestamp` 就直接 return，沒有人會拿掉 `llmlab.dev/demo`。

這就是 00-concepts Part 6 說的風險：**finalizer 對應的 controller 不在了（或從來沒人處理它），物件就永遠刪不掉**。namespace 卡在 Terminating，十之八九是這個原因。查法就是上面那行 jsonpath。

手動解開：

```bash
kubectl patch md smollm --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'
kubectl get deploy,svc,pvc -l app.kubernetes.io/instance=smollm    # GC 接手，子資源消失
```

> **手動移除 finalizer 是最後手段**：如果那個 finalizer 是真的有清理工作要做（例如刪雲端的 bucket），硬拔掉等於跳過清理，外部資源就成了孤兒。拔之前要先確認它在等什麼。

做完重新 `kubectl apply` 範例 CR，等它回到 Available=True。

**8 的結論，寫進 RESULTS**：這個 operator 為什麼不需要 finalizer？（子資源都在同 namespace、都有 ownerReference，GC 處理得了；沒有叢集外資源要清。）什麼情況下它會需要？（例如之後把 endpoint 註冊到外部的 gateway 或 DNS。）

---

## Part 7：記錄與收工

1. RESULTS 的 Phase 2 段落：envtest 數字、Part 5.2 的啟動數字、八個實驗。實驗 2、3、4、6 用表格**並列 Phase 1 的數字**
2. 「與預期不同」：特別是實驗 3 的輪數、實驗 8a 的時間軸、PVC 第一次 re-apply 的多寫一次在 kind 上有沒有看到
3. 更新 `KNOWN-GAPS.md`：
   - 刪掉已經修掉的兩條：「整個覆寫 PodSpec」「模型存在 emptyDir」
   - 新增：PVC 是 RWO，多節點上 replica 無法共用快取；PVC 容量寫死 1Gi，換模型後舊檔案不會被清掉；`Watches(Pod)` 會 cache 叢集內所有 pod；envtest 的 client 沒有 cache，cache 落後造成的行為（例如插曲 3 的重複 Event）只能用 `staleReader` 這種手工模擬或在 kind 上觀察
4. README 的階段表：Phase 2 改成 ✅，「成果」填上 Phase 1 vs Phase 2 的對照數字（只填量過的）
5. commit：

```bash
git add -A
git commit -m "feat: server-side apply, PVC model cache, Degraded condition and events"
```

## 完成條件

- [x] `make test` 全過（Part 4 是 13 個，插曲 1 之後 14 個，插曲 2 之後 15 個，插曲 3 之後 16 個）、`make lint` 0 issues，覆蓋率已記錄
- [x] 插曲 1：修正後，annotate Deployment 不再發出 `Unavailable` Event
- [x] 插曲 2：修正後，刪除 Deployment 時 md 立刻變成 Available=False
- [x] 插曲 3：修正後，每個狀態轉變只有一筆 Event；status 的 409 次數已記錄
- [x] 實驗 2：controller 的 apply 沒有改變 Deployment 的 resourceVersion，log 與 kubectl 兩邊都有證據
- [x] 實驗 3、6：409 的次數已記錄，並和 Phase 1 並列
- [x] 實驗 4：恢復時間已記錄，和 Phase 1 的 61 秒並列
- [x] 實驗 7：Degraded 的 condition 與 Event 原文已貼進 RESULTS
- [x] 實驗 8：寫下「這個 operator 為什麼不需要 finalizer」
- [x] KNOWN-GAPS 已更新
