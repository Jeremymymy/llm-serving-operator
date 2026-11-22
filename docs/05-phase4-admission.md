# LLM Serving Operator 實驗室 — 05：Phase 4，Admission：CEL vs validating webhook

> 成本：**$0**。
> 產出：一條 CEL 規則（requests 不能大於 limits）、一個 validating webhook（擋下會搶走別人 Service 的 md）、cert-manager 管理的 webhook 憑證、chart 重新產生、一個新的 e2e 測試（外加修掉 scaffold 的 cert-manager 競態），以及四個實驗。
> 先讀：`02` 實驗 1（schema 驗證在 API server 層）、`03` Part 1.1（SSA 與 `ForceOwnership`）、`04` 實驗 2、4（readiness、leader election）。
> 預估時間：約 1.5 小時（不含等 CI）。

---

## 這一階段的目的

Phase 1 用 OpenAPI schema（`Pattern`、`Enum`、`Minimum`）擋下了「單一欄位」的錯誤。但有兩類錯誤 schema 擋不住，而且**現在的 controller 會讓它們造成真的傷害**：

| 錯誤 | 現在會發生什麼（寫這份文件時實測） | 誰擋得住 | 這一階段用什麼 |
|---|---|---|---|
| `requests.memory: 2Gi`、`limits.memory: 1Gi` | `kubectl apply` 成功；controller apply Deployment 時被拒，錯誤只出現在 Event；md 的 status **永遠是空的**；PVC 已經先建了 | 要**同時看兩個欄位** —— schema 做不到，CEL 可以 | CEL（`x-kubernetes-validations`） |
| md 取名 `chat`，而 namespace 裡已經有別人的 Service `chat` | controller 用 SSA `ForceOwnership` **把那個 Service 搶過來**：selector 換掉、加上 ownerReference，沒有任何錯誤或 Event。刪掉 md 時，**GC 把別人的 Service 一起刪了** | 要**讀叢集裡的其他物件** —— CEL 做不到，只有 webhook 可以 | validating webhook + cert-manager |

這一階段的重點不是「學會寫 webhook」，而是**知道什麼時候不該寫 webhook**：webhook 是你自己維運的一個 HTTPS 服務，它掛了，使用者就不能建 md。能用 CEL 解決的，就不要用 webhook。

**八個部分：**

1. 概念：一個寫入請求經過哪些關卡
2. CEL：requests 不能大於 limits
3. Validating webhook：擋下撞名（實驗 1）
4. cert-manager 與部署
5. 三個實驗（實驗 2～4），以及一個直接給結果的實測
6. e2e 測試
7. 對照表：CEL、webhook、ValidatingAdmissionPolicy、controller
8. 記錄與收工

---

## Part 1：概念

### 1.1 一個寫入請求經過哪些關卡

```
kubectl apply -f md.yaml
  │
  ├─ 認證、授權（RBAC）
  ├─ mutating admission        mutating webhook（這個 lab 沒有）
  ├─ schema 驗證                OpenAPI（type、pattern、enum、minimum）+ CEL（x-kubernetes-validations）
  ├─ validating admission      ValidatingAdmissionPolicy、validating webhook   ← Part 3 加在這裡
  ├─ 寫進 etcd                  到這裡 kubectl apply 才回傳成功
  │
  └─ controller watch 到 → Reconcile → apply 子資源（子資源的請求再走一次上面整條）
```

**越早擋下來越好**：在 etcd 之前擋下，使用者在 `kubectl apply` 當下就看到錯誤；過了 etcd，錯誤只能靠 controller 寫進 status 或 Event，使用者要自己去找。

實驗 3 會驗證這個順序：一個 md **同時**違反 CEL 和 webhook，使用者只會看到 CEL 的錯誤，webhook 根本沒被呼叫。

### 1.2 CEL 和 webhook 差在哪

| | CEL（寫在 CRD 裡） | validating webhook（你的 pod） |
|---|---|---|
| 看得到什麼 | **只有被寫入的這一個物件**（update 時還有 `oldSelf`） | 任何東西：可以讀叢集裡的其他物件、呼叫外部服務 |
| 誰執行 | API server 自己 | API server 透過 HTTPS 呼叫你的 Service |
| 需要什麼 | 什麼都不用 | TLS 憑證、CA 注入、Service、一直活著的 pod |
| controller 掛了 | **照樣有效**（Phase 1 實驗 1 已經看過，實驗 4 的實測再確認一次） | `failurePolicy: Fail` → **沒人能建 md**；`Ignore` → 默默放行 |
| 寫錯的代價 | `kubectl apply` CRD 時就被拒（cost 預算，見 2.2） | 執行期才出錯 |
| 測試 | envtest（真的 API server） | 單元測試 + envtest（要啟動 webhook server） |

**選擇的原則**：規則只跟這個物件本身有關 → CEL。規則需要知道「叢集裡還有什麼」→ webhook，並且接受它帶來的維運成本。

---

## Part 2：CEL，requests 不能大於 limits

### 2.0 沒有規則時，錯誤藏在哪裡（寫這份文件時實測，不用重做）

在還沒有 CEL 規則的叢集上，建立一個 `requests.memory: 2Gi`、`limits.memory: 1Gi` 的 md：

- `kubectl apply` 回傳 `created`，沒有任何警告
- `kubectl get md`：`READY`、`AVAILABLE` 都是**空白**；`.status` 是空的
- **沒有 Deployment，但有 PVC `greedy-models`**（`Pending`）
- Events：

```
15s (x2 over 15s)   Warning   ReconcileFailed   ModelDeployment/greedy   apply Deployment: Deployment.apps "greedy" is invalid:
spec.template.spec.containers[0].resources.requests: Invalid value: "2Gi": must be less than or equal to memory limit of 1Gi
```

要看懂的三件事：

1. **驗證其實有發生，只是發生在錯的地方**：Kubernetes 本來就會檢查 Pod 的 requests ≤ limits，但那是在 controller apply **Deployment** 的時候。對使用者來說，他寫入的 md 是「成功」的
2. **status 是空的**：`Reconcile` 在 apply Deployment 失敗時直接 `return r.fail(...)`，走不到寫 status 的那一段。使用者看 `kubectl get md` 只會覺得「怎麼一直沒反應」。錯誤原因只在 Event 裡（Phase 2 加 Events 的回報）
3. **副作用已經發生**：PVC 在 Deployment 之前 apply，所以已經建好了。一個錯誤的 md 在叢集裡留下了東西

**要做的只有一件事**：趁叢集上還沒有規則，建一個違規的 md 留給實驗 2（ratcheting）。規則上線之後，就再也建不出這種物件了：

```bash
cat <<EOF | kubectl apply -f -
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: greedy
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q8_0.gguf
    file: SmolLM2-135M-Instruct-Q8_0.gguf
  replicas: 0
  resources:
    requests: {memory: 2Gi}
    limits: {memory: 1Gi}
EOF
```

（`replicas: 0`：就算之後修好，也不會起 pod。）

