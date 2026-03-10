# CLI 参考

## hydra review

```
hydra review [pr-number-or-url] [flags]

Flags:
  -c, --config string      配置文件路径（默认 ~/.hydra/config.yaml）
  -r, --rounds int         最大辩论轮数（覆盖配置）
  -o, --output string      输出保存到文件
  -f, --format string      输出格式：markdown | json（默认 "markdown"）
      --no-converge        禁用收敛检测
      --show-tool-trace    显示 analyzer/reviewer 完整过程输出（默认摘要模式）
  -v, --verbose            `--show-tool-trace` 的别名
  -l, --local              审查本地未提交变更
      --branch string      审查当前分支 vs 指定基准分支
      --files strings      审查指定文件
      --reviewers string   逗号分隔的审查者 ID
  -a, --all                使用所有审查者
      --skip-context       跳过上下文收集
      --no-post            跳过平台评论发布
```

### 使用示例

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
hydra review 42 -v                                           # 显示完整过程输出
hydra review 42 -o result.md                                 # 输出到文件
hydra review 42 -f json -o result.json                       # JSON 格式输出
HYDRA_LOG_LEVEL=debug hydra review 42                        # 调试模式
```

## hydra serve

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

### GitLab Webhook 配置

```bash
# 启动 webhook 服务器
hydra serve --webhook-secret your-secret

# 自定义监听地址和并发数
hydra serve --addr :9090 --max-concurrent 5 --webhook-secret your-secret

# 自托管 GitLab
hydra serve --gitlab-host gitlab.company.com --webhook-secret your-secret
```

在 GitLab 项目 Settings > Webhooks 中配置：
- **URL**: `http://your-server:8080/webhook/gitlab`
- **Secret Token**: 与 `--webhook-secret` 一致
- **Trigger**: 勾选 **Merge request events**

## hydra init

交互式生成配置文件 `~/.hydra/config.yaml`。

```bash
hydra init
```

## 日志

通过环境变量 `HYDRA_LOG_LEVEL` 控制日志级别：

```bash
HYDRA_LOG_LEVEL=debug hydra review 42
```

支持的级别：`debug`、`info`（默认）、`warn`、`error`
