# LLM Serving Operator 實驗室 — 04：Phase 3，讓 controller 住進叢集

> 成本：**$0**（GitHub Actions 公開 repo 免費）。
> 產出：一個 31.7 MB 的 controller image、刪到最小的 RBAC（用 `kubectl auth can-i` 驗證過）、用 `make deploy` 與 Helm 兩種方式部署進 kind、兩個新的 e2e 測試，以及六個「`make run` 看不到」的實驗。
> 先讀：`00-concepts.md` Part 3（leader election）、Part 7（RBAC）；`03` Part 1.1（SSA）、Part 1.2（status 樂觀鎖）。
> 預估時間：約 3 小時（其中 CI 第一次跑 e2e 要等 15～20 分鐘）。

---

## 這一階段的目的

Phase 1、2 的 controller 都是用 `make run` 在**筆電上**跑的。這種跑法有幾個盲點，Phase 3 一個一個補起來：

| `make run` 的盲點 | 為什麼看不到 | Phase 3 怎麼補 |
|---|---|---|
| RBAC marker 寫錯也不會發現 | 用的是**你的** kubeconfig（cluster-admin），什麼都能做 | 部署進叢集，以 ServiceAccount 身份執行；`kubectl auth can-i` 驗證（實驗 1、2） |
| marker 給了太多權限也不會發現 | 同上；scaffold 預設就給 `create;update;patch;delete` | 逐一對照程式實際送出的 API 呼叫，刪掉用不到的 verb（Part 3） |
| 沒有 leader election | `make run` 沒帶 `--leader-elect` | 叢集內的 manager 有帶；scale 成 2 個，殺掉 leader 量 failover 時間（實驗 4） |
| image 怎麼建、多大、跑在誰的身份下 | 沒有 image | 逐行讀 Dockerfile（Part 2） |
| 「controller 有在工作嗎」只能看 log | log 在你的終端機裡 | 讀 `/metrics`（實驗 5），以及看懂 readiness probe **不代表** controller 在工作（實驗 2） |
| envtest 測不到的行為（GC、真的 Ready）都只在 kind 上手動驗證 | 沒有自動化 | e2e 測試 + GitHub Actions（Part 7） |

**八個部分：**

1. 概念：`make run` 和叢集內差在哪
2. 逐行讀 Dockerfile
3. RBAC：刪到最小
4. 部署進 kind
5. 五個實驗
6. Helm chart
7. GitHub Actions 與 e2e 測試
8. 記錄與收工

---

## Part 1：概念

### 1.1 同一份程式，兩種執行方式

```
make run                                   make deploy
─────────────────────────                  ─────────────────────────────────────────
go run ./cmd/main.go（你的筆電）            /manager（叢集內的 pod，distroless image）
身份：~/.kube/config 裡的使用者              身份：ServiceAccount llm-serving-operator-controller-manager
     （kind 預設是 cluster-admin）               token 由 kubelet 掛進 pod，RBAC 決定能做什麼
沒有 --leader-elect                         --leader-elect：同一時間只有一個 replica 在 reconcile
沒有 probe                                  /healthz、/readyz（port 8081）
metrics 關閉（--metrics-bind-address=0）     metrics 在 :8443，HTTPS + 驗證（Part 5 實驗 5）
```

`cmd/main.go` 裡這些都是 flag，scaffold 已經寫好了。叢集內的參數來自兩個地方：`config/manager/manager.yaml` 的 `args`（`--leader-elect`、`--health-probe-bind-address=:8081`），以及 `config/default/manager_metrics_patch.yaml` 加上的 `--metrics-bind-address=:8443`。

> **不要同時跑 `make run` 和叢集內的 controller。** `make run` 沒有參加 leader election，兩邊會同時 reconcile 同一個 md。SSA 的 apply 用同一個 field owner，不會互相衝突，但 status 的樂觀鎖會讓兩邊一直 409，log 很難讀。Part 4 部署前先把 `make run` 停掉。

### 1.2 RBAC verb 對應到哪個 API 呼叫

RBAC 檢查的是**HTTP 請求**，不是 Go 函式。要寫出最小權限，得知道每個 client 呼叫最後送出什麼：

| controller 裡的呼叫 | HTTP 請求 | 需要的 verb |
|---|---|---|
| `r.Get` / `r.List`（讀 cache） | informer 啟動時 LIST、之後 WATCH | `list`、`watch`（`get` 通常一起給） |
| `r.Apply(...)`，物件**已存在** | `PATCH`（`application/apply-patch+yaml`） | `patch` |
| `r.Apply(...)`，物件**還不存在** | 同一個 `PATCH`，API server 把它當成建立 | `patch` **和 `create`** |
| `r.Status().Patch(...)` | `PATCH .../status` | `<resource>/status` 的 `patch` |
| `Recorder.Eventf(...)` | `POST`／`PATCH` `events.k8s.io` | `create`、`patch` |
| 子資源被刪 | **不是 controller 刪的**，是 GC（Phase 1 實驗 6） | 不需要 `delete` |

第三行是容易猜錯的地方。寫這份文件時在 kind 上實測：給一個 ServiceAccount 只有 `get;patch` 的 configmaps 權限，用 `kubectl apply --server-side` 建立一個不存在的 ConfigMap：

```
Error from server (Forbidden): configmaps "demo" is forbidden: User "system:serviceaccount:rbac-test:probe"
cannot create resource "configmaps" in API group "" in the namespace "rbac-test"
```

加上 `create` 就成功；物件建好之後，把 `create` 拿掉，再 apply 一次修改，只有 `patch` 也成功。**SSA 一個請求同時涵蓋建立與修改，所以兩個 verb 都要。**

### 1.3 image tag 與 `imagePullPolicy`

kind 節點沒辦法從你筆電的 Docker 拉 image，要用 `kind load docker-image` 把 image 直接塞進節點的 containerd。這時 tag 很重要：

| tag | 沒寫 `imagePullPolicy` 時，API server 補上的預設值 | 在 kind 上的結果 |
|---|---|---|
| `:latest`（或沒寫 tag） | `Always` | kubelet 堅持去 registry 拉 → `docker.io/library/...` 不存在 → `ImagePullBackOff` |
| 其他（例如 `:v0.0.1`） | `IfNotPresent` | 節點上已經有 → 直接用 |

Makefile 的預設 `IMG ?= controller:latest` 正好踩到第一行。所以這一階段一律用 `IMG=llm-serving-operator:v0.0.1`。實驗 3 會故意踩一次，還會看到一個更隱晦的後續問題。

---

## Part 2：逐行讀 Dockerfile

