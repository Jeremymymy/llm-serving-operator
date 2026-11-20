# RESULTS

> 只填實際跑過、看過的結果。預期與實際不同時，寫進該階段的「與預期不同」，不要改成預期的樣子。

## 環境

| 項目 | 版本 |
|---|---|
| go | go1.26.11 darwin/arm64 |
| kubebuilder | v4.16.0（Kubernetes 1.37.0，commit 4d01fcd，build 2026-09-10） |
| kind | v0.33.0（以 go1.27.0 建置） |
| kubectl（client） | v1.37.2（Kustomize v5.8.1，Homebrew `/opt/homebrew/bin/kubectl`） |
| kind node（`kubectl version` 的 Server Version） | v1.37.0（`kindest/node:v1.37.0`） |
| 主機 | MacBook Air，darwin/arm64 |

> 一開始用的 kubectl 是 OrbStack 內建的 v1.33.9（`/usr/local/bin/kubectl` → `/Applications/OrbStack.app/Contents/MacOS/xbin/kubectl`），跑 `kubectl version` 時出現警告：
> `WARNING: version difference between client (1.33) and server (1.37) exceeds the supported minor version skew of +/-1`
> 改用 `brew install kubectl` 裝的 v1.37.2 之後，警告就沒有了。`kubectl get nodes` 顯示 `operator-lab-control-plane` 為 Ready（v1.37.0）。

## Setup 基線（純 scaffold，還沒改任何程式碼）

| 項目 | 結果 |
|---|---|
| `make manifests generate` | 成功，沒有錯誤輸出 |
| envtest 執行檔 | setup-envtest v0.25.0，Kubernetes 1.37.0（`bin/k8s/1.37.0-darwin-arm64`） |
| `make test` | `ok  internal/controller  7.433s` |
| `internal/controller` 覆蓋率 | 66.7% |
| 其他套件 | `api/v1alpha1`、`cmd`、`test/utils` 都是 0.0%（沒有測試） |
| 覆蓋率細項（`go tool cover -func=cover.out`） | `Reconcile` 100%，`SetupWithManager` 0%。全部套件合計 1.0% |
| scaffold commit | `2f41755 chore: scaffold with kubebuilder`（`bin/`、`cover.out` 沒有被 commit 進去） |

## Phase 1：CRD 與 reconcile

### envtest

| 項目 | 結果 |
|---|---|
| 測試數量／通過數 | 9／9（`Ran 9 of 9 Specs`，`SUCCESS! -- 9 Passed \| 0 Failed \| 0 Pending \| 0 Skipped`，5.3 秒） |
| `internal/controller` 覆蓋率 | 81.2%（scaffold 基線是 66.7%） |
| 覆蓋率細項 | `labelsFor`、`mutateDeployment`、`httpProbe`、`mutateService` 100%；`Reconcile` 80%；`desiredReplicas`、`resourcesFor` 66.7%；`availableCondition` 60%；`SetupWithManager` 0% |
| CRD schema 檢查 | `pattern: ^[A-Za-z0-9._-]+\.gguf$`、`pattern: ^https://`、`default: 1`、`default: llamacpp`、`subresources: status: {}` 都有產生 |
| `config/rbac/role.yaml` | 5 條 rule：`services`、`deployments`、`modeldeployments`、`modeldeployments/finalizers`、`modeldeployments/status` |

### 實驗