### 2.1 為什麼 schema 擋不住，CEL 可以

OpenAPI schema 的每條規則只看**一個欄位**：`Pattern` 看一個字串，`Minimum` 看一個數字。「requests 不能大於 limits」要同時看兩個欄位，而且要懂 `1Gi == 1024Mi`、`2 > 1500m` —— 這是 Kubernetes `Quantity` 的語意，不是字串或數字比較。

CEL（Common Expression Language）是 API server 內建的運算式語言，寫在 CRD 的 `x-kubernetes-validations`。Kubernetes 替它加了 `quantity()` 這類函式庫，所以可以直接比較資源量。

### 2.2 先踩一次 cost 預算（寫這份文件時實測，不用重做）

最直覺的寫法是「requests 裡的每一個 key，如果 limits 也有，就比較」：

```
!has(self.requests) || !has(self.limits) ||
self.requests.all(k, !(k in self.limits) || quantity(string(self.requests[k])).compareTo(quantity(string(self.limits[k]))) <= 0)
```

`make manifests` 不會抱怨，但 `kubectl apply` CRD 時被 API server 拒絕：

```
The CustomResourceDefinition "modeldeployments.serving.llmlab.dev" is invalid:
* spec.validation.openAPIV3Schema.properties[spec].properties[resources].x-kubernetes-validations[0].rule:
  Forbidden: estimated rule cost exceeds budget by factor of more than 100x
  (try simplifying the rule, or adding maxItems, maxProperties, and maxLength where arrays, maps, and strings are declared)
```

**為什麼**：API server 在**接受 CRD 時**就估算每條規則「最壞情況」的執行成本，確保之後每次驗證都有上限、不會被一個超大的物件拖垮。`requests` 是 `corev1.ResourceList`（一個 map），schema 裡**沒有 `maxProperties`**，最壞情況就是請求大小上限內塞滿 key，每個 key 都要跑一次 `quantity()`。

**修法有兩種**：

- 對 map 加 `maxProperties`：但 `ResourceRequirements` 是 `corev1` 的型別，我們不能在它的欄位上加 marker
- **不迭代未知大小的 map，只檢查固定的 key**：`['cpu', 'memory'].all(...)` 迭代的是一個長度 2 的常數 list，成本有上限

這個 lab 用第二種。代價是 `nvidia.com/gpu` 這類擴充資源不檢查（Kubernetes 對擴充資源本來就要求 requests == limits，Deployment 那一層一樣會擋），列進 KNOWN-GAPS。

> **面試可以講的點**：CEL 的成本是在**寫 CRD 的時候**就被檢查的，不是執行期。這讓 CEL 能安全地跑在 API server 裡；webhook 沒有這種保證，一個慢的 webhook 會拖慢每一個請求（所以有 `timeoutSeconds`）。

### 2.3 改 `api/v1alpha1/modeldeployment_types.go`

在 `Resources` 欄位加一行 marker：

```go
	// Resources 覆蓋推論容器預設的 requests/limits。
	// +kubebuilder:validation:XValidation:rule="!has(self.requests) || !has(self.limits) || ['cpu', 'memory'].all(r, !(r in self.requests) || !(r in self.limits) || quantity(string(self.requests[r])).compareTo(quantity(string(self.limits[r]))) <= 0)",message="resources.requests must not exceed resources.limits (cpu, memory)"
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
```

逐段讀：

| 片段 | 意思 |
|---|---|
| `!has(self.requests) \|\| !has(self.limits)` | 只寫了其中一邊就不用比（只寫 limits 時，Kubernetes 會讓 requests 等於 limits） |
| `['cpu', 'memory'].all(r, ...)` | 對這兩個 key 都要成立 |
| `!(r in self.requests) \|\| !(r in self.limits)` | 這個資源只在一邊出現，不用比 |
| `string(self.requests[r])` | ResourceList 的值在 schema 裡是 `int-or-string`：`cpu: 2` 是整數、`cpu: 1500m` 是字串。`quantity()` 只吃字串，所以先轉 |
| `quantity(...).compareTo(...) <= 0` | 用 Quantity 的語意比較，`1024Mi` 等於 `1Gi` |

marker 放在**欄位上**，`self` 就是這個欄位（`spec.resources`），錯誤訊息的路徑也會是 `spec.resources`。

```bash
make manifests
git diff config/crd/bases/
```

**預期**：CRD 的 `resources` 底下多了 `x-kubernetes-validations`，一條 rule、一條 message。

### 2.4 envtest 測試

envtest 跑的是**真的 API server**，所以 CEL 規則可以直接測，不需要 controller。`internal/controller/modeldeployment_controller_test.go` 已經有一個 `DescribeTable("rejects invalid specs at the API server", ...)`（Phase 1 的 pattern 測試），往裡面加兩筆，再加一個「應該接受」的測試。

import 加上：

```go
	"k8s.io/apimachinery/pkg/api/resource"
```

`Entry("negative replicas", ...)` 後面加：

```go
		Entry("memory request above limit", func(md *servingv1alpha1.ModelDeployment) {
			md.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			}
		}),
		Entry("cpu request above limit, in different units", func(md *servingv1alpha1.ModelDeployment) {
			md.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			}
		}),
	)

	It("accepts a request equal to its limit even when the units differ", func() {
		md := newMD()
		md.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1024Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
	})
```

（原本 `DescribeTable` 結尾的 `)` 換成上面這段，`})` 不要動。）

- 第二筆用 `2` 對 `1500m`：同時驗證 `int-or-string` 的整數形式和不同單位
- 「應該接受」那個測試擋的是**規則寫得太嚴**：如果有人把 `quantity()` 換成字串比較，`"1024Mi" > "1Gi"` 會誤判。它在沒有規則時也會過，這是正常的

```bash
make test
```

**預期**：`internal/controller` 19 個測試全過（16 + 3），覆蓋率 83.0% 不變（CEL 不是 Go 程式碼，不算覆蓋率）。

> 寫這份文件時實測：把 marker 拿掉、重跑 `make manifests` 再測，兩個 `Entry` 失敗、「接受」那個通過。

**先不要部署**：CRD 會在 Part 4 跟 webhook 一起用 `make helm-deploy` 升級。

---

## Part 3：Validating webhook，擋下撞名

### 實驗 1：md 會搶走別人的 Service

controller 依 md 的名字建立子資源：Deployment `<name>`、Service `<name>`、PVC `<name>-models`。如果同名的物件**已經存在、而且不是我們的**，會怎樣？

一樣在**還沒改程式**的叢集上做：

```bash
kubectl create service clusterip chat --tcp=80:8000        # 扮演「別的團隊的 Service」
kubectl get svc chat -o jsonpath='{.spec.selector}{"  "}{.spec.ports}{"\n"}'

cat <<EOF | kubectl apply -f -
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: chat
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q8_0.gguf
    file: SmolLM2-135M-Instruct-Q8_0.gguf
EOF
sleep 6
kubectl events --for md/chat
kubectl get svc chat -o jsonpath='{.spec.selector}{"\n"}{.spec.ports}{"\n"}{.metadata.ownerReferences}{"\n"}'
```

