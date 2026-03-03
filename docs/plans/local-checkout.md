# Local Repository Checkout — Revised Plan

## Context

Hydra server 模式只有 diff 文本，`ContextGatherer` 为 nil（`reviewer.go:104`）。CLI 模式 ContextGatherer cwd 硬编码 `"."`（`gatherer.go:116`），Provider SetCwd 从未调用。

本方案为两种模式添加统一 checkout 能力，使 AI provider 可浏览完整代码。

### 审查反馈修订要点

1. **并发隔离**：改为 bare mirror 缓存 + 每次 review 独立 worktree，避免共享工作树串线
2. **Server prompt 模板**：补齐 `server_review.tmpl` 中 `HasLocalRepo` 逻辑
3. **Cleanup 生命周期**：引用计数 + context 取消，Shutdown 时停止清理 goroutine
4. **CLI repo 检测 fallback**：`target.Repo` 为空时尝试 `DetectRepoFromRemote`
5. **测试覆盖**：扩展到并发隔离、cleanup 不误删、fetch 失败回退

---

## Step 1: Config 添加 CheckoutConfig

**文件**: `internal/config/config.go`

在 `HydraConfig` 中添加：

```go
type CheckoutConfig struct {
    Enabled bool   `yaml:"enabled"`
    BaseDir string `yaml:"baseDir,omitempty"` // 默认: ~/.hydra/repos
    TTL     string `yaml:"ttl,omitempty"`     // 默认: "24h"
}
```

HydraConfig 加字段：`Checkout *CheckoutConfig \`yaml:"checkout,omitempty"\``

## Step 2: reviewTarget 添加 MRNumber

**文件**: `cmd/review.go`

- `reviewTarget`（line 58）添加 `MRNumber string`
- `resolveMRTarget`（line 415）返回时设 `MRNumber: mrNumber`

## Step 3: ContextGatherer 注入 cwd

**文件**: `internal/context/gatherer.go`

- 结构体加 `cwd string` 字段，`NewContextGatherer` 初始化 `cwd: "."`
- 添加 `SetCwd(cwd string)` 方法
- `Gather`（line 116）`cwd := "."` → `cwd := g.cwd`

**文件**: `internal/context/adapter.go`

- `ContextGathererAdapter` 添加 `SetCwd` 方法，转发给 `inner.SetCwd`

## Step 4: Provider SetCwdIfSupported

**文件**: `internal/provider/provider.go`

```go
func SetCwdIfSupported(p AIProvider, cwd string) {
    type cwdSetter interface{ SetCwd(string) }
    if cs, ok := p.(cwdSetter); ok { cs.SetCwd(cwd) }
}
```

## Step 5: checkout 包（核心新代码）

**新建** `internal/checkout/` 目录。

### 5a. `checkout.go` — Manager + 公共 API

**核心改动：bare mirror 缓存 + 独立 worktree**

```go
type Params struct {
    Platform string // "github" | "gitlab"
    Repo     string // "owner/repo"
    MRNumber string
    Host     string // 平台域名，空用默认
}

type Result struct {
    RepoDir   string // worktree 绝对路径（调用方设为 cwd）
    Available bool
    FromCache bool
    cleanup   func() // 内部用，删除 worktree
}

// Release 释放本次 checkout 的 worktree，调用方 defer 调用
func (r *Result) Release() {
    if r.cleanup != nil { r.cleanup() }
}

type Manager struct {
    baseDir  string
    ttl      time.Duration
    mu       sync.Mutex        // 保护同 repo 的 clone/fetch 串行
    active   sync.WaitGroup    // 追踪活跃 checkout 数量
    cancelFn context.CancelFunc // Cleanup goroutine 取消
}
```

**缓存策略（修订）**：
- mirror 路径: `baseDir/platform/repo-path.git`（bare mirror，不含工作树）
- 每次 Checkout 创建临时 worktree: `baseDir/worktrees/<uuid>/`
- Cache hit: `git fetch --all`（mirror 上），然后 `git worktree add`
- Cache miss/expired: `git clone --mirror` → `git worktree add`
- `Result.Release()` 负责 `git worktree remove` 清理 worktree