| # | 實驗 | 指令重點 | 觀察到的結果 | 時間 |
|---|---|---|---|---|
| 1 | controller 停機時套用錯誤 CR | 停掉 `make run` 後，`kubectl apply` 一個 `url: http://...`、`file: ../x.gguf` 的 CR | apply 當下就被 API server 拒絕，兩個欄位的 pattern 錯誤一次列出（原文在下方）；`kubectl get md` 沒有 `bad`，只有 `smollm` | 立即 |
| 2 | 子資源與 ownerReference | `kubectl get deploy,svc -l ...`、看 Deployment 的 `ownerReferences`、`kubectl get md -o wide` | Deployment `smollm` 1/1、Service `smollm` ClusterIP 10.96.40.130:8080；ownerReference 指向 `ModelDeployment/smollm`（uid a62c6ef1…），`controller: true`、`blockOwnerDeletion: true`；`-o wide` 多出 `ENDPOINT` 欄 = `http://smollm.default.svc:8080`；`kubectl top` 無法用（kind 沒有 metrics-server），改用 `crictl stats` 看到 llama-server 閒置時 255.2MB、CPU 0% | — |
| 3 | 誤刪 Deployment | `kubectl get deploy -w &` 背景觀察，14:07:30 `kubectl delete deploy smollm` | 新的 `smollm` Deployment 在 watch 上是 AGE 0s 就出現（creationTimestamp 06:07:30Z，和刪除同一秒）；新 pod 重新下載模型（emptyDir，138.1M）；md 先變成 `Available=False`，READY 空白，之後回到 `1 / True`（`AllReplicasReady`，lastTransitionTime 06:08:31Z） | Deployment 重建：< 1 秒／Available 恢復：約 61 秒（14:07:30 → 14:08:31） |
| 4 | 手動 `kubectl scale` | `kubectl scale deploy smollm --replicas=3`，`kubectl get deploy -w` | replicas 被拉回 1：watch 上 UP-TO-DATE 1→3→1，READY 一直是 1/1。Events：ReplicaSet `Scaled up 1 to 3` 之後馬上 `Scaled down 3 to 1`，2 個多出來的 pod（`wz5jh`、`xxmwr`）在同一秒內被建立又被刪除。Deployment generation 2→3，observedGeneration 跟上到 3。controller log 在 14:11:57～58 出現 **9 輪** `reconciled children`，每輪都是 `deployment: updated`，其中 1 輪 `update status` 409 Conflict。md 的 Available 在同一秒內 True→False→True（lastTransitionTime 06:11:57Z） | 拉回：< 1 秒 |
| 5 | 改 CR replicas | `kubectl patch md smollm --type=merge -p '{"spec":{"replicas":2}}'`，`kubectl get md -w` | 14:27:23 patch 後，ReplicaSet `Scaled up 1 to 2`，新 pod `55nk2` 同一秒建立；md 先變成 `1 / False`（watch 沒有顯示 reason，依程式邏輯應該是 `Progressing`），之後 `2 / True`，condition 變成 `AllReplicasReady`、`2/2 replicas ready`（lastTransitionTime 06:28:24Z）。generation／observedGeneration：改前 1（推定：建立後沒有改過 spec，改前沒有實際量）→ 改後 `2 2`，condition 的 observedGeneration 也是 2。改回 1 之後是 `3 3`，`1 / True`；這次縮容（2→1）log 在 14:32:30～31 也有 **9 輪** reconcile，全部是 `deployment: updated`，其中 1 輪 `update status` 409 Conflict，和實驗 4 的擴容同樣規模 | Ready 1→2：約 61 秒（主要是新 pod 下載模型） |
| 6 | 刪 CR，GC 清子資源 | `kubectl delete md smollm`，緊接著 `kubectl get deploy,svc,pods -l ...` | delete 一回傳，馬上查就已經看不到 Deployment 和 Service，只剩 pod `frwbj` 是 `Terminating`；再查一次就是 `No resources found`。controller log 在刪除後**沒有任何新的一行**：最後一行仍然是刪除前 14:32:31 的 `reconciled children`。沒有任何刪除動作，也沒有錯誤 | 子資源消失：查第一次時 Deployment／Service 已經不見，pod 在第二次查詢前消失 |
| 7 | 不改 spec 的 reconcile 是否寫入 | 服務穩定（`1 / True`）後，`kubectl annotate md smollm lab/poke=...`，前後各讀一次 Deployment 的 resourceVersion | 觸發了 **1 輪** reconcile（14:42:25），**`deployment: updated`**、`service: unchanged`；md status 沒有變，所以沒有寫 status，也沒有連鎖觸發下一輪。log：`updated`／resourceVersion：`48595 -> 48595`（**沒有變**）。和預期相同：controller 送出了 Update，但 API server 補回預設值後發現物件沒有變，就沒有實際寫入 | — |
| 8 | controller 停機期間的變更 | 停掉 controller → `kubectl patch md ... replicas:2` → 讀 Deployment `.spec.replicas` → `make run` | 停機期間 Deployment 的 replicas 仍然是 **1**（CR 已經改成 2，但沒有人去同步）。14:49:07 controller 啟動，初次 list 就發現差距，同一秒 ReplicaSet `Scaled up 1 to 2`，新 pod `kcxv6` 被建立；md `2 2`，`AllReplicasReady`、`2/2 replicas ready`（06:50:08Z）。啟動那一秒的 log 共 **7 輪** reconcile，**4 輪失敗**：1 次 `update status` 409、3 次 `reconcile deployment` 409，重試後都成功；14:50:08 pod Ready 時又跑了 2 輪。每一輪成功的都是 `deployment: updated` | controller 啟動後到 Deployment 改成 2：< 1 秒／Ready 2/2：約 61 秒（14:49:07 → 14:50:08） |

**實驗 1 錯誤訊息原文**：

```
The ModelDeployment "bad" is invalid: 
* spec.model.url: Invalid value: "http://example.com/x.gguf": spec.model.url in body should match '^https://'
* spec.model.file: Invalid value: "../x.gguf": spec.model.file in body should match '^[A-Za-z0-9._-]+\.gguf$'
```

**實驗 7：Deployment 裡由 API server 補上、但 mutate 每次都會清掉的欄位**（`kubectl get deploy smollm -o json`）：

```
pod:            restartPolicy=Always, dnsPolicy=ClusterFirst, schedulerName=default-scheduler,
                terminationGracePeriodSeconds=30, securityContext={}
container:      imagePullPolicy=IfNotPresent, terminationMessagePath=/dev/termination-log,
                terminationMessagePolicy=File
readinessProbe: successThreshold=1, timeoutSeconds=1, httpGet.scheme=HTTP
```

mutate 產生的 PodSpec 沒有這些欄位，CreateOrUpdate 和 cache 裡的物件比較時一定不相等，所以每一輪都回報 `updated` 並送出 Update。

**推論請求回應**（`/v1/completions`）：

請求：`{"prompt":"Kubernetes is","max_tokens":16}`，經由 `kubectl port-forward svc/smollm 8080:8080` 送出（2026-11-19）

```
"text": " a container orchestration and management system for containerization and container services. It provides"
"finish_reason": "length"
"model": "/models/SmolLM2-135M-Instruct-Q8_0.gguf"
"system_fingerprint": "b12464-d3c3e242c"
"usage": prompt_tokens 4, completion_tokens 16, total_tokens 20
"timings": prompt 133.16 ms（30.0 tok/s），生成 16 個 token 共 1217.07 ms（12.3 tok/s，每個 token 81.1 ms）
```

