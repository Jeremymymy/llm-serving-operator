# LLM Serving Operator 實驗室 — 00：概念

> 這份不用動手，先讀懂再做 Phase 1。
> 每個 Part 結尾的「**面試會怎麼問**」是自我檢查：講不出來就回頭再讀。

---

## Part 1：Operator 是什麼 —— 為什麼不是 Helm 就好

**Operator = CRD（自訂資源型別）+ controller（持續把現實拉向宣告的程式）。**

Helm 是「渲染模板 → apply 一次」。apply 完，Helm 就不管了：

- 有人 `kubectl scale deploy --replicas=5`，Helm 不知道
- Deployment 被誤刪，要等下次 `helm upgrade` 才回來
- 「這個服務現在好不好」沒有地方回答，要自己拼 `kubectl get` 的結果

Operator 是一直在跑的程式。它 watch 自己負責的資源，只要「宣告的」和「實際的」不一致就動手，
並把觀察結果寫回 CR 的 `status`。這是 Kubernetes 內建資源本來的運作方式 —— Deployment 本身就是由 deployment controller 管 ReplicaSet，
寫 operator 就是用同一套機制管理你自己定義的東西。

**什麼時候不需要 operator**：沒有 day-2 邏輯（升級順序、狀態判讀、故障修復）的東西，Helm chart 就夠了。
一個只是包 Deployment + Service 的 operator，價值主要在學習；真正的價值在把維運知識寫成程式碼
（例如「換模型要先確認新版 MAE 再切流量」）。

**面試會怎麼問**：什麼時候用 operator，什麼時候用 Helm？兩者可以並存嗎？（可以：operator 本身常用 Helm 安裝。）

---

## Part 2：Reconcile 迴圈 —— level-triggered，不是 edge-triggered

controller-runtime 給你的介面只有一個：

```go
Reconcile(ctx, req ctrl.Request) (ctrl.Result, error)   // req 只有 namespace/name
```

注意 `req` **只有名字，沒有「發生了什麼事件」**。這是刻意的設計：

| edge-triggered（事件驅動） | level-triggered（狀態驅動，k8s 的做法） |
|---|---|
| 「收到 replicas 從 1 改成 3 的事件 → 加兩個 pod」 | 「現在宣告 3 個、實際 1 個 → 補到 3 個」 |
| 漏掉一個事件就永遠錯 | 漏掉事件沒關係，下一次 reconcile 會看到差距 |
| 要處理事件順序、重複 | 同一個 req 跑十次結果都一樣（冪等） |

所以 Reconcile 的寫法固定是：**讀目前完整狀態 → 算出應該長什麼樣 → 讓它長那樣 → 寫 status**。
不要在 Reconcile 裡寫「如果是新建就…」這種依賴事件類型的邏輯。

**冪等（idempotent）是硬性要求**：同一個物件會因為各種原因被 reconcile 很多次（子資源狀態變、定期 resync、controller 重啟）。
每次都 Create 就會在第二次報 AlreadyExists；每次都無條件 Update 就會產生大量無效寫入（Phase 1 會親眼看到這個，Phase 2 修）。

**回傳值的意義**：

| 回傳 | 效果 |
|---|---|
| `ctrl.Result{}, nil` | 成功，等下一個事件 |
| `ctrl.Result{}, err` | 失敗，放回 workqueue，**指數退避**重試 |
| `ctrl.Result{RequeueAfter: 30*time.Second}, nil` | 成功，但 30 秒後再看一次（用在要輪詢外部狀態時） |

**面試會怎麼問**：為什麼 Reconcile 不告訴你發生了什麼事件？controller 重啟會不會漏事件？

---

## Part 3：Informer、cache、workqueue —— Reconcile 背後發生什麼

```
API server ──watch──► Informer ──► 本地 cache（client.Get 讀這裡）
                         │
                         └── event handler ──► workqueue（去重的 ns/name 佇列）──► Reconcile
```

三件會影響你寫程式的事：

1. **`r.Get` 讀的是 cache，不是 API server。** 剛 Create 完馬上 Get 可能還讀不到（cache 還沒同步）。
   所以 Reconcile 要能容忍「讀到舊資料」—— 寫錯了沒關係，下一輪會修正；寫入時樂觀鎖會擋下用舊版本覆蓋的情況（409 Conflict）。