**預期**（寫這份文件時實測）：

- Events：`No events found` —— **沒有任何錯誤**。模型下載完、pod Ready 之後（實測約 20 秒內）再查，只會多一個 `Normal  Available  1/1 replicas ready`：從 md 的角度看，一切正常
- selector 從 `{"app":"chat"}` 變成 md 的三個 label：別的團隊的流量現在會被導到 llama-server
- ports 從一個變成兩個：原本的 `80-8000` 加上 `http 8080`（ports 是以 port+protocol 為 key 的 list，SSA 會合併）
- ownerReferences：`ModelDeployment/chat`，`controller: true`

刪掉 md：

```bash
kubectl delete md chat
sleep 3
kubectl get svc chat
```

**預期**：`Error from server (NotFound): services "chat" not found` —— **GC 依 ownerReference 把別人的 Service 刪了。**

**為什麼會這樣**：`03` Part 1.1 選了 `client.ForceOwnership`，理由是「這些欄位本來就歸 controller 管，有人手動改了要搶回來」。這個假設在**物件是我們建的**時候成立；物件不是我們建的，同一個選項就變成「搶走別人的物件」。selector 是 atomic map，整個被換掉；ownerReference 一加上去，這個 Service 的生死就綁在 md 上了。

> **Deployment 撞名比較幸運**：實測先建一個 `kubectl create deployment web --image=nginx:alpine`，再建 md `web`，controller apply 時被拒：`spec.selector: Invalid value: ...: field is immutable`。Deployment 的 selector 不能改，意外擋住了接管。Service 的 selector 可以改，所以沒有這層保護。

### 3.1 為什麼這要用 webhook

這條規則是「**叢集裡**有沒有同名、不屬於我的 Service」。CEL 只看得到被寫入的那個 md，看不到其他物件 —— 這正是 webhook 存在的理由。

> **webhook 不是唯一的防線，也不是最根本的修法。** 根本的修法在 controller：apply 之前先檢查物件的 ownerReference，不是自己的就不碰、把 md 標成 Degraded。webhook 的價值是**在使用者 `kubectl apply` 的當下就回報錯誤**，而不是讓他事後在 status 裡找原因。webhook 有一個擋不住的空窗（3.4 的 TOCTOU），所以正式的做法是兩層都做；這個 lab 只做 webhook，controller 端列進 KNOWN-GAPS。

### 3.2 scaffold

```bash
kubebuilder create webhook --group serving --version v1alpha1 --kind ModelDeployment --programmatic-validation
git status
```

**預期**（寫這份文件時在 scratch 副本實測，kubebuilder v4.16.0）：

```
 M PROJECT                                   # webhooks: validation: true
 M cmd/main.go                               # 註冊 webhook（ENABLE_WEBHOOKS != "false" 時）
 M config/default/kustomization.yaml         # [WEBHOOK]、[CERTMANAGER] 的區塊被自動取消註解
 M config/network-policy/kustomization.yaml
 M test/e2e/e2e_test.go                      # 等待 webhook endpoint、檢查 CA 注入的測試
?? config/certmanager/                       # Issuer、Certificate（webhook 和 metrics 各一張）
?? config/default/manager_webhook_patch.yaml # 掛載憑證 Secret、加 --webhook-cert-path、開 9443 port
?? config/network-policy/allow-webhook-traffic.yaml
?? config/webhook/                           # webhook 的 Service
?? internal/webhook/
```

- 舊版 kubebuilder 要手動取消 `config/default/kustomization.yaml` 裡的註解，v4.16 會自動做。`git diff config/default/kustomization.yaml` 看一下它打開了什麼：`../webhook`、`../certmanager`、`manager_webhook_patch.yaml`，以及把 Certificate 的 DNS 名稱、`cert-manager.io/inject-ca-from` annotation 串起來的 `replacements`
- `--programmatic-validation` 只產生 validating webhook。`--defaulting` 會再產生 mutating webhook，我們的預設值用 `+kubebuilder:default` 就夠了，不需要

### 3.3 改 `internal/webhook/v1alpha1/modeldeployment_webhook.go`

整個檔案換成（保留檔頭的 license 註解）：

```go
package v1alpha1

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

// SetupModelDeploymentWebhookWithManager registers the webhook for ModelDeployment in the manager.
func SetupModelDeploymentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &servingv1alpha1.ModelDeployment{}).
		// mgr.GetClient() 讀的是 controller 本來就有的 informer cache：不用新的 RBAC，也不會每個請求都打一次 API server。
		WithValidator(&ModelDeploymentValidator{Reader: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-serving-llmlab-dev-v1alpha1-modeldeployment,mutating=false,failurePolicy=fail,sideEffects=None,groups=serving.llmlab.dev,resources=modeldeployments,verbs=create,versions=v1alpha1,name=vmodeldeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// ModelDeploymentValidator 在建立 md 時檢查：controller 要用的子資源名稱，是不是已經被別人用掉了。
// 這需要讀叢集裡的其他物件，CEL 做不到（CEL 只看得到被寫入的這一個物件）。
type ModelDeploymentValidator struct {
	Reader client.Reader
}

// ValidateCreate 拒絕名稱撞到既有、且不屬於同名 md 的 Deployment／Service／PVC。
// 不擋的話，controller 的 SSA（ForceOwnership）會接手那個物件，md 被刪時 GC 會連它一起刪掉（docs/05 實驗 1）。
func (v *ModelDeploymentValidator) ValidateCreate(ctx context.Context, md *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	children := []struct {
		kind string
		obj  client.Object
		name string
	}{
		{"Deployment", &appsv1.Deployment{}, md.Name},
		{"Service", &corev1.Service{}, md.Name},
		// 要跟 internal/controller 的 pvcName() 一致。
		{"PersistentVolumeClaim", &corev1.PersistentVolumeClaim{}, md.Name + "-models"},
	}
	for _, c := range children {
		err := v.Reader.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: c.name}, c.obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check %s %q: %w", c.kind, c.name, err)
		}
		if !controlledByModelDeployment(c.obj, md.Name) {
			return nil, fmt.Errorf("%s %q already exists in namespace %q and is not managed by ModelDeployment %q; "+
				"choose another name", c.kind, c.name, md.Namespace, md.Name)
		}
	}
	return nil, nil
}

// ValidateUpdate 不檢查：名稱不能改，子資源在建立時已經確認過。
func (v *ModelDeploymentValidator) ValidateUpdate(_ context.Context, _, _ *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete 不檢查：marker 的 verbs 只有 create，API server 不會為了刪除呼叫這個 webhook。
func (v *ModelDeploymentValidator) ValidateDelete(_ context.Context, _ *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	return nil, nil
}

// controlledByModelDeployment：同名 md 被刪、GC 還沒清完時，子資源的 controller 仍是（舊的）同名 md，這種情況放行。
func controlledByModelDeployment(obj client.Object, mdName string) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil &&
		ref.APIVersion == servingv1alpha1.GroupVersion.String() &&
		ref.Kind == "ModelDeployment" &&
		ref.Name == mdName
}
```

