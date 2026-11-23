# KNOWN GAPS

> 這個 lab 刻意沒做、或做了但有邊界的地方。面試時主動講清楚範圍，比被問到才承認好。

## Phase 1 的邊界

- **只有一種 runtime（llama.cpp，CPU）**：沒有 GPU、沒有 vLLM。GPU 上的推論調校在姊妹專案 `llm-inference-k8s-lab`。
- **沒有 HPA、沒有 canary、沒有縮到零**：這些是 KServe 的範圍，見 `docs/00-concepts.md` Part 8。
- **單一 namespace 的子資源**：沒有跨 namespace 或叢集外資源，所以不需要 finalizer（Phase 2 實驗 8 驗證過理由）。

## Phase 2 的邊界

- **PVC 是 ReadWriteOnce**：只能被同一個節點上的 pod 掛載。kind 是單節點，所以 2 個 replica 能共用快取（實驗 5）；多節點叢集上，排到別的節點的 pod 會掛載失敗。正式做法是 RWX 儲存（NFS、EFS…）或節點本地的模型快取。
- **PVC 容量寫死 1Gi**：沒有 spec 欄位可以調整；換模型後舊的模型檔不會被清掉，會一直佔空間。
- **PVC 跟著 md 一起被 GC 刪掉**：刪 md 或 PVC 被誤刪，快取就沒了，下一個 pod 要重新下載（實驗 8a、8b 約 70 秒）。
- **`Watches(&corev1.Pod{})` 會 cache 叢集內所有 pod**：大叢集上記憶體可觀。可以在 `cmd/main.go` 用 `cache.Options.ByObject` 只 cache 帶 `managed-by=llm-serving-operator` label 的 pod，這個 lab 沒做。
- **Degraded 只認得一種失敗**：init container `fetch-model` 非零結束。llama-server 載入失敗、OOMKilled、image 拉不下來，都還只會顯示 `Progressing`。
- **status 的 409 會以 `ERROR Reconciler error` 出現在 log**：插曲 3 加回樂觀鎖後，狀態轉變時偶爾會有一次預期中的 409（實驗 6 有 1 次）。沒有特別處理成安靜重試。
- **envtest 沒有 cache**：測試用的 client 直接打 API server，cache 落後造成的行為只能用 `staleReader` 手工模擬（插曲 3），或在 kind 上觀察。
- **PVC 建立後第一次 re-apply 會多寫一次**：envtest 實測內容完全相同、只有 resourceVersion 變，之後就穩定。原因沒有查明。

## Phase 3 的邊界

- **leader election 的 Role 仍有 `configmaps` 全部權限**：scaffold 留下的，controller-runtime 現在只用 Lease 當鎖，這條可以再刪，這個 lab 沒動。
- **readiness probe 只是 ping**：informer 起不來時 pod 仍顯示 `1/1 Ready`，2 分鐘（`CacheSyncTimeout`）後才自己退出重啟；連待命、沒拿到 lease 的 replica 也是 Ready（實驗 2、4）。要知道 controller 有沒有在工作，得看 log 的 `forbidden` 或 `/metrics`。
- **Helm chart 是從 `config/` 產生的**：改了 marker 或 CRD 要手動重跑 `kubebuilder edit --plugins=helm/v2-alpha --force`，CI 沒有檢查兩者是否同步。
- **llama-server 的 pod 不符合 PSS `restricted`**（沒有 `runAsNonRoot`、`seccompProfile`…）：只能跑在沒有強制 restricted 的 namespace，e2e 因此把範例 CR 放在 `default`。
- **沒有 release pipeline**：image 只存在本機與 CI 的 kind 裡，沒有推到 registry、沒有版本 tag；`dist/install.yaml` 與 chart 的 image 是 `llm-serving-operator:v0.0.2`（Phase 4 時），別人裝不起來。
- **CI 的 e2e 每次從零 build controller image**：runner 沒有 Docker layer cache，BeforeSuite 約 3 分鐘（下載 `golang:1.26`、`go mod download`、編譯），佔 E2E Tests 9 分鐘裡最大的一塊；llama.cpp image＋模型在 runner 上只要約 30 秒。可以用 `docker/build-push-action` 的 GHA cache 改善，這個 lab 沒做。
- **leader failover 約 16 秒沒有人 reconcile**：沒有打開 `LeaderElectionReleaseOnCancel`，舊 leader 被刪時不會主動釋放 lease（實驗 4）。

