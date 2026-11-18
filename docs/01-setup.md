# LLM Serving Operator 實驗室 — 01：環境與專案骨架

> 成本：**$0**。
> 產出：一個 scaffold 好、`make test` 會過的 kubebuilder 專案，以及一個 kind 叢集。
> 預估時間：約 40 分鐘（大部分在下載）。

---

## Part 1：安裝工具

> 本機已裝好（2026-11-18）：goenv 3.2.2（Homebrew）、Go 1.26.11（goenv global）、kubebuilder v4.16.0（`~/.local/bin`）、kind v0.33.0（Homebrew）。工具已全部到位。
> 選 Go 1.26 而不是最新的 1.27：controller-gen、setup-envtest 這類工具通常比 Go 新版晚一點跟上，用前一版比較不會踩到相容性問題。

**Go（用 goenv 管理版本）**：

```bash
brew install goenv                 # 已裝
# 已寫進 ~/.zshrc 的 eval "$(goenv init - zsh)"，以及 ~/.bash_profile 的 eval "$(goenv init - bash)"，新開的終端機會自動載入
goenv list                         # => * 1.26.11 (set by global)
which go                           # => ~/.goenv/shims/go
go version                         # => go1.26.11
```

要換版本：`goenv use 1.27.4 --global`（還沒裝的話會先安裝，再設成全域預設）。
只想讓這個 repo 用某個版本，就在 repo 根目錄執行 `goenv use 1.27.4`（不加 `--global`），它會寫一個 `.go-version` 檔。

**kubebuilder** 用官方的安裝方式（Kubebuilder Book 的 Quick Start），放在 `~/.local/bin`（已在 PATH 上，不需要 sudo）：

```bash
curl -fsSL -o ~/.local/bin/kubebuilder "https://go.kubebuilder.io/dl/latest/$(go env GOOS)/$(go env GOARCH)"
chmod +x ~/.local/bin/kubebuilder
```

**kind**：

```bash
brew install kind                  # 已裝
kind version                       # => kind v0.33.0
```

**kubectl**：不要用 OrbStack 內建的那一份。它是 `/usr/local/bin/kubectl` 這個 symlink，版本是 v1.33，和 kind v0.33 的 v1.37 節點差了 4 個小版本，超出 kubectl 支援的 ±1 範圍。改用 Homebrew 裝：

```bash
brew install kubectl
hash -r                            # 清掉 shell 快取的舊路徑
which kubectl                      # => /opt/homebrew/bin/kubectl（PATH 上排在 /usr/local/bin 前面）
```

**記錄版本**（貼進 `RESULTS.md` 的〈環境〉）：

```bash
go version
kubebuilder version
kind version
kubectl version --client
```

> 這份文件**不寫死版本號**：kubebuilder 更新很快，scaffold 出來的檔案會因版本略有不同（例如 status 裡是否預設就有 `Conditions`）。
> 遇到跟文件不一樣的 scaffold，以你實際產生的為準，Phase 1 會標出哪些地方可能不同。

---

## Part 2：開 kind 叢集

**先關掉 OrbStack 內建的 k8s**，不要同時開兩個叢集搶記憶體：

```bash
orb stop k8s 2>/dev/null; true
kind create cluster --name operator-lab
kubectl config use-context kind-operator-lab
kubectl get nodes          # 一個 control-plane，STATUS 要是 Ready
```

Phase 1 的模型只有 145 MB，單節點、OrbStack 預設記憶體就夠。

---

## Part 3：scaffold 專案

`kubebuilder init` 會產生自己的 `README.md`，而且不喜歡在非空目錄執行。
所以先把這個 repo 現有的文件**暫時搬走**，scaffold 完再搬回來（我們的 README 會覆蓋掉 scaffold 的）。

```bash
OP=~/Repo/llm-serving-operator
STASH=$(mktemp -d)
cd $OP
mv README.md RESULTS.md KNOWN-GAPS.md docs $STASH/
ls -A                      # 應該是空的
```

**init**：

