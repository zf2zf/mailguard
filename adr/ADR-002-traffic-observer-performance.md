# ADR-002: TrafficObserver 流量处理机制——可配置可靠性策略

**状态**：Proposed
**提出日期**：2025-10-15
**最后更新**：2025-10-15
**版本**：0.4

---

## 背景

`TrafficObserver` 是 MailShield 捕获 SMTP 明文双向流量的核心机制。当前实现在 `conn.go` 中通过 `trafficObserverWriter` 和 `trafficObservedWriter` 包装 `net.Conn` 读写路径，每次 `Write` 操作同步调用用户回调。

**重要澄清**：当前实现**没有队列**，是纯同步回调。ADR 的双策略设计是对**未来实现**的架构约束，不是对现有代码的描述。

---

## 当前实现（已知约束）

### 关键代码（conn.go:101-127）

```go
type trafficObserverWriter struct {
    observer func(conn *Conn, direction TrafficDirection, data []byte)
    conn    *Conn
    direction TrafficDirection
}

func (w trafficObserverWriter) Write(p []byte) (int, error) {
    data := append([]byte(nil), p...)
    w.observer(w.conn, w.direction, data)  // 同步调用，发生在读循环中
    return len(p), nil
}

type trafficObservedWriter struct {
    writer   io.Writer
    observer func(conn *Conn, direction TrafficDirection, data []byte)
    conn     *Conn
    direction TrafficDirection
}

func (w trafficObservedWriter) Write(p []byte) (int, error) {
    n, err := w.writer.Write(p)
    if n > 0 {
        data := append([]byte(nil), p[:n]...)
        w.observer(w.conn, w.direction, data)  // 同步调用，写成功之后
    }
    return n, err
}
```

### 当前行为特征

| 特性 | 当前实现 | 评估 |
|------|---------|------|
| 回调时机（读方向） | `trafficObserverWriter.Write`：读循环中同步调用 | **约束**：若 observer 阻塞，整个 SMTP 服务器读循环暂停 |
| 回调时机（写方向） | `trafficObservedWriter.Write`：写成功之后 | 合理，但回调阻塞仍延迟响应 |
| 内存分配 | 每次 Write `append([]byte(nil), ...)` | 高频场景 GC 压力 |
| 阻塞性 | 完全同步，阻塞读或写路径 | 吞吐量风险 |
| 队列 | 无 | Constitution 不满足 |
| 丢弃策略 | 无 | Constitution 不满足 |
| 回调异常 | 被静默吞没（无感知） | 高风险 |

### 当前实现的已知约束

**读方向阻塞**：`trafficObserverWriter.Write` 在 SMTP 读循环中被调用（SMTP 行读取 → Write → handle 处理）。若 observer 阻塞，服务器对**所有连接**的命令响应延迟，因为 go-smtp 的读循环是每个连接独占的 goroutine，不会跨连接影响。但单个连接的读循环暂停会导致该连接的 EHLO/MAIL/RCPT/DATA 命令无法被处理，直到 observer 返回或连接超时。

**回调异常处理**：`trafficObservedWriter.Write` 捕获回调返回值但不处理任何错误；panic 会被 recover 吞没（SMTP 会话继续但事件丢失）。**当前实现无任何事件丢失检测机制**。

---

## 目标架构分阶段

### 第一阶段（当前实现可直接演进）

实现有界内存队列 + 非阻塞入队 + 双策略切换。

```
SMTP 连接 goroutine
  └─ trafficObserverWriter.Write(p)
        └─ [非阻塞] → 有界事件队列
                            └─ 异步消费者 goroutine
                                  └─ 事件处理

SMTP 连接 goroutine
  └─ trafficObservedWriter.Write(p)
        └─ [非阻塞] → 有界事件队列
```

**Phase 1 包含**：
- 有界内存队列（channel 或 ring buffer）
- 非阻塞入队（`select + default`，队列满时降级）
- 启动时策略选择，运行中不支持切换
- 降级记录（gap record）结构与生成规则
- AUTH PLAIN 命令行过滤
- 基础监控指标
- 回调异常处理（panic recovery + gap record + 日志）
- Phase 1 验收测试覆盖

