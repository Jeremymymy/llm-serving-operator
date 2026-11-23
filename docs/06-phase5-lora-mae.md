# LLM Serving Operator 實驗室 — 06：Phase 5，把 k8s-lab 的 pricer 交給 operator（LoRA adapter 與 MAE 驗收）

> 成本：**$0**（HF 私有 repo 免費）。
> 產出：把 k8s-lab 手寫的 pricer 部署變成一個 ModelDeployment：`spec.adapter`（私有 repo + Secret 參照）、`spec.cacheSize`（CEL transition rule：建立後不能改）、一條跨欄位 CEL、`fetch-adapter` init container、Degraded 多認得兩種失敗、預設關掉 prompt cache、修掉一個在叢集上才踩到的 Available 誤判、chart v0.0.3，以及兩個實驗。驗收標準是 **MAE 跟手寫版一致**，不是 pod Ready。
> 先讀：`05` Part 2（CEL）、`03` 的 Degraded condition、k8s-lab `docs/02-phase0-local-k8s.md` Part 1（adapter 怎麼來的、MAE 怎麼判讀）。
> 預估時間：約 1 小時（其中 MAE 約 7 分鐘；冷啟動下載看網路，寫這份文件時快的一次 3 分鐘、慢的一次 28 分鐘）。
> 環境：OrbStack 的記憶體上限 16 GiB（`orb config show` 的 `memory_mib: 16384`）。

---

## 這一階段的目的

k8s-lab 把 QLoRA 微調的估價模型手寫成一組 manifest（`k8s/base/model-server.yaml`，含註解 216 行）：PVC、initContainer 下載 base、**adapter 用 `kubectl cp` 放進 PVC、initContainer 只檢查它在不在**、依實測冷啟動設的三種 probe、關掉 prompt cache 的 `--cache-ram 0`。每一行都是實測換來的維運知識，但它們只存在那份 YAML 裡，而且 `kubectl apply` 一次就結束了。

這一階段把**同一個 pricer** 交給 operator 部署：

| | k8s-lab：手寫 manifest | Phase 5：operator |
|---|---|---|
| 部署 pricer | 216 行 YAML，加上手動 `kubectl cp` adapter | 一個約 20 行的 ModelDeployment |
| adapter 怎麼來 | 人放進 PVC；PVC 被刪就要再放一次 | 宣告 URL 與 Secret 參照，operator 自己下載，PVC 重建時也會重新下載 |
| adapter 拿不到 | initContainer 失敗（大聲，但要去看 pod log 才知道為什麼） | md 的 `Degraded` 寫出原因（401、Secret 不存在…） |
| 維運知識（prompt cache、probe、模型快取） | 每份 manifest 各自記得 | 寫在 controller 裡，每個 md 自動套用 |
| 狀態 | 自己 `kubectl get` 好幾種資源拼湊 | `kubectl get md` 一行 |

**驗收標準是 MAE**：operator 產出的部署，行為要跟手寫的一樣。k8s-lab 搬上 k3s 時也是用 MAE 確認「k8s 這層沒有改變行為」（$40.79）。實測：

| 部署方式（llama.cpp Q8_0 + 同一個 LoRA） | MAE |
|---|---|
| k8s-lab：Mac 上直接跑 llama-server | $40.79 |
| k8s-lab：手寫 manifest，跑在 OrbStack k8s | $40.86 |
| **Phase 5：operator** | **$41.03**（兩次完全相同） |
| 對照：operator，不掛 adapter | $195.56 |

pod Ready 只能證明「服務起來了」，證明不了「operator 組出來的 Deployment 跟手寫的等價」—— 少一個 `--lora`，pod 照樣 Ready，只有 MAE 會從 $41 變成 $195。

operator 比手寫 manifest 多出來的價值，主要在 **status 會說實話**。做的過程中，在叢集上找到三個 status 說謊的情況，這一階段的重點就是它們：

| 情況 | md 原本的回報 | 這一階段之後 |
|---|---|---|
| 換 adapter 時新 pod 起不來，舊 pod（沒有 LoRA）還在服務 | `Available=True AllReplicasReady`，同時 `Degraded=True` | `Available=False`（插曲） |
| 下載 adapter 的 token 指向不存在的 Secret | `Progressing`（看起來在等） | `Degraded`、`secret "hf-token" not found`（實驗 1） |
| operator 降回舊版 | `Available=True`，但 LoRA 被默默拿掉 | 沒有修，列進 KNOWN-GAPS（Part 7.5） |

**八個部分：**

1. 概念：adapter 怎麼進叢集、token 不經過 controller
2. 準備 adapter（寫這份文件時已完成）
3. API：`spec.adapter`、`spec.cacheSize` 與兩條 CEL
4. controller，以及一個插曲
5. 測試
6. 部署 v0.0.3
7. 建立 pricer 並驗收（實驗 1、2），以及直接給結果的實測
8. 記錄與收工

---

## Part 1：概念

### 1.1 adapter 怎麼進叢集

base model 是公開的（QuantFactory 的 GGUF），用 Phase 1 的 init container 下載就好。adapter 不一樣：它是從 gated repo 的 safetensors 轉出來的 GGUF，**沒有公開 URL**。三種做法：

| 做法 | operator 要做什麼 | 問題 |
|---|---|---|
| 烤進 image | 什麼都不用 | 換 adapter 要重 build image；adapter 跟 runtime 的生命週期綁死 |
| `kubectl cp` 進 PVC（k8s-lab 的做法） | 只宣告檔名 | operator 沒有在「交付」adapter：PVC 被 GC（刪 md、誤刪 PVC）就要手動重放 |
| **私有 registry + token**（這個 lab） | 下載時帶 bearer token | 要處理 Secret |

這個 lab 用 HF 的私有 model repo 當 registry。正式環境換成內部的物件儲存（S3、MinIO、Artifactory）也是同一個模式：一個 URL 加一個 token。

### 1.2 token 不經過 controller

最直覺的寫法是 controller 讀 Secret、把 token 塞進 init container 的 env。這樣做的代價：