scaffold 的 Dockerfile 不用改，但要讀懂每一行在做什麼：

```dockerfile
ARG BASE_IMAGE=golang:1.26
FROM ${BASE_IMAGE} AS builder           # ① 第一階段：有完整 Go 工具鏈的 image（數百 MB）
ARG TARGETOS
ARG TARGETARCH                          # ② buildx 自動填入目標平台

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download                     # ③ 只複製 go.mod/go.sum 就先下載相依套件

COPY . .                                # ④ 再複製原始碼（.dockerignore 決定哪些檔案進得來）

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go   # ⑤

FROM gcr.io/distroless/static:nonroot   # ⑥ 第二階段：只有 CA 憑證、/etc/passwd、tzdata，沒有 shell
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532                        # ⑦ 非 root

ENTRYPOINT ["/manager"]
```

| # | 為什麼這樣寫 |
|---|---|
| ①⑥ 多階段 | 編譯需要 Go 工具鏈，執行不需要。最後的 image 只留下第二階段，Go 工具鏈、原始碼、模組快取都不會出現在裡面 |
| ③④ 先複製 go.mod 再複製原始碼 | Docker 的 layer cache 以「這一步的輸入有沒有變」判斷。只改 `.go` 檔時，③ 的輸入（go.mod/go.sum）沒變，`go mod download` 直接用快取 |
| ④ `.dockerignore` | scaffold 的寫法是「先排除全部（`**`），再加回 `*.go`、`go.mod`、`go.sum`」，而且排除 `*_test.go`。`bin/`（envtest 的 apiserver、etcd，幾百 MB）、`.git/`、`docs/` 都不會被送進 build context |
| ⑤ `CGO_ENABLED=0` | 產生不依賴 libc 的靜態執行檔，才能放進連 libc 都沒有的 `distroless/static` |
| ⑤ `GOARCH=${TARGETARCH}` | 在 M 系列 Mac 上 build 出 arm64、在 GitHub Actions（x86）上 build 出 amd64，各自符合當地的 kind 節點。`-a` 是強制全部重新編譯 |
| ⑥ distroless | 沒有 shell、沒有套件管理器 → 被打進去也沒有工具可用，CVE 掃描的面也小。代價是**不能 `kubectl exec` 進去除錯** |
| ⑦ `USER 65532` | 搭配 `manager.yaml` 的 `runAsNonRoot: true`、`readOnlyRootFilesystem: true`、`capabilities.drop: ALL`，符合 Pod Security Standards 的 `restricted` 等級（e2e 測試會把 namespace 標成 restricted 來驗證） |

**寫這份文件時實測**（M 系列 Mac、OrbStack 的 Docker 29.8.2）：

| 項目 | 結果 |
|---|---|
| 第一次 `docker build` | 2 分 47 秒（下載 `golang:1.26`、`go mod download`、編譯） |
| 只改一個 `.go` 檔再 build | 20 秒；log 裡 `RUN go mod download` 顯示 `CACHED` |
| 節點上的 image 大小（`crictl images`，壓縮後） | **31.7 MB** |
| `docker history` 裡 manager 執行檔那一層 | 72.9 MB（未壓縮） |
| `docker images` 顯示的大小 | 111 MB（算法跟 crictl 不同，不拿來比較） |
| image 的架構與使用者 | `arm64`、`65532:65532` |

> **可以更小，但這個 lab 不改**：加上 `-ldflags="-s -w"`（拿掉符號表與 DWARF 除錯資訊），本機實測執行檔從 72.9 MB 變成 50.9 MB，gzip 後從 30.3 MB 變成 14.9 MB。代價是 panic 時的 stack trace 仍在，但 `dlv` 之類的除錯器不能用。面試被問「image 怎麼瘦身」時可以講這個數字。

**動手**：

```bash
make docker-build IMG=llm-serving-operator:v0.0.1
docker image inspect llm-serving-operator:v0.0.1 --format '{{.Architecture}} {{.Config.User}}'   # 預期 arm64 65532:65532
```

---

## Part 3：RBAC，刪到最小

### 3.1 現在給了什麼

Phase 2 結束時的 marker：

```go
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
```

前三行是 `kubebuilder create api` 產生的預設值，中間三行是 Phase 1 照著 scaffold 的格式寫的。用 Part 1.2 的表逐行對照程式碼：

| 資源 | 程式裡實際做的事 | 需要 | 可以拿掉 |
|---|---|---|---|
| `modeldeployments` | 只有 `r.Get`（讀 cache） | `get;list;watch` | `create;update;patch;delete` —— **controller 從來不改使用者的 spec** |
| `modeldeployments/status` | `r.Status().Patch` | `patch` | `get;update` —— Phase 2 已經從 `Update` 改成 merge patch |
| `modeldeployments/finalizers` | 沒有 finalizer | `update`（理由見 3.2） | — |
| `deployments`、`services`、`persistentvolumeclaims` | `Owns()` 的 informer + `r.Apply` | `get;list;watch;create;patch` | `update;delete` —— SSA 不用 `Update`，刪除是 GC 的事 |
| `pods` | `Watches()` + `r.List` | `get;list;watch` | — |
| `events.k8s.io` `events` | `Recorder.Eventf` | `create;patch` | — |

### 3.2 為什麼 `finalizers: update` 要留著

我們沒有 finalizer，這一行看起來沒用。但 `ownerRef()` 寫了 `blockOwnerDeletion: true`，而 API server 有一個 admission plugin 叫 **`OwnerReferencesPermissionEnforcement`**：開了它，**只有對 owner 的 `finalizers` 子資源有 `update` 權限的人，才能設定 `blockOwnerDeletion`**（理由是：設了它，就能透過 foreground deletion 拖住 owner 的刪除，等於間接動到 owner 的 finalizer）。

- kind 與 upstream Kubernetes 預設**沒有**開這個 plugin，所以拿掉這行在 kind 上也能跑
- OpenShift 預設**有**開。拿掉之後，到了 OpenShift 上每一次 apply 子資源都會被拒

這就是 kubebuilder 預設產生這一行的原因。**最小權限不是「現在的叢集上能跑的最少權限」，而是「程式碼的行為需要的權限」** —— 程式碼設了 `blockOwnerDeletion`，就需要這個權限。

### 3.3 改 marker

把 `internal/controller/modeldeployment_controller.go` 的八行 marker 換成：

```go
// 最小權限（Phase 3）：每個 verb 都對應到一個實際的 API 呼叫，見 docs/04 Part 3.1。
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/status,verbs=patch
// +kubebuilder:rbac:groups=serving.llmlab.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
```