**Phase 1 不包含**：spill-to-disk、磁盘配额、背压传导、持久化确认、崩溃恢复。

### 第二阶段（需要新的架构决策）

- spill-to-disk（临时文件 + rename-on-write）
- 磁盘配额管理与告警
- 背压传导机制（`block_timeout` + 降级）
- 持久化确认与崩溃恢复
- AUTH LOGIN 完整状态机过滤
- 运行中模式切换
- 磁盘队列自动扩容

---

## 读方向非阻塞语义

**所有模式**的读方向（非阻塞入队）语义如下：

> 非阻塞入队；队列满时按当前策略执行降级处理，**不阻塞 SMTP 读循环**。

非阻塞仅表示入队操作本身不等待队列空间，不代表事件一定被完整采集。队列满时降级记录按当前策略产生，读循环继续处理下一个 SMTP 命令。

**ReadTimeout 不是观察器回调终止机制**。`Server.ReadTimeout` 控制的是 SMTP 命令行读取的超时，与 observer 回调的执行时间无关。observer 回调阻塞时，读循环无法执行 `readLine()`，因此连接将在 ReadTimeout 之后被断开，但这是超时断开，不是回调的安全终止。

---

## 策略一：forwarding-priority（转发优先）

**目标**：邮件转发是主业务目标；流量记录是辅助能力，资源紧张时允许降级。

### 设计约束

- **禁止无界等待**：写入路径的等待时间有上限。
- **禁止无界内存增长**：队列深度有硬上限。
- **禁止静默丢弃**：任何丢弃必须产生可观测的缺口记录。
- **转发成功与记录完整独立**：转发成功不以记录完整为前提；记录不完整不得导致转发失败。

### Phase 1 场景行为（内存队列，仅此阶段）

| 场景 | 行为 | 阶段 |
|------|------|------|
| 正常运行 | 事件非阻塞入队，异步消费，记录完整 | Phase 1 |
| 队列满载 | 降级记录（仅元数据）非阻塞入队；缺口计数器 +1；写入路径立即返回 | Phase 1 |
| 消费者慢 | 队列持续满载，降级记录持续产生；监控指标可观测 | Phase 1 |
| 回调异常/panic | 产生 `reason: callback_failed` 缺口记录；SMTP 会话继续；转发不受影响 | Phase 1 |
| 消费者异常退出 | 事件积压；重启后队列清零；产生"队列清零"事件；转发不受影响 | Phase 1 |
| 服务关闭 | 产生关闭事件，统计剩余队列深度后清零；**不得声称"刷盘"**（无持久化机制） | Phase 1 |
| 进程崩溃 | 已入队未处理事件丢失；产生"队列清零"事件；转发不受影响 | Phase 1 |

### 降级记录格式

```json
{
  "type": "gap",
  "gap_id": 1234,
  "conn_ptr": "0xc0000d8000",
  "direction": "client_to_server",
  "original_length": 2048,
  "reason": "queue_full",
  "timestamp": "2025-10-15T10:23:45.123456Z"
}
```

`reason` 可选值：`queue_full`、`persist_failed`、`callback_failed`、`timeout`、`shutdown`。

### 不承诺的内容

- **不承诺进程崩溃后记录完整**：纯内存队列，无持久化机制；崩溃后队列清零是预期行为。
- **不承诺所有事件都有完整内容**：降级模式下仅有元数据，无原始字节。
- **不承诺零丢失**：回调异常产生的缺口记录本身若无法入队（如队列也已满），该事件完全丢失；通过增大队列深度缓解，但无法从架构上消除。

---

## 策略二：capture-completeness-priority（记录完整性优先）

**目标**：优先保证流量记录完整性，转发在资源允许的范围内尽可能配合。

### 读方向非阻塞语义（Phase 1 和 Phase 2 共同要求）

> 非阻塞入队；队列满时按当前策略执行降级处理，**不阻塞 SMTP 读循环**。

在 capture-completeness-priority 模式下，读方向 observer 阻塞会导致该连接的 SMTP 命令处理延迟，最终触发 ReadTimeout 断开。**缓解措施**：读方向 observer 采用非阻塞入队；慢处理在消费者 goroutine 中进行。

### Phase 1 场景行为（有界内存队列）