2. **workqueue 會去重。** 同一個物件 1 秒內被改 10 次，Reconcile 可能只跑 1～2 次。這又是 level-triggered 的理由。
3. **同一個 ns/name 不會被兩個 worker 同時處理。** 所以 Reconcile 內不用為同一個物件上鎖。

**`For()` 與 `Owns()`**：

```go
ctrl.NewControllerManagedBy(mgr).
    For(&ModelDeployment{}).      // ModelDeployment 變了 → reconcile 它
    Owns(&appsv1.Deployment{}).   // 我建的 Deployment 變了 → 找它的 owner → reconcile owner
    Owns(&corev1.Service{})
```

`Owns()` 靠的是子資源上的 ownerReference（Part 4）。少了這行，有人刪掉 Deployment，controller 要等到下次定期 resync 才發現。

**面試會怎麼問**：Get 讀到舊資料會怎樣？兩個 controller replica 同時跑會不會打架？（不會，靠 leader election，只有一個在 reconcile。）

---

## Part 4：ownerReference 與 garbage collection

controller 建立子資源時，用 `controllerutil.SetControllerReference(owner, child, scheme)` 在子資源寫上：

```yaml
metadata:
  ownerReferences:
    - apiVersion: serving.llmlab.dev/v1alpha1
      kind: ModelDeployment
      name: smollm
      uid: 7f3c...
      controller: true            # 只能有一個 controller owner
      blockOwnerDeletion: true
```

它同時做兩件事：

1. **`Owns()` 的 watch 靠它找回 owner**（Part 3）
2. **刪除 owner 時，kube-controller-manager 的 garbage collector 會刪掉子資源** —— 你不用寫任何刪除邏輯

限制：**ownerReference 不能跨 namespace，也不能由 namespaced 資源擁有 cluster-scoped 資源。**
要清理叢集外的東西（雲端儲存、外部 DNS）或跨 namespace 的東西，才需要 finalizer（Part 6）。

**面試會怎麼問**：刪掉 CR，Deployment 是誰刪的？（是 GC，不是你的 controller。）`controller: true` 有什麼意義？

---

## Part 5：Spec、Status、Conditions、observedGeneration

**spec 是使用者的宣告，status 是 controller 的觀察。** 兩者用不同的 API 端點寫入（status subresource）：

- 使用者 `kubectl apply` 只能改 spec，改不到 status
- controller 用 `r.Status().Update()` 只改 status
- **只有 spec 變動會讓 `metadata.generation` +1**，status 變動不會

**observedGeneration** 回答「controller 看過最新的 spec 了嗎」：

```
generation: 5, status.observedGeneration: 4   → controller 還沒處理最新的修改，status 是舊的
generation: 5, status.observedGeneration: 5   → status 反映的是最新的 spec
```

沒有這個欄位，使用者改完 spec 馬上看到 `Available=True`，無法分辨那是新版還是舊版的狀態。

**Conditions** 是 k8s 的慣例（`metav1.Condition`）：

```yaml
conditions:
  - type: Available
    status: "False"
    reason: Progressing            # 機器可讀，CamelCase
    message: "0/1 replicas ready"  # 給人看
    observedGeneration: 5
    lastTransitionTime: ...        # 只有 status 真的改變時才更新
```

用 `meta.SetStatusCondition` 寫入，它會處理 `lastTransitionTime` 的規則。
**不要自己發明 `status.phase: "Running"` 這種單一字串**：一個物件可以同時「Available 但 Degraded」，單一 phase 表達不了。

**面試會怎麼問**：為什麼 status 要獨立成 subresource？observedGeneration 解決什麼問題？

---

## Part 6：Finalizer —— 什麼時候需要、什麼時候不要

finalizer 是 metadata 上的一個字串。只要還有 finalizer，`kubectl delete` 只會設 `deletionTimestamp`，物件不會真的消失，
直到 controller 做完清理、把 finalizer 拿掉。