- controller 要有 `secrets` 的 `get`。Phase 3 特地把它拿掉了（「讀不到 Secret」是 RBAC 縮減的成果之一），而且 Secret 的權限沒辦法只給某一個 Secret（`resourceNames` 對 `list`／`watch` 無效），等於讀得到整個叢集的 Secret
- token 的明文會出現在 Deployment 的 pod template 裡，任何能 `get deployment` 的人都看得到

這個 lab 的做法是**只傳參照**：

```
md.spec.adapter.tokenSecretRef: {name: hf-token, key: token}
      │ controller 原樣抄過去（不讀 Secret）
      ▼
Deployment: env TOKEN valueFrom.secretKeyRef {name: hf-token, key: token}
      │ kubelet 在啟動 init container 時才解析
      ▼
fetch-adapter 的環境變數 TOKEN=hf_...
```

- **RBAC 一個 verb 都不用加**：`make manifests` 之後 `config/rbac/role.yaml` 沒有任何變更（寫這份文件時實測）
- Deployment 裡只有 `secretKeyRef`，沒有 token（實測：`kubectl get deploy pricer -o yaml` 找不到 token 字串）

**代價**：controller 不知道 Secret 存不存在、內容對不對。它只能從 pod status 回頭推論 —— 實驗 1 就是在看這件事。

### 1.3 驗收標準是 MAE

| 你看到的 MAE | 代表什麼 |
|---|---|
| $40～$45 | ✅ LoRA 生效，跟手寫版一致（本文實測 $41.03；k8s-lab 手寫 manifest $40.86） |
| $200 上下 | ❌ LoRA 沒生效，跑的是 base model（本文實測 $195.56） |

---

## Part 2：準備 adapter（寫這份文件時已完成，不用重做）

這一段**已經做完了**，產物都在：

| 產物 | 位置 | 大小 |
|---|---|---|
| base model | `~/Repo/llm-inference-k8s-lab/models/base.gguf` | 3.26 GB（下載 4 分鐘） |
| adapter（safetensors，gated） | `~/Repo/llm-inference-k8s-lab/models/adapter/` | 1.56 GB（下載 1 分 37 秒） |
| adapter（GGUF f16） | `~/Repo/llm-inference-k8s-lab/models/pricer-lora.gguf` | 778 MB（轉檔 2 分鐘），sha256 `c21f79d9…bc66c` |
| 私有 repo | `https://huggingface.co/Jeremymymy/pricer-lora-gguf`（private） | 上傳 2 分 36 秒 |

步驟是 k8s-lab `docs/02` Part 1 的第二、三步，加上上傳。留在這裡當紀錄，**之後換 adapter 時**才需要再跑：

```bash
# 轉檔（llama.cpp 的 convert_lora_to_gguf.py；--base-model-id 只抓 config，不抓 base 的權重）
git clone --depth 1 https://github.com/ggml-org/llama.cpp /tmp/llama.cpp
LAB=~/Repo/llm-inference-k8s-lab
(cd /tmp/llama.cpp && HF_TOKEN="$(cat $LAB/private/hf-token)" \
  uv run --with torch --with transformers --with safetensors --with numpy --with sentencepiece --with gguf \
  python convert_lora_to_gguf.py --base-model-id meta-llama/Llama-3.2-3B --outtype f16 \
    --outfile $LAB/models/pricer-lora.gguf $LAB/models/adapter)

# 上傳到私有 repo：要用有 write 權限的 token
cd $LAB && uv run --with huggingface_hub python3 - <<'PY'
from huggingface_hub import HfApi
api = HfApi(token=open("private/hf-write-token").read().strip())
api.create_repo("Jeremymymy/pricer-lora-gguf", private=True, exist_ok=True)
api.upload_file(path_or_fileobj="models/pricer-lora.gguf", path_in_repo="pricer-lora.gguf",
                repo_id="Jeremymymy/pricer-lora-gguf")
PY
```

**兩個 token，兩種權限**：

- `private/hf-token`（read）：叢集裡下載用，Part 7 放進 Secret
- `private/hf-write-token`（write）：只在筆電上傳時用，**不進叢集**

寫這份文件時第一次上傳失敗，原因就是原本的 token 只有 read（`whoami` 回 `role: read`）。叢集只需要下載，給它 read token 就夠了 —— token 被洩漏時，對方只能讀、不能改你的 repo。更嚴格的做法是 HF 的 fine-grained token，只授權這一個 repo。

---

## Part 3：API

### 3.1 改 `api/v1alpha1/modeldeployment_types.go`

import 加上：

```go
	"k8s.io/apimachinery/pkg/api/resource"
```

在 `ModelDeploymentSpec` **上面**加一個型別，並在 `ModelDeploymentSpec` 的註解下面加一條 CEL：

```go
// AdapterSource 描述 LoRA adapter（GGUF）從哪裡來。
type AdapterSource struct {
	// URL 是 adapter GGUF 的下載位址。只接受 https。
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`

	// File 是存進 /models 底下的檔名，規則跟 model.file 一樣。
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+\.gguf$`
	File string `json:"file"`

	// TokenSecretRef 指向同一個 namespace 裡 Secret 的某個 key，內容是下載用的 bearer token（私有 repo 用）。
	// controller 不讀這個 Secret：它只把參照寫進 pod，由 kubelet 在啟動 init container 時解析。
	// +optional
	TokenSecretRef *corev1.SecretKeySelector `json:"tokenSecretRef,omitempty"`
}

// ModelDeploymentSpec 是使用者宣告的期望狀態。
// +kubebuilder:validation:XValidation:rule="!has(self.adapter) || self.adapter.file != self.model.file",message="adapter.file must differ from model.file"
type ModelDeploymentSpec struct {
```

`Model` 欄位後面加兩個欄位：

```go
	// Adapter 是套在 model 上的 LoRA adapter。不寫就只跑 base model。
	// +optional
	Adapter *AdapterSource `json:"adapter,omitempty"`

	// CacheSize 是模型快取 PVC 的容量，要放得下 model 加 adapter。建立後不能改。
	// +kubebuilder:default="1Gi"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cacheSize is immutable"
	// +optional
	CacheSize *resource.Quantity `json:"cacheSize,omitempty"`
```