| 场景 | 行为 | 阶段 |
|------|------|------|
| 正常运行 | 事件入内存队列，异步消费；完整记录每个事件 | Phase 1 |
| 内存队列满载 | 降级记录（仅元数据）非阻塞入队；缺口计数器 +1；SMTP 读循环继续 | Phase 1 |
| 回调异常/panic | 产生 `reason: callback_failed` 缺口记录；SMTP 会话继续；该事件已入队则记录，丢失则产生缺口 | Phase 1 |
| 消费者异常退出 | 事件积压；重启后队列清零；产生"队列清零"事件 | Phase 1 |
| 服务关闭 | 队列排空（写入关闭事件）；产生"关闭"事件和未处理计数 | Phase 1 |
| 进程崩溃 | 已入队未处理事件丢失；产生"崩溃"事件；无持久化恢复 | Phase 1 |

### Phase 2 场景行为（spill-to-disk，背压传导）

Phase 2 的以下行为属于**新增能力**，需要单独的架构决策和验收测试：

| 场景 | 行为 | 阶段 |
|------|------|------|
| 内存队列满载 | 事件 spill-to-disk（临时文件）；产生 spill-to-disk 指标 | Phase 2 |
| 磁盘队列满载 | 背压传导到写路径；客户端感知延迟增加；产生磁盘队列已满告警 | Phase 2 |
| 磁盘空间不足 | 拒绝新事件 spill-to-disk；背压继续传导；产生持久化失败事件；产生降级记录 | Phase 2 |
| `max_spill_bytes` 达到 | 背压传导到写路径；产生磁盘配额已满告警 | Phase 2 |
| 消费者慢 | 内存队列积压；spill-to-disk 持续；队列全链路深度可监控 | Phase 2 |
| 回调异常/panic | 产生 `reason: callback_failed` 缺口记录；已写入磁盘的事件不受影响 | Phase 2 |
| 消费者异常退出 | 磁盘暂存文件保留；重启后消费者重新打开未完成文件继续处理 | Phase 2 |
| 服务关闭 | 队列排空（写入关闭事件）；尝试刷盘；若无法持久化则产生"未持久化事件"计数 | Phase 2 |
| 进程崩溃 | 磁盘暂存文件未写入部分丢失；产生"崩溃后恢复"事件；已刷盘事件可恢复 | Phase 2 |

### 持久化确认与崩溃恢复（Phase 2）

- 磁盘暂存文件使用 rename-on-write 语义（写临时文件后 rename，确保要么完整要么不存在）。
- 事件在消费者确认写入持久化存储**之前**保留在暂存区。
- 重启时扫描暂存目录，拾取所有未完成的暂存文件。
- **注意**：本阶段（Phase 1）spill-to-disk 和持久化确认尚未实现，是 Phase 2 目标。

### 磁盘配额与故障处理（Phase 2）

| 场景 | 行为 | 阶段 |
|------|------|------|
| `max_spill_bytes` 达到 | 背压传导到写路径；产生磁盘配额已满告警；消费者可继续处理现有积压 | Phase 2 |
| 磁盘空间不足 | 背压传导；产生磁盘空间不足告警；转发受背压影响 | Phase 2 |
| 磁盘写入失败 | 产生 `reason: persist_failed` 降级记录；背压继续传导；转发受背压影响 | Phase 2 |
| 短写入（写入 < 请求字节数） | 视为写入失败，触发背压和告警 | Phase 2 |

**`max_spill_bytes` 待验证**：当前配置中的 `1GB` 是占位值，需要根据实际磁盘 I/O 吞吐和邮件大小分布通过基准测试确定。不得在未验证的情况下将其作为已承诺的保证写入 ADR。

### 背压传导机制（Phase 2）

**写方向**：当 spill-to-disk 队列满或磁盘写入失败时，`trafficObservedWriter.Write` 感知背压。

**背压传导路径**：

```
磁盘队列满/写入失败
  ↓
trafficObservedWriter.Write 返回错误或延迟
  ↓
go-smtp 写路径延迟
  ↓
SMTP 客户端感知响应延迟
  ↓
客户端可能触发 WriteTimeout
```

