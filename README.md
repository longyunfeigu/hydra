# Hydra - 多模型对抗式代码审查工具

Hydra 是一个用 Go 编写的 CLI 工具，利用多个 AI 模型独立审查代码变更，然后通过结构化辩论机制产出高质量的综合审查结果。其核心理念是：**不同的 AI 模型从不同角度审查代码，通过辩论和交叉验证发现单一模型可能遗漏的问题。**

## 核心特性

- **多模型并行审查** - 支持 Claude Code CLI、Codex CLI 和 OpenAI API 等多个 AI 提供者同时审查
- **结构化辩论** - 第 1 轮独立审查，第 2 轮起交叉验证、质疑和补充
- **收敛检测** - 自动检测审查者是否达成共识，提前结束辩论以节省 token
- **智能问题提取** - 从审查讨论中提取结构化 JSON 问题，使用 Jaccard 相似度去重
- **多平台支持** - 同时支持 GitHub PR 和 GitLab MR，自动检测平台类型
- **GitLab Webhook 服务** - 内置 HTTP 服务器，监听 GitLab MR 事件自动触发审查
- **评论降级策略** - 自动发布行内评论到 PR/MR，支持三级降级（行内 → 文件级 → 全局）
- **上下文收集** - 可选收集调用链、相关 PR/MR 历史和文档来丰富审查内容
- **流式输出** - 实时显示审查者的响应，配有彩色终端 UI 和加载动画

## 架构概览

```
                        ┌────────────────────┐
                        │    hydra review     │  CLI 入口
                        │    hydra serve      │  Webhook 服务
                        └─────────┬──────────┘
                                  │
                    ┌─────────────┴─────────────┐
                    │     Orchestrator 编排器     │  核心调度
                    └─────────────┬─────────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              │                   │                   │
     ┌────────┴────────┐ ┌───────┴───────┐ ┌────────┴────────┐
     │   Analyzer 分析  │ │ Reviewers 审查 │ │ Summarizer 总结 │
     │   预分析变更内容  │ │ 多模型并行审查  │ │ 收敛判断+最终结论 │
     └────────┬────────┘ └───────┬───────┘ └────────┬────────┘
              │                   │                   │
              └───────────────────┼───────────────────┘
                                  │
                    ┌─────────────┴─────────────┐
                    │    Provider 抽象层         │
                    │  AIProvider / Session 接口  │
                    └─────────────┬─────────────┘
                                  │
                    ┌─────────────┼─────────────┐
                    │             │             │
              ┌─────┴─────┐ ┌────┴────┐ ┌─────┴─────┐
              │Claude Code│ │Codex CLI│ │  OpenAI   │
              │  Provider │ │Provider │ │  API      │
              └───────────┘ └─────────┘ └───────────┘
                                  │
                    ┌─────────────┴─────────────┐
                    │    Platform 平台抽象层      │
                    │  GitHub / GitLab 统一接口   │
                    └─────────────┬─────────────┘
                                  │
                         ┌────────┼────────┐
                         │                 │
                   ┌─────┴─────┐   ┌──────┴──────┐
                   │  GitHub   │   │   GitLab    │
                   │  (gh CLI) │   │ (glab CLI)  │
                   └───────────┘   └─────────────┘
```

### 数据流

```
1. 输入 (PR/MR/本地变更/分支/Webhook)
       │
2. ┌───┴───┐ 平台检测 + 并行预处理 (errgroup)
   │平台检│ 自动检测 GitHub/GitLab
   │测    │
   ├───────┤
   │上下文  │ 收集调用链、PR/MR 历史、文档
   │收集器  │
   ├───────┤
   │分析器  │ 预分析变更内容，提取关注点
   └───┬───┘
       │
3. 辩论轮次 (1 ~ MaxRounds)
   ├─ 第 1 轮: 各审查者独立分析（并行）
   ├─ 第 2+ 轮: 交叉审查，查看他人反馈
   └─ 每轮后: 收敛判断 → 达成共识则提前结束
       │
4. 后处理
   ├─ 各审查者提交要点总结
   ├─ 总结者生成最终结论
   └─ 提取结构化问题 (JSON) + Jaccard 去重
       │
5. 输出
   ├─ 终端彩色展示
   ├─ GitHub PR / GitLab MR 评论发布
   └─ Markdown/JSON 文件导出
```