```bash
make manifests generate
git diff --stat config/
```

**預期**：只有 CRD 有變更（多了 `adapter`、`cacheSize` 和兩條 `x-kubernetes-validations`），`config/rbac/role.yaml` 沒有變。

### 3.2 讀懂幾個設計決定

**`tokenSecretRef` 直接用 `corev1.SecretKeySelector`**

這是 Pod 的 `env[].valueFrom.secretKeyRef` 用的同一個型別，controller 可以原樣抄進 pod template。它還附帶 `optional` 欄位：設成 `true` 時 Secret 不存在也會啟動（拿不到 token → 401 → `AdapterDownloadFailed`），而不是卡在 `CreateContainerConfigError`。

**跨欄位 CEL：`adapter.file != model.file`**

兩個檔名一樣時，`fetch-adapter` 看到檔案已經存在就跳過（`adapter already present`），然後 llama-server 把 **base model 當成 LoRA** 載入。這要同時看兩個欄位，schema 做不到，所以 marker 放在 `ModelDeploymentSpec` 型別上，`self` 是整個 spec（`05` Part 2 是放在欄位上，`self` 是那個欄位）。

**`cacheSize` 的 `self == oldSelf`：transition rule**

`oldSelf` 是更新前的值，所以這條規則**只在 update 時執行**（create 時沒有 `oldSelf`）。為什麼要擋：

| | 沒有這條規則 | 有這條規則 |
|---|---|---|
| 改 `cacheSize` | `kubectl apply` 成功；controller apply PVC 時被拒（kind 的 StorageClass `allowVolumeExpansion: false`），錯誤只在 Event | `kubectl apply` 當下被拒 |

這就是 `05` Part 2.0 那個模式：**錯誤在寫入當下擋，比事後在 Event 裡找好**。

兩個細節：

- `self == oldSelf` 比的是**原始值**，`5Gi` 改成 `5120Mi` 也會被拒，雖然它們相等。要用 Quantity 語意比較得寫 `quantity(string(self)).compareTo(quantity(string(oldSelf))) == 0`。這個 lab 接受這個限制
- `+kubebuilder:default="1Gi"` 會讓**既有的** md（升級前建立的）在讀出來時被補上 `1Gi`，跟原本寫死的值一樣，所以升級不會改到既有的 PVC

> **`cacheSize` 在 kind 上其實只是宣告。** 寫這份文件時實測：用預設的 `1Gi` 建 md，3.2 GB 的 base model 照樣下載成功、llama-server 照樣起來（`df` 看到的是節點整顆磁碟 342G）。kind 的 local-path provisioner 不限制容量。在 EBS、Ceph 這類真的儲存上，1Gi 的 PVC 會在下載到一半時 `No space left on device`。kind 上「能跑」不代表設定是對的。

---

## Part 4：controller

### 4.1 改 `internal/controller/modeldeployment_controller.go`

**常數**：`fetchContainerName` 後面加一行：

```go
	fetchAdapterName     = "fetch-adapter"
```

**PVC 容量**：`desiredPVC` 裡的 `resource.MustParse(modelCacheSize)` 換成 `cacheSize(md)`，函式後面加：

```go
// cacheSize：CRD 的 default 會在寫入時補上 1Gi，這裡的備案只給沒經過 API server 的物件（單元測試）用。
func cacheSize(md *servingv1alpha1.ModelDeployment) resource.Quantity {
	if md.Spec.CacheSize == nil {
		return resource.MustParse(modelCacheSize)
	}
	return *md.Spec.CacheSize
}
```

**下載 adapter 的 script**：`fetchScript` 後面加：

```go
// fetchAdapterScript 跟 fetchScript 一樣先下載到暫存檔再 mv，多了選用的 bearer token。
// token 從 stdin 交給 curl（-K -），不放在命令列：命令列參數在節點上用 ps 看得到。
const fetchAdapterScript = `set -eu
if [ -f "/models/$ADAPTER_FILE" ]; then echo "adapter already present"; exit 0; fi
tmp="/models/$ADAPTER_FILE.partial.$POD_NAME"
trap 'rm -f "$tmp"' EXIT
if [ -n "${TOKEN:-}" ]; then
  printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | curl -fL --retry 3 --retry-delay 5 -K - -o "$tmp" "$ADAPTER_URL"
else
  curl -fL --retry 3 --retry-delay 5 -o "$tmp" "$ADAPTER_URL"
fi
mv "$tmp" "/models/$ADAPTER_FILE"
ls -lh /models`
```

**`desiredDeployment`**：把 init container 和 args 抽成變數，有 adapter 時各加一份。在 `modelsMount := ...` 那行後面加：

```go
	podName := corev1ac.EnvVar().WithName("POD_NAME").WithValueFrom(corev1ac.EnvVarSource().
		WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath("metadata.name")))

	initContainers := []*corev1ac.ContainerApplyConfiguration{corev1ac.Container().
		WithName(fetchContainerName).
		WithImage(modelFetcherImage).
		WithEnv(
			corev1ac.EnvVar().WithName("MODEL_URL").WithValue(md.Spec.Model.URL),
			corev1ac.EnvVar().WithName("MODEL_FILE").WithValue(md.Spec.Model.File),
			podName,
		).
		WithCommand("sh", "-c", fetchScript).
		WithVolumeMounts(modelsMount)}
	args := []string{
		"--model", modelsDir + "/" + md.Spec.Model.File,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(servingPort),
		"--ctx-size", "2048",
		// 4 個 slot 平分 2048：每個 512 token，估價的 prompt 約 150。跟 k8s-lab 一樣，MAE 才能直接比。
		"--parallel", "4",
		"--metrics",
		// prompt cache 預設最多 8 GiB、不會釋放；估價的 prompt 幾乎不重複，開著只會一路漲到 OOMKilled（k8s-lab 實測過，docs/06 4.2）。
		"--cache-ram", "0",
	}
	if a := md.Spec.Adapter; a != nil {
		env := []*corev1ac.EnvVarApplyConfiguration{
			corev1ac.EnvVar().WithName("ADAPTER_URL").WithValue(a.URL),
			corev1ac.EnvVar().WithName("ADAPTER_FILE").WithValue(a.File),
			podName,
		}
		if ref := a.TokenSecretRef; ref != nil {
			sel := corev1ac.SecretKeySelector().WithName(ref.Name).WithKey(ref.Key)
			if ref.Optional != nil {
				sel.WithOptional(*ref.Optional)
			}
			env = append(env, corev1ac.EnvVar().WithName("TOKEN").WithValueFrom(corev1ac.EnvVarSource().WithSecretKeyRef(sel)))
		}
		initContainers = append(initContainers, corev1ac.Container().
			WithName(fetchAdapterName).
			WithImage(modelFetcherImage).
			WithEnv(env...).
			WithCommand("sh", "-c", fetchAdapterScript).
			WithVolumeMounts(modelsMount))
		args = append(args, "--lora", modelsDir+"/"+a.File)
	}
```

