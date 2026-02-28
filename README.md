# Hydra - 多模型对抗式代码审查工具

Hydra 是一个用 Go 编写的 CLI 工具，利用多个 AI 模型独立审查代码变更，然后通过结构化辩论机制产出高质量的综合审查结果。其核心理念是：**不同的 AI 模型从不同角度审查代码，通过辩论和交叉验证发现单一模型可能遗漏的问题。**

## 核心特性

- **多模型并行审查** - 支持 Claude Code CLI 和 Codex CLI 等多个 AI 提供者同时审查
- **结构化辩论** - 第 1 轮独立审查，第 2 轮起交叉验证、质疑和补充
- **收敛检测** - 自动检测审查者是否达成共识，提前结束辩论以节省 token
- **智能问题提取** - 从审查讨论中提取结构化 JSON 问题，使用 Jaccard 相似度去重
- **GitHub 集成** - 自动发布行内评论到 PR，支持三级降级（行内 → 文件级 → 全局）
- **上下文收集** - 可选收集调用链、相关 PR 历史和文档来丰富审查内容
- **流式输出** - 实时显示审查者的响应，配有彩色终端 UI 和加载动画

## 架构概览

```
                        ┌────────────────────┐
                        │    hydra review     │  CLI 入口
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
              │Claude Code│ │Codex CLI│ │   Mock    │
              │  Provider │ │Provider │ │ Provider  │
              └───────────┘ └─────────┘ └───────────┘
```

### 数据流

```
1. 输入 (PR/本地变更/分支)
       │
2. ┌───┴───┐ 并行预处理 (errgroup)
   │上下文  │ 收集调用链、PR 历史、文档
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
   ├─ GitHub PR 评论发布
   └─ Markdown/JSON 文件导出
```

## 项目结构

```
hydra/
├── main.go                          # 程序入口
├── go.mod                           # Go 模块定义 (Go 1.24)
├── cmd/                             # CLI 命令层
│   ├── root.go                      # Cobra 根命令注册
│   ├── review.go                    # review 子命令：审查目标解析、Provider 创建、编排执行
│   └── init.go                      # init 子命令：交互式生成配置文件
├── internal/                        # 内部包（不对外暴露）
│   ├── config/                      # 配置管理
│   │   ├── config.go                # 配置结构体定义、YAML 加载、环境变量展开、校验
│   │   └── config_test.go           # 配置测试
│   ├── provider/                    # AI 提供者抽象层
│   │   ├── provider.go              # 核心接口：AIProvider、SessionProvider、Message
│   │   ├── factory.go               # 工厂函数：按模型名创建 Provider 实例
│   │   ├── claudecode.go            # Claude Code CLI 提供者：通过 os/exec 调用 claude 命令
│   │   ├── codexcli.go              # Codex CLI 提供者：通过 os/exec 调用 codex 命令
│   │   ├── cliprovider.go           # 共享的 CLI 会话管理器（会话 ID、prompt 构建）
│   │   ├── retry.go                 # 指数退避重试（支持超时、限流等瞬时错误）
│   │   ├── factory_test.go          # 工厂测试
│   │   └── retry_test.go            # 重试测试
│   ├── orchestrator/                # 辩论编排器（核心逻辑）
│   │   ├── orchestrator.go          # DebateOrchestrator：上下文收集→分析→辩论→总结→问题提取
│   │   ├── types.go                 # 类型定义：Reviewer、DebateResult、ReviewIssue 等
│   │   ├── issueparser.go           # JSON 问题解析 + Jaccard 相似度去重
│   │   └── issueparser_test.go      # 问题解析测试
│   ├── github/                      # GitHub 集成
│   │   ├── commenter.go             # PR 评论发布（行内/文件级/全局三级降级）
│   │   ├── diff.go                  # Diff 解析，提取有效行号
│   │   └── diff_test.go             # Diff 解析测试
│   ├── context/                     # 上下文收集器
│   │   ├── gatherer.go              # 主编排：收集引用、历史、文档，调用 AI 分析
│   │   ├── types.go                 # 上下文相关类型定义
│   │   ├── adapter.go               # 适配器：将 context 包类型转换为 orchestrator 包类型
│   │   ├── reference.go             # 调用链分析：从 diff 提取符号，用 ripgrep 查找引用
│   │   ├── history.go               # 相关 PR 历史收集
│   │   ├── docs.go                  # 文档文件收集
│   │   ├── prompt.go                # AI 分析 prompt 构建
│   │   └── reference_test.go        # 引用分析测试
│   ├── display/                     # 终端 UI 和输出格式化
│   │   ├── terminal.go              # 彩色终端输出、spinner 动画、审查进度显示
│   │   └── markdown.go              # Markdown 报告生成 + Glamour 终端渲染
│   └── util/
│       └── logger.go                # 分级日志（debug/info/warn/error），通过 HYDRA_LOG_LEVEL 控制
```