### 3.4 讀懂幾個設計決定

**marker 的 `verbs=create`（scaffold 是 `create;update`）**

名稱建立後就不能改，子資源只需要在建立時檢查一次。少一個 verb，就少一種「operator 掛了會被擋」的操作 —— 實驗 4 的實測：operator 停機時 UPDATE、DELETE 照常運作。marker 改了要 `make manifests`，`config/webhook/manifests.yaml` 的 `operations` 才會變成只有 `CREATE`。

**`failurePolicy=fail`（保留 scaffold 的預設）**

| | webhook 呼叫失敗時（pod 掛了、TLS 錯、逾時） | 代價 |
|---|---|---|
| `Fail` | 拒絕這個請求 | operator 停機期間**不能建 md**（實驗 4） |
| `Ignore` | 放行 | operator 停機期間撞名的 md 會被接受 → 等 operator 回來，就搶走別人的 Service |

這個 webhook 擋的是「會刪掉別人資源」的錯誤，寧可暫時不能建，也不要默默放行。驗證規則只是「建議」性質的 webhook（例如檢查命名慣例），才適合 `Ignore`。

**讀 cache，不讀 API server**

`mgr.GetClient()` 讀的是 controller 本來就在 watch 的 informer cache（Deployment、Service、PVC 都在 `Owns()` 裡）：

- **不需要新的 RBAC**：Phase 3 已經給了 `get;list;watch`
- **快**：Part 5.1 實測平均 10.7 毫秒
- **代價是 TOCTOU（time-of-check to time-of-use）**：Service 在 webhook 檢查**之後**、controller apply **之前**才被建立；或剛建立、cache 還沒看到，webhook 都會放行。改用 `mgr.GetAPIReader()` 直接讀 API server 只能縮小第二種空窗，第一種不管怎麼讀都擋不住。這就是 3.1 說「根本的修法在 controller」的原因

**`sideEffects=None`**

宣告這個 webhook 除了回答「准／不准」之外，不會改任何東西。`kubectl apply --dry-run=server` 只會呼叫 `sideEffects: None` 的 webhook；有副作用的 webhook 會讓 dry-run 直接失敗。

**錯誤用一般的 `error` 回傳**

controller-runtime 會把它包成 `403 Forbidden`，訊息是 `admission webhook "vmodeldeployment-v1alpha1.kb.io" denied the request: ...`。注意 webhook 的 HTTP 回應本身是 **200**（裡面寫 `allowed: false`），Part 5.1 的 metrics 會看到 `code="200"`。

### 3.5 測試

webhook 的邏輯用**單元測試**驗證（fake client 扮演 cache），再用一個**透過 envtest API server** 的測試確認 marker、路徑、TLS 都接對了。scaffold 的 `webhook_suite_test.go` 已經啟動了 envtest 和一個帶 webhook server 的 manager，不用改。

`internal/webhook/v1alpha1/modeldeployment_webhook_test.go` 整個換成（保留檔頭）：

```go
package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Webhook", func() {
	const (
		name = "chat"
		ns   = "default"
	)

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{URL: "https://example.com/m.gguf", File: "m.gguf"},
			},
		}
	}
	// validatorWith 用 fake client 扮演 informer cache，裡面放著叢集「已經有」的物件。
	validatorWith := func(objs ...client.Object) *ModelDeploymentValidator {
		return &ModelDeploymentValidator{Reader: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()}
	}
	ownedBy := func(mdName string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: servingv1alpha1.GroupVersion.String(), Kind: "ModelDeployment",
			Name: mdName, UID: "old-uid", Controller: new(true),
		}}
	}

	It("admits a ModelDeployment whose children do not exist yet", func() {
		Expect(validatorWith().ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	DescribeTable("denies a name that collides with an object it does not own",
		func(existing client.Object, wantMsg string) {
			_, err := validatorWith(existing).ValidateCreate(ctx, newMD())
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("Service created by someone else",
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			`Service "chat" already exists`),
		Entry("Deployment created by someone else",
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			`Deployment "chat" already exists`),
		Entry("model cache PVC created by someone else",
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name + "-models", Namespace: ns}},
			`PersistentVolumeClaim "chat-models" already exists`),
		Entry("Service owned by a different ModelDeployment",
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: ownedBy("other")}},
			`Service "chat" already exists`),
	)

	It("admits a name whose children still belong to a deleted ModelDeployment of the same name", func() {
		leftover := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: ownedBy(name)}}
		Expect(validatorWith(leftover).ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	It("ignores objects with the same name in another namespace", func() {
		elsewhere := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "other"}}
		Expect(validatorWith(elsewhere).ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	It("is wired into the API server: a colliding create is rejected end to end", func() {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, svc)

		// webhook 讀的是 cache，Service 剛建立時 cache 可能還沒看到，所以用 Eventually。
		Eventually(func() error { return k8sClient.Create(ctx, newMD()) }).
			Should(MatchError(ContainSubstring(`denied the request: Service "chat" already exists`)))
	})
})
```

- `Controller: new(true)`：Go 1.26 起 `new` 可以接一個值。寫成 `ptr.To(true)` 的話，lint 的 `modernize` 會要求改成這樣
- 最後一個測試的 `Eventually` 本身就是 TOCTOU 的示範：Service 剛建立，webhook 的 cache 不一定看得到

```bash
make manifests
make test
make lint
```

**預期**（寫這份文件時實測）：

- `internal/controller` 19 個、覆蓋率 83.0%；`internal/webhook/v1alpha1` **8 個**、覆蓋率 **80.0%**
- `make lint` `0 issues.`
- 寫這份文件時另外確認過測試擋得住（不用重做）：把 `ValidateCreate` 第一行改成 `return nil, nil`，5 個失敗（4 個 deny + envtest 那個）、3 個通過

### 3.6 `make run` 從此不能直接用

webhook server 要從 `--webhook-cert-path`（預設是系統暫存目錄）讀憑證，筆電上沒有，manager 一啟動就退出：

```
ERROR  setup  Failed to run manager  {"error": "open /var/folders/.../T/k8s-webhook-server/serving-certs/tls.crt: no such file or directory"}
```

scaffold 在 `cmd/main.go` 留了開關：

```bash
ENABLE_WEBHOOKS=false make run
```

這樣跑時，**webhook 的規則不會生效**（CEL 照樣生效，因為它在 API server 裡）。之後的 Phase 都用叢集內部署，這個開關只在想快速除錯 controller 時用。

---

## Part 4：cert-manager 與部署

### 4.1 為什麼 webhook 需要 cert-manager

API server 呼叫 webhook **一定走 HTTPS**，而且要驗證 webhook 的憑證。所以需要三樣東西：