然後把原本的 `WithInitContainers(corev1ac.Container(). ... WithVolumeMounts(modelsMount)).` 整段換成：

```go
					WithInitContainers(initContainers...).
```

原本的 `WithArgs( "--model", ... "--metrics", ).` 整段換成：

```go
						WithArgs(args...).
```

**Degraded 認得 adapter 的失敗**：`degradedCondition` 上面加：

```go
// downloadFailureReasons：哪個 init container 失敗，對應到哪個 Degraded reason。
var downloadFailureReasons = map[string]string{
	fetchContainerName: "ModelDownloadFailed",
	fetchAdapterName:   "AdapterDownloadFailed",
}
```

`degradedCondition` 迴圈裡的：

```go
			if s.Name != fetchContainerName {
				continue
			}
```

換成：

```go
			reason, ok := downloadFailureReasons[s.Name]
			if !ok {
				continue
			}
			// kubelet 解析不了 env（例如 tokenSecretRef 指向的 Secret 不存在）：container 根本沒啟動。
			// controller 不讀 Secret，只能從這裡知道。Secret 補上後 kubelet 會自己重試。
			if w := s.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
				c.Status, c.Reason = metav1.ConditionTrue, w.Reason
				c.Message = fmt.Sprintf("pod %s: %s: %s", p.Name, s.Name, w.Message)
				return c
			}
```

同一個函式最後的 `"ModelDownloadFailed"` 換成 `reason`，訊息裡的 `fetchContainerName` 換成 `s.Name`：

```go
				c.Status, c.Reason = metav1.ConditionTrue, reason
				c.Message = fmt.Sprintf("pod %s: %s exited with code %d (restarts: %d)", p.Name, s.Name, t.ExitCode, s.RestartCount)
```

**`availableCondition`**：要改兩處，程式碼在下面插曲的「**修法**」。插曲讀起來像故事，但那兩段程式碼要照著改；漏掉的話，Part 5 的回歸測試 `does not report Available while old pods are still serving a rollout` 會失敗（`Expected True to equal False`）。

### 4.2 讀懂幾個設計決定

- **adapter 用獨立的 init container，不併進 `fetch-model`**：失敗時 Degraded 能直接說是哪一個（`AdapterDownloadFailed` vs `ModelDownloadFailed`），token 也只會出現在需要它的那個 container 的環境變數裡
- **token 用 `curl -K -` 從 stdin 給**：寫成 `-H "Authorization: Bearer $TOKEN"` 的話，token 會出現在 curl 的命令列參數，節點上任何人 `ps aux` 都看得到。curl 跟著 HF 的 302 轉址到 CDN 時，不會把自訂的 `Authorization` header 帶到別的 host（實測下載成功：轉址後的 URL 本身帶簽章）
- **預設關掉 prompt cache（`--cache-ram 0`）**：k8s-lab 實測過，llama-server 預設用最多 8 GiB 快取 prompt、不會釋放，壓測期間記憶體從 4.3 漲到 6.5 GiB，把 8 GiB 的 VM 擠到 OOM；估價的 prompt 幾乎不重複，快取沒有收益。這是 workload 的領域知識，正好是 operator 該替使用者決定的事。實測關掉之後，跑完 200 筆 MAE，anon 從頭到尾維持在 4.26 GiB
  - **代價**：這改的是**所有** md 的 pod template，不只是有 adapter 的。operator 一升級，每個既有的 md 都會滾動重啟一次（6.1 的實測）。operator 把領域知識集中在一個地方、改一次全部生效；反過來說，**operator 升級不是只換 controller，可能會重啟叢集上所有的工作負載**。正式的 operator 會把這類變更寫進 release notes，或做成 opt-in 的欄位
- **`--parallel` 從 2 改成 4（跟 k8s-lab 一樣）**：這是 llama-server 同時處理幾個請求（slot 數）。`--ctx-size 2048` 是所有 slot 共用的總量，4 個 slot 各分到 512 token，估價的 prompt 約 150 token 加 5 個輸出，夠用。slot 數會影響同一批一起算的請求怎麼組合，浮點運算的順序跟著變，MAE 也會差一點；要跟 k8s-lab 的 $40.79 比，就要用同一個值。反正 `--cache-ram 0` 已經會讓 pod template 改變，這一條不會多一次重啟
- **init container 依序執行**：`fetch-model` 先跑完（3.2 GB，寫這份文件時 2 分 22 秒～22 分 46 秒，看 HF 當下的速度），`fetch-adapter` 才開始。實驗 1 的 Degraded 要等 base 下載完才會出現

### 插曲：md 說 `AllReplicasReady`，服務的卻是沒有 LoRA 的舊 pod（寫這份文件時實測）

寫這份文件時，對一個沒有 adapter、已經在服務的 pricer，patch 加上一個**下載不到**的 adapter（URL 指向不存在的檔案）。新 pod 的 `fetch-adapter` 一直 404，滾動更新卡住，舊 pod 繼續服務。md 是這樣回報的：

```
Available=True  AllReplicasReady: 1/1 replicas ready
Degraded=True   AdapterDownloadFailed: pod pricer-xxx: fetch-adapter exited with code 22 (restarts: 3)
```