**最大等待时间（`block_timeout`）**：背压传导后，写入路径的最大等待时间。超过后：
- 产生 `reason: timeout` 降级记录
- 事件降级（仅元数据）写入内存队列
- SMTP 协议层继续正常处理，不返回错误给客户端

**不得将背压传导描述为"保证零丢失"**：背压+超时降级意味着仍有事件丢失，ADR 承诺的是**尽力完整性**而非**绝对完整性**。

---

## 两种策略对比

| 维度 | forwarding-priority | capture-completeness-priority |
|------|-------------------|------------------------------|
| 主目标 | 邮件转发可用性 | 尽力完整性 + 可观测性 |
| 队列介质 | 纯内存有界（Phase 1） | 内存有界 + 磁盘暂存（Phase 2） |
| 队列满时行为 | 降级记录（仅元数据） | Phase 1：降级记录；Phase 2：spill-to-disk + 背压 |
| 读方向阻塞 | 无（非阻塞入队） | 无（非阻塞入队，Phase 1 和 Phase 2 相同） |
| 背压对写入路径影响 | 无 | Phase 2：有（延迟） |
| 转发成功率保障 | 高 | Phase 2：受背压影响 |
| 持久化 | 无 | Phase 2：spill-to-disk + 崩溃恢复 |
| 进程崩溃后恢复 | 无 | Phase 2：仅已刷盘事件可恢复 |
| AUTH 凭证过滤 | PLAIN + 基础 LOGIN（Phase 1） | 同左（Phase 1） |

---

## 配置设计

### 配置字段

```yaml
traffic_capture:
  mode: forwarding-priority   # forwarding-priority | capture-completeness-priority
  queue_depth: 1000          # 有界队列深度（事件数）；必须 > 0，panic 否则
  spill_threshold: 0.8        # capture-completeness-priority Phase 2：队列达此比例时开始 spill-to-disk
  max_spill_bytes: 1GB        # Phase 2：磁盘暂存上限（占位值，待基准测试验证）
  block_timeout: 100ms        # Phase 2：背压最大等待时间（占位值）
  auth_credential_filter: true # 两者均适用：是否过滤 AUTH 明文凭证
```

**`queue_depth`、`block_timeout` 为占位参数**，不得在未测试的情况下将其作为已承诺的性能目标。**`max_spill_bytes` 为占位值**，Phase 2 实现前须通过基准测试验证。

### 模式切换

- 启动时选择；运行中不支持切换。
- 切换需重启：forwarding-priority 队列清零；capture-completeness-priority 尝试排空后重启。

### 非法配置处理

- `mode` 非法：启动失败。
- `queue_depth <= 0`：`panic`。
- `spill_threshold < 0` 或 `> 1`：归一化。
- `max_spill_bytes <= 0`：`panic`。

---

## 敏感内容过滤

### AUTH 凭证过滤

**过滤在事件进入队列或磁盘暂存之前执行**，不依赖于队列策略。

go-smtp 支持的 AUTH 机制（`conn.go:858-936`）：PLAIN、LOGIN，以及通过 `AuthSession` 接口的自定义机制。

**实现路径分析**（conn.go:858-936）：
- AUTH 命令处理通过 `handleAuth()` → SASL `sasl.Next()` 循环。
- LOGIN 机制的用户名/密码行通过 `c.readLine()` 读取，与普通 SMTP 命令行走同一读循环。
- **LOGIN 用户名/密码行会经过 `trafficObserverWriter.Write`**，因此在采集层可以实现过滤。
- PLAIN 凭证在 `AUTH ` 命令行中（`handleAuth` 的 `arg` 参数），同样经过读循环。

#### AUTH PLAIN

```
C: AUTH PLAIN <base64-encoded-identity>
```

PLAIN 凭证在 `AUTH ` 命令行中，以空格分隔。过滤规则：
- 识别 `AUTH PLAIN` 开头的命令行
- 提取 AUTH 方法名（保留），丢弃空格后的全部参数
- 结果示例：`AUTH PLAIN [FILTERED]`

**实现复杂度**：低。只需在流量事件处理层检测 `client_to_server` 行内容即可，无需修改 go-smtp 内部逻辑。

#### AUTH LOGIN

```
C: AUTH LOGIN
S: 334 VXNlcm5hbWU6        (Username:)
C: <base64-encoded-username>
S: 334 UGFzc3dvcmQ6         (Password:)
C: <base64-encoded-password>
```

