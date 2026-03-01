# Plan: GitLab MR Webhook 自动触发 Review

## Context

Hydra 目前是纯 CLI 工具。用户希望在 GitLab 创建 Merge Request 时自动触发 Hydra review，并将结果推送回 GitLab MR（逐条 inline comment + 一条总结评论）。

**方案：** `hydra serve` 启动 webhook server（`net/http` 标准库），接收 GitLab MR 事件，复用现有 `GitLabPlatform` + `glab api` 获取 diff 和发布评论（server 上需安装并配置 glab）。

## Implementation

### Step 1: 修改 `internal/platform/gitlab/gitlab.go` — 3 个方法改用 `glab api`

现有 `GetDiff`、`GetInfo`、`GetHeadCommitInfo` 使用 `glab mr` 命令（需要在 git 仓库目录中执行）。Server 模式下没有 git clone，但 webhook payload 提供了 `project.path_with_namespace`，可以通过 `glab api` 显式指定项目路径。

**修改前（以 `GetDiff` 为例）：**
```go
func (g *GitLabPlatform) GetDiff(mrID, repo string) (string, error) {
    out, err := exec.Command("glab", "mr", "diff", mrID).Output()  // 需要 git 上下文
    ...
}
```

**修改后：**
```go
func (g *GitLabPlatform) GetDiff(mrID, repo string) (string, error) {
    // 有 repo 参数时用 glab api（server 模式），否则用 glab mr（CLI 模式，向后兼容）
    if repo != "" {
        return g.getDiffViaAPI(mrID, repo)
    }
    out, err := exec.Command("glab", "mr", "diff", mrID).Output()
    ...
}

func (g *GitLabPlatform) getDiffViaAPI(mrID, repo string) (string, error) {
    encoded := encodeProject(repo)
    out, err := exec.Command("glab", "api",
        fmt.Sprintf("projects/%s/merge_requests/%s/diffs", encoded, mrID),
    ).Output()
    // API 返回 JSON 数组，每个元素有 old_path, new_path, diff 字段
    // 拼接为 unified diff 格式: "--- a/old_path\n+++ b/new_path\n{diff}\n"
    ...
}
```

**同样修改 `GetInfo` 和 `GetHeadCommitInfo`：**
- `GetInfo`: `repo != ""` 时用 `glab api projects/:id/merge_requests/:iid` 解析 title + description
- `GetHeadCommitInfo`: `repo != ""` 时用 `glab api projects/:id/merge_requests/:iid` 解析 diff_refs

**影响范围：** 仅添加新的 `if repo != ""` 分支，原有 `glab mr` 逻辑不变，CLI 模式完全向后兼容。

### Step 2: 创建 `internal/server/webhook.go` — Webhook 解析与验证

```go
type MergeRequestEvent struct {
    ObjectKind       string       `json:"object_kind"`       // "merge_request"
    Project          ProjectInfo  `json:"project"`
    ObjectAttributes MRAttributes `json:"object_attributes"`
}

type ProjectInfo struct {
    ID                int    `json:"id"`
    PathWithNamespace string `json:"path_with_namespace"` // "group/project"
    WebURL            string `json:"web_url"`
}

type MRAttributes struct {
    IID          int    `json:"iid"`
    Title        string `json:"title"`
    Description  string `json:"description"`
    State        string `json:"state"`          // "opened"/"merged"/"closed"
    Action       string `json:"action"`         // "open"/"update"/"close"/"merge"
    SourceBranch string `json:"source_branch"`
    TargetBranch string `json:"target_branch"`
    URL          string `json:"url"`
}
```

- `ValidateWebhookRequest(r, secret)` — 校验 `X-Gitlab-Token` header（`subtle.ConstantTimeCompare`）
- `ShouldTriggerReview(event)` — 仅 action=`open`/`reopen`/`update` 且 state=`opened` 触发；跳过 Title 以 "Draft:" 或 "WIP:" 开头的 MR

### Step 3: 创建 `internal/display/noop.go` — Server 模式 Display

