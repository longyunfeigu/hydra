# 方案：Webhook 同 MR 重复触发时取消重跑

## 背景

当前 server 模式下，同一个 MR 正在 review 时收到新的 webhook，直接丢弃新请求（`sync.Map.LoadOrStore` 去重）。
这会导致开发者 push 新 commit 后，正在进行的 review 用的是旧代码，新代码的 review 被跳过。

改为：取消正在进行的 review，等待其清理完成，然后用最新代码重新 review。

## 核心思路

### 取消机制基于 Go 的 Context

每次 review 创建一个 `context.WithTimeout`，会产生两样东西：
- `ctx` — 传给所有干活的函数，它们可以检查是否被取消
- `cancel` — 调用它就让 ctx 失效，通知所有持有 ctx 的函数停止工作

当前代码已经把 ctx 一路传递下去了：

```
RunServerReview(ctx)
  └→ orch.RunStreaming(ctx)
       └→ errgroup.WithContext(ctx)
            └→ reviewer.Provider.ChatStream(ctx)
                 └→ exec.CommandContext(ctx, "claude", ...)
```

cancel() 被调用后，信号沿着这条链从上往下传播，最终所有进行中的操作都会退出。

**之前的问题**：`cancel` 存在 `triggerReview` 的局部变量里，没有人能在外部调用它。
**方案的做法**：把 `cancel` 保存到一个 map 里，新 webhook 到达时可以拿到并调用它。

### 关键数据结构

```go
// 记录一个正在进行的 review
type inFlightEntry struct {
    cancel context.CancelFunc   // 让外部能取消这个 review
    done   chan struct{}         // 让外部能等这个 review 退出
}

// Server 里用 map 记录所有正在 review 的 MR
// key 格式: "mygroup/myproject/123"（项目路径/MR编号）
s.inFlight = map[string]*inFlightEntry{
    "mygroup/myproject/123": entry,  // MR !123 正在 review
}
```

- `cancel` — 回答"怎么让它停"
- `done` — 回答"它什么时候真正停了"（是一个 channel，review 退出时 close 它，等待者解除阻塞）

### 为什么取消后还要等（done 的作用）

`cancel()` 只是发出信号，不是立即杀死。调用后旧 review 还需要：
1. 正在进行的 AI API 调用检测到取消，返回 error
2. orchestrator 的 errgroup 退出
3. `RunServerReview` return
4. `defer checkoutResult.Release()` 清理 worktree
5. `defer func() { <-s.sem }()` 归还信号量

如果不等就开始新 review，可能两个 review 同时操作同一个 mirror，或者信号量没归还导致资源泄漏。

```
旧 review goroutine                          新 webhook goroutine
───────────────────                          ──────────────────────
正在执行 AI review...                         到达，发现旧的正在跑
                                             │
                                             old.cancel()    ← 发信号让旧的停
                                             <-old.done      ← 等旧的退出
                                             │ (阻塞中...)
AI 检测到 ctx 取消，开始退出                    │
清理 worktree...                              │
归还信号量...                                  │
close(done) ← 通知等待者                       │
                                             解除阻塞 ←
                                             开始新 review ✓
```

## 修改文件

### 1. `internal/server/server.go`

#### a) 新增类型

```go
type inFlightEntry struct {
    cancel context.CancelFunc
    done   chan struct{}
}
```

#### b) 修改 Server struct

```go
// 替换:
inFlight      sync.Map      // "projectPath/mrIID" → bool，去重

// 为:
mu            sync.Mutex
inFlight      map[string]*inFlightEntry
```

#### c) 修改 New()

`inFlight` 初始化改为 `make(map[string]*inFlightEntry)`

#### d) 重写 triggerReview()

核心逻辑：先注册自己到 map（替换旧 entry），再等旧 review 退出。
这样保证 map 中永远只有最新的 entry，后续新 webhook 取消的是自己而不是更旧的。