```go
func NewManager(cfg *config.CheckoutConfig) *Manager  // nil/disabled → nil
func (m *Manager) Checkout(params Params) Result       // nil receiver → 零值
func (m *Manager) StartCleanup(ctx context.Context)    // 定期清理过期 mirror
func (m *Manager) Wait()                               // 等待活跃 checkout 完成
```

### 5b. `clone.go` — 克隆、fetch、worktree 操作

```go
func (m *Manager) mirrorDir(params Params) string           // baseDir/platform/repo.git
func (m *Manager) ensureMirror(cloneURL, mirrorDir string) (fromCache bool, err error)
func (m *Manager) createWorktree(mirrorDir string, params Params) (string, error)
func (m *Manager) removeWorktree(mirrorDir, worktreeDir string)
func buildCloneURL(platform, repo, host string) string
```

MR ref 策略（在 worktree add 时 fetch）：
- GitHub: `git fetch origin pull/<N>/head:pr-<N>` → `git worktree add <dir> pr-<N>`
- GitLab: `git fetch origin merge-requests/<N>/head:mr-<N>` → `git worktree add <dir> mr-<N>`

### 5c. `checkout_test.go` — 单元测试

基础：
- `TestNewManager_Disabled` — nil/disabled config → nil manager
- `TestNewManager_Defaults` — 默认 baseDir 和 TTL
- `TestBuildCloneURL` — GitHub/GitLab URL 构建
- `TestMirrorDir` — 缓存路径生成

并发与安全：
- `TestConcurrentCheckout_Isolation` — 同 repo 两个 MR 并发 checkout 得到不同 worktree 路径
- `TestRelease_CleansWorktree` — Release 后 worktree 目录被删除
- `TestCleanup_SkipsActiveCheckouts` — 有活跃 checkout 时 Cleanup 不删 mirror
- `TestFetchFailure_FallbackToReclone` — fetch 失败时删 mirror 并重新 clone

## Step 6: CLI 模式集成

**文件**: `cmd/review.go`

### 6a. Repo 检测 fallback

在 `resolveMRTarget` 中，当 `mrRepo` 仍为空时，加 fallback：

```go
if mrRepo == "" && plat != nil {
    if repo, err := plat.DetectRepoFromRemote(); err == nil {
        mrRepo = repo
    }
}
```

### 6b. Checkout 集成

在 contextGatherer 创建后、orchestrator 创建前插入：

```go
var checkoutResult checkout.Result
mgr := checkout.NewManager(cfg.Checkout)
if target.Type == "pr" && mgr != nil && target.Repo != "" {
    d.SpinnerStart("Checking out repository...")
    platName := ""
    if plat != nil { platName = plat.Name() }
    host := ""
    if cfg.Platform != nil { host = cfg.Platform.Host }
    checkoutResult = mgr.Checkout(checkout.Params{
        Platform: platName, Repo: target.Repo,
        MRNumber: target.MRNumber, Host: host,
    })
    if checkoutResult.Available {
        defer checkoutResult.Release()
        d.SpinnerSucceed("Repository checked out")
    } else {
        d.SpinnerSucceed("Checkout skipped, using diff-only mode")
    }
}

// 设置 CWD
cwd := "."
if checkoutResult.Available { cwd = checkoutResult.RepoDir }
for _, r := range reviewers { provider.SetCwdIfSupported(r.Provider, cwd) }
provider.SetCwdIfSupported(analyzerProvider, cwd)
provider.SetCwdIfSupported(summarizerProvider, cwd)
if contextGatherer != nil {
    if cg, ok := contextGatherer.(interface{ SetCwd(string) }); ok {
        cg.SetCwd(cwd)
    }
}

// Prompt 增强
if checkoutResult.Available {
    target.Prompt += "\n\nNote: The full repository source code is available in your working directory.\nYou can browse files, read implementations, and examine the broader codebase context beyond the diff."
}
```

## Step 7: Server 模式集成

### 7a. `internal/server/reviewer.go`

签名增加 `checkoutMgr`：

```go
func RunServerReview(ctx context.Context, event *MergeRequestEvent,
    plat platform.Platform, cfg *config.HydraConfig,
    checkoutMgr *checkout.Manager, logger *log.Logger) error {
```

在 diff 获取后、orchestrator 创建前：