> `finish_reason: length` 表示停下來是因為達到 `max_tokens: 16` 的上限，不是模型自己結束。速度是單一 pod、limit 1Gi 記憶體、request 250m CPU、只跑在 CPU 上的數字。

### 與預期不同

- **第一次部署要等很久才 Ready，時間花在拉 image，不是 controller 的問題**（2026-11-19）：`curlimages/curl:8.11.1` 拉了 11.2 秒；init container 下載模型 138.1M 花了 56 秒（約 2.5 MB/s）；之後拉 `ghcr.io/ggml-org/llama.cpp:server`（292 MB）花了 **7 分 57 秒**。這段期間 pod 是 `PodInitializing`，md 是 `Available=False / Progressing / 0/1 replicas ready`。image 拉完後，llama-server 只花 0.29 秒就載入模型並開始 listen，13:47:29 controller 跑了 2 輪 reconcile，md 在 AGE 9m16s 時變成 `READY 1 / Available True`。
- **`make run` 剛啟動就連續 reconcile，還出現一次 409 Conflict**：第一輪 `deployment: created`，接下來同一秒內又跑了 4 輪，每輪都是 `deployment: updated`、`service: unchanged`，其中一輪失敗：
  `reconcile deployment: Operation cannot be fulfilled on deployments.apps "smollm": the object has been modified; please apply your changes to the latest version and try again`
  下一輪重試就成功了。推測原因：mutate 整個覆寫 PodSpec，所以每輪都判定為 `updated` 並送出 Update；同時 deployment controller 也在更新同一個 Deployment，controller 手上 cache 裡的版本就舊了。這和實驗 7 是同一個問題，Phase 2 要修。
- **實驗 4 預期只會有「一次 `deployment: updated`」，實際是 1 秒內 9 輪 reconcile**（2026-11-19 14:11:57）：每一輪都是 `updated`，其中一輪失敗，錯誤是 `update status: Operation cannot be fulfilled on modeldeployments.serving.llmlab.dev "smollm": the object has been modified`，這次衝突的是 **md 的 status**，不是 Deployment。推測的連鎖反應：scale 改了 Deployment → reconcile 把 replicas 改回 1；接著 deployment controller 每更新一次 Deployment status（observedGeneration、replicas、updatedReplicas…），就觸發一輪 reconcile；同時 Available 在 Progressing 和 AllReplicasReady 之間切換，每切換一次就寫一次 md status，又觸發一輪。cache 還沒收到上一輪寫入的結果，下一輪就開始了，所以拿到舊版本，造成 409。

## Phase 2：讓 controller 值得信任

### Phase 1 的 managedFields（改程式前，對照實驗 1 用）

`kubectl get deploy smollm --show-managed-fields -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\t"}{.operation}{"\t"}{.subresource}{"\n"}{end}'`（2026-11-19）

```
main                     Update
kube-controller-manager  Update  status
```

> `main` 是 Phase 1 controller 的 field manager：`make run` 跑的是 `go run ./cmd/main.go`，controller-runtime 預設用執行檔名稱當 manager。spec 與 metadata 都是用 `Update` 寫入的，不是 `Apply`。

### envtest

| 項目 | 結果 |
|---|---|
| 測試數量／通過數 | 13／13（`Ran 13 of 13 Specs`，`SUCCESS! -- 13 Passed \| 0 Failed \| 0 Pending \| 0 Skipped`，7.4 秒）。Phase 1 是 9／9 |
| `internal/controller` 覆蓋率 | 80.9%（Phase 1 是 81.2%） |
| 覆蓋率細項 | `labelsFor`、`pvcName`、`ownerRef`、`desiredPVC`、`desiredDeployment`、`httpProbe`、`resourcesFor`、`desiredService` 100%；`degradedCondition` 92.3%；`emitTransitionEvents` 88.9%；`Reconcile` 81.0%；`deploymentFromApply`、`availableCondition` 80%；`desiredReplicas` 66.7%；`fail`、`podToModelDeployment`、`SetupWithManager` 0% |
| `make lint` | `0 issues.`（golangci-lint v2.13.1） |
| 插曲：回歸測試在舊程式上 | **14 個跑 13 個過**，失敗在 `keeps Available when only the Deployment's annotations change` 的 `IsStatusConditionTrue`（test.go:261，`Expected <bool>: false to be true`）→ 重現了實驗 2 的 Available 閃爍。同一個測試裡「Deployment 加 annotation 後 generation +1」的斷言**通過** |
| 插曲：修正 `availableCondition` 之後 | 14／14（`Ran 14 of 14 Specs`，7.5 秒）；覆蓋率 **81.2%**（修正前 80.9%）；`make lint` `0 issues.`（2026-11-20） |
| `config/rbac/role.yaml` | 7 條 rule（Phase 1 是 5 條）：core group 的 rule 多了 `persistentvolumeclaims`；新增 `pods`（get/list/watch）、`events.k8s.io` `events`（create/patch） |

