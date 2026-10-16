# Research: Phase 001 双 TLS 架构可行性验证

**Date**: 2026-10-16
**Spec**: [spec.md](./spec.md)

---

## Decision 1：Go crypto/tls KeyLogWriter TLS 1.3 字段覆盖度

**结论**：Go 1.24 `crypto/tls` KeyLogWriter 仅能导出 TLS 1.3 握手阶段的 4 个 traffic secret，无法导出密钥更新后的新代系 secret，也无法导出 `exporter_master_secret` 和 `resumption_master_secret`。

### TLS 1.3 — Go 可导出的字段

| 字段 | NSS Label | 说明 |
|------|-----------|------|
| `client_handshake_traffic_secret` | `CLIENT_HANDSHAKE_TRAFFIC_SECRET` | TLS 1.3 握手密钥推导后，两侧均输出 |
| `server_handshake_traffic_secret` | `SERVER_HANDSHAKE_TRAFFIC_SECRET` | TLS 1.3 握手密钥推导后，两侧均输出 |
| `client_traffic_secret_0` | `CLIENT_TRAFFIC_SECRET_0` | 服务器 Finished 后输出，仅 epoch 0 |
| `server_traffic_secret_0` | `SERVER_TRAFFIC_SECRET_0` | 服务器 Finished 后输出，仅 epoch 0 |

**数据来源**：Go 源码 `handshake_client_tls13.go:506-515`（握手 secrets）、`718-727`（流量 secrets）；`handshake_server_tls13.go:766-775`、`913-922` 对应服务端侧。

### TLS 1.3 — Go 不可导出的字段

| 字段 | 不可导出的原因 |
|------|---------------|
| `client_traffic_secret_N`（N>0） | 密钥更新时 Go 不写 keylog；`writeKeyLog` 仅在首次密钥推导时调用 |
| `server_traffic_secret_N`（N>0） | 同上 |
| `exporter_master_secret` | 仅通过 `ExportKeyingMaterial()` API 暴露，不写入 keylog |
| `resumption_master_secret` | 仅用于 session ticket PSK 推导，Go 不输出 |

### TLS 1.2

| 字段 | NSS Label | 说明 |
|------|-----------|------|
| `client_random` | `CLIENT_RANDOM` | 客户端侧输出（16进制） |
| `master_secret` | 随 `CLIENT_RANDOM` 同一行输出 | 客户端侧输出（16进制，48字节） |

### 对 Phase 001 验收的影响

- **可验证通过**：TLS 1.3 握手阶段 traffic secret（4个）；TLS 1.2 master_secret
- **无法通过 KeyLogWriter 验证**：TLS 1.3 密钥更新后的新代系 secret（无 keylog 条目）；`exporter_master_secret`；`resumption_master_secret`
- **验证报告中须明确标记**：上述字段属于"当前机制无法获取"类别，并评估通过其他机制（如直接拦截 TLS 握手消息）获取的可行性

---

## Decision 2：HTTP CONNECT 与邮件客户端兼容性

**结论**：主流桌面邮件客户端中，**仅 Thunderbird** 原生支持 HTTP CONNECT 代理 SMTP 流量；Apple Mail 和 Outlook 均不支持，需要通过外部工具或操作系统代理实现。

### 各客户端兼容性

| 客户端 | HTTP CONNECT for SMTP | SOCKS for SMTP | 备注 |
|--------|----------------------|----------------|------|
| Thunderbird | ✅ 原生支持 | ✅ 原生支持 | 路由所有协议通过 HTTP 代理 |
| Apple Mail | ❌ 不支持 | ❌ 不支持 | 直接 TCP，忽略系统 HTTP 代理设置 |
| Microsoft Outlook | ❌ 不支持 | ❌ 不支持 | IMAP/SMTP 不走系统代理 |
| Mailbird | ✅（走系统代理） | 未知 | |
| K-9 Mail / Thunderbird Android | ✅（PR #11155） | ✅ | |

### HTTP CONNECT + SMTP 的实际可行性

- **协议层面**：HTTP CONNECT（RFC 9110）可建立到任意端口的 TCP 隧道，SMTP 流量透传完全合规
- **实际限制**：大多数企业/公共代理默认阻止 25/465/587 端口（Squid 默认配置即阻止），防止成为垃圾邮件中继
- **Phase 001 验证策略**：使用 curl、openssl s_client 作为协议验证工具；Thunderbird 作为唯一已确认兼容的真实客户端；其他客户端记录为"未验证"

### 自签名 CA 信任建立

- **关键结论**：HTTP CONNECT 隧道不改变 TLS 证书验证模型；证书信任建立与代理无关
- **Thunderbird**：交互式例外对话框（cert9.db 存储），IMAP 和 SMTP 证书例外需分别添加
- **Apple Mail**：需操作 macOS Keychain，无交互式对话框，较为繁琐
- **Outlook**：需导入 Windows 证书存储，无交互式 SMTP 例外流程

---

## Decision 3：STARTTLS 最小 SMTP 协议状态处理

Phase 001 STARTTLS 验证仅需处理以下状态转换：

1. **检测 `STARTTLS` 命令**：在 SMTP 命令行中识别 `STARTTLS`（不解析其他命令）
2. **触发 TLS 握手升级**：返回 `220 Ready to start TLS`，启动 TLS 握手
3. **握手完成后明文命令可继续**：TLS 握手完成后，SMTP 会话继续处理 EHLO/AUTH/MAIL/RCPT/DATA 等

**不属于 Phase 001 范围**：解析 EHLO 响应内容、处理 AUTH 命令参数、执行邮件投递逻辑。

---

## Alternatives Considered

1. **使用 Wireshark/LTSV 插件直接解析 TLS**：替代 KeyLogWriter 方案，可导出完整密钥材料，但实现复杂度高，不适合 Phase 001 验证
2. **使用真实邮件客户端（Thunderbird + Outlook）验证 CONNECT 兼容性**：Thunderbird 可验证，Outlook 不支持 HTTP CONNECT for SMTP，无法作为通用验收标准
