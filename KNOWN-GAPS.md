# KNOWN GAPS

> 這個 lab 刻意沒做、或做了但有邊界的地方。面試時主動講清楚範圍，比被問到才承認好。

## Phase 1 的邊界

- **controller 在本機跑（`make run`）**：用的是開發者的 kubeconfig，RBAC 寫錯也不會發現。Phase 3 部署進叢集、用 `kubectl auth can-i` 驗證後才算數。
- **只有一種 runtime（llama.cpp，CPU）**：沒有 GPU、沒有 vLLM。GPU 上的推論調校在姊妹專案 `llm-inference-k8s-lab`。
- **沒有 HPA、沒有 canary、沒有縮到零**：這些是 KServe 的範圍，見 `docs/00-concepts.md` Part 8。
- **單一 namespace 的子資源**：沒有跨 namespace 或叢集外資源，所以不需要 finalizer（Phase 2 實驗 8 驗證過理由）。
- **envtest 不涵蓋的行為**（GC、Deployment 真的 Ready）只在 kind 上手動驗證，沒有自動化 e2e 測試。

## Phase 2 的邊界

- **PVC 是 ReadWriteOnce**：只能被同一個節點上的 pod 掛載。kind 是單節點，所以 2 個 replica 能共用快取（實驗 5）；多節點叢集上，排到別的節點的 pod 會掛載失敗。正式做法是 RWX 儲存（NFS、EFS…）或節點本地的模型快取。
- **PVC 容量寫死 1Gi**：沒有 spec 欄位可以調整；換模型後舊的模型檔不會被清掉，會一直佔空間。
- **PVC 跟著 md 一起被 GC 刪掉**：刪 md 或 PVC 被誤刪，快取就沒了，下一個 pod 要重新下載（實驗 8a、8b 約 70 秒）。
- **`Watches(&corev1.Pod{})` 會 cache 叢集內所有 pod**：大叢集上記憶體可觀。可以在 `cmd/main.go` 用 `cache.Options.ByObject` 只 cache 帶 `managed-by=llm-serving-operator` label 的 pod，這個 lab 沒做。
- **Degraded 只認得一種失敗**：init container `fetch-model` 非零結束。llama-server 載入失敗、OOMKilled、image 拉不下來，都還只會顯示 `Progressing`。
- **status 的 409 會以 `ERROR Reconciler error` 出現在 log**：插曲 3 加回樂觀鎖後，狀態轉變時偶爾會有一次預期中的 409（實驗 6 有 1 次）。沒有特別處理成安靜重試。
- **envtest 沒有 cache**：測試用的 client 直接打 API server，cache 落後造成的行為只能用 `staleReader` 手工模擬（插曲 3），或在 kind 上觀察。
- **PVC 建立後第一次 re-apply 會多寫一次**：envtest 實測內容完全相同、只有 resourceVersion 變，之後就穩定。原因沒有查明。