> 覆蓋率比 Phase 1 少了 0.3 個百分點，但測試從 9 個變成 13 個。原因是新增的程式碼裡有三個函式 envtest 沒有跑到：`fail`（apply 失敗的路徑）、`podToModelDeployment`（只有 manager 啟動、真的在 watch pod 時才會被呼叫）、`SetupWithManager`（測試直接呼叫 `Reconcile`，不經過 manager）。後兩個要到 kind 上才會被用到。

### 啟動（Part 5）

先刪掉 Phase 1 建的 `smollm`，再用新的 controller 重建（2026-11-20）。

| 項目 | 結果 | Phase 1 對照 |
|---|---|---|
| 從 apply 到 Available=True | **約 71 秒**（14:09:59 建立 → 14:11:10 `AllReplicasReady`；`kubectl get md -w` 在 AGE 71s 變成 `1 / True`）。主要是 init container 下載模型；image 已經在節點上 | 9 分 16 秒（其中 7 分 57 秒在拉 `llama.cpp:server` image），兩者不能直接比 |
| PVC | `smollm-models` `Bound`，1Gi、RWO、StorageClass `standard` | （emptyDir） |
| `Applied children` 輪數 | **17 輪**：14:10:00 四輪、14:10:03 三輪、14:10:04 四輪（PVC 綁定、pod 建立）；14:11:08～10 六輪（pod 變 Ready） | 第一秒 5 輪（1 輪 `created`、4 輪 `updated`） |
| 409 | 貼出的 log 中**沒有** `the object has been modified` | 1 次（`reconcile deployment`） |
| `serviceRV` | 17 輪都是 `90063`，從頭到尾沒有變 | — |
| `deploymentRV` | `90060 → 90071 → 90080`（14:10:00 內），之後一直是 `90080`，到 14:11:10 pod Ready 時變成 `90220` | — |
| `pvcRV` | `90059 → 90069 → 90081 → 90102 → 90103`，14:10:03 之後一直是 `90103` | — |
| Events | 只有一筆：`Normal  Available  modeldeployment-controller  1/1 replicas ready`（14:11:10） | （沒有 Events） |
| conditions | `Available True AllReplicasReady`；`Degraded False NoFailure "no failure detected"` | 只有 Available |

> **RV 變化不能直接當成「我們寫了」**：`deploymentRV` 變成 `90220` 的時間點正好是 pod Ready，比較可能是 deployment controller 寫 status；`pvcRV` 的四次變化發生在 PVC 從 Pending 到 Bound 期間（scheduler 選節點、PV controller 綁定）。光看 log 分不出哪幾次是我們的 apply，要看 managedFields 裡 `llm-serving-operator` 那筆的 `time`。

**managedFields 的時間**（啟動完成後量，2026-11-20）：

```
llm-serving-operator     Apply   2026-11-20T06:10:00Z
kube-controller-manager  Update  2026-11-20T06:11:09Z
```

- 我們的 apply 最後一次真的改到 Deployment 是 **06:10:00Z（14:10:00）**，也就是建立那一秒。之後 log 又跑了十幾輪，這個時間都沒有更新 → 那些輪次的 apply 都沒有寫入
- `deploymentRV` 在 14:11:10 變成 `90220`，對應的是 kube-controller-manager 在 **06:11:09Z** 的 Update（deployment controller 寫 status），不是我們
- 限制：managedFields 的時間只到秒。14:10:00 那一秒內的三次 RV 變化（90060 → 90071 → 90080），分不出哪幾次是我們建立／apply、哪幾次是 deployment controller 寫 status
- Phase 1 對照（Part 3.1）：當時是 `main  Update`，現在換成 `llm-serving-operator  Apply`，`main` 這個 manager 已經不存在
> 輪數從 5 輪變 17 輪，是因為多 watch 了 pod 與 PVC，而且 Phase 1 只數了第一秒。17 輪裡至少 `serviceRV` 完全沒變，符合 SSA 在沒有變更時不寫入的預期。

**推論請求**（Part 5 收尾，經由 `kubectl port-forward svc/smollm 8080:8080`，2026-11-20）：

```
"text": " a great choice if you're interested in managing a cluster of containers for a specific"
"finish_reason": "length"
"model": "/models/SmolLM2-135M-Instruct-Q8_0.gguf"
"system_fingerprint": "b12464-d3c3e242c"
"usage": prompt_tokens 4, completion_tokens 16, total_tokens 20
"timings": prompt 133.92 ms（29.9 tok/s），生成 16 個 token 共 974.12 ms（15.4 tok/s，每個 token 64.9 ms）
```

> 模型從 PVC 載入，服務正常。生成速度比 Phase 1 那次（12.3 tok/s）快，但兩邊各只有一次請求，這個差距不代表什麼。Phase 2 沒有改 llama-server 的參數和資源設定。

### 實驗

