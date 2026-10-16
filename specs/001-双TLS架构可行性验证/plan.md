# Implementation Plan: Phase 001 双 TLS 架构可行性验证

**Branch**: `001-双TLS架构可行性验证` | **Date**: 2026-10-16 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-双TLS架构可行性验证/spec.md`

---

## Summary

Phase 001 目标：验证 MailShield 采用 HTTP CONNECT、客户端侧 TLS 卸载、独立上游 TLS 会话的 SMTP 代理架构是否可行。核心研究结论：

1. **KeyLogWriter 覆盖度**：Go 1.24 `crypto/tls` KeyLogWriter 只能导出 TLS 1.3 握手阶段的 4 个 traffic secret（`CLIENT/SERVER_HANDSHAKE_TRAFFIC_SECRET`、`CLIENT/SERVER_TRAFFIC_SECRET_0`）和 TLS 1.2 `CLIENT_RANDOM`+`master_secret`；密钥更新后的新代系 secret、`exporter_master_secret`、`resumption_master_secret` 均无法通过 KeyLogWriter 获取。
2. **HTTP CONNECT 兼容性**：主流客户端中仅 Thunderbird 原生支持 HTTP CONNECT for SMTP；Apple Mail 和 Outlook 不支持 HTTP CONNECT 代理 SMTP 流量。
3. **STARTTLS 最小状态处理**：仅需检测 `STARTTLS` 命令 → 触发 TLS 握手升级 → 完成后明文继续，不构成完整 SMTP 协议解析。

---

## Technical Context

**Language/Version**: Go 1.24.0（与仓库 `tests/go.mod` 一致）

**Primary Dependencies**:
- Go 标准库：`crypto/tls`（KeyLogWriter）、`net/http`（HTTP CONNECT 代理）、`crypto/x509`（证书生成验证）
- `github.com/emersion/go-smtp`（vendored，本地补丁版，仅读取不修改）
- `openssl`（证书生成和 TLS 握手调试，仅验证用）

**Storage**: N/A——Phase 001 是可行性验证原型，不实现持久化存储

**Testing**: `go test`（回归测试保护）；手动 TLS 握手验证；原型验证代码自含测试

**Target Platform**: Linux/macOS，本地 localhost 验证环境

**Project Type**: SMTP 代理原型（可行性验证）

**Performance Goals**: N/A——Phase 001 不追求性能指标，验证目标为技术可行性

**Constraints**:
- 不得修改 Constitution、Accepted ADR 或受保护文件（`third_party/go-smtp/`、`tests/`）
- 原型代码纳入仓库正式工程目录结构（`cmd/`、`internal/` 等）
- TLS 密钥日志不得提交版本库（含虚构测试密钥）

**Scale/Scope**: 单机本地验证，无并发规模要求

---

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 约束 | 状态 | 说明 |
|------|------|------|
| Principle I 产品边界 | ⚠️ GATE | TLS 卸载架构尚未批准；Phase 001 原型验证结果将作为提出新 ADR 的技术依据；原型代码与生产代码隔离，不违反 Constitution |
| Principle II ADR 治理 | ✓ | 不修改已保护文件；不将 Proposed ADR 当作已批准约束 |
| Principle V 测试 | ✓ | 不修改 `tests/` 下的 13 项集成测试资产 |
| Principle VI Spec-Kit SDD | ✓ | 严格遵循 Spec-Kit 工作流 |

**复杂度跟踪**：无需跟踪——Phase 001 为隔离的可行性验证原型，不引入新架构模式或增加项目复杂度

---

## Project Structure

### Documentation（this feature）

```
specs/001-双TLS架构可行性验证/
├── spec.md              # 需求规格
├── plan.md              # 本文件
├── research.md           # Phase 0 输出（Go KeyLogWriter 研究、HTTP CONNECT 客户端兼容性研究）
├── data-model.md        # Phase 1 输出（验证数据实体定义）
├── quickstart.md        # Phase 1 输出（原型验证运行指南）
└── contracts/          # Phase 1 输出（N/A——Phase 001 无外部接口契约）
```

### Source Code（repository root）

```
cmd/mailshield-phase1/          # Phase 001 原型主程序
internal/
├── tlskeylog/                  # TLS KeyLogWriter 验证模块
│   ├── keylog_writer.go        # KeyLogWriter 包装
│   └── verifier.go             # 字段提取与验证
├── connectproxy/                # HTTP CONNECT 代理模块
│   └── proxy.go                # CONNECT 隧道处理
├── tlsterminate/               # 客户端侧 TLS 终止模块
│   └── server.go               # TLS 服务器端终止
├── upstreamtls/                # 上游 TLS 会话模块
│   └── client.go               # 上游 TLS 客户端
├── plaintextcapture/          # 明文流量采集模块
│   └── capture.go              # 双向明文采集与关联
└── starttls/                  # STARTTLS 评估模块
    └── assessor.go             # STARTTLS vs 隐式 TLS 行为评估

tests/integration_test.go       # 现有 13 项测试（受 ADR-001 保护，不修改）
third_party/go-smtp/            # go-smtp vendored（仅读取，不修改）
```

**Structure Decision**: Phase 001 原型代码放置于 `cmd/mailshield-phase1/` 和 `internal/` 下的新模块目录，按正式 Go 项目结构组织，作为后续阶段演进基础。原型不修改 `third_party/go-smtp/` 和 `tests/` 下的任何文件。

---

## Phase 0: Research

**完成状态**：✅ 已完成

- **Research 1**：Go `crypto/tls` KeyLogWriter TLS 1.3 字段覆盖度——明确了 4 个可导出字段和 3 个无法导出的字段
- **Research 2**：HTTP CONNECT 与邮件客户端兼容性——明确了 Thunderbird 是唯一原生支持的桌面客户端

**输出**：[research.md](./research.md)

---

## Phase 1: Design

**完成状态**：✅ 已完成

- **data-model.md**：定义了验证数据实体（TLSKeyLogEntry、KeyFieldVerdict、BidirectionalSMTPSession、SMTPEvent、STARTTLSAssessment）和数据流图
- **quickstart.md**：提供了 5 个验证场景的运行指南（HTTP CONNECT 隧道、双 TLS 终止明文采集、TLS 1.2/1.3 密钥导出、STARTTLS 评估）
- **contracts/**：N/A — Phase 001 是隔离的技术原型，不暴露外部接口

**输出**：[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)