LOGIN 的用户名和密码在独立的响应行中。**注意**：go-smtp 的 AUTH LOGIN 不使用独立状态机，而是通过 SASL 接口实现。LOGIN 的"等待用户名/等待密码"状态由 SASL 实现层管理，`handleAuth()` 中无法直接区分当前在等待哪一步。

**过滤方案比较**：

| 方案 | 实现路径 | 复杂度 | 可靠性 | 建议 |
|------|---------|--------|--------|------|
| A. 采集层状态机 | 在消费者 goroutine 中跟踪 AUTH LOGIN 握手状态 | 中 | 高（需解析 SMTP 协议流） | **Phase 1 采用** |
| B. go-smtp SASL 扩展 | 修改 go-smtp 的 LOGIN 实现，暴露状态 | 高 | 高（但修改基线库） | Phase 2 评估 |
| C. 保守方案：仅过滤 AUTH LOGIN 命令行 | 不过滤用户名/密码行，仅过滤 `AUTH LOGIN` 命令本身 | 低 | 低（用户名/密码仍暴露） | 临时方案 |

**推荐**：Phase 1 采用方案 A（采集层状态机）。通过解析 `client_to_server` 和 `server_to_client` 事件流，识别 AUTH LOGIN 握手序列，在消费者 goroutine 中跟踪状态并过滤用户名/密码行。

**风险**：方案 A 依赖协议流解析，若 SMTP 实现不标准（如服务器响应不包含标准提示文本），状态机可能错位。建议在测试中覆盖多种 AUTH LOGIN 变体。

#### AUTH CRAM-MD5（如果实现）

```
C: AUTH CRAM-MD5
S: 334 <challenge>
C: <base64-encoded-response>
```

CRAM-MD5 响应包含用户名和 HMAC 摘要。过滤规则：
- 识别 `AUTH CRAM-MD5` 后的 challenge（服务端）
- 识别客户端响应中的用户名（base64，解码后提取），丢弃 HMAC 部分
- **注意**：HMAC 本身包含密码的 challenge-response，无法在不解析的情况下过滤；安全评估后决定是否支持 CRAM-MD5 或禁用

#### 其他 AUTH 机制

其他方法（包括自定义 SASL 机制）遵循通用规则：
- 无法识别的 AUTH 命令参数**默认视为敏感**，全部过滤
- 产生 `auth_method: unknown` 标记，供安全评估审查

#### 认证状态跟踪（采集层实现）

| 状态 | 触发条件 | 行为 |
|------|---------|------|
| `idle` | 连接建立 | 等待 AUTH 命令 |
| `auth_start` | 收到 `AUTH <method>` | 等待响应（LOGIN）或不等待（PLAIN） |
| `auth_username` | LOGIN 收到用户名（`client_to_server` 行，过滤） | 过滤下一行 |
| `auth_password` | LOGIN 收到密码（`client_to_server` 行，过滤） | 过滤下一行后重置 |
| `auth_success` | 认证成功（`server_to_client` 含 235） | 记录成功事件 |
| `auth_failure` | 认证失败（`server_to_client` 含 535 或 454） | 记录失败事件 |
| `auth_abort` | 连接关闭/超时 | 记录中断事件 |

**状态机实现在消费者 goroutine 中**，不修改 go-smtp 内部逻辑。消费者通过解析双向事件流重建 AUTH 握手状态。

#### Base64 编码不是脱敏

base64 编码是格式，不是安全机制。过滤规则**不得**仅因为内容是 base64 编码就视为安全。

#### 降级记录中的凭证保护

降级记录（gap record）**永远不包含**原始凭证数据，仅包含：`reason`、`direction`、`original_length`（若可知）和 `auth_method`（方法名，无参数）。

---

## 回调异常处理

### Panic 恢复

`trafficObserverWriter.Write` 和 `trafficObservedWriter.Write` 中的回调调用在 SMTP 连接 goroutine 中执行。go-smtp 在 `handle` 方法中通过 `recover()` 捕获 panic（`conn.go:133-141`），但**当前实现中 observer 回调的 panic 会被 recover 吞没，不会导致连接关闭，也不会产生任何错误日志**。