照原本的位置換掉就好（marker 區塊和 `func Reconcile` 之間保持原本的空行）。

重新產生並測試：

```bash
make manifests
git diff config/rbac/role.yaml
make test
make lint
```

**預期**：

- `role.yaml` 少了 `update`、`delete` 這些 verb；`modeldeployments` 只剩 `get;list;watch`；`modeldeployments/status` 只剩 `patch`
- 測試 16／16、覆蓋率 83.0% 不變 —— **envtest 的 client 是 admin 身份，RBAC 改了什麼它都不會發現**（寫這份文件時實測確認）。RBAC 的驗證只能在叢集內做，這就是 Part 4、5 的工作

### 3.4 scaffold 帶來的其他 RBAC（不改，但要知道）

`make deploy` 裝進去的不只有 `manager-role`。`config/rbac/kustomization.yaml` 裡還有：

| 檔案 | 給誰 | 做什麼 |
|---|---|---|
| `leader_election_role.yaml` | controller，**只在自己的 namespace**（Role，不是 ClusterRole） | `leases`（leader election 的鎖）、`events`；還有 `configmaps` 全部權限 —— 那是舊版 leader election 用 ConfigMap 當鎖時留下的，controller-runtime 現在用 Lease |
| `metrics_auth_role.yaml` | controller | 建立 `tokenreviews`、`subjectaccessreviews`：有人來讀 `/metrics` 時，controller 要問 API server「這個 token 是誰、他能不能讀」 |
| `metrics_reader_role.yaml` | **沒有綁給任何人** | 允許 `GET /metrics` 這個非資源 URL。要讀 metrics 的人（Prometheus）自己綁（實驗 5） |
| `modeldeployment_{admin,editor,viewer}_role.yaml` | **沒有綁給任何人** | 給叢集管理員用的範本：要讓某個團隊能建 md，就把 editor role 綁給他 |

`configmaps` 那條是可以再刪的權限，這個 lab 不動，列進 KNOWN-GAPS。

---

## Part 4：部署進 kind

### 4.1 停掉 `make run`

```bash
# 終端機 A：Ctrl-C 停掉 Phase 2 的 make run
kubectl get md smollm          # md 和子資源都還在，只是現在沒有 controller
```

Phase 2 留下的 `smollm` 不用刪。叢集內的 controller 用同一個 field owner（`llm-serving-operator`）做 SSA，接手時不會衝突。

### 4.2 build、load、deploy

```bash
export IMG=llm-serving-operator:v0.0.1
make docker-build IMG=$IMG          # Part 2 已經 build 過的話，這一步幾乎是全快取
kind load docker-image $IMG --name operator-lab
make deploy IMG=$IMG
```

`make deploy` 做三件事：

1. `make manifests`（所以 Part 3 的 marker 一定會生效）
2. `cd config/manager && kustomize edit set image controller=$IMG` —— **這一步會改寫 `config/manager/kustomization.yaml`**，`git status` 會看到它。這是 scaffold 的設計（`make build-installer` 也一樣），commit 時要決定要不要收進去
3. `kustomize build config/default | kubectl apply -f -`：namespace、CRD、RBAC、Deployment、metrics Service 一次套用

```bash
N=llm-serving-operator-system
kubectl -n $N rollout status deploy/llm-serving-operator-controller-manager
kubectl -n $N get pods
kubectl -n $N logs deploy/llm-serving-operator-controller-manager | head -20
```

**預期**（寫這份文件時在另一個 kind 叢集實測的 log，時間會不同）：

```
INFO  setup                       Starting manager
INFO  controller-runtime.metrics  Starting metrics server
INFO  leaderelection              Attempting to acquire leader lease...  {"lock": "llm-serving-operator-system/4c0c70e0.llmlab.dev"}
INFO  leaderelection              Successfully acquired lease
INFO  Starting EventSource        ... "source": "kind source: *v1.Pod"
...（ModelDeployment、Deployment、Service、PersistentVolumeClaim 各一行）
INFO  controller-runtime.metrics  Serving metrics server  {"bindAddress": ":8443", "secure": true}
INFO  Starting workers            {"controller": "modeldeployment", "worker count": 1}
```

接著確認它接手了既有的 md：

```bash
kubectl -n $N logs deploy/llm-serving-operator-controller-manager | grep "Applied children" | tail -3
kubectl get md smollm
kubectl -n $N get lease
```

**預期**：啟動後跑了幾輪 `Applied children`（每個 md 至少一輪），三個 RV 跟 Phase 2 結束時一樣（沒有寫入）；md 仍是 `1 / True`；Lease 的 `HOLDER` 是這個 pod 的名字。

> 寫這份文件時的實測叢集是全新的，log 裡唯一的 `ERROR` 是 `patch status: Operation cannot be fulfilled ...` —— 那是 Phase 2 插曲 3 加回的樂觀鎖擋下的那一輪，不是權限問題。**記下 log 裡有沒有任何 `forbidden`**，應該是 0：

```bash
kubectl -n $N logs deploy/llm-serving-operator-controller-manager | grep -ci forbidden
```

---

## Part 5：五個實驗

格式跟前兩階段一樣：**指令、關鍵輸出、時間**記進 RESULTS 的 Phase 3 段落。

先設好兩個變數，後面的指令都會用到：

```bash
N=llm-serving-operator-system
SA=system:serviceaccount:$N:llm-serving-operator-controller-manager
```

### 實驗 1：RBAC 矩陣

```bash
for c in \
  "create deployments.apps" "patch deployments.apps" "update deployments.apps" "delete deployments.apps" \
  "list pods" "delete pods" "get secrets" \
  "patch modeldeployments.serving.llmlab.dev" \
  "patch modeldeployments.serving.llmlab.dev --subresource=status" \
  "update modeldeployments.serving.llmlab.dev --subresource=finalizers" \
  "create leases.coordination.k8s.io -n $N" "create leases.coordination.k8s.io -n default"
do printf '%-72s %s\n' "$c" "$(kubectl auth can-i $c --as=$SA)"; done
```

**預期**（寫這份文件時實測）：

```
create deployments.apps                                                  yes
patch deployments.apps                                                   yes
update deployments.apps                                                  no
delete deployments.apps                                                  no
list pods                                                                yes
delete pods                                                              no
get secrets                                                              no
patch modeldeployments.serving.llmlab.dev                                no
patch modeldeployments.serving.llmlab.dev --subresource=status           yes
update modeldeployments.serving.llmlab.dev --subresource=finalizers      yes
create leases.coordination.k8s.io -n llm-serving-operator-system         yes
create leases.coordination.k8s.io -n default                             no
```

