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