**同時 Available 又 Degraded**，而且會一直維持下去。Deployment 的 status 是 `replicas: 2, updatedReplicas: 1, readyReplicas: 1`。原本的判斷是 `UpdatedReplicas >= desired && ReadyReplicas >= desired`：

- `updatedReplicas: 1` 是那個**起不來的新 pod**（新 ReplicaSet 的 pod，不管 ready 沒有都算）
- `readyReplicas: 1` 是那個**舊 pod**（沒有 LoRA）

兩個數字各自「夠」，湊起來就變成 True。Phase 2 寫的「Degraded 時把原因覆寫到 Available」也沒幫上忙：那段只在 Available 是 False 時才覆寫。

不只卡住時會錯：**正常的滾動更新也有一段窗口**。同樣用舊的判斷，把 adapter 加回一個 base-only 的 pricer（檔案都在快取裡），從新 pod 建立到舊 pod 退出的約 10 秒內，md 一直是 `True AllReplicasReady`，服務的卻是舊 pod。下載是冷的話，這段窗口就是下載加載入的時間。這時候去量 MAE，會量到 $195，然後以為 adapter 壞了。

**修法**：跟 `kubectl rollout status` 一樣，再要求舊 ReplicaSet 的 pod 都走了（`Replicas == UpdatedReplicas`）。在 `availableCondition` 裡，把 `case dep.Status.UpdatedReplicas >= desired && dep.Status.ReadyReplicas >= desired:` 換成：

```go
	// Replicas == UpdatedReplicas：舊 ReplicaSet 的 pod 都走了。少了這個條件，滾動更新卡住時
	// 「新 pod 算在 UpdatedReplicas、舊 pod 算在 ReadyReplicas」會湊成 True（docs/06 插曲）。
	case dep.Status.UpdatedReplicas >= desired && dep.Status.Replicas == dep.Status.UpdatedReplicas &&
		dep.Status.ReadyReplicas >= desired:
```

同一個函式 `default` 分支的 `c.Message` 也換掉，帶上 updated 的數字，不然 `Progressing: 1/1 replicas ready` 讀起來像沒事：

```go
		c.Message = fmt.Sprintf("%d/%d replicas ready, %d/%d on the latest spec",
			dep.Status.ReadyReplicas, desired, dep.Status.UpdatedReplicas, dep.Status.Replicas)
```

修好後：下載不到時是 `Available=False AdapterDownloadFailed`（Degraded 的原因覆寫上去了）；正常滾動更新的過程中是 `Available=False Progressing: 1/1 replicas ready, 1/2 on the latest spec`，舊 pod 退出後才變 True。回歸測試在 Part 5。

> 這個缺陷從 Phase 1 就在，一直沒被發現：之前每次滾動更新都很快（smollm 幾秒就 Ready），兩個數字「各自夠」的窗口只有幾秒。要一個起不來的新 pod，才會讓它變成持續的錯誤回報 —— 而 adapter 是第一個「會下載失敗、而且失敗時舊 pod 還在服務」的東西。

---

## Part 5：測試

`internal/controller/modeldeployment_controller_test.go`：

**1. 第一個測試（`creates owned PVC, Deployment and Service with defaults applied`）補幾行**，確認沒有 adapter 時一切照舊、`--cache-ram 0` 有加、預設容量是 1Gi。在 `ContainElement("/models/model.gguf")` 那行後面加：

```go
		Expect(dep.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("--lora"))
		Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElements("--cache-ram", "0"))
		Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
```

`ConsistOf(corev1.ReadWriteOnce)` 那行後面加：

```go
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
```

**2. 插曲的回歸測試**，放在 `// Phase 2 實驗 2 的回歸測試` 那個測試前面：

```go
	// docs/06 插曲的回歸測試：滾動更新卡住時，新 pod 算在 UpdatedReplicas、舊 pod 算在 ReadyReplicas，
	// 兩個數字都「夠」，但新的 spec 沒有任何一個 pod 在服務。
	It("does not report Available while old pods are still serving a rollout", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a.Status).To(Equal(metav1.ConditionFalse))
		Expect(a.Reason).To(Equal("Progressing"))
		Expect(a.Message).To(Equal("1/1 replicas ready, 1/2 on the latest spec"))
	})
```

**3. adapter 與 cacheSize**。把原本的 `It("reports Degraded when the model download fails", ...)` **整個換成**下面這段（原本的測試變成 DescribeTable 的第一筆，再加一筆 adapter）：