- `--as=system:serviceaccount:...` 是**模擬（impersonate）** 那個 ServiceAccount 去問 API server，不需要拿到它的 token。你能這樣做是因為你是 cluster-admin
- `get secrets: no` 是面試最常被問的一題：**controller 被打進去，攻擊者能不能讀到整個叢集的 Secret？** 這個 controller 不能
- 最後兩行對照：leader election 的權限是 Role（只限自己的 namespace），不是 ClusterRole

### 實驗 2：少給一個權限會怎樣

`make run` 時期 RBAC 寫錯不會被發現。現在故意拿掉 `pods` 的權限，看叢集內的 controller 會發生什麼：

```bash
kubectl get clusterrole llm-serving-operator-manager-role -o jsonpath='{.rules[1].resources}{"\n"}'   # 先確認第二條是 ["pods"]
kubectl patch clusterrole llm-serving-operator-manager-role --type=json -p '[{"op":"remove","path":"/rules/1"}]'
date +%T; kubectl -n $N rollout restart deploy/llm-serving-operator-controller-manager
kubectl -n $N get pods -w
```

> 如果第一行印出來的不是 `["pods"]`，把 `/rules/1` 換成 pods 那條的索引。

另一個視窗看 log：

```bash
kubectl -n $N logs -f deploy/llm-serving-operator-controller-manager | grep -E 'forbidden|timed out|Failed to run'
```

**預期**（寫這份文件時實測）：

1. 新 pod 先等 leader lease（舊 pod 被刪時沒有主動釋放 lease，要等它過期，實測約 30 秒）
2. 拿到 lease 後，pod 的 informer 開始重複報錯：`Failed to watch ... "failed to list *v1.Pod: pods is forbidden: User \"system:serviceaccount:...\" cannot list resource \"pods\"`
3. **這段期間 pod 一直是 `1/1 Running`**
4. 拿到 lease 後**整整 2 分鐘**，manager 放棄並退出：`Failed to run manager {"error": "failed to wait for modeldeployment caches to sync kind source: ...: timed out waiting for cache to be synced for kind source: ..."}`
   - `kind source:` 後面寫的**不一定是 Pod**：寫文件時實測是 `*v1.Pod`，照做時是 `*v1alpha1.ModelDeployment`（前一行 `Could not wait for Cache to sync` 又寫 `*v1.PersistentVolumeClaim`）。每個 source 等的都是**整個 cache** 同步（controller-runtime `pkg/internal/source/kind.go` 的 `ks.Cache.WaitForCacheSync`），所以 Pod 起不來時全部一起 timeout，訊息寫的是最先回報的那個。**真正缺哪個權限，要看前面的 `forbidden`**
5. container 重啟，`RESTARTS` 變成 1，然後重複 2～4

**這個實驗要記住的三件事**：

- **一個 informer 起不來，整個 controller 都不工作**。controller-runtime 要等**所有** watch 的 cache 都同步完才啟動 worker，所以只缺 pods 權限，連 Deployment 被刪都不會被修復
- **readiness probe 說 Ready，不代表 controller 在工作**。scaffold 的 `/readyz` 是 `healthz.Ping`，只要 process 活著就回 200。2 分鐘的空窗期間，`kubectl get pods` 看起來一切正常
- 2 分鐘來自 controller-runtime 的 `CacheSyncTimeout` 預設值。症狀是「很慢的 CrashLoopBackOff」，比立刻 crash 更難發現。**部署後第一件事是 `kubectl logs` 搜尋 `forbidden`**，而不是看 pod 狀態

做完還原（`make deploy` 會重新套用正確的 ClusterRole）：

```bash
make deploy IMG=$IMG
kubectl -n $N rollout restart deploy/llm-serving-operator-controller-manager
kubectl -n $N rollout status deploy/llm-serving-operator-controller-manager
```

### 實驗 3：`:latest` 的兩個陷阱

**3a：kind load 了，還是拉不到**

```bash
docker tag $IMG llm-serving-operator:latest
kind load docker-image llm-serving-operator:latest --name operator-lab
kubectl -n $N delete deploy llm-serving-operator-controller-manager   # 先刪掉，理由見下方
make deploy IMG=llm-serving-operator:latest
kubectl -n $N get pods -w          # 等到 ImagePullBackOff，Ctrl-C
kubectl -n $N get deploy llm-serving-operator-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].imagePullPolicy}{"\n"}'
```

**預期**（實測）：新 pod `ImagePullBackOff`，`imagePullPolicy` 是 `Always`。Events 裡：

```
Failed to pull image "llm-serving-operator:latest": ... failed to resolve reference "docker.io/library/llm-serving-operator:latest":
pull access denied, repository does not exist or may require authorization
```

image 明明已經在節點上（`docker exec operator-lab-control-plane crictl images | grep llm` 看得到），但 `Always` 讓 kubelet 一定要先問 registry。因為上面先刪了 Deployment，這段期間**沒有任何 controller 在跑**。

> **為什麼要先刪 Deployment**：照做時第一次沒有刪，直接 `make deploy IMG=...:latest`，結果新 pod 正常 `Running`，`imagePullPolicy` 是 `IfNotPresent`。原因跟 3b 一模一樣，只是方向相反：Deployment 是 Part 4 用 `v0.0.1` 建立的，API server 當時補了 `IfNotPresent`；這次 apply 只換了 image，沒寫 `imagePullPolicy`，舊值就留著。所以只有**第一次建立**時就用 `:latest`，才會踩到這個陷阱。不刪也可以觀察：`get deploy -o jsonpath` 會看到 `llm-serving-operator:latest IfNotPresent` 這個組合，這本身就是 3b 要講的事。

**3b：換回 v0.0.1，還是拉不到**

```bash
make deploy IMG=$IMG
kubectl -n $N get pods -w          # 等一下
kubectl -n $N get deploy llm-serving-operator-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].image} {.spec.template.spec.containers[0].imagePullPolicy}{"\n"}'
```

**預期**（實測，這是寫文件時意外踩到的）：image 換成 `v0.0.1` 了，但 `imagePullPolicy` **還是 `Always`**，新 pod 一樣 `ImagePullBackOff`。

**為什麼**：`imagePullPolicy` 是 API server 在**欄位為空時**補的預設值。3a 那次建立的 pod template 沒寫這個欄位，API server 依當時的 tag（`latest`）補了 `Always`。這次 `kubectl apply` 送的 manifest 一樣沒寫 `imagePullPolicy` —— 沒寫的欄位不歸 apply 管（跟 `03` Part 1.1 的 SSA 同一個道理），所以舊值 `Always` 留著，不會因為 tag 換了就重算。