| # | 實驗 | 指令重點 | 觀察到的結果 | Phase 1 對照 |
|---|---|---|---|---|
| 1 | managedFields 換人了 | `kubectl get deploy smollm --show-managed-fields -o yaml` | 只有兩個 manager：`llm-serving-operator  Apply`（06:10:00Z，負責 metadata.labels、ownerReferences、spec）、`kube-controller-manager  Update  subresource: status`（06:11:09Z，負責 status 與 `deployment.kubernetes.io/revision` annotation）。我們那一筆的 `fieldsV1` 裡**沒有** `imagePullPolicy`、`terminationMessagePath`、`terminationMessagePolicy`、probe 的 `scheme`／`timeoutSeconds`／`successThreshold`、`dnsPolicy`、`restartPolicy`、`schedulerName`、`securityContext`、`terminationGracePeriodSeconds`，也沒有 `progressDeadlineSeconds`、`revisionHistoryLimit`、`strategy`。這些值都在 spec 裡（API server 補的預設值），但**不屬於任何 manager** | `main  Update`：Phase 1 用 Update 送出整個物件，跟 cache 裡含預設值的版本比較，永遠判定不一樣 |
| 2 | 沒有變更的 reconcile 會不會寫入 | (a) `kubectl annotate md smollm lab/poke=...`；(b) `kubectl annotate deploy smollm lab/poke=...`，前後讀 Deployment RV | (a) 終端機 A **沒有新的 log**，最後一行仍然是 14:11:10 那輪 → predicate 擋掉了 annotation 事件。(b) RV `90220 -> 92630`；14:39:52 跑了 **2 輪**：第 1 輪 `deploymentRV: 92628`，接著發出 `Warning Unavailable "Progressing: waiting for deployment controller to observe the latest spec"`；第 2 輪 `deploymentRV: 92630`，接著發出 `Normal Available "1/1 replicas ready"`。annotation `lab/poke=1795156792` 還在。**Available 在同一秒內 True→False→True，多了兩筆 Event**（見「與預期不同」） | Phase 1：poke md 觸發 1 輪，`deployment: updated`，RV `48595 -> 48595` |
| 3 | 手動 `kubectl scale` | 15:39:38 `kubectl scale deploy smollm --replicas=3`，`kubectl get deploy -w` | replicas 被拉回 1：watch 上 UP-TO-DATE 1→3→1，READY 一直是 1/1，拉回 < 1 秒。log：15:39:38 **6 輪**、15:39:39 2 輪、15:39:40 3 輪，共 **11 輪**；**409：0 次**（`grep -c "the object has been modified"` = 0）；log 裡**沒有 `Event occurred`**，md 的 Available 一直是 True。`deploymentRV` `96987 → 96992 → 96995 → 97011 → 97020 → 97025`，`serviceRV`／`pvcRV` 不變 | 14:11:57～58 共 9 輪、**1 次 409**（status），Available True→False→True |
| 4 | 誤刪 Deployment（插曲 2 修正後重做） | 重啟 controller，16:11:09 `kubectl delete deploy smollm`，`kubectl get md -w`，`grep "Event occurred"` | Deployment 立刻重建（16:11:09 那一輪 `deploymentRV: 99191`）。init container 印 `model already present`，**沒有重新下載**。`kubectl get md -w` 刪除後第一行就是 `False`（插曲 2 修正有效，不再出現「READY 空白 / True」）。Events：16:11:09 `Warning Unavailable "Progressing: waiting for deployment controller to observe the latest spec"` → 16:11:15 `Normal Available "1/1 replicas ready"`，**後者發了兩次**（describe 顯示 `(x2 over 51s)`）。log 16:11:09～15 共 15 輪，409：0 | 刪除到恢復 **約 61 秒**（新 pod 重新下載 138 MB 模型） |
| 5 | 兩個 replica 共用快取 | 16:18:18 `kubectl patch md smollm ... replicas:2`，`kubectl get md -w`，`kubectl logs ... -c fetch-model --prefix` | 兩個 pod（`2ccmm`、`cvvx9`）的 init container 都印 `model already present`。新 pod 共用同一個 PVC，沒有下載。md patch 後**立刻**變成 `1 / False`（符合插曲 2 的表：replicas 1→2 時 ready 1 < 2，不保留舊的 True），之後 `2 / True`。Ready 1→2：**約 6 秒**（16:18:18 `Warning Unavailable "Progressing: 1/2 replicas ready"` → 16:18:24 `Normal Available "2/2 replicas ready"`）。`kubectl get pods -o wide`：兩個 pod 都在 `operator-lab-control-plane`（RWO 在單節點上可以被兩個 pod 同時掛載）。做完已改回 1 | Ready 1→2 約 61 秒（新 pod 下載模型） |
| 6 | controller 停機期間的變更 | 停 controller → `kubectl patch md ... replicas:2` → 讀 Deployment `.spec.replicas` → `make run` | 停機期間 Deployment 仍是 **1**。17:06:44 controller 啟動，**第一輪**就把 Deployment 改成 2（`kubectl get deploy -w`：1/1 → 1/2 → 2/2），接著發 `Warning Unavailable "Progressing: waiting for deployment controller to observe the latest spec"`；17:06:50 `Normal Available "2/2 replicas ready"`（Ready 2/2 約 6 秒，新 pod 不用下載模型）。輪數：17:06:44 **4 輪**、:45 4 輪、:49 1 輪、:50 3 輪，共 12 輪。**409：1 次，是 `patch status`**（插曲 3 的樂觀鎖擋下讀到舊 md 的那一輪）；**子資源 apply 的 409：0 次**。每個轉變只有一筆 Event | 啟動那一秒 7 輪、**4 次 409**（1 次 status、**3 次 deployment**）；Ready 2/2 約 61 秒 |
| 7 | 模型下載失敗 | `kubectl apply` 一個 URL 不存在的 md `broken`，`kubectl get pods -w`，讀 conditions 與 Events | pod 在 AGE 5s 進入 `Init:Error`，之後 `Init:CrashLoopBackOff`，21 秒內重啟 2 次。conditions：`Available False ModelDownloadFailed` 與 `Degraded True ModelDownloadFailed`，message 都是 `pod broken-85fd9bf6d7-vd2jl: fetch-model exited with code 22 (restarts: 2)`。Events **只有一筆** `Warning ModelDownloadFailed "... exited with code 22 (restarts: 0)"`：之後 restarts 從 0 增加到 2，message 跟著更新，但 Degraded 一直是 True、沒有轉變，所以沒有再發 Event。也沒有 `Unavailable` Event（Available 從來沒有 True 過）。condition 的 restarts 會自己跟著更新，代表 **pod watch（`podToModelDeployment`）在真的叢集上有作用**（envtest 覆蓋率 0% 的那條路）。`kubectl logs ... -c fetch-model --previous` 失敗（`unable to retrieve container logs for containerd://a4e8...`）；**不加 `--previous`** 就拿得到：`curl: (22) The requested URL returned error: 404` —— Hugging Face 對不存在的檔案回 404，`curl -f` 以 exit code 22 結束 | Phase 1 遇到同樣情況只會回報 `Progressing` |
| 8a | Kubernetes 自己的 finalizer（`pvc-protection`） | 看 PVC finalizers → 17:27:29 `kubectl delete pvc smollm-models --wait=false` → 打推論 → 17:29:43 `kubectl delete deploy smollm` | PVC 的 finalizers 是 `["kubernetes.io/pvc-protection"]`。delete 之後 PVC 停在 **`Terminating`**（2 分多鐘都沒消失），這段期間推論請求**照常回應**（`" a container orchestration system that allows you to create, manage, and scale your"`，13.7 tok/s）。刪掉 Deployment（pod 終止，沒人在用 PVC）之後，舊 PVC 消失，controller **重建了一個新的 PVC**：新的 volume `pvc-49248fa0…`（舊的是 `pvc-78dd2a6e…`），`Pending` → 3 秒後 `Bound`。新 pod `6z7k4` 的 init container 跑了約 **52 秒**（Init:0/1 從 AGE 13s 看到 65s，快取是空的，重新下載），AGE 70s 時 `1/1 Running`。md：17:29:43 `Warning Unavailable` → 17:30:53 `Normal Available`，**恢復約 70 秒**（快取沒了，跟 Phase 1 沒有快取時的 61 秒同一個量級） | （Phase 1 沒有 PVC） |
| 8b | 亂加 finalizer 的後果 | `kubectl patch md smollm` 加上 `llmlab.dev/demo` → 17:35:10 `kubectl delete md smollm --wait=false` → 查狀態 → 手動移除 finalizer | md 有了 `deletionTimestamp: 2026-11-20T09:35:10Z`，finalizers 仍是 `["llmlab.dev/demo"]`，**一直沒有消失**；子資源**全部還在**（Deployment 1/1、Service、PVC Bound）—— owner 還沒真的被刪，GC 不動手。約 2 分鐘後（PVC AGE 5m59s → 7m53s）用 JSON patch 移除 finalizer：md 立刻被刪，Deployment、Service 馬上消失，PVC 先 `Terminating`（`pvc-protection` 等 pod 終止），再查一次就 `No resources found`。重新 apply 範例 CR，**69 秒**回到 `1 / True`（PVC 被 GC 刪掉，重新下載模型） | （Phase 1 沒做） |

