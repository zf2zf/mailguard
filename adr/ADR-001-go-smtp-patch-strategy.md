# ADR-001: go-smtp 本地补丁维护策略

**状态**：Accepted
**提出日期**：2025-10-15
**最后更新**：2025-10-15
**决策者**：MailShield 项目组

---

## 背景

MailShield 需要对 SMTP 连接的双向流量进行观测，以实现邮件内容记录和安全审计。上游 `github.com/emersion/go-smtp` v0.25.0 标准库不提供 `TrafficObserver` 机制，需要在本地源码上增加小型观测回调。

go-smtp 项目未提供官方插件式流量观测 API，无法通过外部扩展实现，必须修改协议处理层的读写路径。

---

## 决策

采用**本地源码副本（vendored copy）+ replace 指令**方式维护 go-smtp，并通过补丁文件记录所有本地修改，支持从上游版本可重现地重建。

---

## 理由

### 为什么不直接使用上游依赖

1. 上游 go-smtp v0.25.0 的协议读写路径（`conn.go`）不提供任何流量观测 hook。
2. `TrafficObserver` 机制需要插入到 `Conn.init()` 的读写包装层，必须修改协议处理代码。
3. 官方尚无等效的 extension API，直接引入上游会破坏已验证的流量观测能力。

### 为什么不 Fork 全项目

1. Fork 全项目需要持续跟踪上游合并，维护成本高。
2. go-smtp 协议处理稳定，更新频率低；本地修改仅涉及两个文件（`server.go`、`conn.go`）的少量代码，补丁清晰。
3. 补丁模式保持了源码可审视性：任何人都可以从补丁内容理解本地修改的范围和目的。

### 为什么不 Vendoring 而不使用 replace

tests 模块通过 `replace github.com/emersion/go-smtp => ../third_party/go-smtp` 指向本地副本，使得：
- 测试始终使用本地补丁版本，无需每次下载和打补丁。
- `third_party/go-smtp` 是完整 vendored 副本，包含完整的 go-smtp 源码和自身的 go.mod。
- 升级上游时，只需在 `third_party/go-smtp/` 中重新应用补丁，保持测试模块的 replace 路径不变。

---

## 技术基线

### 上游版本

- **仓库**：`https://github.com/emersion/go-smtp`
- **版本 tag**：`v0.25.0`
- **本地 HEAD**：`v0.25.0-1-g5b1791c`（tag + 1 个本地 commit）

### 本地修改记录

| 文件 | 变更内容 |
|------|---------|
| `server.go` | 新增 `TrafficDirection` 类型定义和 `Server.TrafficObserver` 字段 |
| `conn.go` | 新增 `trafficObserverWriter` 和 `trafficObservedWriter` 结构体；在 `init()` 中插入读写包装逻辑 |
| `conn.go`（STARTTLS 后） | `handleStartTLS` 调用 `c.init()` 重新初始化读写层，observer 继续生效 |

本地额外 commit：`5b1791c Add TrafficObserver for SMTP application bytes inspection`。

本地还包含上游 tag 自带的 BDAT 修复 commit：`642c374 server: restore line limit when a BDAT transaction is reset`。

### 补丁清单和应用顺序

**补丁目录**：`third_party/patches/go-smtp/`

| 顺序 | 文件名 | 内容 | 应用到的源码文件 |
|------|--------|------|----------------|
| 1 | `go-smtp-v0.25.0-traffic-observer.patch` | 引入 `TrafficDirection` 类型和 `Server.TrafficObserver(func(direction, data))`；添加 `trafficObserverWriter`/`trafficObservedWriter`（无 conn 参数） | `server.go`、`conn.go` |
| 2 | `go-smtp-v0.25.0-connection-observer-incremental.patch` | 将 `TrafficObserver` 签名升级为 `func(conn, direction, data)`；将 `*Conn` 指针注入到每次回调中 | `server.go`、`conn.go` |

应用顺序：**patch1 → patch2**，先定义类型和基础框架，再升级为含连接上下文的版本。

当前 `third_party/go-smtp/` 中的源码已经是应用 patch1 + patch2 之后的最终状态。

---

## 可重现的重建方法

从干净上游源码重建当前基线的步骤：

```bash
# 1. 克隆干净上游
git clone --depth 1 --branch v0.25.0 https://github.com/emersion/go-smtp.git /tmp/go-smtp-clean

# 2. 应用补丁（按顺序）
cd /tmp/go-smtp-clean
git apply ../mailshield/third_party/patches/go-smtp/go-smtp-v0.25.0-traffic-observer.patch
git apply ../mailshield/third_party/patches/go-smtp/go-smtp-v0.25.0-connection-observer-incremental.patch

# 3. 比对差异（预期：仅 TrafficObserver 签名和 conn 字段增量）
diff -rq /tmp/go-smtp-clean third_party/go-smtp
# 预期输出：仅 conn.go 和 server.go 存在差异，且差异等于 patch1→patch2 的增量
```