**這就是 Phase 2 學的「預設值」又出現了一次**，只是這次的主角是 `kubectl apply`，不是我們的 controller。

修法：刪掉 Deployment 重建（新物件的欄位是空的，會重新依 tag 算預設值）：

```bash
kubectl -n $N delete deploy llm-serving-operator-controller-manager
make deploy IMG=$IMG
kubectl -n $N rollout status deploy/llm-serving-operator-controller-manager
kubectl -n $N get deploy llm-serving-operator-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].imagePullPolicy}{"\n"}'     # 預期 IfNotPresent
```

> 另一個修法是在 `config/manager/manager.yaml` 明確寫 `imagePullPolicy: IfNotPresent`，從此不依賴預設值。這個 lab 不改，因為正式環境用可變的 tag 時，`Always` 反而比較安全。**根本解法是永遠不要部署 `:latest`。**

### 實驗 4：leader election failover

```bash
kubectl -n $N scale deploy/llm-serving-operator-controller-manager --replicas=2
kubectl -n $N rollout status deploy/llm-serving-operator-controller-manager
kubectl -n $N get pods
kubectl -n $N get lease 4c0c70e0.llmlab.dev \
  -o jsonpath='{.spec.holderIdentity}{"  duration="}{.spec.leaseDurationSeconds}{"\n"}'
```

兩個 pod 都是 `1/1 Running`，但 lease 只有一個 holder。看看沒拿到 lease 的那個 pod 的 log：

```bash
kubectl -n $N logs <不是 leader 的那個 pod> | tail -3      # 預期停在 "Attempting to acquire leader lease..."，沒有 Starting workers
```

**殺掉 leader，量多久有人接手**：

```bash
L=$(kubectl -n $N get lease 4c0c70e0.llmlab.dev -o jsonpath='{.spec.holderIdentity}'); P=${L%%_*}; echo "leader pod: $P"
date +%T; kubectl -n $N delete pod $P --wait=false
for i in $(seq 1 60); do
  h=$(kubectl -n $N get lease 4c0c70e0.llmlab.dev -o jsonpath='{.spec.holderIdentity}')
  if [ "${h%%_*}" != "$P" ]; then date +%T; echo "new leader: $h"; break; fi
  sleep 1
done
```

**預期**（寫這份文件時實測）：**約 19 秒**後換人（10:00:39 → 10:00:58）。

- 15 秒是 `leaseDurationSeconds`：舊 leader 被刪時**沒有主動釋放 lease**，其他人要等它過期。再加上候選者每 2 秒重試一次（`RetryPeriod`）
- **接手的不一定是原本待命的那個 pod。** 實測是 Deployment 補上的**新 pod** 搶到的 —— lease 過期那一刻，誰的重試剛好先到誰就贏。兩種結果都正常，記下你看到的是哪一種
- 這 19 秒內**沒有任何 controller 在 reconcile**。這段期間 md 被改、Deployment 被刪，都要等新 leader 上來才處理（level-triggered，所以不會漏，只是會晚）

> `cmd/main.go` 有一行被註解掉的 `LeaderElectionReleaseOnCancel: true`。打開它，leader 收到 SIGTERM 時會主動釋放 lease，failover 可以縮短到幾秒。scaffold 的註解說明了前提：**manager 停止後程式要立刻結束**，不能還有其他工作在跑，否則釋放 lease 之後會出現兩個 leader 同時寫入的空窗。這個 lab 不打開，面試可以講這個取捨。

做完 scale 回 1：

```bash
kubectl -n $N scale deploy/llm-serving-operator-controller-manager --replicas=1
```

### 實驗 5：用 metrics 取代 grep log

Phase 2 數「reconcile 了幾輪」是 `grep -c "Applied children"`。叢集內的 controller 有 `/metrics`，controller-runtime 自動提供這些數字。

metrics 端點有保護（HTTPS + 每個請求都做 TokenReview／SubjectAccessReview，Part 3.4）。開一個專門讀 metrics 的 ServiceAccount：

```bash
kubectl create sa metrics-viewer
kubectl create clusterrolebinding metrics-viewer \
  --clusterrole=llm-serving-operator-metrics-reader --serviceaccount=default:metrics-viewer
TOKEN=$(kubectl create token metrics-viewer)
kubectl -n $N port-forward svc/llm-serving-operator-controller-manager-metrics-service 8443:8443 &
sleep 2
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics \
  | grep -E '^controller_runtime_reconcile_total|^controller_runtime_reconcile_errors_total|^workqueue_depth\{.*modeldeployment|^process_resident_memory_bytes'
```

> 這一段寫文件時沒有在本機實測（e2e 測試用的是同樣的 token 機制，只是改在叢集內的 curl pod 裡打）。如果 port-forward 或 curl 的行為跟描述不同，以實際看到的為準，寫進「與預期不同」。

**預期看到的指標**：

| 指標 | 意義 | 對照 |
|---|---|---|
| `controller_runtime_reconcile_total{controller="modeldeployment",result="success"}` | 成功的 Reconcile 次數 | Phase 2 的 `grep -c "Applied children"` |
| `controller_runtime_reconcile_total{...,result="error"}` | 回傳 error 的次數 | Phase 2 status 樂觀鎖的 409 會算在這裡 |
| `workqueue_depth{name="modeldeployment"}` | 佇列裡等著被處理的物件數 | 平常應該是 0；一直增加代表 controller 處理不過來 |
| `process_resident_memory_bytes` | controller 的記憶體 | `manager.yaml` 的 limit 是 128Mi |

**記憶體那一行要寫進 RESULTS**：KNOWN-GAPS 有一條「`Watches(&corev1.Pod{})` 會 cache 叢集內所有 pod」。kind 上只有十幾個 pod，記憶體應該遠低於 128Mi；這個數字是之後如果做 `cache.Options.ByObject` 優化時的基準。

做完：

```bash
kill %1                              # 停掉 port-forward
kubectl delete clusterrolebinding metrics-viewer; kubectl delete sa metrics-viewer
```

---

## Part 6：Helm chart

### 6.1 為什麼還要 Helm

`make deploy` 需要整個 repo 和 kustomize。別人想裝你的 operator，常見的兩種方式：

| | `dist/install.yaml`（`make build-installer`） | Helm chart |
|---|---|---|
| 使用者要裝什麼 | 只要 `kubectl` | `helm` |
| 客製化 | 自己改 YAML | `--set` / `values.yaml` |
| 升級、回滾、知道裝了哪一版 | 沒有 | `helm upgrade`、`helm rollback`、`helm history` |