### 實驗 8 的結論：這個 operator 為什麼不需要 finalizer

- **它管的東西 GC 都處理得了**：Deployment、Service、PVC 都在 md 的同一個 namespace，都有 `controller: true` 的 ownerReference。刪 md 時 kube-controller-manager 的 garbage collector 會依 ownerReference 把它們清掉（Phase 1 實驗 6、Phase 2 實驗 8b 都看到了），controller 不用寫任何刪除邏輯
- **它沒有 GC 管不到的東西**：沒有叢集外的資源（雲端 bucket、DNS 紀錄、外部 gateway 的路由），也沒有跨 namespace 或 cluster-scoped 的資源
- **加了反而有風險**：8b 證明，finalizer 沒有人處理時，md 和它所有子資源都會卡住，只能手動移除；如果那是真的有清理工作的 finalizer，硬拔等於跳過清理
- **什麼時候會需要**：要清理 ownerReference 管不到的東西時。例如之後把 endpoint 註冊到外部的 gateway 或 DNS —— 刪 md 時要先把註冊拿掉，才能讓 md 消失
- **8a 是正確用法的示範**：`kubernetes.io/pvc-protection` 擋的是 GC 判斷不了的事（volume 還有 pod 在用），而且有 PV controller 負責在條件滿足時拿掉它

### Phase 2 總結（與 Phase 1 對照，皆為實測）

| 指標 | Phase 1 | Phase 2 | 來源 |
|---|---|---|---|
| 沒有變更時，controller 對 Deployment 的寫入 | 每輪送一次 Update（`deployment: updated`） | 不寫入（managedFields 裡 `llm-serving-operator` 的時間只在真的有變更時更新） | Part 5、實驗 2、3 |
| `kubectl scale` 時的 409 | 1 次（status） | 0 次 | 實驗 3 |
| controller 停機重啟時的 409 | 4 次（3 次 deployment、1 次 status） | 1 次（status，樂觀鎖擋下讀到舊資料的那一輪）；子資源 0 次 | 實驗 6 |
| 刪 Deployment 後恢復 | 約 61 秒 | 約 6 秒 | 實驗 4 |
| 擴容 1→2 的 Ready 時間 | 約 61 秒 | 約 6 秒 | 實驗 5 |
| 模型下載失敗時的回報 | `Progressing` | `Degraded / ModelDownloadFailed` + 一筆 Warning Event | 實驗 7 |
| envtest | 9 個，覆蓋率 81.2% | 16 個，覆蓋率 83.0% | Part 4、插曲 1～3 |