## 项目结构

```
hydra/
├── main.go                          # 程序入口
├── go.mod                           # Go 模块定义 (Go 1.24)
├── cmd/                             # CLI 命令层
│   ├── root.go                      # Cobra 根命令注册（review、init、serve）
│   ├── review.go                    # review 子命令：平台检测、Provider 创建、编排执行
│   ├── init.go                      # init 子命令：交互式生成配置文件
│   └── serve.go                     # serve 子命令：GitLab Webhook HTTP 服务器
├── internal/                        # 内部包（不对外暴露）
│   ├── config/                      # 配置管理
│   │   └── config.go                # 配置结构体定义、YAML 加载、环境变量展开、校验
│   ├── provider/                    # AI 提供者抽象层
│   │   ├── provider.go              # 核心接口：AIProvider、SessionProvider、Message
│   │   ├── factory.go               # 工厂函数：按模型名创建 Provider 实例
│   │   ├── claudecode.go            # Claude Code CLI 提供者：通过 os/exec 调用 claude 命令
│   │   ├── codexcli.go              # Codex CLI 提供者：通过 os/exec 调用 codex 命令
│   │   ├── openai.go                # OpenAI API 提供者：直接 HTTP 调用，支持 SSE 流式
│   │   ├── cliprovider.go           # 共享的 CLI 会话管理器（会话 ID、prompt 构建）
│   │   └── retry.go                 # 指数退避重试（支持超时、限流等瞬时错误）
│   ├── orchestrator/                # 辩论编排器（核心逻辑）
│   │   ├── orchestrator.go          # DebateOrchestrator：上下文收集→分析→辩论→总结→问题提取
│   │   ├── types.go                 # 类型定义：Reviewer、DebateResult、ReviewIssue 等
│   │   └── issueparser.go           # JSON 问题解析 + Jaccard 相似度去重
│   ├── platform/                    # 多平台抽象层
│   │   ├── platform.go              # Platform 统一接口 + 共享类型定义
│   │   ├── format.go                # 共享格式化工具（问题正文、严重度徽章）
│   │   ├── diffparse.go             # Diff 解析、评论分类（行内/文件/全局）、去重
│   │   ├── detect/
│   │   │   └── detect.go            # 平台自动检测：从 git remote URL 识别 GitHub/GitLab
│   │   ├── github/
│   │   │   └── github.go            # GitHub 平台实现：通过 gh CLI 操作 PR
│   │   └── gitlab/
│   │       └── gitlab.go            # GitLab 平台实现：通过 glab CLI 操作 MR
│   ├── server/                      # GitLab Webhook 服务器
│   │   ├── server.go                # HTTP 服务器：路由、并发控制、去重、优雅关闭
│   │   ├── webhook.go               # Webhook 事件解析、Secret 验证、触发条件过滤
│   │   └── reviewer.go              # 服务端审查流程：非交互式审查 + 评论发布
│   ├── context/                     # 上下文收集器
│   │   ├── gatherer.go              # 主编排：收集引用、历史、文档，调用 AI 分析
│   │   ├── types.go                 # 上下文相关类型定义
│   │   ├── adapter.go               # 适配器：将 context 包类型转换为 orchestrator 包类型
│   │   ├── reference.go             # 调用链分析：从 diff 提取符号，用 ripgrep 查找引用
│   │   ├── history.go               # 相关 PR/MR 历史收集
│   │   ├── docs.go                  # 文档文件收集
│   │   └── prompt.go                # AI 分析 prompt 构建
│   ├── display/                     # 终端 UI 和输出格式化
│   │   ├── terminal.go              # 彩色终端输出、spinner 动画、审查进度显示
│   │   ├── markdown.go              # Markdown 报告生成 + Glamour 终端渲染
│   │   └── noop.go                  # 静默显示（服务端模式，日志替代终端 UI）
│   └── util/
│       └── logger.go                # 分级日志（debug/info/warn/error），通过 HYDRA_LOG_LEVEL 控制
```