kubebuilder 有一個 plugin 從 kustomize 的輸出**產生** chart，不用手寫模板。

### 6.2 產生 chart

```bash
brew install helm        # 先裝好。Makefile 的 install-helm 在找不到 helm 時會 curl 一個安裝腳本 | bash，自己裝比較清楚裝了什麼
helm version --short

kubebuilder edit --plugins=helm/v2-alpha
git status
```

**預期**（寫這份文件時在 scratch 副本實測；kubebuilder v4.16.0）：

```
 M Makefile                              # 新增 helm-deploy、helm-uninstall、helm-status、helm-history、helm-rollback
 M PROJECT                               # 記錄 helm plugin 的設定（PROJECT 是自動產生的，不要手改）
 M config/manager/kustomization.yaml     # ⚠ image 被改回 controller:latest
?? .github/workflows/test-chart.yml      # 新的 CI：在 kind 上 helm lint + helm install
?? dist/                                 # dist/install.yaml 和 dist/chart/
```

> **`config/manager/kustomization.yaml` 被改回 `controller:latest`**：plugin 內部會跑 `make build-installer`（用預設的 `IMG`）產生 `dist/install.yaml`，再從它轉成 chart。這不影響 chart，但之後忘了帶 `IMG=` 直接 `make deploy`，就會踩到實驗 3。下一次 `make deploy IMG=$IMG` 會再改回來。
>
> 照做時**沒有**被改回來：Part 4 的 `export IMG=llm-serving-operator:v0.0.1` 還在同一個 shell 裡，Makefile 的 `IMG ?=` 遇到環境變數就不用預設值，所以 kustomization、`dist/install.yaml`、`values.yaml` 的 image 都是 `v0.0.1`。`git status` 裡這個檔案仍然是 `M`，那是 Part 4 的 `make deploy` 改的。

看一下產生了什麼：

```bash
find dist/chart -type f | sort
cat dist/chart/values.yaml | grep -vE '^\s*#|^\s*$' | head -40
```

幾個值得注意的地方：

- **`templates/rbac/manager-role.yaml` 的內容就是 Part 3 刪過的版本**：chart 是從 `config/` 產生的。**之後改了 marker，要重跑 `kubebuilder edit --plugins=helm/v2-alpha --force` 才會同步到 chart**（`--force` 會覆蓋 `values.yaml`，有自訂的話先備份）。兩邊不同步是 chart 最常見的 bug，列進 KNOWN-GAPS
- **CRD 放在 `templates/crd/`，不是 Helm 慣例的 `crds/` 目錄**，而且帶 `helm.sh/resource-policy: keep`（`values.yaml` 的 `crd.keep: true`）：
  - Helm 對 `crds/` 目錄只在第一次安裝時建立，**永遠不會升級**，也不會刪除。放在 `templates/` 才能讓 `helm upgrade` 更新 CRD
  - `keep` 讓 `helm uninstall` **不刪 CRD**。刪 CRD 會連帶刪掉叢集上**所有**的 md，再由 GC 刪掉所有推論服務和模型快取 —— 實驗 6 會看到 `make undeploy` 正是這樣做的
- `manager.image.pullPolicy: IfNotPresent` 寫死在 values 裡，所以 chart 不會踩到 `:latest` 的預設值陷阱
- `rbac.helpers.enabled: false`：admin/editor/viewer 這三個範本 role 預設不裝（kustomize 版本預設會裝）

```bash
helm lint dist/chart
helm template llm-serving-operator dist/chart -n llm-serving-operator-system \
  | grep -E '^kind:|^  name:' | paste - - | sort
```

- `helm lint` 檢查 chart 本身（必填欄位、模板語法、產生的 YAML）；`helm template` 把模板展開成 `helm install` 會送出的 YAML，但不送出。兩個都不碰叢集
- release 名稱用 `make helm-deploy` 的 `llm-serving-operator`，資源名稱會**跟 kustomize 部署的完全一樣**（例如 `llm-serving-operator-manager-role`）—— 這就是實驗 6 要先 `make undeploy` 的原因。換成別的 release 名稱（例如 `test`），名稱會變成 `test-llm-serving-operator-...`
- ClusterRole 只有 3 個（manager、metrics-auth、metrics-reader），kustomize 版有 6 個：admin/editor/viewer 因為 `rbac.helpers.enabled: false` 沒有產生

> 照做時實測（helm v4.3.2）：`lint` 是 `1 chart(s) linted, 0 chart(s) failed`，另有一行 `[INFO] Chart.yaml: icon is recommended`，不算失敗。

### 6.3 實驗 6：用 Helm 部署，以及 `make undeploy` 與 `helm uninstall` 的差別

Helm 不能接管不是它建立的資源（會報 `invalid ownership metadata`），所以要先把 kustomize 部署的拿掉。**先看清楚 `make undeploy` 會刪什麼：**

```bash
bin/kustomize build config/default | grep '^kind:' | sort | uniq -c   # 它會刪的資源種類
```

清單裡有 `CustomResourceDefinition`。**刪 CRD = 刪掉叢集上所有 md**：

```bash
kubectl get md,deploy,pvc
date +%T; make undeploy
kubectl get md                                                 # 預期 error：server 不認得 modeldeployments
kubectl get deploy,svc,pvc -l app.kubernetes.io/instance=smollm  # 預期 No resources found
```

**預期**：md 隨 CRD 被刪；Deployment、Service、PVC 由 GC 依 ownerReference 刪掉（Phase 1 實驗 6 的機制），**模型快取也沒了**。在正式環境這等於「升級 operator 的人不小心把所有客戶的推論服務都刪了」。

用 Helm 裝回來：

```bash
make helm-deploy IMG=$IMG
make helm-status
kubectl -n $N get pods
kubectl apply -f config/samples/serving_v1alpha1_modeldeployment.yaml
kubectl get md -w                    # 模型要重新下載，約 1 分鐘
```

> `make helm-deploy` 把 `IMG` 拆成 `manager.image.repository` 和 `manager.image.tag` 傳給 helm，並帶 `--wait --timeout 5m`。

等 md 變成 `1 / True`，然後**用 Helm 移除 operator**：

```bash
date +%T; make helm-uninstall
kubectl get crd modeldeployments.serving.llmlab.dev      # 預期還在（keep）
kubectl get md smollm
kubectl get deploy,svc,pvc -l app.kubernetes.io/instance=smollm
```