```go
	It("adds an adapter download and --lora, passing the token by Secret reference only", func() {
		md := newMD()
		md.Spec.Adapter = &servingv1alpha1.AdapterSource{
			URL:  "https://example.com/lora.gguf",
			File: "lora.gguf",
			TokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "hf-token"}, Key: "token",
			},
		}
		md.Spec.CacheSize = new(resource.MustParse("5Gi"))
		// 刻意不建立 Secret hf-token：controller 不讀它，reconcile 照樣成功。
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		spec := dep.Spec.Template.Spec
		Expect(spec.Containers[0].Args).To(ContainElements("--lora", "/models/lora.gguf"))
		Expect(spec.InitContainers).To(HaveLen(2))
		Expect(spec.InitContainers[1].Name).To(Equal("fetch-adapter"))
		Expect(spec.InitContainers[1].Env).To(ContainElement(corev1.EnvVar{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "hf-token"}, Key: "token",
			}},
		}))

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("5Gi"))
	})

	It("rejects a change to cacheSize", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		md.Spec.CacheSize = new(resource.MustParse("5Gi"))
		err := k8sClient.Update(ctx, md)
		Expect(err).To(MatchError(ContainSubstring("cacheSize is immutable")))
	})

	DescribeTable("reports Degraded when a download fails",
		func(initContainer, wantReason string) {
			md := newMD()
			Expect(k8sClient.Create(ctx, md)).To(Succeed())

			// 扮演 kubelet：做一個 init container 以 exit 22（curl -f 遇到 HTTP 4xx）結束的 pod。
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: ns, Labels: labelsFor(md)},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: initContainer, Image: modelFetcherImage}},
					Containers:     []corev1.Container{{Name: "llama-server", Image: defaultLlamaCppImage}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
				Name:         initContainer,
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
			Expect(d.Reason).To(Equal(wantReason))
			Expect(d.Message).To(ContainSubstring(initContainer + " exited with code 22"))
			a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
			Expect(a.Reason).To(Equal(wantReason))
			Expect(recorder.Events).To(Receive(HavePrefix("Warning " + wantReason)))
		},
		Entry("model", fetchContainerName, "ModelDownloadFailed"),
		Entry("adapter", fetchAdapterName, "AdapterDownloadFailed"),
	)

	// controller 不讀 Secret，所以只能從 pod status 知道 Secret 不存在（docs/06 實驗 1）。
	It("reports Degraded when kubelet cannot resolve the token Secret", func() {
		md := newMD()
		Expect(k8sClient.Create(ctx, md)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: ns, Labels: labelsFor(md)},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: fetchAdapterName, Image: modelFetcherImage}},
				Containers:     []corev1.Container{{Name: "llama-server", Image: defaultLlamaCppImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  fetchAdapterName,
			Image: modelFetcherImage,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: `secret "hf-token" not found`,
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		d := meta.FindStatusCondition(md.Status.Conditions, conditionDegraded)
		Expect(d.Status).To(Equal(metav1.ConditionTrue))
		Expect(d.Reason).To(Equal("CreateContainerConfigError"))
		Expect(d.Message).To(ContainSubstring(`fetch-adapter: secret "hf-token" not found`))
	})
```

**4. 跨欄位 CEL**：`DescribeTable("rejects invalid specs at the API server", ...)` 的 `Entry("negative replicas", ...)` 後面加：

```go
		Entry("adapter with the same file name as the model", func(md *servingv1alpha1.ModelDeployment) {
			md.Spec.Adapter = &servingv1alpha1.AdapterSource{URL: "https://example.com/a.gguf", File: md.Spec.Model.File}
		}),
```

- `new(resource.MustParse("5Gi"))`：Go 1.26 的 `new` 可以接值（`05` Part 3.5 說過）。寫成 `ptr.To(...)` 的話 lint 的 `modernize` 會報 2 個 issue（寫這份文件時實測）
- 「adds an adapter…」刻意**不建立** Secret：它證明 controller 不碰 Secret。如果有人改成「controller 先讀 Secret 檢查」，這個測試會因為 Secret 不存在而失敗（而且 envtest 是 admin 身份，RBAC 擋不住，只有這個測試擋得住）

```bash
make test
go test ./internal/controller/ -v 2>&1 | grep -E 'Ran|SUCCESS|FAIL'
make lint
```

**預期**（寫這份文件時實測）：`internal/controller` **25 個**全過（19 + 6），覆蓋率 **85.6%**；webhook 8 個、80.0% 不變；`make lint` `0 issues.`

> 寫這份文件時確認過回歸測試擋得住：插曲的測試在修 `availableCondition` 之前失敗、修之後通過；Secret 那個測試在加 `CreateContainerConfigError` 判斷之前失敗。

---

## Part 6：部署 v0.0.3

```bash
export IMG=llm-serving-operator:v0.0.3
make docker-build IMG=$IMG
kind load docker-image $IMG --name operator-lab
kubebuilder edit --plugins=helm/v2-alpha --force
git status --short
date +%T; make helm-deploy IMG=$IMG; date +%T
```

**預期**：`git status` 是 `M config/manager/kustomization.yaml`、`M dist/chart/templates/crd/...`、`M dist/chart/values.yaml`（tag `v0.0.3`）、`M dist/install.yaml`，加上你改的 Go 檔。**沒有** RBAC 或 webhook 的變更。`config/manager/kustomization.yaml` 跟 Phase 4 一樣，commit 前還原。

> **`helm-deploy` 回來之後馬上建 md，可能拿到 `connection refused`**：寫這份文件時實測，`rollout status` 顯示完成、立刻建 md，webhook 回 `failed calling webhook ... connect: connection refused`，等 5 秒再試就成功。這就是 `05` KNOWN-GAPS 的「readiness probe 沒有檢查 webhook server」。遇到就再送一次。

### 6.1 operator 升級會重啟既有的 md（寫這份文件時實測，不用重做）

在 Phase 4 版本上先建一個 smollm，再把 controller 換成這一版：

| | 升級前 | 升級後（40 秒內） |
|---|---|---|
| smollm Deployment 的 generation | 1 | **2** |
| ReplicaSet | `smollm-db99bc8c7`（1/1） | `smollm-6b5dc78df8`（1/1）、舊的 `DESIRED 0` |
| llama-server 的 args | `... --parallel 2 --metrics` | `... --parallel 4 --metrics --cache-ram 0` |

pod 重啟了，原因就是 4.2 說的：`--parallel 4`、`--cache-ram 0` 改到了**所有** md 的 pod template。對照 `05` Part 4.3：Phase 4 升級時，smollm 的 Deployment resourceVersion 完全沒變，因為那次 pod template 沒有被改到。

---

## Part 7：建立 pricer 並驗收

### 7.1 資源要怎麼填（寫這份文件時實測，不用重做）

| 量測（`/sys/fs/cgroup/memory.stat`） | 值 |
|---|---|
| 只有 base model，anon | 3.54 GiB |
| base + LoRA，啟動後 anon | 4.22 GiB |
| base + LoRA，連跑兩次 200 筆 MAE 之後（`--cache-ram 0`） | 4.26 GiB，幾乎沒有成長 |
| `memory.peak` | 剛好等於 limit（6Gi）：多出來的是 page cache，可回收，不會 OOM |

所以 pricer 用 `requests.memory: 5Gi`、`limits.memory: 6Gi`。

換 adapter 時新舊兩個 pod 會同時存在（滾動更新的 `maxSurge` 預設 25%，1 個 replica 時無條件進位成 1），節點要放得下兩份。寫這份文件時實測：兩個 pod 同時跑，沒有 OOM，13 秒換完（檔案都在 PVC 裡）。