### 修复要求

1. **Panic 不影响 SMTP 连接**：回调 panic 时，连接正常处理，事件丢失但 SMTP 协议继续运行。
2. **产生 `reason: callback_failed` 缺口记录**：若入队成功，产生缺口记录；若队列也已满，事件完全丢失（尽力恢复）。
3. **日志记录**：每次回调 panic 产生结构化日志，包含连接标识、方向和堆栈摘要（不记录敏感数据）。

### goroutine 终止的限制

不得使用 `context.WithTimeout` + `go func()` 来"安全终止"任意阻塞的 observer 回调。Go 的 goroutine 无法从外部强制终止，只能通过关闭 channel 协调优雅退出。如果 observer 回调阻塞在某个系统调用上（如写入文件），goroutine 无法中断该调用。

**安全设计**：observer 回调本身必须是可中断的（快速返回）。如果 observer 需要执行慢 I/O，必须先入队（不阻塞），再由独立的消费者 goroutine 处理慢 I/O。

### 缺口记录自身失败的递归防护

缺口记录用于报告其他事件丢失，因此**禁止以任何方式触发递归观察**：

- 缺口记录的产生不得调用原始 `TrafficObserver` 回调（避免递归 panic）。
- 缺口记录入队失败（队列也已满）时，记录内部计数器并产生进程级指标，**不得再次产生缺口记录**。
- 进程级指标示例：`capture_gap_dropped_total`（缺口记录自身丢弃计数）。

---

## 内存分配与数据所有权

### 当前实现的分配成本

`trafficObserverWriter.Write` 和 `trafficObservedWriter.Write` 均执行 `append([]byte(nil), p...)`，每次 Write 分配一个新的字节数组。

在高频场景（每个 SMTP 命令行一次 Write；DATA 阶段每块数据一次 Write），这会产生大量短生命周期数组，增加 GC 压力。

### 目标架构的分配策略

**入队时**：不再在 Writer 中复制数据。回调接收到原始 `p []byte` 切片后，直接尝试非阻塞入队。

**所有权转移**：数据的所有权从 SMTP 连接 goroutine 转移到消费者 goroutine 时，必须通过以下方式之一：

1. **所有权转移（推荐）**：入队操作后，连接 goroutine 不再持有 `p` 的引用，消费者 goroutine 持有唯一引用。
2. **复制到堆**：消费者 goroutine 从 channel 收到数据后，执行 `append([]byte(nil), p...)` 复制。Writer 中不复制的代价是消费者必须立即复制。

**不得复用 `p` 缓冲区**：Go SMTP 行读取可能复用同一个底层数组（`textproto.Reader` 使用固定大小的 buffer）。如果 observer 回调持有 `p` 的引用后返回，连接 goroutine 的下一次读取会覆盖该缓冲区。因此，**从 channel 接收数据后，消费者必须立即复制或处理，不得持有原始引用超过处理完成**。

### 缓冲区复用风险

`trafficObserverWriter.Write` 被调用时，传入的 `p []byte` 底层数组可能在下一次 `readLine` 调用时被覆盖。这在同步回调模式下尤其危险：在 observer 处理完成前，底层 buffer 可能已被下一次读取覆盖。

**目标架构的缓解**：通过非阻塞入队，将数据所有权转移给消费者 goroutine 后，连接 goroutine 可立即返回并继续读取下一行。

---

## 持久化确认与 ADR-001 的关系

### 两个 ADR 的关注点不同

| ADR | 关注点 | 约束内容 |
|-----|--------|---------|
| ADR-001 | go-smtp 源码的补丁维护 | TrafficObserver 回调的签名、语义和调用位置 |
| ADR-002 | TrafficObserver 事件的消费与存储 | 队列、背压、持久化、过滤 |

ADR-002 的"持久化确认"指：事件从队列中被消费者处理后，写入持久化存储（如文件、数据库）并获得确认。这是**事件消费生命周期**的一部分，与 ADR-001 的回调性能约束是正交的两个维度。

ADR-001 约束的是：回调本身要快（无阻塞），避免影响 SMTP 协议处理。ADR-002 约束的是：消费者如何处理事件（队列、背压、持久化、过滤）。

### 潜在冲突