```go
var checkoutResult checkout.Result
if checkoutMgr != nil {
    checkoutResult = checkoutMgr.Checkout(checkout.Params{
        Platform: plat.Name(), Repo: repo, MRNumber: mrID,
    })
    if checkoutResult.Available {
        defer checkoutResult.Release()
    }
}

// ContextGatherer（仅 checkout 成功时创建）
var contextGatherer orchestrator.ContextGathererInterface
if checkoutResult.Available && cfg.ContextGatherer != nil && cfg.ContextGatherer.Enabled {
    contextModel := cfg.ContextGatherer.Model
    if contextModel == "" { contextModel = cfg.Analyzer.Model }
    ctxProvider, err := provider.CreateProvider(contextModel, "", "", cfg)
    if err != nil {
        logger.Printf("[warn] context gatherer provider failed: %v", err)
    } else {
        adapter := appctx.NewContextGathererAdapter(ctxProvider, cfg.ContextGatherer, plat)
        adapter.SetCwd(checkoutResult.RepoDir)
        contextGatherer = adapter
    }
}

// SetCwd for all providers
if checkoutResult.Available {
    for i := range reviewers {
        provider.SetCwdIfSupported(reviewers[i].Provider, checkoutResult.RepoDir)
    }
    provider.SetCwdIfSupported(analyzerProvider, checkoutResult.RepoDir)
    provider.SetCwdIfSupported(summarizerProvider, checkoutResult.RepoDir)
}
```

Prompt 模板变量增加 `HasLocalRepo`:

```go
reviewPrompt := prompt.MustRender("server_review.tmpl", map[string]any{
    "MRURL":       mrURL,
    "Title":       mrInfo.Title,
    "Description": mrInfo.Description,
    "Diff":        annotatedDiff,
    "HasLocalRepo": checkoutResult.Available,
})
```

ContextGatherer 传入 orchestrator：`ContextGatherer: contextGatherer`（替换原来的 `nil`）。

### 7b. `internal/server/server.go`

- `Server` 加字段 `checkoutMgr *checkout.Manager`
- `New()` 中: `checkoutMgr: checkout.NewManager(cfg.HydraConfig.Checkout)`
- `Start()` 中启动 cleanup goroutine（带 context）：

```go
ctx, cancel := context.WithCancel(context.Background())
s.cancelCleanup = cancel
if s.checkoutMgr != nil {
    s.checkoutMgr.StartCleanup(ctx)
}
```

- `Shutdown()` 中：先 cancel cleanup goroutine，再 `s.checkoutMgr.Wait()` 等活跃 checkout 完成
- `triggerReview` 传 `s.checkoutMgr` 给 `RunServerReview`

### 7c. `internal/server/webhook.go`

`ProjectInfo` 添加 `HTTPURLToRepo string \`json:"http_url_to_repo"\``

### 7d. `internal/prompt/templates/server_review.tmpl`

末尾追加：

```
{{if .HasLocalRepo}}

Note: The full repository source code is available in your working directory.
You can browse files, read implementations, and examine the broader codebase context beyond the diff.
{{end}}
```

## 文件清单

| 文件 | 操作 | 步骤 |
|------|------|------|
| `internal/config/config.go` | 修改 | 1 |
| `cmd/review.go` | 修改 | 2, 6 |
| `internal/context/gatherer.go` | 修改 | 3 |
| `internal/context/adapter.go` | 修改 | 3 |
| `internal/provider/provider.go` | 修改 | 4 |
| `internal/checkout/checkout.go` | **新建** | 5a |
| `internal/checkout/clone.go` | **新建** | 5b |
| `internal/checkout/checkout_test.go` | **新建** | 5c |
| `internal/server/reviewer.go` | 修改 | 7a |
| `internal/server/server.go` | 修改 | 7b |
| `internal/server/webhook.go` | 修改 | 7c |
| `internal/prompt/templates/server_review.tmpl` | 修改 | 7d |

## 错误处理

clone/worktree 失败**绝不阻断** review。`Checkout()` 返回 `Result{Available: false}`，调用方退化为 diff-only 模式。

## 验证

1. `go test ./internal/checkout/...` — 新包单元测试（含并发隔离测试）
2. `go test ./...` — 确保现有测试不破
3. `go build ./...` — 编译通过