## Phase 4 的邊界

- **controller 端沒有撞名檢查**：webhook 有 TOCTOU 空窗（webhook 檢查之後才建立的 Service、informer cache 還沒看到的 Service），`ENABLE_WEBHOOKS=false make run` 時也沒有 webhook。這些情況下 controller 的 SSA（`ForceOwnership`）仍會接手別人的 Service，刪 md 時 GC 一起刪掉（實驗 1）。根本的修法是 controller apply 前檢查 ownerReference，不是自己的就不碰、標成 Degraded。
- **webhook 是 `failurePolicy: Fail`**：operator 停機期間不能建立 md，使用者看到的是 `InternalError`（實驗 4）。只註冊 `CREATE`，所以 UPDATE、DELETE 不受影響。
- **readiness probe 沒有檢查 webhook server**：新 pod 可能在 webhook server 開始 listen 前就 Ready、被加進 endpoint。可用 `mgr.GetWebhookServer().StartedChecker()` 當 readyz check。
- **CEL 規則只檢查 cpu、memory**：迭代整個 `ResourceList` 會超過 CRD 的 cost 預算（`docs/05` Part 2.2），`nvidia.com/gpu` 這類擴充資源不檢查（Deployment 那一層仍會擋）。
- **自簽 Issuer**：沒有接真正的 CA；憑證更新（90 天簽發、到期前 30 天更新）沒有實際等到、驗證過。
- **`metrics-certs` 憑證有建立但沒有使用**：metrics server 仍用 controller-runtime 自己產生的自簽憑證（沒有打開 `[METRICS-WITH-CERTS]`）。
- **`Chart.yaml` 的版本沒有跟著 image 升級**：`version: 0.1.0`、`appVersion` 都沒動，`helm history` 分不出 v0.0.1 和 v0.0.2。
- **`test-chart.yml` 裝 cert-manager 沒有釘版本**：CI 實際裝到 v1.21.3，e2e 固定 v1.21.1，兩個 workflow 已經在用不同版本。
- **PVC 名稱規則（`<name>-models`）有兩份**：webhook 和 controller 的 `pvcName()` 各寫一次，改一邊要記得改另一邊。

## Phase 5 的邊界

- **換 adapter 的滾動更新要兩倍記憶體**：新舊 pod 會同時存在（`maxSurge` 1 個），operator 沒有 `strategy: Recreate` 欄位；節點只放得下一個 pod 時，新 pod 會一直 Pending。
- **operator 降版會默默拿掉 LoRA**：舊版 controller 不認得 `spec.adapter`，SSA 送出的 Deployment 就沒有 `--lora`、沒有 `fetch-adapter`，md 卻仍回報 `Available=True`，MAE 回到 $200 上下（`docs/06` 7.5）。正式做法是讓 CRD 欄位搭配 controller 最低版本，或在 status 寫出 controller 處理過哪些欄位。
- **換 adapter 版本一定要換檔名**：只改 URL 不改 `file`，快取裡的舊檔會被當成「已經存在」；也沒有 checksum 驗證檔案內容。
- **`--cache-ram 0`、`--parallel 4` 寫死在 operator 裡**：所有 md 共用，operator 一升級就全部滾動重啟（`docs/06` 6.1）；沒有 `extraArgs` 之類的逃生口。
- **只支援一個 adapter**：llama.cpp 可以掛多個 `--lora`，vLLM 可以依 request 選 adapter，這裡都沒做。
- **`cacheSize` 在 kind 上只是宣告**：local-path provisioner 不限制容量（1Gi 的 PVC 放得下 3.2 GB）；`self == oldSelf` 比的是原始值，`5Gi` 改成 `5120Mi` 也會被拒。
- **MAE 驗收是手動的**：沒有進 CI（runner 下載 4 GB 不實際）；跟 k8s-lab 差 $0.17～$0.24，剩下的差別（llama.cpp 版本沒有釘、並發數不同）沒有在同一個環境對照。
- **Secret 的錯誤只能事後從 pod status 發現**：controller 刻意不讀 Secret（`docs/06` 1.2 的取捨），要等 base 下載完、`fetch-adapter` 要啟動時才知道 Secret 不存在（實驗 1 等了約 22 分鐘）。
- **冷啟動很慢**：base 3.2 GB＋adapter 742 MB 依序下載，實測 22 分 21 秒＋4 分 53 秒；init container 依序執行，不能平行下載。