## 安装

### 1. 安装 Hydra

```bash
# 需要 Go 1.24+
go version

# 克隆仓库
git clone https://github.com/guwanhua/hydra.git
cd hydra

# 编译
go build -o hydra .

# 或直接安装到 $GOPATH/bin
go install .
```

### 2. 安装依赖工具

根据你的使用场景，安装对应的工具：

#### 平台 CLI（用于获取 PR/MR 信息和发布评论）

**GitHub CLI** (`gh`) - 审查 GitHub PR 时必需：

```bash
# macOS
brew install gh

# Debian/Ubuntu
sudo apt install gh

# 安装后认证
gh auth login
```

**GitLab CLI** (`glab`) - 审查 GitLab MR 时必需：

```bash
# macOS
brew install glab

# Debian/Ubuntu（通过官方仓库）
# 参考 https://gitlab.com/gitlab-org/cli#installation

# 通过 Go 安装
go install gitlab.com/gitlab-org/cli/cmd/glab@latest

# 安装后认证（二选一）
glab auth login                          # 交互式登录
export GITLAB_TOKEN=your-access-token    # 或通过环境变量

# 自托管 GitLab 需要指定 host
glab auth login --hostname gitlab.company.com
```

#### AI 提供者（至少安装一个）

**Claude Code CLI** (`claude`) - claude-code 提供者：

```bash
# 通过 npm 安装
npm install -g @anthropic-ai/claude-code

# 验证
claude --version
```

**Codex CLI** (`codex`) - codex-cli 提供者：

```bash
# 通过 npm 安装
npm install -g @openai/codex

# 验证
codex --version
```

**OpenAI API** - openai 提供者（无需安装 CLI，需配置 API Key）：

```bash
# 在配置文件中设置，或通过环境变量
export OPENAI_API_KEY=sk-your-key
```

#### 可选工具

**ripgrep** (`rg`) - 上下文收集中的调用链分析：

```bash
# macOS
brew install ripgrep

# Debian/Ubuntu
sudo apt install ripgrep
```

### 3. 依赖总结