**需要**：要清理 ownerReference 管不到的東西（叢集外資源、跨 namespace 資源、要先 drain 流量再刪）。
**不需要**：只有同 namespace 的子資源 —— GC 就夠了。加了沒用的 finalizer 反而有風險：
**controller 掛了或被卸載，所有 CR 都刪不掉**（namespace 也會卡在 Terminating）。

Phase 1 刻意不加 finalizer，用實驗證明 GC 已經處理子資源；Phase 2 再挑一個真的需要的情境來做。

**面試會怎麼問**：namespace 卡在 Terminating 怎麼查？（通常是某個 CR 的 finalizer 對應的 controller 不在了。）

---

## Part 7：RBAC —— controller 也是一個要被限制權限的使用者

operator 在叢集內以一個 ServiceAccount 執行。它能做什麼，完全由 RBAC 決定。
Kubebuilder 用 marker 宣告權限，`make manifests` 會把它們產生成 `config/rbac/role.yaml`（ClusterRole）：

```go
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
```

幾個要點：

- **`get;list;watch` 是 informer 必需的**，少了 list/watch，cache 起不來，controller 直接卡住
- `modeldeployments/status` 要另外給（status subresource 是獨立權限）
- RBAC 只能加不能減（沒有 deny 規則），所以**最小權限要靠一開始就只宣告需要的**
- Phase 1 的 `make run` 是用**你的** kubeconfig 跑（通常是 cluster-admin），RBAC 寫錯也不會發現；
  Phase 3 部署進叢集後，用 `kubectl auth can-i --as=system:serviceaccount:...` 驗證才算數

**面試會怎麼問**：Role 和 ClusterRole 差在哪？operator 要不要 cluster-wide 權限？（看它是否 watch 所有 namespace。）

---

## Part 8：跟 KServe 的關係 —— 這個 repo 是 InferenceService 的極簡版

| | 這個 repo 的 `ModelDeployment` | KServe `InferenceService` |
|---|---|---|
| 子資源 | Deployment + Service | Knative Service（serverless 模式）或 Deployment（raw 模式） |
| 模型下載 | initContainer 用 curl | storage-initializer（支援 s3、gs、pvc、hf…） |
| runtime | 寫死 llama.cpp | `ServingRuntime` CRD，可插拔 |
| 擴縮 | 固定 replicas（Phase 5 加 HPA） | Knative 依併發擴縮，可縮到零 |
| 流量 | 單一版本 | canary 分流、版本回滾 |
| LLM 專屬 | 無 | 搭配 Gateway API Inference Extension 做依 KV cache / 佇列的路由 |

做完這個 repo，你講得出 KServe 的每個欄位背後是哪一段 reconcile 邏輯，而不是只會寫 YAML。

**Gateway API Inference Extension**（概念，這個 repo 不實作）：一般 Service 是 round-robin，
但 LLM 請求長短差很多，round-robin 會把請求塞給 KV cache 已滿的 pod。Inference Extension 讓 gateway
依照每個 model server 回報的佇列長度、KV cache 使用率挑 endpoint。
（`llm-inference-k8s-lab` 依佇列長度做 HPA 擴容，是同一個訊號的另一種用法。）

---

## Part 9：測試 —— envtest 不是完整叢集

Kubebuilder 預設的測試用 **envtest**：在本機起一個真的 `kube-apiserver` + `etcd`，**但沒有 kube-controller-manager、沒有 scheduler、沒有 kubelet**。

| envtest 有 | envtest 沒有 |
|---|---|
| CRD schema 驗證、defaulting（會真的擋下錯的欄位） | deployment controller（Deployment 的 status 永遠不會更新） |
| ownerReference 寫入 | garbage collector（刪 owner，子資源不會消失） |
| optimistic locking、status subresource | pod 不會真的跑起來 |

所以測試分工：

- **envtest**：驗證「給這個 CR，controller 產生的子資源長什麼樣」「欄位驗證會擋什麼」
- **kind 上的實驗**：驗證 GC、drift 拉回、pod 真的 Ready、推論真的回應

把 envtest 測不到的東西寫成測試、然後納悶為什麼 Deployment 永遠沒 Ready，是新手最常卡的地方。