## 前置依赖

- **Go 1.24+**
- **[Claude Code CLI](https://github.com/anthropics/claude-code)** (`claude` 命令) - 如果使用 claude-code 提供者
- **[Codex CLI](https://github.com/openai/codex)** (`codex` 命令) - 如果使用 codex-cli 提供者
- **[GitHub CLI](https://cli.github.com/)** (`gh` 命令) - 用于 PR 信息获取和评论发布
- **[ripgrep](https://github.com/BurntSushi/ripgrep)** (`rg` 命令) - 用于上下文收集中的调用链分析

## 安装

```bash
# 克隆仓库
git clone https://github.com/guwanhua/hydra.git
cd hydra

# 编译
go build -o hydra .

# 或直接安装到 $GOPATH/bin
go install .
```

## 快速开始

### 1. 初始化配置

```bash
hydra init
```

这将在 `~/.hydra/config.yaml` 生成默认配置文件。

### 2. 审查 GitHub PR

```bash
# 通过 PR 编号审查
hydra review 42

# 通过 PR URL 审查
hydra review https://github.com/owner/repo/pull/42
```

### 3. 审查本地变更

```bash
# 审查未提交的本地变更
hydra review --local

# 审查当前分支 vs main 的变更
hydra review --branch main

# 审查指定文件
hydra review --files main.go,cmd/review.go
```

## CLI 用法

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
      --no-post            跳过 GitHub 评论发布
```

## 配置说明

配置文件位于 `~/.hydra/config.yaml`：

```yaml
# AI 提供者配置
providers:
  claude-code:
    enabled: true
  codex-cli:
    enabled: true

# 默认设置
defaults:
  max_rounds: 3          # 最大辩论轮数
  output_format: markdown # 输出格式
  check_convergence: true # 启用收敛检测

# 分析器：预分析 PR 变更，提取关注点
analyzer:
  model: claude-code
  prompt: |
    You are a senior code analyst...

# 总结者：判断收敛、生成最终结论
summarizer:
  model: claude-code
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

# 可选：上下文收集配置
contextGatherer:
  enabled: true
  callChain:
    maxDepth: 2            # 调用链最大深度
    maxFilesToAnalyze: 20  # 最大分析文件数
  history:
    maxDays: 30            # 查找历史 PR 的天数范围
    maxPRs: 10             # 最大相关 PR 数
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
```

## 三个 AI 角色

| 角色 | 职责 | 时机 |
|------|------|------|
| **Analyzer（分析器）** | 预分析 PR 变更，提取摘要和建议关注点 | 与上下文收集并行，在第 1 轮之前 |
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

## GitHub 评论降级策略

当发布审查评论到 PR 时，采用三级降级策略：

1. **行内评论** (Inline) - 如果行号在 diff 有效范围内，直接发到对应行
2. **文件级评论** (File-level) - 如果文件在 diff 中但行号无效，发到文件级
3. **全局评论** (Global) - 如果文件不在 diff 中，作为 PR 全局评论发布

评论发布前会检查已有评论，避免重复发布。

## 关键设计决策

1. **无 AI SDK 依赖** - 通过 `os/exec` 调用 CLI 工具，与 API 变更解耦
2. **errgroup 并行执行** - 审查者并行运行，上下文收集与分析并行
3. **会话复用** - CLI 提供者通过会话 ID 复用连接，避免重复发送完整历史
4. **流式输出** - 实时显示审查者响应
5. **优雅降级** - GitHub 集成、上下文收集、收敛检测均为非致命性故障

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