過程中在自己的 controller 裡發現並修正了三個缺陷（都先寫回歸測試、確認失敗再修）：插曲 1（改 Deployment annotation 讓 Available 閃一下）、插曲 2（插曲 1 的修正太寬，Deployment 被刪時仍回報 True）、插曲 3（cache 落後造成重複的 Event）。


### 與預期不同

- **文件裡的 `kubectl get md,pvc -w` 不能用**：`error: you may only specify a single resource type`。`kubectl get -w` 一次只能 watch 一種資源，改成分兩個視窗跑 `kubectl get md -w` 和 `kubectl get pvc -w`。（文件已修正，實驗 8a 也有同樣的寫法，一併改了）
- **文件原本預期「穩定之後三個 RV 都不再變」，這個說法不夠精確**：`deploymentRV` 在 pod Ready 時又變了一次（90080 → 90220），那是 Deployment status 的變化，不是 spec。log 裡的 RV 是物件目前的版本，任何寫入者都會讓它變。
- **實驗 2(b) 預期 RV 只變一次、不會有 Event；實際 RV 變了兩次，Available 還閃了一下**（2026-11-20 14:39:52）：只是替 Deployment 加一個 annotation，md 就發出 `Warning Unavailable`，同一秒內又發出 `Normal Available`。服務本身沒有中斷（pod 沒動）。
  `Unavailable` 的 note 是 `waiting for deployment controller to observe the latest spec`，表示當下 Deployment 的 `metadata.generation` 大於 `status.observedGeneration`。推測原因：**Deployment 的 annotation 有變動時，API server 也會讓 generation +1**（annotation 會被複製到 ReplicaSet，所以 deployment controller 要重新處理）。這跟 00-concepts Part 5「只有 spec 變動才會讓 generation +1」的說法不同，那是 CR 的行為，Deployment 是例外。
  兩次 RV 變化推測是：`92628` = `kubectl annotate` 的寫入（generation 1→2），`92630` = deployment controller 把 `status.observedGeneration` 更新成 2。我們的 `availableCondition` 看到 generation 落後就回報 `Progressing`，所以 Available 閃了一下。
  **已確認**（同日）：Deployment 的 generation／observedGeneration 是 `2 2`（建立時是 `1 1`）；managedFields 是 `llm-serving-operator  Apply  06:10:00Z`（**沒有變** → 我們這次也沒有寫入）、`kube-controller-manager  Update  06:39:52Z`、新出現的 `kubectl-annotate  Update  06:39:52Z`。envtest 也證實了：測試裡對 Deployment 加 annotation，generation 會 +1。
  **根本原因**：`availableCondition` 在 Deployment generation 落後時一律回報 False，分不出「我們改了 spec」和「別人碰了 Deployment」。Phase 1 實驗 4 的 True→False→True 很可能也是這個原因（`kubectl scale` 一樣會讓 generation +1）。
  **修正**：generation 落後時保留上一次的 Available condition，只有第一次建立時才回報 Progressing。先加回歸測試 `keeps Available when only the Deployment's annotations change`，確認在舊程式上失敗，再修。修正步驟寫在 `docs/03` Part 6 的「插曲」。
  **kind 上驗證修正**（2026-11-20 15:33～15:35）：重啟 controller 後，啟動時跑了 **1 輪**（15:33:14），三個 RV 跟重啟前完全相同（`92630`／`90063`／`90103`），沒有寫入、沒有 409。15:35:43 再 annotate Deployment：RV `92630 -> 96655`，log 一樣 **2 輪**（`deploymentRV` `96653` → `96655`，分別對應 annotate 的寫入和 deployment controller 更新 observedGeneration），但**沒有任何 `Event occurred`**；md 一直是 `1 / True`。修正有效。
  **annotation 被複製到 ReplicaSet**：`kubectl get rs -l app.kubernetes.io/instance=smollm` → `smollm-db99bc8c7  lab/poke=1795160143`，跟 Deployment 上的值相同，證實了 generation +1 的理由（deployment controller 要把 annotation 同步到 ReplicaSet）。
