# Hydra Inline Comment Lifecycle Plan

## 背景

当前 Hydra 对 inline comment 的处理主要是“判重后跳过”：

- 通过 marker 或 `(path, line, body 前缀)` 判断评论是否重复
- 重复则 `skip`
- 不重复则创建新评论

这一策略适合避免刷屏，但不适合管理评论生命周期。现有实现的限制包括：

- 同一问题的描述变了、建议修复变了、严重度变了，无法更新旧评论
- 同一问题挂载位置变了，无法表达“旧评论被新评论替代”
- 某个问题在新一轮 review 中不再出现，无法将旧评论标记为已解决
- summary note 已经支持 upsert，但 inline comment 仍停留在 append-only 语义

目标是把当前策略升级为完整的生命周期管理：

- `create`: 新问题，创建新评论
- `update`: 同一问题仍然存在，但正文内容变了
- `resolve`: 历史问题在本轮未再复现，标记为已解决
- `supersede`: 历史评论被同一问题的新版本评论替代，通常发生在挂载位置变化时
- `noop`: 历史评论与本轮期望状态一致，不做任何动作

第一期不追求平台原生“resolved thread”全覆盖，而是先实现平台无关的逻辑层生命周期管理。

## 设计目标

1. 尽量不改 orchestrator 主流程，优先在 platform 层落地
2. 兼容历史 Hydra 评论，不要求一次性迁移
3. 把“展示正文”和“识别元数据”分离
4. 生命周期判断尽量基于确定性规则，不依赖模型二次推理
5. GitHub 和 GitLab 共享 planner，平台层只负责执行

## 总体方案

整体分为四层：

1. `issue identity`
   用稳定的 `IssueKey` 回答“本次 issue 和历史 issue 是否是同一个问题”
2. `comment metadata`
   在隐藏 marker 中存储 Hydra 自己需要的元数据，而不只存一个短 hash
3. `lifecycle planner`
   输入“当前期望评论集合”和“历史 Hydra 评论集合”，输出 create/update/resolve/supersede/noop
4. `platform executor`
   GitHub/GitLab 分别执行 planner 产出的动作

## 第一阶段的边界

第一期建议只做这些能力：

- 升级 marker 格式，包含可解析元数据
- 构建稳定 `IssueKey`
- 引入 planner
- 支持 `create/update/resolve/supersede/noop`
- GitHub 支持 edit review comment body
- GitLab 支持 edit discussion/note body
- summary note 展示本轮生命周期统计

第一期不做：

- 平台原生 resolved thread/discussion 状态联动
- 跨文件 rename 的智能迁移
- IssueKey 的 AI 归一化
- 多条历史活跃评论的高级收敛策略
- 跨 run 自动插入“替代评论链接”

## 当前实现的关键限制

当前 marker 只基于 `file + line + severity + title`，这几个字段的问题是：

- `line` 很不稳定，代码移动就失效
- `severity` 可能随着辩论被调整，不应影响身份识别
- `title` 可能被微调，但问题本质没变
- `description` 和 `suggestedFix` 完全不在识别维度里，导致无法区分“同问题内容变化”与“新问题”

当前 `GetExistingComments` 也只拉回了很弱的信息，无法支持更新：

- GitHub 现状主要有 `path/line/body`
- GitLab 现状主要有 `path/line/body`
- 缺少统一的 comment ID / note ID / thread ID / 是否为 Hydra 评论 / 解析后的 meta

## 数据结构设计

### 1. 扩展 ExistingComment

建议扩展为：

```go
type ExistingComment struct {
    ID       string
    ThreadID string
    Path     string
    Line     *int
    Body     string
    Source   string // inline | file | global
    IsHydra  bool
    Meta     *HydraCommentMeta
}
```

说明：

- `ID`: 平台评论 ID，统一使用 string，避免 GitHub/GitLab int 差异
- `ThreadID`: discussion/review 线程 ID，GitHub 可为空
- `Source`: 当前评论来源类型
- `IsHydra`: 是否能识别为 Hydra 管理的评论
- `Meta`: 从隐藏 marker 解析出的结构化信息

### 2. 新增 HydraCommentMeta

```go
type HydraCommentMeta struct {
    IssueKey   string
    Status     string // active | resolved | superseded
    RunID      string
    HeadSHA    string
    BodyHash   string
    AnchorHash string
}
```

字段含义：

- `IssueKey`: 问题稳定身份
- `Status`: 生命周期状态
- `RunID`: 本次 review 的唯一 ID
- `HeadSHA`: 当前评论对应的代码版本
- `BodyHash`: 展示正文 hash，用于判断内容是否变化
- `AnchorHash`: 挂载位置 hash，用于判断位置是否变化

### 3. 新增 DesiredComment

