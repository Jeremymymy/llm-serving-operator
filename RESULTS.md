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

## Phase 1：CRD 與 reconcile

### envtest

| 項目 | 結果 |
|---|---|
| 測試數量／通過數 | |
| `internal/controller` 覆蓋率 | |

### 實驗

| # | 實驗 | 指令重點 | 觀察到的結果 | 時間 |
|---|---|---|---|---|
| 1 | controller 停機時套用錯誤 CR | | 錯誤訊息原文貼在下方 | |
| 2 | 子資源與 ownerReference | | | |
| 3 | 誤刪 Deployment | | | Deployment 重建：／Available 恢復： |
| 4 | 手動 `kubectl scale` | | | |
| 5 | 改 CR replicas | | generation／observedGeneration： | |
| 6 | 刪 CR，GC 清子資源 | | | |
| 7 | 不改 spec 的 reconcile 是否寫入 | | log：／resourceVersion： | |
| 8 | controller 停機期間的變更 | | | |

**實驗 1 錯誤訊息原文**：

```
```

**推論請求回應**（`/v1/completions`）：

```
```

### 與預期不同

-