ADR-001 的当前实现分析指出：回调在 `Write()` goroutine 中同步执行，慢回调会阻塞协议读写。这是**当前实现**的限制。

ADR-002 的目标架构通过非阻塞入队消除了这一限制：SMTP 连接 goroutine 仅执行入队操作（不阻塞），慢处理在消费者 goroutine 中进行。

两个 ADR 不存在实际冲突：ADR-001 管理 go-smtp 补丁层面的决策；ADR-002 管理消费端的架构决策。

### ADR-005 依赖说明

ADR-002 正文中引用了 ADR-005（STARTTLS 后 TrafficObserver 连续性保证）。**ADR-005 目前尚未创建**，属于 Phase 1 期间的待交付物。

本 ADR 中关于 STARTTLS 行为（`handleStartTLS` 调用 `c.init()` 重新初始化 observer 包装）的描述基于对 `conn.go:959-994` 的代码分析，不依赖 ADR-005 的存在。ADR-005 的创建计划应与 Phase 1 实现同步确认。

---

## 共同约束（两种模式均适用）

### 采集层边界

| 采集层 | 捕获对象 | 采集位置 | 两种模式的适用性 |
|--------|---------|---------|----------------|
| TLS 密文层 | TLS Record Layer 原始字节 | `net.Conn` 读写（`tappedConn`） | 两种模式均独立运行 |
| SMTP 明文层 | 解密后 SMTP 命令/响应/DATA 内容 | `trafficObserverWriter`/`trafficObservedWriter` | 两种模式共享接口，差异在消费端 |

### 完整性指标（独立于邮件投递统计）

- `capture_events_total`：总事件数
- `capture_events_degraded`：降级记录数
- `capture_events_spilled`：磁盘暂存事件数（Phase 2）
- `capture_events_persisted`：已持久化确认事件数（Phase 2）
- `capture_gap_count`：缺口记录总数
- `capture_gap_dropped_total`：缺口记录自身丢弃计数
- `capture_callback_errors`：回调异常次数
- `mail_deliveries_total` / `mail_deliveries_failed`：邮件投递统计（与采集指标独立）

---

## 实施复杂度与分阶段范围

### Phase 1（必须实现）

| 功能 | 复杂度 | 实现目标 |
|------|-------|---------|
| 有界内存队列 | 低 | channel 或 ring buffer |
| 非阻塞入队（降级记录） | 低 | select + default case |
| 缺口记录结构 | 低 | JSON |
| forwarding-priority 模式 | 低 | 队列满则降级 |
| AUTH PLAIN 过滤 | 低 | 命令行参数过滤 |
| AUTH LOGIN 状态机（采集层） | 中 | 消费者 goroutine 中解析协议流 |
| 回调异常处理 | 低 | recover + 缺口记录 + 日志 + 递归防护 |
| 监控指标导出 | 低 | 结构化日志 + Prometheus 指标 |
| 读方向非阻塞 | 低 | 非阻塞入队，读循环不受影响 |
| Phase 1 验收测试 | 中 | panic、队列满载、缺口记录、连接隔离 |

### Phase 2（增强能力）

| 功能 | 复杂度 | 说明 |
|------|-------|------|
| spill-to-disk | 中 | 临时文件 + rename-on-write |
| 背压传导 | 中 | block_timeout + 降级 |
| 持久化确认与恢复 | 中 | 崩溃后扫描和恢复暂存文件 |
| AUTH CRAM-MD5 评估 | 低 | 安全评估后决定是否支持 |
| 运行中模式切换 | 高 | 需要队列迁移 |
| 磁盘队列自动扩容 | 中 | 根据磁盘空间动态调整 |

---

## 验证标准

以下标准在 Phase 1 实现完成后必须满足。**不得将尚未执行的测试写成已通过。**

### Phase 1 功能验证

1. `cd tests && go test -race -v ./...` 全部通过（来自 ADR-001 的 13 项集成测试基线）。
2. forwarding-priority：队列满载时，写入路径不阻塞；降级记录产生；缺口计数器增加；邮件转发不受影响。
3. capture-completeness-priority：内存队列满载时，读循环继续（无阻塞）；降级记录产生；SMTP 命令处理不受影响。