> md 和子資源要分開查：一是 md 本身**沒有** `app.kubernetes.io/instance` 這個 label（label 是 controller 加在子資源上的），用 `-l` 永遠查不到 md；二是 CRD 不存在時，`kubectl get md,deploy,...` 會在 md 那裡直接報錯，後面的資源都不會印出來。

**預期**：CRD、md、子資源**全部還在**，推論服務照常回應（02 Part 4.3 的 curl）。只是現在沒有 controller：刪掉 Deployment 不會被修復。

裝回來（後面的 Phase 需要 controller 在跑）：

```bash
make helm-deploy IMG=$IMG
```

> 這個實驗的 Helm 部分寫文件時沒有實測。`make undeploy` 刪 CRD、刪 CRD 連帶刪 CR，是 Kubernetes 的既定行為；`keep` 是 Helm 的既定行為。實際的時間軸與輸出記進 RESULTS。

**這個實驗要記住的**：operator 的「卸載」和「CR 的生命週期」要分開。**CRD 是使用者資料的 schema，刪它等於刪資料庫的 table。** 這也是為什麼 KServe、cert-manager 這類專案的文件都會特別警告卸載時不要刪 CRD。

**之後用哪種方式？** 後面的 Phase 用 Helm 或 kustomize 都可以，但**兩種不要混用**。這份文件之後的指令以 `make helm-deploy` 為準；想換回 kustomize，先 `make helm-uninstall`，CRD 會因為 `keep` 留下來，kustomize 的 `kubectl apply` 可以直接接手 CRD。

---

## Part 7：GitHub Actions 與 e2e 測試

### 7.1 scaffold 已經給了什麼

```bash
ls .github/workflows/
```

| workflow | 觸發 | 做什麼 |
|---|---|---|
| `lint.yml` | push、PR | `make lint-config` + `make lint` |
| `test.yml` | push、PR | `make test`（envtest） |
| `test-e2e.yml` | push、PR | 裝 kind → `make test-e2e` |
| `test-chart.yml`（Part 6 新增） | push、PR | kind → build image → `helm lint` → `make helm-deploy` |

scaffold 的 workflow 有幾個值得學的寫法，面試講 CI 安全時用得到：

- `permissions: {}` 在檔案層級把 `GITHUB_TOKEN` 的權限歸零，每個 job 只開需要的（`contents: read`）。被注入惡意步驟時，token 也不能推 code 或發 release
- `actions/checkout@de0fac2e...  # v6.0.2`：action **釘在 commit SHA**，不是 tag。tag 可以被移動（供應鏈攻擊的常見手法），SHA 不行
- `persist-credentials: false`：checkout 完不把 token 留在 `.git/config`，後面的步驟讀不到

### 7.2 現在的 e2e 測了什麼、沒測什麼

`test/e2e/e2e_test.go` 的兩個測試只驗證「**manager pod 跑起來了**」和「**metrics 端點回 200**」—— 跟 `ModelDeployment` 無關。KNOWN-GAPS 的 Phase 1 有一條「envtest 不涵蓋的行為（GC、Deployment 真的 Ready）只在 kind 上手動驗證，沒有自動化 e2e 測試」，這一節把它補起來。

新增兩個測試：

1. **RBAC**：把實驗 1 的矩陣寫成斷言。以後有人在 marker 加了 `delete`，CI 會擋下來
2. **端到端**：套用範例 CR → 等 `Available=True`（真的拉 llama.cpp image、真的下載模型）→ 刪掉 CR → 等 GC 刪掉 Deployment、Service、PVC

### 7.3 改 `test/e2e/e2e_test.go`

import 加上 `"strings"`：

```go
import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	...
```

在 `// +kubebuilder:scaffold:e2e-webhooks-checks` 這行**上面**插入（**不要刪 scaffold marker**，Phase 4 加 webhook 時 kubebuilder 會在這裡插入程式碼）：

```go
		It("should grant the manager only the permissions it needs", func() {
			sa := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)
			cases := []struct {
				check string
				want  string
			}{
				{"create deployments.apps", "yes"}, // SSA 建立物件需要 create
				{"patch deployments.apps", "yes"},  // SSA 是 PATCH
				{"update deployments.apps", "no"},
				{"delete deployments.apps", "no"}, // 刪除是 GC 的事
				{"patch modeldeployments.serving.llmlab.dev --subresource=status", "yes"},
				{"patch modeldeployments.serving.llmlab.dev", "no"}, // spec 是使用者的
				{"get secrets", "no"},
			}
			for _, c := range cases {
				args := append([]string{"auth", "can-i"}, strings.Fields(c.check)...)
				args = append(args, "--as="+sa)
				// 答案是 no 時 kubectl 以 exit code 1 結束，所以只看輸出、不看 err。
				out, _ := utils.Run(exec.Command("kubectl", args...))
				Expect(strings.TrimSpace(out)).To(Equal(c.want), "kubectl auth can-i %s", c.check)
			}
		})

		It("should serve a ModelDeployment and garbage-collect its children on delete", func() {
			By("applying the sample ModelDeployment")
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", "config/samples/serving_v1alpha1_modeldeployment.yaml"))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Available=True (pulls the llama.cpp image and downloads the model)")
			_, err = utils.Run(exec.Command("kubectl", "wait", "md/smollm", "--for=condition=Available", "--timeout=15m"))
			Expect(err).NotTo(HaveOccurred())

			By("deleting the ModelDeployment")
			_, err = utils.Run(exec.Command("kubectl", "delete", "md", "smollm"))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the garbage collector to delete the children")
			for _, obj := range []string{"deployment/smollm", "service/smollm", "pvc/smollm-models"} {
				_, err = utils.Run(exec.Command("kubectl", "wait", obj, "--for=delete", "--timeout=2m"))
				Expect(err).NotTo(HaveOccurred(), "%s was not garbage-collected", obj)
			}
		})

```

幾個設計決定：

- **用 `kubectl` 而不是 Go client**：跟 scaffold 其他測試一致，而且測試步驟就是你在終端機會打的指令，失敗時直接複製出來重現
- **`kubectl wait --for=condition=Available`**：直接讀 md 的 condition。這是 Phase 1 把狀態寫成 `metav1.Condition` 的回報 —— 標準格式，標準工具就能等
- **`--for=delete` 對已經不存在的物件會立刻成功**（實測 exit code 0），所以 GC 比 `kubectl wait` 先跑完也不會失敗
- **15 分鐘**：GitHub 的 runner 上 llama.cpp image（292 MB）要冷拉。寫這份文件時本機的 kind 拉這個 image 花了 6 分 7 秒
- **測試放在範例 CR 的 namespace（`default`）**，不是 manager 的 namespace。manager 的 namespace 被標成 PSS `restricted`，而 llama-server 的 pod 沒有設 `runAsNonRoot` 這些欄位，放進去會被拒（列進 KNOWN-GAPS）