### 實驗 1：Secret 還不存在時建立 md

**先建 md、後建 Secret**，看 operator 怎麼回報一個它看不到的東西（pricer 和 Secret 本來就都要建，只是調換順序）：

```bash
cat <<EOF | kubectl apply -f -
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: pricer
spec:
  model:
    url: https://huggingface.co/QuantFactory/Llama-3.2-3B-GGUF/resolve/main/Llama-3.2-3B.Q8_0.gguf
    file: Llama-3.2-3B.Q8_0.gguf
  adapter:
    url: https://huggingface.co/Jeremymymy/pricer-lora-gguf/resolve/main/pricer-lora.gguf
    file: pricer-lora.gguf
    tokenSecretRef: {name: hf-token, key: token}
  cacheSize: 5Gi
  resources:
    requests: {cpu: "2", memory: 5Gi}
    limits: {memory: 6Gi}
EOF
date +%T
kubectl get pod -l app.kubernetes.io/instance=pricer -w
```

**預期**：pod 先是 `Init:0/2`（`fetch-model` 在下載 base，幾分鐘到二十幾分鐘，看網路），base 下載完會短暫閃過 `Init:1/2`，接著變成 `Init:CreateContainerConfigError`。看到之後 Ctrl-C：

```bash
date +%T
kubectl get md pricer -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl events --for md/pricer | tail -3
```

**預期**（寫這份文件時實測）：

```
Available=False CreateContainerConfigError: pod pricer-xxx: fetch-adapter: secret "hf-token" not found
Degraded=True CreateContainerConfigError: pod pricer-xxx: fetch-adapter: secret "hf-token" not found
```

Event 有一筆 `Warning  CreateContainerConfigError`。沒有 4.1 加的那段判斷時，同一個狀態只會顯示 `Progressing`：使用者會一直等，等不到。

補上 Secret（**read** token），**不動 md**：

```bash
kubectl create secret generic hf-token --from-file=token=$HOME/Repo/llm-inference-k8s-lab/private/hf-token
date +%T
kubectl wait md/pricer --for=condition=Available --timeout=10m
date +%T
P=$(kubectl get pod -l app.kubernetes.io/instance=pricer -o name)
kubectl get $P -o jsonpath='{range .status.initContainerStatuses[*]}{.name} {.state.terminated.startedAt} → {.state.terminated.finishedAt}{"\n"}{end}'
kubectl logs $P -c llama-server | grep -c llama_adapter_lora_init_impl
```

**預期**：

- 不用重建 pod：kubelet 會自己重試。實測 Secret 建立後 **8～11 秒** `fetch-adapter` 就開始下載
- `fetch-adapter` 下載 742 MB：快的時候 35 秒，慢的時候 4 分 53 秒（兩次實測都是 4 分 53 秒，約 2.5 MB/s）。慢的時候加上 kubelet 重試和 llama-server 載入，從建 Secret 到 Available 約 5 分 10 秒，所以 `--timeout` 給 10 分鐘；給 5 分鐘會在 Available 前幾秒逾時（寫這份文件時實測踩到）
- llama-server 的 log 有很多行 `llama_adapter_lora_init_impl: lora for 'blk.N...' cannot use buft 'CPU_REPACK', fallback to CPU`：**這就是 adapter 有被載入的證據**（數字不是 0）。它同時說明了為什麼 LoRA 讓 CPU 推論變慢：LoRA 的張量用不了 repack 過的快速路徑

**為什麼這很重要**：controller 沒有 Secret 的權限，所以它無法在建立 Deployment **之前**檢查 Secret。這是 1.2 選擇的代價，Degraded 是補救：controller 看不到 Secret，但看得到 kubelet 對 Secret 的反應。

> **token 錯誤（而不是 Secret 不存在）的情況**，寫這份文件時也實測過（不用重做）：adapter 不帶 `tokenSecretRef` 時，`fetch-adapter` log 是 `curl: (22) The requested URL returned error: 401`，md 是 `Degraded / AdapterDownloadFailed: ... exited with code 22`。HF 對沒授權的私有 repo 回 401，curl 的 `-f` 讓它以 22 結束，跟 Phase 2 下載 base 失敗是同一條路徑。

### 實驗 2：MAE 驗收

```bash
kubectl port-forward svc/pricer 18080:8080 &
cd ~/Repo/llm-inference-k8s-lab
date +%T
uv run --with datasets eval/measure.py \
  --base-url http://localhost:18080 --model pricer --size 200 \
  --label "operator-llamacpp-q8-lora"
date +%T
kill %1
cd ~/Repo/llm-serving-operator
kubectl get $P -o jsonpath='{.status.containerStatuses[0].restartCount}{"\n"}'
```

**預期**（寫這份文件時在同一個 pod 上連跑兩次，你跑一次就好）：

| | 第一次 | 第二次 |
|---|---|---|
| 成功 | 200/200 | 200/200 |
| MAE | **$41.03** | **$41.03** |
| R² / Hit rate | 71.9% / 75.0% | 71.9% / 75.0% |
| p50 / p95 | 9,206 / 11,024 ms | 11,381 / 12,579 ms |
| 總耗時 | 6 分 9 秒（0.5 req/s） | 7 分 33 秒（0.4 req/s） |
| llama-server restartCount | 0 | 0 |

判讀：