**注意**：补丁文件使用假 SHA（`index 1111111..2222222`），`git apply` 无法自动 3-way 合并。手动应用或使用 `--build` 选项时需确保上下文匹配。

### 验证结果（2025-10-15 实际执行）

- 干净上游 v0.25.0 源码与当前 `third_party/go-smtp` 比对：仅 `conn.go` 和 `server.go` 存在差异。
- 差异精确等于 patch1→patch2 的增量修改（`TrafficObserver` 签名升级 + `conn` 字段添加），无其他差异。
- 结论：**当前源码 = 干净 v0.25.0 + patch1 + patch2**。

---

## 如何升级上游版本

当需要升级 go-smtp 到新版本时：

1. 在 `third_party/go-smtp/` 中执行 `git fetch --tags` 拉取新版本。
2. 切到新版本 tag。
3. 依次应用 patch1 和 patch2，逐一处理冲突：
   - 如果 `server.go` 的 `Logger` 接口或 `Server` 结构体定义发生变化，patch 可能失败，需要手工调整补丁。
   - 如果补丁应用失败，**暂停升级**，提出 ADR 说明冲突内容并等待决策。
4. 补丁应用成功后，运行全部回归测试：
   ```bash
   cd tests && go test -race -v ./...
   ```
5. 测试全部通过后，更新本 ADR 的上游版本记录。

---

## 受保护的技术基线文件

以下文件属于 MailShield 技术基线，**禁止未经评估直接修改**：

| 文件 | 保护原因 |
|------|---------|
| `third_party/go-smtp/server.go` | `TrafficDirection` 类型和 `TrafficObserver` 字段定义 |
| `third_party/go-smtp/conn.go` | 读写包装逻辑、`trafficObserverWriter`、`trafficObservedWriter` |
| `third_party/patches/go-smtp/*.patch` | 可重现性的权威来源 |
| `tests/go.mod` | replace 指令指向本地副本的路径声明 |
| `tests/` 下的 13 项测试 | 技术基线的验证保障 |

任何对上述文件的修改必须先提出变更申请，说明影响范围，并通过回归测试验证。

---

## 备选方案

### 1. 直接 fork 全项目

- **缺点**：维护成本高；与上游同步工作量大；fork 历史会增加仓库复杂度。
- **结论**：不采用。

### 2. 完全自研 SMTP 协议层

- **缺点**：实现周期长；需重新实现 RFC 5321 及多个扩展，工作量大。
- **结论**：不采用。

### 3. 通过外部包扩展（如 go-plugin）

- **缺点**：go-smtp 当前架构不支持；需要 fork 至少部分代码。
- **结论**：不采用。

---

## 后果

### 正面后果

- 流量观测能力通过补丁实现，不影响上游协议处理的稳定性。
- 补丁文件清晰记录了所有本地修改，支持任何人验证重建结果。
- 测试模块与 vendored 副本通过 `replace` 解耦，测试路径稳定。

### 负面后果

- 上游更新时需要重新应用补丁，可能出现冲突，需要手工处理。
- `trafficObserverWriter` 每次 `Write` 都进行 `append` 分配，高频场景下可能增加 GC 压力（当前测试规模下无异常）。
- 回调在 `Write()` goroutine 中同步执行，慢回调会阻塞协议读写（Constitution Principle III 要求回调必须无阻塞）。

### 风险缓解

- 通过 ADR-002（TrafficObserver 回调性能要求）对回调执行进行约束。
- 通过回归测试（13 项测试）对所有修改进行验证。

---

## 验证标准

1. 从干净 v0.25.0 源码依次应用两个补丁后，差异文件仅 `conn.go` 和 `server.go`，且差异内容与补丁增量完全一致。
2. `cd tests && go test -v ./...` 全部通过。
3. `cd tests && go test -race -v ./...` 全部通过，race detector 无警告。
4. `TrafficObserver(conn, direction, data)` 回调签名与 `server.go:51-53` 声明一致。

---

## 相关 ADR

- ADR-002：TrafficObserver 回调性能要求（异步处理、队列容量、过载策略）
- ADR-005：STARTTLS 后 TrafficObserver 连续性保证

---

## 修订历史

| 日期 | 版本 | 变更内容 |
|------|------|---------|
| 2025-10-15 | 1.0 | 初始版本，基于技术基线验证报告制定 |