```go
func (s *Server) triggerReview(event *MergeRequestEvent) {
    key := fmt.Sprintf("%s/%d", event.Project.PathWithNamespace, event.ObjectAttributes.IID)

    // 1. 注册自己，取消已有的 review
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    done := make(chan struct{})
    entry := &inFlightEntry{cancel: cancel, done: done}

    s.mu.Lock()
    if existing, ok := s.inFlight[key]; ok {
        s.logger.Printf("[cancel] cancelling previous review for %s", key)
        existing.cancel()
        waitCh := existing.done
        s.inFlight[key] = entry // 立即替换，保证后续新 webhook 取消的是自己
        s.mu.Unlock()
        <-waitCh // 等旧 review 清理完（worktree 释放、sem 归还）
    } else {
        s.inFlight[key] = entry
        s.mu.Unlock()
    }

    // 2. 清理（无论正常结束还是被取消）
    defer func() {
        cancel()
        close(done)
        s.mu.Lock()
        if cur, ok := s.inFlight[key]; ok && cur == entry {
            delete(s.inFlight, key)
        }
        s.mu.Unlock()
    }()

    // 3. 等待期间可能被更新的 webhook 取消了
    if ctx.Err() != nil {
        s.logger.Printf("[superseded] review for %s was superseded", key)
        return
    }

    // 4. 信号量
    select {
    case s.sem <- struct{}{}:
        defer func() { <-s.sem }()
    default:
        s.logger.Printf("[skip] max concurrent reviews reached, dropping %s", key)
        return
    }

    // 5. 执行 review
    s.logger.Printf("[trigger] starting review for %s", key)
    plat := gitlab.New(s.cfg.GitLabHost)
    if err := RunServerReview(ctx, event, plat, s.cfg.HydraConfig, s.checkoutMgr, s.logger); err != nil {
        if ctx.Err() != nil {
            s.logger.Printf("[cancelled] review for %s was cancelled", key)
        } else {
            s.logger.Printf("[error] %s: %v", key, err)
        }
    } else {
        s.logger.Printf("[done] review completed for %s", key)
    }
}
```

#### e) 修改 Shutdown()

在现有逻辑前加上取消所有进行中的 review：

```go
s.mu.Lock()
for _, entry := range s.inFlight {
    entry.cancel()
}
s.mu.Unlock()
```

### 2. `internal/server/server_test.go` — 重写 TestDeduplication

旧测试直接操作 `sync.Map`，需要适配新结构。验证：模拟一个正在 review 的 entry，触发同 MR 的新 webhook，断言旧 entry 的 context 被取消。

### 3. `internal/server/README.md` — 更新描述

将 `sync.Map` 去重相关描述改为取消重跑。

## 不需要修改的文件

- `reviewer.go` — 已正确传递 ctx，defer Release()
- `checkout.go` — Release() 用 sync.Once，并发安全
- `orchestrator.go` — 已用 errgroup.WithContext(ctx)，响应取消
- 所有 provider — 已用 CommandContext/NewRequestWithContext

## 并发正确性分析

三个 webhook A→B→C 依次到达同一 MR 的场景：

```
A 注册并运行
B 到达 → 注册 entry_B（替换 entry_A），取消 A，等 A 的 done
C 到达 → 注册 entry_C（替换 entry_B），取消 B，等 B 的 done
A 结束 → close(done_A)，检查 map 发现不是自己(是 entry_C)，不删
B 解除阻塞 → 检查 ctx.Err()≠nil（被 C 取消了）→ 打日志 "superseded" 并 return → close(done_B)
C 解除阻塞 → ctx 有效 → 拿信号量 → 执行 review
```

最终只有 C（最新代码）运行，A 和 B 正确退出。

## 验证方式

```bash
cd /home/guwanhua/Desktop/git/hydra
go test ./internal/server/ -run TestDeduplication -v
go test ./internal/server/ -v
go vet ./internal/server/
```