```go
type DesiredComment struct {
    IssueKey   string
    Path       string
    Line       *int
    Body       string
    Source     string
    BodyHash   string
    AnchorHash string
}
```

说明：

- `DesiredComment` 表示“本轮 review 结束后，平台上应该存在的评论”
- 它是 planner 的输入，不直接等同于平台 API payload

### 4. 新增 LifecyclePlan

```go
type LifecyclePlan struct {
    Create    []DesiredComment
    Update    []CommentUpdate
    Resolve   []CommentResolve
    Supersede []CommentSupersede
    Noop      []DesiredComment
}

type CommentUpdate struct {
    Existing ExistingComment
    Desired  DesiredComment
}

type CommentResolve struct {
    Existing ExistingComment
}

type CommentSupersede struct {
    Existing ExistingComment
    Desired  DesiredComment
}
```

## IssueKey 设计

### 目标

`IssueKey` 需要稳定到足以表达“同一问题”，但不能被高频变化字段干扰。

### 第一版建议输入字段

- `normalized file`
- `normalized category`
- `normalized title`
- `normalized root-cause description`

### 第一版不要纳入的字段

- `severity`
- `suggestedFix`
- `line`
- `raisedBy`

原因：

- `severity` 可能升级或降级
- `suggestedFix` 经常在多轮辩论后改写
- `line` 漂移非常常见
- `raisedBy` 是来源信息，不是问题身份

### 建议的规范化策略

对 `title` 和 `description` 做简单归一化：

- 转小写
- 去 markdown 标记
- 多空格折叠为单空格
- 去首尾空格
- `description` 截断到前 120~160 字符

第一期不做语义近似匹配，只做稳定字符串 hash。

## Marker 升级方案

### 当前问题

当前隐藏 marker 只有一个短 hash，无法表达状态和版本信息。

### 新格式建议

建议使用单行 JSON 形式，便于扩展：

```html
<!-- hydra:issue {"key":"abc123","status":"active","run":"20260306-1","head":"deadbeef","body":"h1","anchor":"h2"} -->
```

### 优点

- 易解析
- 易扩展
- 不需要手写脆弱的 kv parser
- 后续想补字段不需要改格式协议

### 需要的工具函数

建议新增 `internal/platform/marker.go`，提供：

```go
const HydraIssueMetaPrefix = "<!-- hydra:issue "

func EncodeHydraMeta(meta HydraCommentMeta) string
func ParseHydraMeta(body string) (*HydraCommentMeta, bool)
func StripHydraMeta(body string) string
func BodyHash(body string) string
func AnchorHash(path string, line *int, source string) string
```

### 兼容策略

- 保留旧 marker 识别能力
- 历史旧评论能识别为 Hydra 评论
- 但旧评论只参与保守匹配，不做激进 update/supersede

## 评论正文格式

评论正文应拆成两部分：

1. 顶部隐藏 metadata
2. 对人展示的正文

示例：

```md
<!-- hydra:issue {"key":"abc123","status":"active","run":"run-001","head":"deadbeef","body":"bhash","anchor":"ahash"} -->
🟠 **Missing error handling**

The function ignores the returned error and may proceed with invalid state.

**Suggested fix:** Check the error before continuing.

_Raised by: reviewer-a, reviewer-b_
```

## Lifecycle Planner 设计

### planner 职责

输入：

- 当前 run 生成的 `DesiredComment` 列表
- 平台上已有的 `ExistingComment` 列表

输出：

- `LifecyclePlan`

planner 不做任何 API 调用，只做纯内存判断，保证高可测。

### planner 前置步骤

1. 过滤历史评论，只保留 Hydra 评论
2. 默认只让 `status=active` 的历史评论参与匹配
3. 建立索引：
   - `existingByKey`
   - `desiredByKey`

### 匹配规则

对每个 `DesiredComment`：

1. 找到同 `IssueKey` 的候选历史活跃评论
2. 如果没有候选：
   - `create`
3. 如果有候选，取最新一条：
   - `BodyHash` 相同且 `AnchorHash` 相同：`noop`
   - `BodyHash` 不同且 `AnchorHash` 相同：`update`
   - `AnchorHash` 不同：`supersede + create`

处理完 `desired` 后：

- 所有未匹配到且仍为 `active` 的历史 Hydra 评论：
  - 如果其 `IssueKey` 在本轮 `desiredByKey` 中不存在，则 `resolve`

### 为什么位置变化不直接 update

因为“改位置”在语义上不是同一条评论的简单编辑：

- 很多平台对 inline comment 的位置并不支持真正编辑
- 即使支持，用户看到旧线程突然跳位置也很怪
- 更符合用户理解的方式是：
  - 旧评论 `superseded`
  - 新位置创建新评论

### planner 伪代码