```
Issuer（selfSigned）
  └─ 簽發 Certificate serving-cert
        dnsNames: llm-serving-operator-webhook-service.llm-serving-operator-system.svc（.cluster.local）
        └─ 存成 Secret webhook-server-cert（tls.crt、tls.key、ca.crt）
              ├─ 掛進 manager pod：/tmp/k8s-webhook-server/serving-certs  ← webhook server 用它做 TLS
              └─ cainjector 讀 ValidatingWebhookConfiguration 的 annotation
                   cert-manager.io/inject-ca-from: llm-serving-operator-system/llm-serving-operator-serving-cert
                   把 CA 填進 .webhooks[].clientConfig.caBundle                ← API server 用它驗證 webhook
```

| 角色 | 沒有它會怎樣 |
|---|---|
| Certificate 的 `dnsNames` | API server 連的是 Service 的 DNS 名稱，憑證上沒有這個名稱 → `x509: certificate is valid for ..., not ...` |
| Secret 掛進 pod | pod 起不來（volume 等不到 Secret），或 webhook server 沒有憑證 |
| cainjector 填 `caBundle` | API server 不信任這張自簽憑證 → `x509: certificate signed by unknown authority`（Part 6 會在 cert-manager **自己的** webhook 上看到這個錯誤） |

**憑證更新不用重啟**：cert-manager 預設簽 90 天、到期前 30 天更新（實測 `notAfter` 2027-02-19、`renewalTime` 2027-01-20）。controller-runtime 的 certwatcher 每 10 秒檢查一次檔案（log：`Starting certificate poll+watcher ... "interval": "10s"`），Secret 更新後 kubelet 會同步到 volume，webhook server 直接換用新憑證。這個 lab 沒有實際等到更新，列進 KNOWN-GAPS。

### 4.2 在 `operator-lab` 裝 cert-manager

用跟 e2e 測試同一個版本（`test/utils/utils.go` 的 `certmanagerVersion`）：

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
kubectl -n cert-manager wait deploy --all --for=condition=Available --timeout=3m
```

**還不能馬上用**：deployment Available 之後，cainjector 還要幾秒才會把 CA 填進 cert-manager **自己的** webhook 設定。這段空窗內建立 Certificate 會被拒（Part 6 的 e2e 就是這樣失敗的）。用 server-side dry-run 建一個 Issuer 當探針，等到它成功：

```bash
until cat <<'EOF' | kubectl apply --dry-run=server -f - >/dev/null 2>&1
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: probe, namespace: cert-manager}
spec: {selfSigned: {}}
EOF
do sleep 2; done; echo "cert-manager webhook is ready"
```

### 4.3 build、重新產生 chart、升級

```bash
export IMG=llm-serving-operator:v0.0.2      # 新版本用新 tag（04 實驗 3：不要重用 tag）
make docker-build IMG=$IMG
kind load docker-image $IMG --name operator-lab

kubebuilder edit --plugins=helm/v2-alpha --force
git status
```

`04` Part 6.2 說過：**改了 marker 或 CRD，要重跑 plugin 才會同步到 chart**。這次兩個都改了。

**預期**（寫這份文件時實測）：

```
 M .github/workflows/test-chart.yml                       # 加上「用 Helm 裝 cert-manager」的步驟
 M config/manager/kustomization.yaml                      # plugin 內部跑 make build-installer，image 設成 v0.0.2
 M dist/chart/templates/crd/modeldeployments...yaml       # 多了 x-kubernetes-validations
 M dist/chart/templates/manager/manager.yaml              # webhook 憑證的 volume、port、參數
 M dist/chart/values.yaml                                 # tag v0.0.2、certManager.enabled: true、webhook.enabled: true
 M dist/install.yaml
?? dist/chart/templates/cert-manager/                     # Issuer、2 張 Certificate
?? dist/chart/templates/network-policy/allow-webhook-traffic.yaml
?? dist/chart/templates/webhook/                          # Service、ValidatingWebhookConfiguration
```

- `--force` 會覆蓋 `values.yaml`。Phase 3 沒有自訂過 values（`git diff dist/chart/values.yaml` 只有 tag 和兩個 `enabled`），所以安全。有自訂的話，先備份再還原
- `IMG` 有 export，所以 values 的 tag 是 `v0.0.2`（`04` 的「與預期不同」：Makefile 的 `?=` 讓環境變數優先）
- `helm lint dist/chart`：`1 chart(s) linted, 0 chart(s) failed`
- `config/manager/kustomization.yaml` 被 `kustomize edit set image` 改寫（`04` Part 4.1 說過的同一件事）。Phase 3 commit 時沒有收進去，這次也一樣：commit 前 `git checkout config/manager/kustomization.yaml`

升級：

```bash
N=llm-serving-operator-system
date +%T; make helm-deploy IMG=$IMG; date +%T
kubectl -n $N get pods,certificate
kubectl get md
```

**預期**（寫這份文件時在 scratch 叢集上，從 v0.0.1 的 Helm release 升級，實測）：

- `Release "llm-serving-operator" has been upgraded`、`REVISION: 2`，**11.6 秒**
- 兩張 Certificate（`serving-cert`、`metrics-certs`）都是 `READY True`
- md 仍是 `1 / True`（叢集上有 `smollm` 的話；沒有的話只會看到 `greedy`，status 仍是空的）。實測 `smollm` Deployment 的 resourceVersion 升級前後沒有變（新的 controller 接手時沒有寫入，跟 `04` Part 4 同一個機制），不用再量
- CA 有沒有注入、webhook server 有沒有起來，實驗 3 送出第一個請求就知道；e2e 也有專門的檢查

> **`metrics-certs` 那張憑證目前沒有人用**：chart 和 kustomize 都會建立它，但 metrics server 仍用 controller-runtime 自己產生的自簽憑證（manager 的參數裡沒有 `--metrics-cert-path`）。要用它得打開 `config/default/kustomization.yaml` 的 `[METRICS-WITH-CERTS]`。這個 lab 不改，列進 KNOWN-GAPS。

> **`helm history` 分不出兩個版本**：`make helm-deploy` 只改 image tag，`Chart.yaml` 的 `version: 0.1.0`、`appVersion` 都沒動，兩個 revision 看起來一樣。正式的 chart 每次發版都要改 `version`，列進 KNOWN-GAPS（跟「沒有 release pipeline」是同一件事）。

---

## Part 5：三個實驗，以及一個不用重做的實測

```bash
N=llm-serving-operator-system
```

### 實驗 2：已經存在的錯誤物件怎麼辦（ratcheting）

Part 2.0 建的 `greedy` 是在 CEL 規則**之前**建立的，現在它違反規則。對它做各種修改：

```bash
echo "== 只改 metadata"; kubectl annotate md greedy lab/poke=1
echo "== 改不相關的欄位"; kubectl patch md greedy --type=merge -p '{"spec":{"image":"ghcr.io/ggml-org/llama.cpp:server"}}'
echo "== 改 resources，但還是錯的"; kubectl patch md greedy --type=merge -p '{"spec":{"resources":{"limits":{"memory":"1536Mi"}}}}'
echo "== 改成對的"; kubectl patch md greedy --type=merge -p '{"spec":{"resources":{"limits":{"memory":"2Gi"}}}}'
```

**預期**（寫這份文件時實測）：

```
== 只改 metadata
Warning: spec.resources: Invalid value: resources.requests must not exceed resources.limits (cpu, memory)
modeldeployment.serving.llmlab.dev/greedy annotated
== 改不相關的欄位
Warning: spec.resources: Invalid value: resources.requests must not exceed resources.limits (cpu, memory)
modeldeployment.serving.llmlab.dev/greedy patched
== 改 resources，但還是錯的
The ModelDeployment "greedy" is invalid: spec.resources: Invalid value: resources.requests must not exceed resources.limits (cpu, memory)
== 改成對的
modeldeployment.serving.llmlab.dev/greedy patched
```

**這叫 validation ratcheting**：API server 只對**這次有變動的欄位**嚴格執行規則；沒動到的欄位即使違反規則也放行，只回一個 `Warning`。

**為什麼這很重要**：沒有 ratcheting 的話，加一條新規則的那一刻，所有既有的違規物件都會變成「**連 annotation 都改不了**」—— 包括 controller 寫 status、`kubectl label`、GitOps 工具補 annotation。也就是說，**收緊 API 會讓既有的 CR 卡死**。ratcheting 讓你可以先上規則、再慢慢清理舊資料，使用者在每次修改時都會看到 Warning。

> 寫這份文件時在 kind v1.37 上實測。ratcheting 是 KEP-4008 加入的功能，較舊的叢集上行為可能不同；正式環境加新規則前，先在同版本的叢集上用既有資料試一次。webhook 沒有內建的 ratcheting：`ValidateUpdate` 拿得到 `oldObj`，要不要比對新舊值、只檢查有變動的部分，得自己寫。

做完刪掉：

```bash
kubectl delete md greedy
```

### 實驗 3：webhook 擋下撞名，以及誰先執行

```bash
kubectl create service clusterip chat --tcp=80:8000

