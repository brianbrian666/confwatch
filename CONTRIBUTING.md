# 贡献指南

感谢你考虑为 confwatch 做贡献。这个项目的目标很窄：**把配置的原子发布、失败隔离和生命周期
做对**。功能少一点、语义清楚一点，比功能多但边界模糊更有价值。

## 环境准备

```bash
go version              # 需要 Go 1.24 及以上（开发使用 1.27.x）
git clone https://github.com/brianbrian666/confwatch.git
cd confwatch
go test ./...
```

依赖下载走默认的 Go module proxy 即可，无需特殊配置。

## 提交前必须全部通过

```bash
gofmt -l .              # 输出必须为空
go vet ./...
go test ./...
go test -race ./...
go test -bench=. -benchmem ./...
```

CI 会跑同样的命令（Go 1.24.x 与 1.27.x 两个版本）。**不要提交注释掉的测试、`t.Skip` 掉的
用例或"以后补"的 TODO 测试**；如果某个场景确实无法稳定测出，在测试里写清原因，并在 PR 描述里说明。

## 代码要求

- **核心包零第三方依赖**。`confwatch`、`decode/json`、`source/file`、`watcher/poll` 只允许
  使用标准库。需要第三方库的能力请放到可选子包。
- **不允许在仓库根包引入全局可变状态**。所有状态挂在 `Manager[T]` 上。
- **不允许用 `time.Sleep` 做同步**。测试里的等待用「轮询条件 + 上限」（见 `waitFor`）或 channel 握手；
  库代码里用 ticker/timer + ctx。
- **并发相关改动必须提供可执行的证据**：要么有测试，要么有基准，要么在 PR 里解释为什么无法测试。
  "看起来不会竞争"不是理由。
- **错误**：新增错误类别前先想清楚调用方是否需要区分。用 `errors.Is` 判类别，不要比较字符串；
  错误信息用中文或英文都行，但一个仓库内保持一致。
- **注释写"为什么"和"违反了会发生什么"**，不要复述代码。文档注释以被导出标识符开头。
- **任何对公开 API 的破坏性变更**都要：在 CHANGELOG 的 **Breaking** 下说明 + 给迁移方式 + 在
  PR 描述里单独列出。

## 测试要求

新增行为至少覆盖：正常路径、失败路径（保持旧快照）、取消/停止路径。涉及并发的改动请同时考虑：

- 是否需要在 `-race -count=10` 下稳定通过（偶发失败必须是测试写法问题，先修测试，不要靠重试）；
- 是否需要 goroutine 泄漏断言（`runtime.NumGoroutine` 回归基线）；
- 是否会引入无界队列或随事件增长的内存。

## 提交信息与 PR

- 提交信息用祈使句，一行说清"改了什么"：`watch: treat silent watcher exit as failure`。
- 一个 PR 只做一件事。格式化、重构、行为变更不要混在一个 PR 里。
- PR 描述请包含：动机、行为变化、测试方式（贴命令与结果）、是否影响公开 API。
- 新增或修改依赖：单独提交，附 `git diff go.mod go.sum` 与升级理由。
- 提交者需确保自己有权提交该代码（DCO 式声明：`Signed-off-by: Name <email>` 可选但欢迎）。

## 版本发布流程（维护者）

1. 确认 `CHANGELOG.md` 的 `[Unreleased]` 已按版本号归档，日期正确。
2. 全绿检查：`gofmt -l .` 为空、`go vet ./...`、`go test -race ./...`、`go test -bench=. -benchmem ./...`。
3. 打 tag（语义化版本，**不加 `v` 以外的前缀**）：
   ```bash
   git tag -a v0.3.0 -m "v0.3.0"
   git push origin main --follow-tags
   ```
4. 在 GitHub 创建 Release，正文直接使用 CHANGELOG 对应段落（含 Breaking 说明与迁移方式）。
5. tag 推送后 pkg.go.dev 会自动索引，检查文档页是否正常显示（`https://pkg.go.dev/github.com/brianbrian666/confwatch`）。

## 报告问题

- 功能缺陷、文档错误、行为疑问：开 issue，附最小复现（最好是一个能 `go test` 的片段）。
- 安全漏洞：**不要开公开 issue**，见 [SECURITY.md](SECURITY.md)。

## 行为准则

讨论聚焦技术问题。对事不对人；不接受人身攻击、歧视性言论或骚扰行为。维护者有权删除不当评论并
拒绝不遵守本准则的贡献者。