- **實驗 3 文件裡數輪數的指令不能直接用**：`grep "<scale 那一秒，例如 15:20:31>"` 是佔位字串，照貼會得到 0。改成 `grep "Applied children" ... | cut -c12-19 | uniq -c`，按秒列出輪數（文件已修正）。本次輪數是從貼出的 log 手動數的
- **實驗 3 的輪數沒有變少（9 → 11），但 409 從 1 次變成 0、Available 不再閃**：跟文件預期一致。多出來的輪數來自 pod watch，被 scale 出來又立刻刪掉的 2 個 pod，每次狀態變化都會觸發一輪
- **實驗 3 的 managedFields**（scale 後量）：`llm-serving-operator  Apply  07:39:38Z`（從 06:10:00Z **更新了** → 這次是我們真的寫入：用 `ForceOwnership` 把 replicas 改回 1）；`kubectl-annotate  Update  07:35:43Z`；`kube-controller-manager  Update  status  07:39:40Z`。**`kubectl scale` 的 manager 整筆消失**：它只擁有 `spec.replicas`，被我們搶回來之後就不擁有任何欄位，API server 把空的那筆拿掉了
- **實驗 4 刪除 Deployment 後，md 先顯示 `READY 空白 / AVAILABLE True`**（2026-11-20 15:45:52）：一個 pod 都沒有卻回報 Available，之後才 `False`，再回到 `1 / True`。init container 印 `model already present`（PVC 快取有效）。
  **原因**：插曲 1 的修正太寬。重建的 Deployment generation=1、observedGeneration=0，落進「generation 落後 → 保留上一次的 condition」分支，沿用了舊的 True；而且 md 的 spec 沒變，condition 的 observedGeneration 跟 md 一樣，讀的人看不出它是舊的。
  **修正（插曲 2）**：generation 落後時，只有 `ReadyReplicas >= desired` 才保留上一次的 condition，否則回報 Progressing。先加回歸測試 `reports Progressing when the Deployment is recreated`，在插曲 1 的程式上確認失敗（`Expected True to equal False`），修正後 15／15、覆蓋率 82.1%、lint 0 issues（寫文件時在 scratch 副本實測）。恢復時間待修正後重做實驗 4 再量
- **實驗 4 的恢復時間：約 6 秒**（16:11:09 `Unavailable` → 16:11:15 `Available`），Phase 1 是約 61 秒。省下的就是模型下載時間，剩下的是排程、llama-server 載入、等 probe（週期 5 秒）
- **插曲 2 在 kind 上驗證有效**：插曲 1 的程式在 15:45:52 刪除時先顯示 `True`；插曲 2 修正後在 16:11:09 刪除，第一行就是 `False`
- **`Normal Available` 在 16:11:15 發了兩次**，文件沒有預期到。log 裡兩次 `Event occurred` 之間只隔了一輪 `Applied children`。推測原因：status 改用不帶 resourceVersion 的 merge patch（Part 1.2），第一輪寫完 status 後，informer cache 還沒收到新版本，下一輪（被 pod／Deployment 事件觸發）從 cache 讀到的仍是舊的 `False`，又算了一次「False → True」，再 patch、再發 Event。用 `Update` 的話這一輪會 409 重試，不會重複。結果是重複但無害的寫入（內容相同），Event 被 API 聚合成 `x2`。這是拿掉樂觀鎖的代價，文件 Part 1.2 已補上說明；程式維持不變。envtest 抓不到這個現象，因為測試用的 client 沒有 cache
- **實驗 5 也出現同樣的重複 Event，而且證實了上面的推測**：16:18:18 有**兩筆** `Warning Unavailable`，note 不同 —— 第一筆是 `Progressing: waiting for deployment controller to observe the latest spec`（第一輪：Deployment generation 落後、ready 1 < 2），第二筆是 `Progressing: 1/2 replicas ready`（下一輪：deployment controller 已經處理完，但這一輪從 cache 讀到的 md status 仍是舊的 `True`，所以又判定了一次 True→False）。`kubectl get md -w` 連續兩行 `1 False` 也對應這兩次 status 寫入。兩筆 note 不同，證明是兩輪各自從「舊的 True」出發算出來的，而不是同一輪發了兩次。到目前為止，**每次狀態轉變後若緊接著有 pod／Deployment 事件，就會重複一次**（實驗 4、5 都是）
  **決定改回有樂觀鎖的 status patch（插曲 3）**：`client.MergeFromWithOptions(..., client.MergeFromWithOptimisticLock{})`。新增回歸測試 `does not repeat a transition when it reads a stale ModelDeployment`，用包一層 `staleReader`（Get md 時回傳舊快照）模擬 cache 落後。寫文件時在 scratch 副本實測：沒有鎖時新測試失敗（`IsConflict` 那行 `got: <nil>`），加上鎖後 16／16、覆蓋率 83.0%、lint 0 issues。
  **本機照做的結果**（2026-11-20）：新測試在舊程式上失敗於 test.go:319（`got: <nil>`，15／16）；改好後 16／16、覆蓋率 83.0%、`make lint` 0 issues，跟 scratch 實測一致。
  **kind 上驗證**（16:54:22 重啟 controller，16:55:26 replicas 改成 2）：每個轉變**只有一筆** Event —— 16:55:26 `Warning Unavailable "Progressing: waiting for deployment controller to observe the latest spec"`、16:55:32 `Normal Available "2/2 replicas ready"`。log 裡 **1 次** `patch status` 409（`ERROR Reconciler error`，16:55:26），也就是被擋下的那一輪；重試後沒有再發 Event。`kubectl get md -w` 仍然有兩行 `1 False`：第二行是重試那一輪把 message 從 `waiting for...` 更新成 `1/2 replicas ready`，status 一樣是 False，不算轉變，所以沒有 Event。新 pod `npj59` 一樣印 `model already present`，Ready 1→2 約 6 秒（16:55:26 → 16:55:32），跟實驗 5 相同。插曲 3 修正有效
- **實驗 7 文件裡的 `kubectl logs ... --previous` 拿不到 log**：`unable to retrieve container logs`。pod 停在 `Init:Error` 時，最近一次失敗的就是「目前」的 container，不加 `--previous` 就讀得到（文件已修正）