| 工具 | 命令 | 何时需要 | 安装方式 |
|------|------|----------|----------|
| Go 1.24+ | `go` | 编译 Hydra | [golang.org](https://golang.org/dl/) |
| GitHub CLI | `gh` | 审查 GitHub PR | `brew install gh` / `apt install gh` |
| GitLab CLI | `glab` | 审查 GitLab MR | `brew install glab` / `go install gitlab.com/gitlab-org/cli/cmd/glab@latest` |
| Claude Code | `claude` | 使用 claude-code 提供者 | `npm install -g @anthropic-ai/claude-code` |
| Codex CLI | `codex` | 使用 codex-cli 提供者 | `npm install -g @openai/codex` |
| OpenAI API Key | - | 使用 openai 提供者 | 配置 `OPENAI_API_KEY` 环境变量 |
| ripgrep | `rg` | 上下文收集（可选） | `brew install ripgrep` / `apt install ripgrep` |

## 快速开始

### 1. 初始化配置

```bash
hydra init
```

这将在 `~/.hydra/config.yaml` 生成默认配置文件。

### 2. 审查 GitHub PR

前置条件：`gh auth login` 完成认证。

```bash
# 在 GitHub 项目目录中，通过 PR 编号审查
cd /path/to/your-github-repo
hydra review 42

# 通过 PR URL 审查（可在任意目录执行）
hydra review https://github.com/owner/repo/pull/42

# 指定审查者
hydra review 42 --reviewers claude,codex

# 审查但不发布评论到 PR
hydra review 42 --no-post

# 保存审查结果到文件
hydra review 42 -o review-result.md
```

### 3. 审查 GitLab MR

前置条件：`glab auth login` 完成认证。

Hydra 会从 `git remote` URL 自动检测平台类型。如果你的项目 remote 指向 `gitlab.com`，会自动使用 GitLab 模式。

```bash
# 在 GitLab 项目目录中，通过 MR 编号审查
cd /path/to/your-gitlab-repo
hydra review 42

# 通过 MR URL 审查
hydra review https://gitlab.com/group/project/-/merge_requests/42

# 指定使用 claude 和 codex 审查
hydra review 42 --reviewers claude,codex

# 使用所有配置的审查者
hydra review 42 --all
```

**自托管 GitLab**：如果使用自托管 GitLab，需要在配置中指定平台信息：

```yaml
# ~/.hydra/config.yaml
platform:
  type: gitlab
  host: gitlab.company.com    # 你的 GitLab 域名
```

同时确保 `glab` 已对该 host 认证：

```bash
glab auth login --hostname gitlab.company.com
```

### 4. 审查本地变更

无需平台 CLI，直接审查本地 git 变更：

```bash
# 审查未提交的本地变更（如果无未提交变更，自动审查最近一次 commit）
hydra review --local

# 审查当前分支 vs main 的变更
hydra review --branch main

# 审查指定文件
hydra review --files main.go,cmd/review.go
```

### 5. 启动 GitLab Webhook 服务

部署为长期运行的服务，GitLab 新建/更新 MR 时自动触发审查：

```bash
# 启动 webhook 服务器
hydra serve --webhook-secret your-secret

# 自定义监听地址和并发数
hydra serve --addr :9090 --max-concurrent 5 --webhook-secret your-secret

# 自托管 GitLab
hydra serve --gitlab-host gitlab.company.com --webhook-secret your-secret

# 通过环境变量配置
export HYDRA_WEBHOOK_SECRET=your-secret
export HYDRA_ADDR=:8080
export GITLAB_HOST=gitlab.company.com
hydra serve
```

在 GitLab 项目 Settings > Webhooks 中配置：
- **URL**: `http://your-server:8080/webhook/gitlab`
- **Secret Token**: 与 `--webhook-secret` 一致
- **Trigger**: 勾选 **Merge request events**

服务会自动过滤：仅处理 open/reopen/update 状态的 MR，跳过 Draft/WIP。

### 使用示例汇总

```bash
# === GitHub PR 审查 ===
hydra review 42                                              # PR 编号
hydra review https://github.com/owner/repo/pull/42           # PR URL
hydra review 42 --reviewers claude,codex --rounds 2          # 指定审查者和轮数

# === GitLab MR 审查 ===
hydra review 42                                              # MR 编号（自动检测 GitLab）
hydra review https://gitlab.com/group/project/-/merge_requests/42  # MR URL

# === 本地审查 ===
hydra review --local                                         # 未提交变更
hydra review --branch main                                   # 分支对比
hydra review --files "internal/server/*.go"                  # 指定文件

# === 通用选项 ===
hydra review 42 --no-post                                    # 不发布评论
hydra review 42 --skip-context                               # 跳过上下文收集
hydra review 42 -o result.md                                 # 输出到文件
hydra review 42 -f json -o result.json                       # JSON 格式输出
HYDRA_LOG_LEVEL=debug hydra review 42                        # 调试模式
```

## CLI 用法

### hydra review

```
hydra review [pr-number-or-url] [flags]

Flags:
  -c, --config string      配置文件路径（默认 ~/.hydra/config.yaml）
  -r, --rounds int         最大辩论轮数（覆盖配置）
  -o, --output string      输出保存到文件
  -f, --format string      输出格式：markdown | json（默认 "markdown"）
      --no-converge        禁用收敛检测
  -l, --local              审查本地未提交变更
      --branch string      审查当前分支 vs 指定基准分支
      --files strings      审查指定文件
      --reviewers string   逗号分隔的审查者 ID
  -a, --all                使用所有审查者
      --skip-context       跳过上下文收集
      --no-post            跳过平台评论发布
```

### hydra serve

```
hydra serve [flags]

Flags:
  -c, --config string          配置文件路径（默认 ~/.hydra/config.yaml）
      --addr string            监听地址（默认 ":8080"，环境变量 HYDRA_ADDR）
      --webhook-secret string  GitLab Webhook Secret（必填，环境变量 HYDRA_WEBHOOK_SECRET）
      --max-concurrent int     最大并发审查数（默认 3）
      --gitlab-host string     GitLab 主机地址（默认 "gitlab.com"，环境变量 GITLAB_HOST）
```

**端点：**
- `POST /webhook/gitlab` - 接收 GitLab MR Webhook 事件
- `GET /health` - 健康检查

**行为：**
- 收到 MR 事件后立即返回 202 Accepted，异步执行审查
- 自动过滤：仅处理 open/reopen/update 动作，跳过 Draft/WIP MR
- 并发控制：通过信号量限制同时进行的审查数
- 去重：同一 MR 不会重复触发审查
- 单次审查超时 10 分钟，服务优雅关闭超时 30 秒

## 配置说明

配置文件位于 `~/.hydra/config.yaml`：

```yaml
# 平台配置（可选）
platform:
  type: auto           # auto | github | gitlab（默认 auto，从 git remote 自动检测）
  host: ""             # 自托管 GitLab 地址，如 gitlab.company.com

# AI 提供者配置
providers:
  claude-code:
    enabled: true
  codex-cli:
    enabled: true
  openai:
    api_key: ${OPENAI_API_KEY}
    base_url: ""       # 可选，自定义 API 端点（支持 Azure OpenAI、Ollama 等兼容 API）

# 默认设置
defaults:
  max_rounds: 3          # 最大辩论轮数
  output_format: markdown # 输出格式
  check_convergence: true # 启用收敛检测

# 分析器：预分析变更，提取关注点
analyzer:
  model: claude-code
  prompt: |
    You are a senior code analyst...

# 总结者：判断收敛、生成最终结论、提取结构化问题
# ⚠️ 重要：summarizer 必须使用 API 模型（如 gpt-4o），不要用 CLI 模型（如 claude-code）。
# 原因：structurizeIssues 阶段要求 summarizer 严格输出 JSON 格式，
# CLI 模型（claude-code/codex-cli）会在响应中附加额外文本/工具调用格式，
# 导致 JSON 解析失败 → ParsedIssues 为空 → 无法自动发布评论到 PR/MR。
summarizer:
  model: gpt-4o   # 必须用 API 模型，不能用 claude-code/codex-cli
  prompt: |
    You are a senior engineering lead...

# 审查者：执行实际的代码审查
reviewers:
  claude:
    model: claude-code
    prompt: |
      You are a thorough code reviewer...
  codex:
    model: codex-cli
    prompt: |
      You are a pragmatic code reviewer...
  gpt4o:
    model: gpt-4o
    prompt: |
      You are an experienced code reviewer...

# 可选：上下文收集配置
contextGatherer:
  enabled: true
  callChain:
    maxDepth: 2            # 调用链最大深度
    maxFilesToAnalyze: 20  # 最大分析文件数
  history:
    maxDays: 30            # 查找历史 PR/MR 的天数范围
    maxPRs: 10             # 最大相关 PR/MR 数
  docs:
    patterns:              # 文档搜索模式
      - "docs"
      - "README.md"
    maxSize: 50000         # 单文件最大大小（字节）
```

### 环境变量

配置文件中支持 `${VAR}` 语法引用环境变量：

```yaml
providers:
  claude-code:
    api_key: ${ANTHROPIC_API_KEY}
  openai:
    api_key: ${OPENAI_API_KEY}
```

Webhook 服务相关环境变量：

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `HYDRA_ADDR` | 服务监听地址 | `:8080` |
| `HYDRA_WEBHOOK_SECRET` | GitLab Webhook Secret | 无（必填） |
| `GITLAB_HOST` | 自托管 GitLab 地址 | `gitlab.com` |

## AI 提供者

| 提供者 | 类型 | 模型示例 | 说明 |
|--------|------|----------|------|
| **claude-code** | CLI | claude-code | 通过 `claude` CLI 调用，支持会话复用 |
| **codex-cli** | CLI | codex-cli | 通过 `codex` CLI 调用，支持会话复用 |
| **openai** | API | gpt-4o, o1-*, o3-* | 直接 HTTP 调用 OpenAI API，支持 SSE 流式 |

OpenAI 提供者支持自定义 `base_url`，兼容 Azure OpenAI、Ollama 等 OpenAI 兼容 API。

## 三个 AI 角色

| 角色 | 职责 | 时机 |
|------|------|------|
| **Analyzer（分析器）** | 预分析变更，提取摘要和建议关注点 | 与上下文收集并行，在第 1 轮之前 |
| **Reviewers（审查者）** | 执行代码审查和辩论，输出结构化问题 | 多轮辩论，第 2 轮起可看到他人反馈 |
| **Summarizer（总结者）** | 收敛判断、最终结论生成、问题结构化提取 | 每轮后（收敛检测）+ 辩论结束后 |

## 辩论流程

```
第 1 轮: 独立审查
  ├─ Reviewer A: 独立分析代码变更
  └─ Reviewer B: 独立分析代码变更
         ↓
第 2 轮: 交叉审查
  ├─ Reviewer A: 阅读 B 的反馈，补充遗漏，挑战弱点
  └─ Reviewer B: 阅读 A 的反馈，补充遗漏，挑战弱点
         ↓
  收敛检查: Summarizer 判断是否达成共识
    ├─ CONVERGED → 提前结束，节省 token
    └─ NOT_CONVERGED → 继续下一轮
         ↓
第 N 轮: 继续交叉审查...
         ↓
总结阶段:
  ├─ 各审查者提交要点总结
  ├─ Summarizer 生成最终结论
  └─ Summarizer 提取结构化 JSON 问题（最多重试 3 次）
```

## 平台评论降级策略

当发布审查评论到 PR/MR 时，采用三级降级策略：

1. **行内评论** (Inline) - 如果行号在 diff 有效范围内，直接发到对应行
2. **文件级评论** (File-level) - 如果文件在 diff 中但行号无效，发到文件级
3. **全局评论** (Global) - 如果文件不在 diff 中，作为 PR/MR 全局评论发布

评论发布前会检查已有评论，避免重复发布。

**GitLab 特殊支持：**
- 使用 Discussions API 发布行内/文件级评论
- 尝试 Draft Notes API + bulk_publish（Premium 功能，不可用时自动降级）
- 服务端模式下通过 `glab api` 直接调用 GitLab API

## 关键设计决策

1. **无 AI SDK 依赖** - CLI 提供者通过 `os/exec` 调用命令行工具，OpenAI 提供者通过原生 `net/http` 调用 API，均不依赖第三方 SDK
2. **errgroup 并行执行** - 审查者并行运行，上下文收集与分析并行
3. **会话复用** - CLI 提供者通过会话 ID 复用连接，避免重复发送完整历史
4. **流式输出** - 实时显示审查者响应（CLI 和 OpenAI SSE 均支持）
5. **优雅降级** - 平台集成、上下文收集、收敛检测均为非致命性故障
6. **平台抽象** - 统一的 Platform 接口，GitHub 和 GitLab 实现可互换
7. **自动平台检测** - 从 git remote URL 自动识别 GitHub/GitLab，支持手动覆盖
8. **Summarizer 必须用 API 模型** - structurizeIssues 要求严格 JSON 输出，CLI 模型（claude-code/codex-cli）无法可靠生成纯 JSON，会导致评论发布失败

## 日志

通过环境变量 `HYDRA_LOG_LEVEL` 控制日志级别：

```bash
HYDRA_LOG_LEVEL=debug hydra review 42
```

支持的级别：`debug`、`info`（默认）、`warn`、`error`

## 依赖库

| 库 | 用途 |
|---|---|
| [cobra](https://github.com/spf13/cobra) | CLI 框架 |
| [yaml.v3](https://github.com/go-yaml/yaml) | YAML 配置解析 |
| [errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) | 并发 goroutine 管理 |
| [color](https://github.com/fatih/color) | 终端彩色输出 |
| [spinner](https://github.com/briandowns/spinner) | 加载动画 |
| [glamour](https://github.com/charmbracelet/glamour) | 终端 Markdown 渲染 |

## 许可证

MIT