cat > /tmp/chat-md.yaml <<EOF
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: chat
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q8_0.gguf
    file: SmolLM2-135M-Instruct-Q8_0.gguf
EOF

cat > /tmp/chat-md-bad.yaml <<EOF
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: chat
spec:
  model:
    url: https://huggingface.co/bartowski/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q8_0.gguf
    file: SmolLM2-135M-Instruct-Q8_0.gguf
  resources: {requests: {memory: 2Gi}, limits: {memory: 1Gi}}
EOF

echo "== 撞名"; kubectl apply -f /tmp/chat-md.yaml
echo "== 撞名，而且 resources 也錯"; kubectl apply -f /tmp/chat-md-bad.yaml
kubectl get svc chat -o jsonpath='{.spec.selector}{"\n"}'
```

**預期**（寫這份文件時實測）：

```
== 撞名
Error from server (Forbidden): error when creating "/tmp/chat-md.yaml": admission webhook "vmodeldeployment-v1alpha1.kb.io" denied the request:
Service "chat" already exists in namespace "default" and is not managed by ModelDeployment "chat"; choose another name
== 撞名，而且 resources 也錯
The ModelDeployment "chat" is invalid: spec.resources: Invalid value: resources.requests must not exceed resources.limits (cpu, memory)
{"app":"chat"}
```

- 第一個：webhook 擋下，Service 的 selector 還是 `{"app":"chat"}`，沒有被搶走（對照實驗 1）
- 第二個：**只看到 CEL 的錯誤**。schema 驗證（含 CEL）在 validating admission **之前**（Part 1.1），驗證失敗就直接回傳，webhook 根本沒被呼叫。使用者修好 resources 再送一次，才會看到撞名的錯誤 —— 兩層驗證的錯誤**不會一次列出來**

```bash
kubectl delete svc chat
```

### 實驗 4：operator 停機時建立 md

```bash
kubectl -n $N scale deploy/llm-serving-operator-controller-manager --replicas=0
kubectl -n $N wait pod -l control-plane=controller-manager --for=delete --timeout=60s

cat <<EOF | kubectl apply -f -
apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: other
spec:
  model: {url: "https://example.com/a.gguf", file: a.gguf}
EOF

kubectl -n $N scale deploy/llm-serving-operator-controller-manager --replicas=1
kubectl -n $N rollout status deploy/llm-serving-operator-controller-manager
```

**預期**（寫這份文件時實測）：

```
Error from server (InternalError): error when creating "STDIN": Internal error occurred: failed calling webhook "vmodeldeployment-v1alpha1.kb.io":
failed to call webhook: Post "https://llm-serving-operator-webhook-service.llm-serving-operator-system.svc:443/validate-serving-llmlab-dev-v1alpha1-modeldeployment?timeout=10s":
dial tcp 10.96.x.x:443: connect: connection refused
```

- webhook 掛掉時，**錯誤是 `InternalError`，不是 `Forbidden`**：使用者看到的是「系統壞了」，而不是「你的 md 有問題」。這是 `failurePolicy: Fail` 的代價
- 實測 0.04 秒就回來，因為 Service 沒有 endpoint，連線直接被拒。如果 pod 還在但卡住不回應，API server 會等滿 `?timeout=10s` 才放棄 —— **每一個建立 md 的請求都會卡 10 秒**

同一段停機期間，寫這份文件時另外實測了其他操作（不用重做，結果都可以從設定推出來）：

| 操作 | 結果 | 原因 |
|---|---|---|
| 建立一個 resources 錯的 md | CEL 照樣擋下，訊息跟 operator 活著時一樣 | CEL 是 CRD 的一部分，在 API server 裡執行（Phase 1 實驗 1 是同一件事） |
| `kubectl patch` 既有的 md | 成功 | webhook 只註冊了 `CREATE`（`verbs=create`） |
| `kubectl delete` 既有的 md | 成功 | 同上 |

### 5.1 兩個 replica 時，誰在服務 webhook（寫這份文件時實測，不用重做）

把 controller scale 成 2，送 20 個會被拒絕的撞名請求，再從**每個 pod** 的 `/metrics` 讀 `controller_runtime_webhook_requests_total`（`04` 實驗 5 的方法，port-forward 到 pod 而不是 Service）：

| | 非 leader（`8tjjj`） | leader（`sdcw6`） |
|---|---|---|
| log 有 `Serving webhook server` | 有 | 有 |
| log 有 `Starting workers` | **沒有** | 有 |
| 在 webhook Service 的 endpoint 裡 | `ready=true` | `ready=true` |
| 處理的 webhook 請求 | **20**（`code="200"`），延遲合計 0.214 秒 | **0** |

20 個請求**全部**被正確拒絕。四件事：

1. **webhook 不受 leader election 管**：controller-runtime 只讓 controller（reconcile）等 lease，webhook server 和 informer cache 在每個 replica 上都會啟動。所以多開 replica **確實**提升了 webhook 的可用性，即使 reconcile 仍然只有一個人在做
2. **非 leader 的 cache 也是活的**：它正確地看到了 `chat` Service 並拒絕。如果 cache 只在 leader 上跑，非 leader 會放行所有請求
3. **20 個請求都打到同一個 pod**：API server 會重用到 webhook 的 TLS 連線，而 Service 的負載平衡是以**連線**為單位，不是以請求為單位。不要期待 webhook 的流量平均分到每個 replica
4. **被拒絕的請求，HTTP code 是 200**：拒絕寫在 AdmissionReview 回應裡（`allowed: false`），不是 HTTP 錯誤。`code="500"` 才代表 webhook 自己出錯。平均延遲 0.214 / 20 ≈ **10.7 毫秒**（讀 cache，不打 API server）

> 2 個 replica 要讓 webhook 真的高可用，還差一件事：readiness probe 仍是 `healthz.Ping`（`04` 實驗 2）。新 pod 可能在 webhook server 開始 listen 之前就 Ready、被加進 endpoint。controller-runtime 提供 `mgr.GetWebhookServer().StartedChecker()` 可以當 readyz check，這個 lab 沒有加，列進 KNOWN-GAPS。

---

## Part 6：e2e 測試

### 6.1 scaffold 加了什麼

`kubebuilder create webhook` 已經在 `test/e2e/e2e_test.go` 加了：

- metrics 測試裡：等 webhook Service 有 endpoint、等 `caBundle` 被注入（`+kubebuilder:scaffold:e2e-metrics-webhooks-readiness` 那裡）
- 兩個新的 `It`：`should provisioned cert-manager`（Secret `webhook-server-cert` 存在）、`should have CA injection for validating webhooks`（插在 `+kubebuilder:scaffold:e2e-webhooks-checks` 上面 —— Phase 3 留著這個 marker 就是為了這一刻）

### 6.2 加一個撞名的測試

在 `// +kubebuilder:scaffold:e2e-webhooks-checks` 這行**上面**插入：