```bash
kubebuilder init \
  --domain llmlab.dev \
  --repo github.com/Jeremymymy/llm-serving-operator
```

- `--domain` 決定 API group 的後綴：等一下的 group `serving` 會變成 `serving.llmlab.dev`
- `--repo` 是 Go module path，之後 import 自己的套件都用它

**建立 API**（`--resource` 產生 CRD 型別、`--controller` 產生 controller 骨架，不加的話會互動式詢問）：

```bash
kubebuilder create api \
  --group serving --version v1alpha1 --kind ModelDeployment \
  --resource --controller
```

**搬回文件**：

```bash
mv $STASH/README.md $STASH/RESULTS.md $STASH/KNOWN-GAPS.md $STASH/docs $OP/
rmdir $STASH
```

---

## Part 4：看懂 scaffold 了什麼

```bash
tree -L 2 -I 'bin|vendor'          # 沒有 tree 的話：brew install tree
# 不想裝的話，用 find 也看得到一樣的東西：
find . -maxdepth 2 -not -path './bin*' -not -path './.git/*' | sort
```

kubebuilder v4.16 還會產生幾個 Phase 1 用不到的檔案：`AGENTS.md`（給 AI coding agent 看的專案說明）、`.devcontainer/`、`.github/workflows/`、`.golangci.yml`／`.custom-gcl.yml`、`test/e2e/`。留著就好，不用刪。

你只會常改這幾個地方：

| 路徑 | 是什麼 | 誰寫 |
|---|---|---|
| `api/v1alpha1/modeldeployment_types.go` | CRD 的 Go 型別 + marker | **你** |
| `api/v1alpha1/zz_generated.deepcopy.go` | DeepCopy 方法 | `make generate` 產生，**不要手改** |
| `internal/controller/modeldeployment_controller.go` | Reconcile 邏輯 + RBAC marker | **你** |
| `internal/controller/*_test.go`、`suite_test.go` | envtest 測試 | **你**（suite 用 scaffold 的） |
| `config/crd/bases/*.yaml` | CRD manifest | `make manifests` 產生 |
| `config/rbac/role.yaml` | controller 的 ClusterRole | `make manifests` 從 RBAC marker 產生 |
| `config/samples/*.yaml` | 範例 CR | **你** |
| `cmd/main.go` | manager 進入點（註冊 controller、metrics、health probe、leader election） | scaffold，Phase 1 不改 |
| `Dockerfile` | 多階段建置 | scaffold，Phase 3 會逐行讀 |
| `Makefile` | 所有指令的入口 | scaffold |

**`make` 目標**（Phase 1 會用到的）：

| 指令 | 做什麼 |
|---|---|
| `make generate` | 改了 `*_types.go` 之後，重新產生 DeepCopy |
| `make manifests` | 從 marker 重新產生 CRD 與 RBAC YAML |
| `make test` | 下載 envtest 的 apiserver/etcd 執行檔，跑測試 |
| `make install` | 把 CRD 裝進目前的叢集 |
| `make run` | 在**本機**跑 controller（用你的 kubeconfig 連叢集） |

---

## Part 5：基線檢查

還沒改任何程式碼之前，先確認 scaffold 本身是好的。之後出錯才知道是自己改壞的：

```bash
make manifests generate
make test
```

`make test` 第一次會下載 envtest 執行檔，要等一下。最後要看到 `ok` 與覆蓋率。

**初始化 git**（scaffold 已經產生 `.gitignore`）：

```bash
git init
git add -A
git commit -m "chore: scaffold with kubebuilder"
```

> 把「純 scaffold」獨立成一個 commit，之後 `git diff` 就只看得到你自己寫的東西 —— 面試時講「我寫了什麼」很好用。

---

## 完成條件

- [ ] 四個工具版本已記進 `RESULTS.md`
- [ ] `kubectl get nodes` 看得到 `operator-lab-control-plane` Ready
- [ ] `make test` 通過
- [ ] scaffold commit 已建立，`README.md` 是這個 lab 的版本（不是 kubebuilder 產生的）