实现 `orchestrator.DisplayCallbacks` 接口（定义在 `internal/orchestrator/types.go:162`，6 个方法），用 `*log.Logger` 记录关键事件：

- `OnWaiting` → log reviewer ID
- `OnMessage` → log 截断到 200 字符
- `OnParallelStatus` → log round + reviewer count
- `OnRoundComplete` → log round + converged
- `OnConvergenceJudgment` → log verdict
- `OnContextGathered` → log module/PR count

注意：`cmd/review.go` 中的 `SpinnerStart`, `ReviewHeader`, `FinalConclusion`, `IssuesTable`, `TokenUsage` 等方法不属于此接口，在 `reviewer.go` 中直接用 logger 替代。

### Step 4: 创建 `internal/server/reviewer.go` — Review Pipeline 桥接

```go
func RunServerReview(ctx context.Context, event *MergeRequestEvent,
    plat platform.Platform, cfg *config.HydraConfig, logger *log.Logger) error
```

流程（镜像 `cmd/review.go:runReview` L62-213）：

1. 从 webhook 提取 `repo = event.Project.PathWithNamespace`, `mrID = event.ObjectAttributes.IID`
2. 调用 `plat.GetDiff(mrID, repo)` 和 `plat.GetInfo(mrID, repo)` 获取 MR 信息
3. 构建 review prompt（格式同 `cmd/review.go` L362）
4. 创建 providers — 复用 `provider.CreateProvider(rc.Model, cfg)`
5. 创建 orchestrator — 复用 `orchestrator.New(oCfg)`，**ContextGatherer 设为 nil**（server 无本地文件系统）
6. 运行 `orch.RunStreaming(ctx, label, prompt, noopDisplay)`
7. **逐条 inline comment：** 调用 `plat.PostIssuesAsComments(mrID, platIssues, repo)` 将每个 issue 精确定位到代码行（三级降级：行内 → 文件级 → 全局）
8. **总结评论（新增）：** 通过 `glab api` 发一条 MR note，内容为 `result.FinalConclusion`（整体审核结论 + 问题优先级列表）

### Step 5: 创建 `internal/server/server.go` — HTTP Server

```go
type ServerConfig struct {
    HydraConfig   *config.HydraConfig
    Addr          string          // ":8080"
    WebhookSecret string
    MaxConcurrent int             // 默认 3
    GitLabHost    string          // "gitlab.com" 或自托管域名
}

type Server struct {
    cfg      ServerConfig
    sem      chan struct{}    // 并发信号量
    inFlight sync.Map        // "projectPath/mrIID" → bool，去重
    logger   *log.Logger
}
```

**路由（`net/http` 标准库）：**
- `POST /webhook/gitlab` → 验证 + 触发异步 review
- `GET /health` → `{"status":"ok"}`

**并发与去重：**
```go
func (s *Server) triggerReview(event *MergeRequestEvent) {
    key := fmt.Sprintf("%s/%d", event.Project.PathWithNamespace, event.ObjectAttributes.IID)

    // 去重：同一 MR 正在 review 时跳过（防止连续 push 触发多次）
    if _, loaded := s.inFlight.LoadOrStore(key, true); loaded {
        s.logger.Printf("[skip] MR %s already in review", key)
        return
    }
    defer s.inFlight.Delete(key)

    // 信号量：限制并发数
    select {
    case s.sem <- struct{}{}:
        defer func() { <-s.sem }()
    default:
        s.logger.Printf("[skip] max concurrent reviews reached, dropping %s", key)
        return
    }

    plat := gitlab.New(s.cfg.GitLabHost) // 复用现有 GitLabPlatform
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()

    if err := RunServerReview(ctx, event, plat, s.cfg.HydraConfig, s.logger); err != nil {
        s.logger.Printf("[error] %s: %v", key, err)
    }
}
```

**Webhook handler：** 验证后立即返回 HTTP 202 Accepted（避免 GitLab 10s 超时），`go s.triggerReview(event)` 异步执行。

**生命周期：** SIGINT/SIGTERM → graceful shutdown（30s 超时等待 in-flight review）。