### 7.4 改 `Makefile`：e2e 的 timeout

`go test` 預設的 timeout 是 **10 分鐘**，超過就 panic。光是等 llama.cpp image 就可能用掉大半。把 `test-e2e` 那一行加上 `-timeout 30m`：

```makefile
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout 30m
	$(MAKE) cleanup-test-e2e
```

### 7.5 在本機跑一次 e2e

`make test-e2e` 會**建一個新的 kind 叢集** `llm-serving-operator-test-e2e`，跑完再**刪掉它**（AGENTS.md：e2e 要在隔離的叢集上跑）。它會用你目前的 kubectl context：`kind create cluster` 會自動切到新叢集，所以測試不會碰到 `operator-lab`；但跑完刪除後，目前的 context 不會自動切回 `operator-lab`，記得自己切回來。

> 已經有一個叫 `llm-serving-operator-test-e2e` 的叢集時，`setup-test-e2e` 會直接沿用它，**跑完一樣會刪掉**。不要用這個名字建你想保留的叢集。

```bash
go vet -tags=e2e ./test/e2e/      # 先確認編譯得過
time make test-e2e 2>&1 | tee /tmp/operator-phase3-e2e.log
kubectl config use-context kind-operator-lab
grep -E '^(Ran|SUCCESS|FAIL)|Passed' /tmp/operator-phase3-e2e.log
```

**預期**：`SUCCESS! -- 4 Passed | 0 Failed`（scaffold 的 2 個 + 新的 2 個）。

寫這份文件時的實測（在一個**已經拉過 llama.cpp image** 的 kind 叢集上跑，所以比冷啟動快）：

| 測試 | 時間 |
|---|---|
| should run successfully | 3.2 秒 |
| should ensure the metrics endpoint is serving metrics | 23.5 秒 |
| should grant the manager only the permissions it needs | 0.2 秒 |
| should serve a ModelDeployment and garbage-collect its children on delete | 76.5 秒（大部分是下載模型） |
| 整個 `make test-e2e`（含 build image、裝 cert-manager、刪叢集） | 2 分 53 秒 |

你的 e2e 叢集是全新的，llama.cpp image 要冷拉，預期多 5～6 分鐘。**記下實際時間**，跟 GitHub Actions 的時間比較。


### 7.6 推上 GitHub

```bash
git add -A
git commit -m "feat: least-privilege RBAC, helm chart, e2e tests for ModelDeployment"
git push
```

到 GitHub 的 Actions 頁面看四個 workflow。記下每個 workflow 的時間，尤其是 `E2E Tests` 裡 `Running Test e2e` 那一步 —— 它的大部分時間應該在等 llama.cpp image。

> 如果 `E2E Tests` 在 GitHub 上失敗、本機卻會過，最常見的原因是時間（runner 比較慢）或架構（runner 是 amd64，llama.cpp 的 `:server` tag 有 amd64 版本，應該沒問題）。失敗時 `AfterEach` 會印出 controller log 和 Events，先看那裡。

---

## Part 8：記錄與收工

1. RESULTS 的 Phase 3 段落：
   - Part 2 的 image 大小、架構、使用者
   - Part 3 的 `role.yaml` diff 摘要（拿掉了哪些 verb）
   - Part 4 部署後的 log 摘要、`forbidden` 次數
   - 六個實驗；實驗 2 的時間軸（拿到 lease、第一次 forbidden、timeout、restart）、實驗 4 的 failover 秒數與接手的是哪個 pod、實驗 5 的指標原文
   - 實驗 6 的時間軸
   - e2e：本機與 GitHub Actions 的時間、通過數
2. 「與預期不同」：特別是實驗 5（寫文件時沒實測）、實驗 6 的 Helm 部分、`helm lint` 的輸出
3. 更新 `KNOWN-GAPS.md`：
   - 刪掉已經補上的兩條：Phase 1 的「controller 在本機跑（`make run`）」、「envtest 不涵蓋的行為只在 kind 上手動驗證，沒有自動化 e2e 測試」（e2e 現在涵蓋了 Ready 與 GC）
   - 新增 Phase 3 的邊界：
     - leader election 的 Role 仍有 `configmaps` 全部權限（scaffold 留下的，controller-runtime 只用 Lease）
     - readiness probe 只是 ping：informer 起不來時 pod 仍顯示 Ready，2 分鐘後才重啟（實驗 2）
     - Helm chart 是從 `config/` 產生的，改了 marker 要手動重跑 plugin，CI 沒有檢查兩者是否同步
     - llama-server 的 pod 不符合 PSS `restricted`（沒有 `runAsNonRoot`、`seccompProfile`…），只能跑在沒有強制 restricted 的 namespace
     - 沒有 release pipeline：image 只存在本機與 CI 的 kind 裡，沒有推到 registry、沒有版本 tag
     - e2e 每次 push 都要冷拉 292 MB 的 llama.cpp image
4. README 的階段表：Phase 3 改成 ✅、填上文件連結；「成果」的表格加一欄 Phase 3（只填量過的：image 大小、RBAC 規則數、failover 時間、e2e 時間）
5. commit（7.6 已經 push 的話，這裡 commit 文件的更新）

## 完成條件

- [x] `make docker-build` 成功，image 大小、架構、使用者已記錄
- [x] RBAC marker 已縮減，`make test` 16／16、`make lint` 0 issues
- [x] 叢集內的 controller 接手既有的 md，log 裡 `forbidden` 為 0
- [x] 實驗 1：`auth can-i` 矩陣跟預期一致
- [x] 實驗 2：記下「pod Ready 但 controller 沒在工作」的時間長度
- [x] 實驗 3：兩個 `:latest` 陷阱都重現過，並用刪除重建修好
- [x] 實驗 4：failover 秒數已記錄
- [x] 實驗 5：reconcile 次數與記憶體已從 `/metrics` 讀到
- [x] 實驗 6：`make undeploy` 刪光 md、`helm uninstall` 留下 md，兩者都親眼看過
- [x] `make test-e2e` 本機通過；GitHub Actions 四個 workflow 都是綠的
- [x] KNOWN-GAPS、README 已更新

> 下一階段（Phase 4）：Admission —— CEL 驗證規則 vs validating webhook。webhook 需要 TLS 憑證，會用到 cert-manager，也會用到這一階段建立的叢集內部署。