```go
		It("should reject a ModelDeployment whose name is taken by another Service", func() {
			By("creating a Service that the ModelDeployment would otherwise take over")
			_, err := utils.Run(exec.Command("kubectl", "create", "service", "clusterip", "taken", "--tcp=80:8000"))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _, _ = utils.Run(exec.Command("kubectl", "delete", "service", "taken")) })

			By("applying a ModelDeployment with the same name")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(`apiVersion: serving.llmlab.dev/v1alpha1
kind: ModelDeployment
metadata:
  name: taken
spec:
  model:
    url: https://example.com/m.gguf
    file: m.gguf
`)
			out, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "the webhook should have denied the request")
			Expect(out).To(ContainSubstring(`denied the request: Service "taken" already exists`))
		})

```

envtest 已經驗證過 webhook 的邏輯和接線，這個 e2e 多驗證的是**envtest 沒有的東西**：cert-manager 簽出來的憑證、cainjector 注入的 CA、叢集內的 Service 路由、PSS `restricted` namespace 裡的 pod 能不能掛上憑證 Secret。

### 6.3 修 scaffold 的 cert-manager 競態

寫這份文件時第一次跑 `make test-e2e`，**第一個測試的 BeforeAll 就失敗了**：

```
STEP: installing CertManager @ 22:41:31
running: "kubectl wait deployment.apps/cert-manager-webhook --for condition=Available ..."
...
STEP: deploying the controller-manager @ 22:41:54
Error from server (InternalError): error when creating "STDIN": Internal error occurred: failed calling webhook "webhook.cert-manager.io":
failed to call webhook: Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s":
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

**這就是 4.1 表格最後一行的錯誤，只是主角換成 cert-manager 自己的 webhook**：cert-manager 的 webhook pod 已經 Available，但 cainjector 還沒把 CA 填進 cert-manager 自己的 `ValidatingWebhookConfiguration`。`make deploy` 在 1 秒後建立 Certificate，API server 不信任 cert-manager webhook 的憑證，請求就被拒。

Phase 3 的 e2e 一樣會裝 cert-manager，但**沒有建立任何 cert-manager 的資源**，所以從來沒踩到。加了 webhook 之後，`make deploy` 會建 Issuer 和 Certificate，競態就出現了。

修法是讓 `InstallCertManager` 等到 cert-manager 的 webhook **真的能用**，而不是只等 pod Available。`test/utils/utils.go` 的 import 加上 `"time"`，把 `InstallCertManager` 結尾的：

```go
	_, err := Run(cmd)
	return err
}
```

換成：

```go
	if _, err := Run(cmd); err != nil {
		return err
	}

	// webhook pod Available，不代表 cainjector 已經把 CA 填進 cert-manager 自己的 webhook 設定。
	// 這段空窗內建立 Issuer／Certificate 會得到 "x509: certificate signed by unknown authority"。
	// 用 server-side dry-run 建一個 Issuer 當探針（dry-run 一樣會呼叫 webhook），等到它成功為止。
	probe := `apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: probe, namespace: cert-manager}
spec: {selfSigned: {}}
`
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cmd = exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = strings.NewReader(probe)
		_, err := Run(cmd)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}