```text
for each desired:
  candidates = existing active comments with same issue_key

  if no candidates:
    plan.create += desired
    continue

  pick latest candidate

  if candidate.meta.body_hash == desired.body_hash
     and candidate.meta.anchor_hash == desired.anchor_hash:
      plan.noop += desired
      mark candidate matched
      continue

  if candidate.meta.anchor_hash == desired.anchor_hash:
      plan.update += (candidate, desired)
      mark candidate matched
      continue

  plan.supersede += (candidate, desired)
  plan.create += desired
  mark candidate matched

for each unmatched existing active comment:
  if existing.meta.issue_key not in desiredByKey:
      plan.resolve += existing
```

## 平台执行顺序

执行顺序建议固定为：

1. `resolve`
2. `supersede`
3. `update`
4. `create`
5. summary upsert

这样做的原因：

- 先把历史状态收口
- 再发新评论，用户看到的状态演进更自然
- summary 统计也更准确

## 平台接口改造建议

第一期不必大规模改平台接口，可以先在 `PostIssuesAsComments` 内部接入 planner。

长期建议增加一个更高层接口：

```go
type CommentLifecycleManager interface {
    ListHydraComments(mrID, repo string) ([]ExistingComment, error)
    UpdateComment(mrID, repo, commentID, body string) error
}
```

第二阶段再考虑引入：

```go
ApplyLifecyclePlan(mrID, repo string, plan LifecyclePlan, commitInfo CommitInfo) ReviewResult
```

## GitHub 落地方案

### 已有能力

- 能列出 PR review comments
- 有 PATCH JSON helper
- 能创建 review comments 和全局 note

### 现状不足

- `GetExistingComments` 没有保留 comment ID
- 无法对 inline review comment 做更新

### 第一阶段改造内容

1. 扩展 `GetExistingComments`：
   - 解析 `id`
   - 解析 marker/meta
   - 识别 `IsHydra`
2. 增加：

```go
func (g *GitHubPlatform) UpdateComment(commentID string, body string, repo string) error
```

3. `PostIssuesAsComments` 改为：
   - issue -> desired comments
   - list existing Hydra comments
   - planner
   - execute plan

### 状态文案建议

#### resolved

```md
This Hydra finding was not reproduced in the latest review run and is now marked as resolved.

Previous finding:
...
```

#### superseded

```md
This Hydra finding has been superseded by a newer comment for the same issue in the latest review run.
```

第一期不强依赖“插入跳转到新评论链接”。

## GitLab 落地方案

### 已有能力

- 能列 discussions
- 能创建 discussion/note
- summary note 已支持 upsert/update

### 现状不足

- `GetExistingComments` 没保留 note ID / discussion ID
- 无法对 discussion note 做 body update

### 第一阶段改造内容

1. 扩展 `GetExistingComments`：
   - 解析 note ID
   - 解析 discussion ID
   - 解析 marker/meta
   - 识别 `IsHydra`
2. 增加：

```go
func (g *GitLabPlatform) UpdateDiscussionNote(mrID, repo, noteID string, body string) error
func (g *GitLabPlatform) UpdateMergeRequestNote(mrID, repo, noteID string, body string) error
```

3. 执行策略：
   - inline/file 评论优先更新 discussion note
   - global 评论更新 MR note
   - 更新失败时可保守降级为仅创建新评论，但不阻塞主流程

### 第二阶段可选增强

- 使用 GitLab discussion 的原生 resolved 状态
- 把 `resolve` 映射为平台原生 resolve，而不只是正文变更

## 与现有 IsDuplicateComment 的关系

第一期不建议删除 `IsDuplicateComment`，但要降级其职责。

新的角色分工：

- planner 决定 comment lifecycle
- `IsDuplicateComment` 只作为 create 前的最后一道保险
- 长期可以逐步从主流程中移除

也就是说，后续“要不要 create”主要由 planner 决定，而不是由 `IsDuplicateComment` 决定。

## Summary 联动

summary note 已经支持 upsert，适合加一段生命周期统计：

```md
Hydra comment lifecycle for this run:
- New: 3
- Updated: 2
- Resolved: 1
- Superseded: 1
- Unchanged: 4
```

价值：

- 用户能直观看到本轮 review 对历史评论的影响
- 降低“评论怎么突然变了”的困惑

## 兼容策略

### 旧评论兼容

- 旧评论没有新 meta，但可能带旧 marker
- 旧评论仍可识别为 Hydra 评论
- 旧评论先只参与保守匹配，不做高风险更新

### 新旧混合期

- 新创建评论全部用新 meta
- 老评论逐步自然淘汰
- 不需要离线迁移脚本

### 风险控制

第一期遇到无法识别、无法解析或更新失败时，优先：

- 不误更新
- 不误 resolve
- 最多创建新评论

原则是“宁可保守重复，也不要误伤已有评论”。

## 建议新增文件

