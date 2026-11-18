# KNOWN GAPS

> 這個 lab 刻意沒做、或做了但有邊界的地方。面試時主動講清楚範圍，比被問到才承認好。

## Phase 1 的邊界

- **controller 在本機跑（`make run`）**：用的是開發者的 kubeconfig，RBAC 寫錯也不會發現。Phase 3 部署進叢集、用 `kubectl auth can-i` 驗證後才算數。
- **整個覆寫 PodSpec**：會讓每一輪 reconcile 都送出 Update（見 RESULTS 實驗 7），Phase 2 處理。
- **模型存在 emptyDir**：每個 pod、每次重建都重新下載。Phase 2 改成 PVC 快取。
- **只有一種 runtime（llama.cpp，CPU）**：沒有 GPU、沒有 vLLM。GPU 上的推論調校在姊妹專案 `llm-inference-k8s-lab`。
- **沒有 HPA、沒有 canary、沒有縮到零**：這些是 KServe 的範圍，見 `docs/00-concepts.md` Part 8。
- **單一 namespace 的子資源**：沒有跨 namespace 或叢集外資源，所以不需要 finalizer。
- **envtest 不涵蓋的行為**（GC、Deployment 真的 Ready）只在 kind 上手動驗證，沒有自動化 e2e 測試。