```

- 探針用 **dry-run**：會經過完整的 admission（包括 cert-manager 的 webhook），但不會真的建立 Issuer，不用清理
- 這也是 4.2 那段 `until ... kubectl apply --dry-run=server` 的由來

> **這個競態不是每次都會出現**。寫這份文件時兩次在全新叢集上跑 e2e，只有第一次失敗；修好之後那次，探針第一次就成功。在已經拉過 image 的叢集上重裝 cert-manager 三次，也都沒有空窗。推測是全新叢集上 cainjector 的 image 還在拉，webhook pod 先 Available 了（CI 每次都是全新叢集）。
>
> 因為重現不穩定，「修好之後 e2e 通過」證明不了探針有效，所以另外**手動製造這個狀態**來驗證：把 cainjector scale 成 0、清掉 cert-manager webhook 設定裡的 `caBundle`，探針失敗（`failed calling webhook "webhook.cert-manager.io" ... tls: failed to verify certificate`）；cainjector scale 回 1，1 秒後探針成功。
- **對自己的 operator 的啟示**：使用者裝完你的 operator 馬上建 md，也會遇到同樣的空窗（你的 webhook pod Ready 了、CA 還沒注入）。Helm 的 `--wait` 只等 pod Ready，不等 CA 注入

### 6.4 本機不用跑 e2e，交給 CI

本機只確認編譯得過：

```bash
go vet -tags=e2e ./test/...
```

完整的 e2e 寫這份文件時已經在本機跑過（7 個通過，結果在下表），GitHub Actions 每次 push 也會在全新的 kind 叢集上再跑一次。本機再跑一次只是重複，還得等 2～3 分鐘、事後切回 context。**CI 失敗時**才在本機跑 `make test-e2e` 除錯。

**預期**：`SUCCESS! -- 7 Passed | 0 Failed`（scaffold 2 + Phase 3 的 2 + scaffold 的 webhook 2 + 新的 1）。

寫這份文件時的實測（scratch 副本，全新的 e2e 叢集）：

| 測試 | 時間 |
|---|---|
| BeforeSuite（build image、kind load、裝 cert-manager、探針） | 31.5 秒 |
| should run successfully | 3.2 秒 |
| should ensure the metrics endpoint is serving metrics（現在多等 webhook endpoint 與 CA 注入） | 24.3 秒 |
| should grant the manager only the permissions it needs | 0.2 秒 |
| should serve a ModelDeployment and garbage-collect its children on delete | 67.7 秒 |
| should provisioned cert-manager | 0.04 秒 |
| should have CA injection for validating webhooks | 0.04 秒 |
| should reject a ModelDeployment whose name is taken by another Service | 8.1 秒（大部分是 `AfterAll` 的清理，Ginkgo 算在最後一個 spec 裡） |
| 整個 `make test-e2e` | **2 分 32 秒**，`SUCCESS! -- 7 Passed` |

### 6.5 推上 GitHub

```bash
git add -A
git commit -m "feat: CEL rule for resources, validating webhook for child name collisions"
git push
```

`Test Chart` workflow 這次會多一步「用 Helm 裝 cert-manager」（plugin 自動取消註解的）。記下四個 workflow 的時間。

> `test-chart.yml` 新增的步驟是 `helm install cert-manager jetstack/cert-manager`，**沒有指定版本**，每次都裝最新版；e2e 則固定 v1.21.1。scaffold 其他地方都把 action 釘在 commit SHA（`04` Part 7.1），這一步沒有。列進 KNOWN-GAPS。

---

## Part 7：對照表

面試被問「CRD 的驗證放哪裡」時，用這張表回答：

| | OpenAPI schema | CEL | ValidatingAdmissionPolicy | validating webhook | controller |
|---|---|---|---|---|---|
| 這個 lab 用在 | `file` 的 pattern、`url` 只收 https | requests ≤ limits | 沒用 | 子資源撞名 | 最後一道防線（沒做撞名檢查，見 KNOWN-GAPS） |
| 看得到 | 一個欄位 | 整個物件、`oldSelf` | 物件 + 一個參數物件 | 任何東西 | 任何東西 |
| 錯誤出現在 | `kubectl apply` 當下 | `kubectl apply` 當下 | `kubectl apply` 當下 | `kubectl apply` 當下 | status／Event（事後） |
| operator 停機時 | 有效 | 有效 | 有效 | `Fail`：擋下所有建立；`Ignore`：失效 | 失效 |
| 維運成本 | 無 | 無（cost 在寫 CRD 時檢查） | 無 | TLS、CA 注入、HA、延遲 | — |
| 誰寫 | CRD 作者 | CRD 作者 | **叢集管理員**（跨 CRD 的政策） | CRD 作者 | CRD 作者 |

- **ValidatingAdmissionPolicy**（VAP）也是 CEL，但它是獨立的資源，不寫在 CRD 裡。適合叢集管理員訂「所有 Deployment 都要有 owner label」這類跨資源的政策；CRD 作者驗證自己的型別，用 `x-kubernetes-validations` 就好。它一樣**看不到任意的其他物件**，所以撞名檢查它也做不到
- **為什麼 KServe 這類專案還是有 webhook**：InferenceService 的驗證需要看 ServingRuntime、叢集設定這些其他物件，而且有 mutating 的需求（注入預設的 runtime）。CEL 出現之前，webhook 是唯一的選擇，很多既有的 webhook 現在可以部分換成 CEL

---

## Part 8：記錄與收工

1. RESULTS 新增 Phase 4 段落：
   - Part 2：`make test` 的測試數與覆蓋率
   - Part 3：`kubebuilder create webhook` 的 `git status`、`make test`（兩個套件）、`make lint`
   - Part 4：`helm-deploy` 升級時間
   - 四個實驗的關鍵輸出
   - e2e：GitHub Actions 四個 workflow 的時間、E2E Tests 的通過數
2. 「與預期不同」
3. 更新 `KNOWN-GAPS.md`，新增 Phase 4 的邊界：
   - **controller 端沒有撞名檢查**：webhook 有 TOCTOU 空窗（檢查後才建立的 Service、cache 還沒看到的 Service），`ENABLE_WEBHOOKS=false make run` 時也沒有 webhook。根本的修法是 controller apply 前檢查 ownerReference，不是自己的就不碰、標成 Degraded
   - **webhook 是 `failurePolicy: Fail`**：operator 停機期間不能建立 md（實驗 4）
   - **readiness probe 沒有檢查 webhook server**：可用 `mgr.GetWebhookServer().StartedChecker()`
   - **CEL 規則只檢查 cpu、memory**：迭代整個 ResourceList 會超過 cost 預算（Part 2.2）
   - **自簽 Issuer**：沒有接真正的 CA；憑證更新（90 天）沒有實際驗證過
   - **`metrics-certs` 憑證有建立但沒有使用**
   - **`Chart.yaml` 的版本沒有跟著 image 升級**，`helm history` 分不出版本
   - **`test-chart.yml` 裝 cert-manager 沒有釘版本**
   - **webhook 的 PVC 名稱規則（`<name>-models`）跟 controller 的 `pvcName()` 是兩份**，改一邊要記得改另一邊
4. README：
   - 階段表：Phase 4 改成 ✅
   - 「這個專案在解什麼問題」表格的「API server 在寫入當下就拒絕（OpenAPI schema / CEL / webhook）」現在三個都有了
   - 「成果」加一個 Phase 4 的表格（只填量過的：CEL 擋下的錯誤、webhook 延遲（Part 5.1 的 10.7 毫秒可以直接引用）、operator 停機時的行為、CI 的 e2e 時間）
5. commit

## 完成條件

- [x] 規則上線前建好違規的 `greedy`
- [x] CEL 規則與 3 個 envtest 測試，`make test` 通過
- [x] 實驗 1：親眼看到 md 搶走 Service、刪 md 時 Service 被 GC
- [x] webhook 與 8 個測試，`make test`、`make lint` 通過
- [x] cert-manager 裝好，`make helm-deploy` 升級成功
- [x] 實驗 2：ratcheting 的四種修改結果都看過
- [x] 實驗 3：webhook 擋下撞名；同時違規時只看到 CEL 的錯誤
- [x] 實驗 4：operator 停機時 CREATE 得到 `InternalError`
- [x] GitHub Actions 四個 workflow 都是綠的（E2E Tests 7 個通過）
- [x] KNOWN-GAPS、README 已更新

> 下一階段（Phase 5，選做）：讓 md 支援 LoRA adapter，接上 `llm-inference-k8s-lab` 的 pricer 模型，用 MAE 驗收（不做 HPA）。