### AUTH 凭证过滤验证（Phase 1）

4. AUTH PLAIN：发送 `AUTH PLAIN <base64-encoded>`，降级记录中不包含原始凭证，仅包含 `AUTH PLAIN [FILTERED]`。
5. AUTH LOGIN：完整 LOGIN 握手（用户名+密码行），降级记录中不包含 base64 编码的用户名或密码。
6. 其他 AUTH 方法：`AUTH FOO arg1 arg2` 中，参数被过滤，结果为 `AUTH FOO [FILTERED]`。
7. 认证状态跟踪：成功认证产生 `auth_success` 事件；失败产生 `auth_failure` 事件；连接中断产生 `auth_abort` 事件。

### 回调异常验证（Phase 1）

8. observer 回调 panic：连接正常处理；产生 `reason: callback_failed` 缺口记录；SMTP 协议继续。
9. 降级记录在队列满时仍能入队（不死锁）：消费者停止消费后，队列满载后降级记录继续产生。
10. 缺口记录自身无法入队时：进程级 `capture_gap_dropped_total` 计数器增加；不产生递归缺口记录。

### 连接隔离验证（Phase 1）

11. 读方向 observer 阻塞：仅影响当前连接的读循环；其他连接的 SMTP 命令处理不受影响。

### Phase 2 验证（Phase 2 实现后补充）

12. capture-completeness-priority：磁盘队列满载时，背压传导（延迟可测）；事件写入磁盘（可验证文件存在）；重启后文件被拾取。
13. forwarding-priority：消费者停止消费，队列满载后降级记录持续产生；重启后队列清零，无残留。

---

## 待确认事项

以下事项需要基准测试或安全评估后才能确定：

| 事项 | 待验证/待决策 | 责任边界 |
|------|-------------|---------|
| `queue_depth` 默认值 | 基准测试：32 连接×10 事件/连接时内存占用 | 实现团队 |
| `block_timeout` 默认值 | 基准测试：捕获吞吐量和 SMTP 超时配置 | 实现团队 |
| p99 回调延迟目标 | 基准测试：1ms / 5ms / 其他 | 产品确认 |
| `max_spill_bytes` 实际合理值 | Phase 2 基准测试：磁盘 I/O 吞吐和邮件大小分布 | 实现团队 |
| AUTH CRAM-MD5 是否支持 | Phase 2 安全评估：HMAC 无法完全过滤，是否禁用 | 安全评估 |
| Subject 行和正文是否过滤 | 合规评估 | 产品/法务 |
| 降级记录保留期限 | 需求澄清 | 产品 |
| 邮件内容保留策略 | 独立专项 | 产品 |
| Go 版本最低支持策略 | Constitution 未解决 | 项目级决策 |
| ADR-005 创建计划 | STARTTLS 后 observer 连续性保证 | 实现团队 |

---

## 相关 ADR

- ADR-001：go-smtp 本地补丁维护策略
- ADR-002：本文档
- ADR-003：测试回归门槛（参见 `tests/` 下的 13 项集成测试）
- ADR-005：**待创建**：STARTTLS 后 TrafficObserver 连续性保证

**测试基线保护**：ADR-001 明确保护的 13 项集成测试（`tests/integration_test.go` 等）构成 MailShield 技术基线的回归门槛。任何影响 SMTP 核心库、TrafficObserver、TLS 或连接生命周期的变更，必须运行全部回归测试。

---

## 修订历史

| 日期 | 版本 | 变更内容 |
|------|------|---------|
| 2025-10-15 | 0.1 | 初始草案 |
| 2025-10-15 | 0.2 | 新增双策略模式 |
| 2025-10-15 | 0.3 | 全面修订：当前实现与目标架构区别；AUTH LOGIN/PLAIN/CRAM-MD5 过滤；磁盘配额与故障处理；服务关闭语义；回调异常处理；ADR-001 关系澄清；内存所有权 |
| 2025-10-15 | 0.4 | 分离 Phase 1 / Phase 2 场景行为表；修正读方向非阻塞语义描述；AUTH LOGIN 实现路径分析（采集层状态机，推荐 Phase 1 采用）；缺口记录递归防护；ADR-005 标记为待创建；测试基线保护引用；Go 版本依赖记录 |
