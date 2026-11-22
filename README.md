# LLM Serving Operator — 用 Go 寫一個管理模型推論服務的 Kubernetes Operator

宣告一個 `ModelDeployment`，operator 負責把它變成真的在跑的推論服務（Deployment + Service），
並持續把實際狀態拉回宣告的狀態、把結果寫回 `status`。

> **這是一個學習實驗室**，不是生產系統。已知的邊界誠實列在 [`KNOWN-GAPS.md`](KNOWN-GAPS.md)。
> 姊妹專案：[`llm-inference-k8s-lab`](https://github.com/Jeremymymy/llm-inference-k8s-lab) —— 那邊是「手寫 manifest 把推論服務做到可維運」，這邊是「把那套知識寫成 controller」。

---

## 這個專案在解什麼問題

`llm-inference-k8s-lab` 裡，一個推論服務要手寫 PVC、Deployment、Service、probe、HPA，換模型要改好幾個檔案，
而且部署完之後**沒有任何東西持續確認它還是對的** —— 有人手動 `kubectl scale`、誤刪 Deployment，都只能靠人發現。

| 手寫 manifest（k8s-lab） | Operator（這個 repo） |
|---|---|
| 換模型改好幾個檔案 | 改 CR 的一個欄位 |
| `kubectl apply` 一次就結束 | controller 持續 reconcile，偏離就拉回 |
| 狀態要自己 `kubectl get` 好幾種資源拼湊 | `kubectl get md` 一行看到 Ready 數、Available、endpoint |
| 欄位填錯要等 pod 起不來才知道 | API server 在寫入當下就拒絕（OpenAPI schema / CEL / webhook） |
| 權限就是你的 kubeconfig | controller 只拿得到 RBAC marker 宣告的最小權限 |

這也是 KServe `InferenceService` 在做的事的極簡版。做過一次，才講得清楚 KServe 多做了什麼（見 `docs/00-concepts.md` Part 8）。

---

## 架構

```
  kubectl apply                 ┌─────────────────────────────────────────────┐
  ModelDeployment ──────────►   │ API server（CRD 驗證：schema / CEL / webhook）│
                                └───────────────┬─────────────────────────────┘
                                                │ watch（informer cache）
                                                ▼
                               ┌──────────────────────────────────┐
                               │ llm-serving-operator             │
                               │  Reconcile(ns/name):             │
                               │   1. 讀 ModelDeployment          │
                               │   2. CreateOrUpdate Deployment ──┼──► Pod: init 下載 GGUF
                               │   3. CreateOrUpdate Service      │         → llama-server
                               │   4. 寫回 status / conditions    │
                               └──────────────────────────────────┘
                 Owns(Deployment, Service)：子資源被改或被刪 → 觸發同一個 Reconcile
```

---

## 階段

| 階段 | 文件 | 內容 | 狀態 |
|---|---|---|---|
| 概念 | [`docs/00-concepts.md`](docs/00-concepts.md) | operator 模式、reconcile、ownerReference、status、RBAC、與 KServe 的關係 | 📖 |
| 環境 | [`docs/01-setup.md`](docs/01-setup.md) | Go、kubebuilder、kind、scaffold 專案 | ✅ |
| Phase 1 | [`docs/02-phase1-crd-and-reconcile.md`](docs/02-phase1-crd-and-reconcile.md) | CRD 型別與驗證、Deployment/Service reconcile、status、envtest、在 kind 上跑真模型 | ✅ |
| Phase 2 | [`docs/03-phase2-trustworthy-controller.md`](docs/03-phase2-trustworthy-controller.md) | controller 的可信度：Server-Side Apply、status 樂觀鎖與 predicate、PVC 模型快取、Degraded condition 與 Events、finalizer 的效果與風險（實驗） | ✅ |
| Phase 3 | [`docs/04-phase3-in-cluster.md`](docs/04-phase3-in-cluster.md) | 上叢集：多階段 Dockerfile、RBAC 最小權限驗證、`make deploy`、Helm chart、GitHub Actions | ✅ |
| Phase 4 | [`docs/05-phase4-admission.md`](docs/05-phase4-admission.md) | Admission：CEL 驗證 vs validating webhook（cert-manager）、validation ratcheting、webhook 停機與 HA 的行為 | ✅ |
| Phase 5 | （選做） | 接上 k8s-lab 的 pricer 模型（LoRA adapter），用 MAE 驗收（不做 HPA） | ⬜ |

---

## 成果

> 做到哪裡就填到哪裡。**沒量過的不要填。** 完整紀錄在 [`RESULTS.md`](RESULTS.md)。

環境：kind v0.33.0（Kubernetes v1.37.0）、kubebuilder v4.16.0、controller-runtime v0.25.0，MacBook Air（darwin/arm64，只用 CPU）。

| 指標 | Phase 1 | Phase 2 |
|---|---|---|
| 沒有變更時，controller 對 Deployment 的寫入 | 每輪送一次 Update | 不寫入（Server-Side Apply） |
| 手動 `kubectl scale` 時的 409 Conflict | 1 次 | 0 次 |
| controller 停機重啟時，子資源的 409 | 3 次 | 0 次 |
| Deployment 被誤刪後恢復 | 約 61 秒 | 約 6 秒（PVC 模型快取） |
| 擴容 1→2 的 Ready 時間 | 約 61 秒 | 約 6 秒 |
| 模型 URL 錯誤時的回報 | `Progressing` | `Degraded / ModelDownloadFailed` + Warning Event |
| envtest 測試／`internal/controller` 覆蓋率 | 9 個／81.2% | 16 個／83.0% |

- 錯誤的欄位（`file: ../x.gguf`、`url: http://...`）在 controller 沒有執行時，仍被 API server 當場拒絕（Phase 1 實驗 1）
- 刪除 CR 時，子資源由 garbage collector 依 ownerReference 清掉，controller 沒有任何刪除邏輯（Phase 1 實驗 6）
- Phase 2 過程中，靠新加的 Events 抓到 controller 自己的三個缺陷，各自先寫回歸測試再修正（`docs/03` 的插曲 1～3）
- `make undeploy` 會刪掉 CRD，連帶刪光所有 md 與模型快取；Helm chart 的 CRD 帶 `resource-policy: keep`，`helm uninstall` 後 md 與子資源都還在、Deployment 仍是 1/1（Phase 3 實驗 6）

**Phase 3：controller 住進叢集**

| 指標 | 結果 |
|---|---|
| controller image（distroless、非 root、arm64） | 31.7 MB（節點上 `crictl images`） |
| RBAC | 7 條 rule，縮減時拿掉 10 個 verb（spec 不可寫、沒有 `update`／`delete`、讀不到 Secret），`kubectl auth can-i` 12 項與預期一致，叢集內 log 的 `forbidden` 為 0 |
| leader failover（刪掉 leader pod 到新 leader 接手） | 16 秒 |
| 少給 `pods` 權限時，pod 顯示 Ready 但 controller 沒在工作 | 每輪約 2 分 16 秒，之後才 CrashLoopBackOff |
| controller 記憶體（`/metrics`） | 約 29 MiB（limit 128Mi） |
| e2e（4 個測試，含真的拉 llama.cpp、等 Available、驗證 GC） | 本機 3 分 3 秒；GitHub Actions 9 分 0 秒 |

**Phase 4：Admission，CEL 與 validating webhook**

| 指標 | 結果 |
|---|---|
| 沒有規則時，`requests.memory: 2Gi`／`limits.memory: 1Gi` 的 md | `kubectl apply` 成功，status 空白，錯誤只在 Event |
| 加上 CEL 規則後 | `kubectl apply` 當下被拒；operator 停機時一樣有效 |
| 規則上線前就存在的違規物件（ratcheting） | 改 metadata、改不相關欄位：成功＋Warning；改 resources 仍違規：被拒 |
| 撞名的 md（沒有 webhook 時） | 搶走別人的 Service（selector 換掉、加上 ownerReference），沒有 Warning；刪 md 時 Service 被 GC 刪掉 |
| 撞名的 md（有 webhook 時） | `kubectl apply` 當下 `Forbidden`，Service 不變 |
| 同時違反 CEL 和 webhook | 只看到 CEL 的錯誤（schema 驗證先於 validating admission） |
| operator 停機時建立 md | `InternalError`（connection refused），`failurePolicy: Fail` 的代價 |
| webhook 延遲（讀 informer cache） | 平均約 10.7 毫秒（寫文件時 20 個請求實測） |
| envtest 測試／覆蓋率 | `internal/controller` 19 個／83.0%；`internal/webhook` 8 個／80.0% |
| e2e（7 個測試，含撞名、CA 注入） | GitHub Actions 4 分 33 秒 |

---

## Attribution

- Phase 1 的示範模型是 [SmolLM2-135M-Instruct](https://huggingface.co/HuggingFaceTB/SmolLM2-135M-Instruct)，GGUF 量化版來自 [bartowski/SmolLM2-135M-Instruct-GGUF](https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF)。選它只因為小（145 MB），本機 CPU 幾秒就載得起來。
- 專案骨架由 [Kubebuilder](https://book.kubebuilder.io/) 產生。