### Step 6: 创建 `cmd/serve.go` — Cobra 命令

| Flag | Env | Default | 说明 |
|------|-----|---------|------|
| `--config, -c` | - | `~/.hydra/config.yaml` | Hydra 配置文件 |
| `--addr` | `HYDRA_ADDR` | `:8080` | 监听地址 |
| `--webhook-secret` | `HYDRA_WEBHOOK_SECRET` | （必填） | Webhook 验证密钥 |
| `--max-concurrent` | - | `3` | 最大并发 review 数 |
| `--gitlab-host` | `GITLAB_HOST` | `gitlab.com` | GitLab 域名 |

**注册：** 在 `cmd/root.go:init()` 添加 `rootCmd.AddCommand(serveCmd)`

注意：GitLab API Token 不通过 hydra 管理，由 `glab auth login` 或 `GITLAB_TOKEN` 环境变量提供（glab 自身的认证机制）。

### Step 7: 测试

**`internal/server/webhook_test.go`：**
- `ValidateWebhookRequest` 有效/无效 secret
- `ShouldTriggerReview` 各种 action（open/update/close/merge）
- Draft MR 跳过

**`internal/server/server_test.go`：**
- `httptest.NewRecorder` 测试 HTTP 202/401/400
- 去重：同一 MR 重复 webhook 被跳过

**`internal/platform/gitlab/gitlab_test.go`（扩展）：**
- `getDiffViaAPI` JSON → unified diff 拼接
- `getInfoViaAPI` JSON 解析

**`internal/display/noop_test.go`：**
- 所有 6 个回调方法不 panic

## GitLab MR 上的效果

收到 webhook 后，MR 页面会出现：

1. **多条 inline comments** — 每个问题精确挂在对应代码行旁（支持三级降级定位）
2. **一条 summary note** — 整体审核结论 + 问题优先级汇总表

## Files

| 操作 | 文件 | 说明 |
|------|------|------|
| **修改** | `internal/platform/gitlab/gitlab.go` | GetDiff/GetInfo/GetHeadCommitInfo 增加 `glab api` 分支 |
| **新建** | `internal/server/webhook.go` | Webhook 事件结构体 + 验证 + 触发判断 |
| **新建** | `internal/server/server.go` | HTTP server + 路由 + 并发控制 + 去重 |
| **新建** | `internal/server/reviewer.go` | Server 模式 review 执行器 |
| **新建** | `internal/server/webhook_test.go` | Webhook 解析测试 |
| **新建** | `internal/server/server_test.go` | Server handler 测试 |
| **新建** | `internal/display/noop.go` | Server 模式 DisplayCallbacks |
| **新建** | `internal/display/noop_test.go` | NoopDisplay 测试 |
| **新建** | `cmd/serve.go` | `hydra serve` 命令 |
| **修改** | `cmd/root.go` | 注册 `serveCmd` |
| **扩展** | `internal/platform/gitlab/gitlab_test.go` | glab api 模式测试 |

## Verification

1. `go build ./...` — 编译通过
2. `go test ./...` — 所有测试通过（包括现有 CLI 模式测试不受影响）
3. `go vet ./...` — 无警告
4. 手动测试：
   ```bash
   # Server 上需要 glab 已认证
   export GITLAB_TOKEN=glpat-xxx

   # 启动 server
   hydra serve -c config.yaml --webhook-secret mysecret --gitlab-host gitlab.example.com

   # 模拟 webhook
   curl -X POST http://localhost:8080/webhook/gitlab \
     -H "X-Gitlab-Token: mysecret" \
     -H "Content-Type: application/json" \
     -d '{"object_kind":"merge_request","project":{"id":123,"path_with_namespace":"group/project","web_url":"https://gitlab.example.com/group/project"},"object_attributes":{"iid":1,"action":"open","state":"opened","title":"Test MR","source_branch":"feature","target_branch":"main","url":"https://gitlab.example.com/group/project/-/merge_requests/1"}}'

   # 健康检查
   curl http://localhost:8080/health
   ```