- **$40～$45 = LoRA 生效**。對照：同一個 operator、同樣 `--parallel 4`，不掛 adapter 是 $195.56
- **兩次 MAE 逐位元相同**（k8s-lab 在 Mac 上也是 4 次都 $40.79）。寫這份文件時第一版用 `--parallel 2`，兩次是 $41.25、$41.34，差了 $0.09；為什麼 4 個 slot 時可以重現、2 個不行，沒有查證。所以不要把「逐位元相同」當成驗收條件，$40～$45 才是
- **跟 k8s-lab 手寫 manifest 的 $40.86 差 $0.17**（跟 Mac 上直接跑的 $40.79 差 $0.24）：模型、adapter、`--parallel 4`、`--ctx-size 2048`、`--cache-ram 0` 都一樣，剩下的差別是 llama.cpp 的版本（兩邊都用沒釘版本的 `:server` tag，拉取時間差了兩個月）和並發數（k8s-lab 那次是 `--workers 30` 的壓測）。差距在 0.5% 以內、能用這兩點解釋，就判定 operator 沒有改變行為
- **restartCount 一定要是 0**、成功一定要是 200/200：失敗的請求不算進 MAE，pod 中途重啟過的話，剩下的數字不可信
- **LoRA 的代價是延遲**：不掛 adapter 時 p50 5,094 ms、1.0 req/s，掛了變 9～11 秒、0.4～0.5 req/s（吞吐約一半）。就是上面那行 `fallback to CPU`

### 7.5 直接給結果的實測（不用重做）

**operator 降版會默默拿掉 LoRA**

寫這份文件時，把 scratch 叢集上的 controller 換回 Phase 4 的 image（md 和 CRD 都不動；當時的版本是 `--parallel 2`，下面的 args 是那一版的）：

```
Deployment pricer 的 args：  [... "--metrics"]                        ← 沒有 --lora、沒有 --cache-ram
init containers：            fetch-model                               ← 沒有 fetch-adapter
md pricer 的 spec.adapter：   pricer-lora.gguf                         ← 還在
md pricer 的 Available：      True  AllReplicasReady                   ← 一切正常
```

舊版 controller 的 Go struct 不認得 `spec.adapter`，讀出來就丟掉了；它用 SSA 送出「它所知道的完整 Deployment」，Deployment 上的 `--lora` 就被拿掉了。md 回報 `Available=True`，MAE 會回到 $200 上下。

這就是「服務正常、答案錯誤」在 operator 層級的版本。`make helm-rollback` 會遇到同樣的事（chart 的 CRD 也會一起回滾，細節沒有實測）。正式的做法：CRD 的新欄位要搭配 controller 的最低版本、或在 status 寫上「controller 處理過哪些欄位」讓使用者發現。列進 KNOWN-GAPS。

**兩條新的 CEL 在叢集上的錯誤訊息**（envtest 已經驗證過規則本身）：

```
$ kubectl patch md pricer --type=merge -p '{"spec":{"cacheSize":"10Gi"}}'
The ModelDeployment "pricer" is invalid: spec.cacheSize: Invalid value: "10Gi": cacheSize is immutable

$ kubectl apply --dry-run=server -f same-file.yaml     # model.file 和 adapter.file 都是 m.gguf
The ModelDeployment "same-file" is invalid: spec: Invalid value: adapter.file must differ from model.file
```

第二個的路徑是 `spec`：規則掛在 spec 型別上（`05` 的 resources 規則掛在欄位上，路徑是 `spec.resources`）。

**kind 的 PVC 不限制容量**：見 3.2 的說明（1Gi 的 PVC 放了 3.2 GB）。

---

## Part 8：記錄與收工

```bash
kubectl delete md pricer
kubectl delete secret hf-token
```

（刪 md 會連 PVC 一起刪，下次建 pricer 要重新下載 4 GB。想保留就先不要刪。）

e2e 不用改：CI 的 runner 下載 4 GB 的模型不實際，MAE 驗收留在本機。`git push` 之後確認四個 workflow 仍然是綠的（e2e 的 smollm 不受 `--cache-ram` 影響）。

1. RESULTS 新增 Phase 5 段落：
   - Part 3：`make manifests` 的 diff（RBAC 沒有變）
   - Part 5：`make test` 的測試數與覆蓋率、`make lint`
   - Part 6：`git status`、`helm-deploy` 時間
   - Part 7：實驗 1 的 condition 與時間軸、實驗 2 的 MAE 那一行
2. 「與預期不同」
3. 更新 `KNOWN-GAPS.md`，新增 Phase 5 的邊界：
   - **換 adapter 的滾動更新要兩倍記憶體**：operator 沒有 `strategy: Recreate` 欄位，節點只放得下一個 pod 時新 pod 會一直 Pending
   - **operator 降版會默默拿掉 LoRA**，md 仍回報 `Available=True`（7.5）
   - **換 adapter 版本一定要換檔名**：只改 URL 不改 `file`，快取裡的舊檔會被當成「已經存在」；也沒有 checksum 驗證檔案內容
   - **`--cache-ram 0`、`--parallel 4` 寫死在 operator 裡**：所有 md 共用，改了就全部重啟；沒有 `extraArgs` 之類的逃生口
   - **只支援一個 adapter**：llama.cpp 可以掛多個 `--lora`，vLLM 可以依 request 選 adapter，這裡都沒做
   - **`cacheSize` 在 kind 上只是宣告**：local-path 不限制容量；`self == oldSelf` 比的是原始值，`5Gi` 改成 `5120Mi` 也會被拒
   - **MAE 驗收是手動的**：沒有進 CI；跟 k8s-lab 差 $0.24（Metal vs 容器內 CPU，沒有在同一個環境對照）
   - **Secret 的錯誤只能事後從 pod status 發現**：controller 刻意不讀 Secret（1.2 的取捨）
4. README：
   - 階段表：Phase 5 改成 ✅
   - 環境那一行補上 OrbStack 記憶體上限 16 GiB
   - 「成果」加一個 Phase 5 的表格（MAE 有／沒有 adapter、冷啟動時間、記憶體、LoRA 的延遲代價）
5. commit

## 完成條件

- [ ] `spec.adapter`、`spec.cacheSize` 與兩條 CEL，`make manifests` 後 RBAC 沒有變
- [ ] controller 改完，`make test` 25 個通過、`make lint` 0 issues
- [ ] `make helm-deploy` 升級到 v0.0.3
- [ ] 實驗 1：Secret 不存在時 md 是 `Degraded / CreateContainerConfigError`；補上 Secret 後不動 md 就恢復
- [ ] 實驗 2：MAE 在 $40～$45，200/200，restartCount 0
- [ ] GitHub Actions 四個 workflow 都是綠的
- [ ] RESULTS、KNOWN-GAPS、README 已更新