建议新增：

- `internal/platform/marker.go`
- `internal/platform/lifecycle.go`
- `internal/platform/lifecycle_test.go`

## 建议修改文件

- `internal/platform/platform.go`
- `internal/platform/format.go`
- `internal/platform/diffparse.go`
- `internal/platform/github/github.go`
- `internal/platform/gitlab/gitlab.go`

## 核心函数签名建议

### marker.go

```go
const HydraIssueMetaPrefix = "<!-- hydra:issue "

func EncodeHydraMeta(meta HydraCommentMeta) string
func ParseHydraMeta(body string) (*HydraCommentMeta, bool)
func StripHydraMeta(body string) string
func BodyHash(body string) string
func AnchorHash(path string, line *int, source string) string
```

### format.go

```go
func BuildIssueKey(issue IssueForComment) string
func BuildHydraCommentMeta(issue IssueForComment, runID, headSHA, source string) HydraCommentMeta
func FormatIssueBody(issue IssueForComment, meta HydraCommentMeta) string
```

### lifecycle.go

```go
func BuildDesiredComments(issues []IssueForComment, runID, headSHA string) []DesiredComment
func FilterHydraComments(existing []ExistingComment) []ExistingComment
func PlanLifecycle(existing []ExistingComment, desired []DesiredComment) LifecyclePlan
func RenderResolvedBody(existing ExistingComment) string
func RenderSupersededBody(existing ExistingComment, replacement DesiredComment) string
```

### GitHub / GitLab

```go
func (g *GitHubPlatform) UpdateComment(commentID string, body string, repo string) error
func (g *GitLabPlatform) UpdateDiscussionNote(mrID, repo, noteID string, body string) error
func (g *GitLabPlatform) UpdateMergeRequestNote(mrID, repo, noteID string, body string) error
```

## 执行流程改造

当前：

1. issue -> body
2. classify by diff
3. duplicate check
4. post

目标：

1. 获取 `commitInfo`
2. issue -> `DesiredComment`
3. 拉取 existing Hydra comments
4. 运行 planner
5. `resolve/supersede/update/create`
6. summary upsert

## 测试方案

### 1. marker tests

- encode/parse roundtrip
- malformed meta
- strip meta
- old marker compatibility

### 2. lifecycle tests

- same key same body same anchor -> noop
- same key different body same anchor -> update
- same key same body different anchor -> supersede + create
- old active missing in desired -> resolve
- resolved historical comments ignored for matching
- multiple existing active comments with same key -> choose latest

### 3. platform tests

- GitHub parse existing comments with ID/meta
- GitLab parse note/discussion IDs with meta
- update failure fallback behavior

## 实施顺序建议

最稳的开发顺序：

1. `marker.go`
2. `lifecycle.go` + 单测
3. 扩 `ExistingComment`
4. GitHub 接通完整生命周期
5. GitLab 接通完整生命周期
6. summary 增加 lifecycle stats
7. 最后清理旧 `IsDuplicateComment` 的主路径依赖

## 任务拆分建议

### Task 1: Marker 和元数据模型

- 扩 `ExistingComment`
- 新增 `HydraCommentMeta`
- 实现 marker 编解码
- 保留旧 marker 兼容读取

### Task 2: Lifecycle Planner

- 实现 `IssueKey`
- 实现 `DesiredComment`
- 实现 `PlanLifecycle`
- 完成纯逻辑单测

### Task 3: GitHub 执行器

- 扩展 `GetExistingComments`
- 新增 `UpdateComment`
- 将 `PostIssuesAsComments` 改为 lifecycle 模式

### Task 4: GitLab 执行器

- 扩展 discussions 解析
- 新增 note update
- 将 `PostIssuesAsComments` 改为 lifecycle 模式

### Task 5: Summary 联动

- 输出 lifecycle 统计
- 在 summary note 中 upsert 展示

## 风险与取舍

### 1. IssueKey 不够稳

风险：

- 同一问题被误判为新问题
- 不同问题被误判为同一问题

应对：

- 第一版先保守
- 宁可多 create，不要误 update
- 单测覆盖真实案例

### 2. 平台更新接口存在限制

风险：

- 某些 inline/discussion note 不支持 update

应对：

- 更新失败时保守降级为 create
- 不阻塞主流程

### 3. 新旧评论混合期行为不一致

风险：

- 部分评论能 lifecycle，部分只能 legacy duplicate

应对：

- 在 summary 中明确本轮统计
- 逐步淘汰老评论

## 推荐结论

这个方案可以落地，而且适合分两期做。

第一期建议只做：

- 新 marker
- 稳定 IssueKey
- planner
- update/resolve/supersede 的正文级表达
- GitHub/GitLab 平台执行器

这样能显著提升评论体验，同时避免一次性引入过多平台特性复杂度。
