# Data Model: Phase 001 双 TLS 架构可行性验证

**Phase**: Phase 1
**Spec**: [spec.md](./spec.md)

---

## 一、验证数据实体

Phase 001 为可行性验证，原型代码中使用以下数据结构记录验证结果。

### 1.1 TLSKeyLogEntry（TLS 密钥日志条目）

从 Go `KeyLogWriter` 输出的 NSS 格式日志行解析得到的结构。

```
字段：
  - Label: string          // NSS keylog 标签，如 "CLIENT_HANDSHAKE_TRAFFIC_SECRET"
  - ClientRandom: []byte   // ClientHello.random，32 字节
  - Secret: []byte        // 对应 secret 原始字节
  - Timestamp: time.Time   // 行写入时间（可选，用于排序）
```

**用途**：验证 KeyLogWriter 对各字段的实际输出，记录字段可获取性。

### 1.2 KeyFieldVerdict（字段可获取性判定）

```
字段：
  - FieldName: string     // 字段名，如 "client_handshake_traffic_secret"
  - Verdict: enum {
      AVAILABLE,           // 可直接获取
      AVAILABLE_ALT,      // 需其他机制获取
      UNAVAILABLE         // 当前机制无法获取
    }
  - Evidence: string      // 证据：原始 keylog 行内容或 Go 源码引用
  - Notes: string         // 备注：获取条件、生命周期等
```

**用途**：按字段逐一记录可获取性分类，构成验证报告核心。

### 1.3 TLSSessionKeyMaterial（TLS 会话密钥材料汇总）

```
字段：
  - TLSVersion: string    // "1.2" 或 "1.3"
  - HandshakeSecrets: []KeyFieldVerdict  // 握手阶段各字段判定
  - ApplicationSecrets: []KeyFieldVerdict  // 应用数据阶段各字段判定
  - KeyUpdateHandled: bool  // 密钥更新是否可追踪（TLS 1.3）
  - KeyUpdateEvidence: string  // 密钥更新时的观察记录
```

### 1.4 BidirectionalSMTPSession（双向 SMTP 会话）

```
字段：
  - SessionID: string          // 唯一会话标识（UUID）
  - ClientToServerEvents: []SMTPEvent  // 客户端→服务器事件
  - ServerToClientEvents: []SMTPEvent  // 服务器→客户端事件
  - ClientTLSConnID: string   // 客户端侧 TLS 连接标识
  - UpstreamTLSConnID: string // 上游 TLS 连接标识
  - Associated: bool           // 双向流是否已关联
  - AssociationKey: string    // 关联键（如连接对标识）
```

### 1.5 SMTPEvent（SMTP 事件）

```
字段：
  - Direction: enum { client_to_server, server_to_client }
  - RawBytes: []byte         // 原始字节
  - DecryptedContent: string  // 解密后文本内容（ASCII）
  - IsAUTHCommand: bool       // 是否为 AUTH 相关命令（用于标记，不过滤）
  - Timestamp: time.Time
  - ConnectionID: string
```

### 1.6 STARTTLSAssessment（STARTTLS 评估结果）

```
字段：
  - Mode: enum { implicit_tls, explicit_starttls }
  - TLSHandshakeCompleted: bool  // TLS 握手是否完成
  - PostHandshakePlaintextOK: bool  // 握手完成后明文 SMTP 命令是否正常处理
  - ObserverContinuityMaintained: bool  // TrafficObserver 在 TLS 升级后是否连续
  - Notes: string
```

---

## 二、数据流（原型）

```
[测试客户端]
     │（明文 SMTP 或 TLS）
     ▼
[客户端侧 TLS 终止] ──→ [TrafficObserver] ──→ [明文采集 channel]
     │
     │（独立上游 TLS 会话）
     ▼
[上游 SMTP 服务器]
     │
     ▼
[KeyLogWriter] ──→ [密钥日志文件] ──→ [字段验证模块] ──→ [KeyFieldVerdict]
```

---

## 三、验证输出结构

Phase 001 原型生成以下验证报告（JSON 或 Markdown）：

```
Phase001VerificationReport:
  - TLS12:
      - client_random: KeyFieldVerdict
      - server_random: KeyFieldVerdict
      - cipher_suite: KeyFieldVerdict
      - master_secret: KeyFieldVerdict
  - TLS13:
      - client_handshake_traffic_secret: KeyFieldVerdict
      - server_handshake_traffic_secret: KeyFieldVerdict
      - client_traffic_secret_0: KeyFieldVerdict
      - server_traffic_secret_0: KeyFieldVerdict
      - key_update_handling: KeyFieldVerdict
      - exporter_secret: KeyFieldVerdict
      - resumption_master_secret: KeyFieldVerdict
  - HTTP_CONNECT: BidirectionalSMTPSession
  - STARTTLS: STARTTLSAssessment
  - EndToEnd: BidirectionalSMTPSession
  - TestFixturesUsed: []string  // 虚构 SMTP 测试数据和凭证标识
  - Limitations: []string      // 已知限制
```

---

## 四、数据保护约束

- 所有 `SMTPEvent.RawBytes` 和 `DecryptedContent` 字段在报告输出前必须使用虚构测试数据替换
- `TLSSessionKeyMaterial` 中的 `Secret` 字段使用虚构 48 字节替换
- 验证报告生成后立即删除原始密钥日志文件
