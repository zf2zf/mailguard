---
description: "Task list for Phase 001 双TLS架构可行性验证"
---

# Tasks: Phase 001 双 TLS 架构可行性验证

**Input**: Design documents from `/specs/001-双TLS架构可行性验证/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, quickstart.md

**Tests**: Phase 001 为可行性验证，原型包含自含验证代码；不新增 `tests/` 目录下的集成测试

---

## Phase 1: Setup（项目初始化）

**Purpose**: 建立 Phase 001 原型项目结构，按正式 Go 工程目录组织

- [ ] T001 [P] 初始化 Go 模块：`go mod init github.com/zf2zf/mailshield`（路径根目录 `go.mod`）
- [ ] T002 [P] 创建 `cmd/mailshield/main.go`，含命令行标志定义（`-mode`, `-listen`, `-upstream`, `-keylog`, `-report` 等）
- [ ] T003 [P] 创建 `internal/tlskeylog/` 包结构
- [ ] T004 [P] 创建 `internal/connectproxy/` 包结构
- [ ] T005 [P] 创建 `internal/tlsterminate/` 包结构
- [ ] T006 [P] 创建 `internal/upstreamtls/` 包结构
- [ ] T007 [P] 创建 `internal/plaintextcapture/` 包结构
- [ ] T008 [P] 创建 `internal/starttls/` 包结构
- [ ] T009 创建 `testdata/` 目录（用于存放测试证书生成脚本）

**Checkpoint**: 项目结构建立，可运行 `go build ./cmd/mailshield/`

---

## Phase 2: Foundational（基础组件）

**Purpose**: 核心基础设施，所有用户场景共用的组件

- [ ] T010 [P] 实现 `internal/tlskeylog/keylog_writer.go`：包装 `crypto/tls.Config.KeyLogWriter`，输出到文件
- [ ] T011 [P] 实现 `internal/tlskeylog/verifier.go`：解析 NSS 格式 keylog 行，输出 `TLSKeyLogEntry`；按 `KeyFieldVerdict` 分类字段可获取性
- [ ] T012 [P] 实现 `internal/connectproxy/proxy.go`：实现 HTTP CONNECT 隧道（`net/http` HandleConnect），建立到上游 TCP 连接并双向透传
- [ ] T013 [P] 实现 `internal/upstreamtls/client.go`：实现上游 SMTP TLS 客户端（TLS 1.2/1.3），支持 `crypto/tls.Config` 注入（含 KeyLogWriter）
- [ ] T014 [P] 实现 `internal/plaintextcapture/capture.go`：实现双向明文采集 channel，`Direction` 标记 `client_to_server`/`server_to_client`，通过 `ConnectionID` 关联会话
- [ ] T015 [P] 实现 `internal/tlsterminate/server.go`：实现客户端侧 TLS 终止（作为 TLS 服务器），可注入 `crypto/tls.Config`（含 KeyLogWriter）
- [ ] T016 实现 `internal/starttls/assessor.go`：实现 STARTTLS 评估——检测 `STARTTLS` 命令、触发 TLS 握手、验证握手完成后明文 SMTP 命令可继续

**Checkpoint**: 基础组件就绪，可组合实现场景

---

## Phase 3: US1 - 场景 0 端到端全链路原型（P0）

**Goal**: 验证完整双 TLS 终止架构的端到端明文采集链路（客户端 TLS 终止 + 上游 TLS 会话 + 双向明文采集全组件串接）

**Independent Test**: 运行原型，`openssl s_client` 连接客户端侧 TLS 端口，发送 SMTP 命令序列，验证双向明文输出

- [ ] T017 [P] 实现 `cmd/mailshield/` 中的场景 0 组合逻辑：客户端侧 TLS 终止 → `plaintextcapture` → 上游 `upstreamtls` → 上游 SMTP 服务器
- [ ] T018 [P] 实现 `BidirectionalSMTPSession` 关联逻辑：根据 `ConnectionID`（客户端侧连接 ID + 上游连接 ID）关联双向流
- [ ] T019 实现场景 0 验证命令脚本 `scripts/verify_scene0.sh`：启动测试 SMTP 服务器、运行原型、触发 TLS 连接、输出双向明文采集结果
- [ ] T020 验证场景 0 输出 `BidirectionalSMTPSession`：双向 SMTP 明文均可采集，方向可区分，会话可关联

---

## Phase 4: US2 - 场景 1/FR-001 TLS 1.2 密钥采集（P1）

**Goal**: 验证 Go KeyLogWriter 对 TLS 1.2 `master_secret`、`client_random`、`server_random`、`cipher_suite` 的导出能力

**Independent Test**: 运行 TLS 1.2 连接，检查 keylog 文件包含 `CLIENT_RANDOM` 行且 `master_secret` 长度 48 字节

- [ ] T021 [P] 实现 TLS 1.2 KeyLogWriter 配置注入（在 `tlsterminate/server.go` 和 `upstreamtls/client.go` 中均可配置）
- [ ] T022 [P] 实现 `internal/tlskeylog/tls12_verifier.go`：解析 TLS 1.2 keylog，验证 `CLIENT_RANDOM` 32 字节、`master_secret` 48 字节、`cipher_suite` 非空
- [ ] T023 实现场景 1 验证脚本 `scripts/verify_scene1_tls12.sh`：强制 TLS 1.2 握手（`-tls1_2`），输出 keylog 分析结果
- [ ] T024 按 spec.md FR-001 表逐字段验证，记录 `KeyFieldVerdict`；`master_secret` 无法获取时记录为"已知限制"

---

## Phase 5: US3 - 场景 2/FR-002 TLS 1.3 密钥采集（P1）

**Goal**: 验证 Go KeyLogWriter 对 TLS 1.3 各字段的导出，区分可获取与不可获取字段

**Independent Test**: 运行 TLS 1.3 连接，检查 keylog 包含 4 个握手 traffic secret；验证密钥更新后无新 keylog 条目

- [ ] T025 [P] 实现 TLS 1.3 KeyLogWriter 配置注入（同 T021）
- [ ] T026 [P] 实现 `internal/tlskeylog/tls13_verifier.go`：解析 TLS 1.3 keylog，验证 4 个握手阶段 secret（`CLIENT/SERVER_HANDSHAKE_TRAFFIC_SECRET`、`CLIENT/SERVER_TRAFFIC_SECRET_0`）均可获取
- [ ] T027 实现场景 2 验证脚本 `scripts/verify_scene2_tls13.sh`：强制 TLS 1.3 握手（`-tls1_3`），输出 keylog 分析结果
- [ ] T028 逐字段验证 TLS 1.3：`client_traffic_secret_N`（N>0）标记 UNAVAILABLE（密钥更新后无 keylog 条目）；`exporter_master_secret` 标记 UNAVAILABLE（仅 API 暴露，不写 keylog）；`resumption_master_secret` 标记 UNAVAILABLE（Go 不输出）

---

## Phase 6: US4 - 场景 3 HTTP CONNECT 隧道（P1）

**Goal**: 验证 HTTP CONNECT 代理可建立 TCP 隧道并透传 SMTP 流量

**Independent Test**: 通过 HTTP CONNECT 发送 SMTP EHLO 命令，验证收到 250 响应

- [ ] T029 [P] 完善 `internal/connectproxy/proxy.go`：支持多个并发 CONNECT 会话，正确处理连接关闭和隧道释放
- [ ] T030 实现场景 3 验证脚本 `scripts/verify_scene3_connect.sh`：通过 curl 或 netcat 发送 HTTP CONNECT + SMTP 命令序列，验证双向透传
- [ ] T031 验证至少 5 并发 CONNECT 会话同时维持

---

## Phase 7: US5 - 场景 4 客户端证书信任（P1）

**Goal**: 验证 MailShield 可生成目标 hostname 证书，记录客户端信任链建立方式

**Independent Test**: 使用 openssl 验证证书链、`Subject Alternative Name` 匹配目标 hostname

- [ ] T032 [P] 实现 `internal/tlsterminate/cert.go`：使用 `crypto/x509` 生成测试用 CA 根证书和目标 hostname 证书（RSA + ECDSA）
- [ ] T033 [P] 实现 `scripts/gen_test_certs.sh`：自动生成 `testdata/testca.crt`、`testdata/client.crt`、`testdata/client.key`
- [ ] T034 验证证书链可被 openssl 验证（`openssl verify -CAfile testdata/testca.crt testdata/client.crt`）；记录 Thunderbird/Apple Mail/Outlook 各自证书信任建立步骤

---

## Phase 8: US6 - 场景 5 明文流量采集与会话关联（P2）

**Goal**: 验证双向解密后 SMTP 明文字节流均可采集，方向可区分，会话可关联

**Independent Test**: 运行全链路，输出 `BidirectionalSMTPSession` JSON，验证 `Direction`、`ConnectionID` 字段正确

- [ ] T035 [P] 完善 `plaintextcapture/capture.go` 中的 `SMTPEvent` 结构：含 `Direction`（`client_to_server`/`server_to_client`）、`RawBytes`、`DecryptedContent`、`IsAUTHCommand`
- [ ] T036 [P] 实现 `BidirectionalSMTPSession` 关联逻辑：根据连接对标识（客户端地址:端口 + 上游连接 ID）生成 `AssociationKey`
- [ ] T037 实现 `scripts/verify_scene5_plaintext.sh`：运行全链路，输出双向 SMTP 明文采集 JSON，验证 AUTH PLAIN/LOGIN 命令行可识别（标记，不过滤）

---

## Phase 9: US7 - 场景 6 STARTTLS 评估（P2）

**Goal**: 评估隐式 TLS 和 STARTTLS 行为差异，验证 TrafficObserver 在 TLS 升级后连续性

**Independent Test**: 分别构造隐式 TLS 和 STARTTLS 连接，验证两种模式状态转换正确

- [ ] T038 [P] 实现 `internal/starttls/assessor.go` 中的隐式 TLS 评估：直接 TLS 连接（无 STARTTLS），验证 TLS 握手完成后 SMTP 命令正常处理
- [ ] T039 [P] 实现 STARTTLS 评估：检测 `STARTTLS` 命令 → 返回 `220 Ready to start TLS` → TLS 握手 → 握手完成后明文 SMTP 命令继续处理
- [ ] T040 实现 `scripts/verify_scene6_starttls.sh`：分别运行隐式 TLS 和 STARTTLS 模式，输出 `STARTTLSAssessment`
- [ ] T041 验证 `ObserverContinuityMaintained`：STARTTLS 握手完成后，`TrafficObserver` 可继续采集明文（不中断）

---

## Phase 10: US8 - 验证报告生成（P1）

**Goal**: 生成 Phase 001 汇总验证报告，包含各字段可获取性分类和已知限制

**Independent Test**: 运行报告生成命令，验证报告包含所有验收标准项

- [ ] T042 [P] 实现 `internal/tlskeylog/reporter.go`：汇总 TLS 1.2/1.3 字段验证结果，生成 `Phase001VerificationReport`
- [ ] T043 [P] 实现 `cmd/mailshield/` 中的报告生成逻辑：汇总所有场景验证结果，输出 Markdown 格式报告
- [ ] T044 验证报告包含：TLS 1.2/1.3 各字段 `KeyFieldVerdict` 分类；HTTP CONNECT 隧道结果；双向明文采集结果；STARTTLS vs 隐式 TLS 差异；已知限制列表；测试数据清单（虚构）

---

## Phase 11: Polish & Cross-Cutting

**Purpose**: 原型收尾和数据保护

- [ ] T045 [P] 实现 `scripts/cleanup.sh`：验证完成后删除所有 keylog 临时文件、testdata/ 测试证书、后台 SMTP 进程
- [ ] T046 [P] 在 `quickstart.md` 基础上更新 `scripts/run_all_scenes.sh`：一键运行所有验证场景（A/B/C/D/E）
- [ ] T047 验证报告中的 `TestFixturesUsed` 均为虚构标识（不含真实认证数据）；`TLSSessionKeyMaterial.Secret` 使用虚构 48 字节替换
- [ ] T048 确保原型代码纳入仓库正式工程结构（`cmd/mailshield/`、`internal/`），不修改 `third_party/go-smtp/` 和 `tests/` 下的任何文件
- [ ] T049 运行 `go build ./cmd/mailshield/` 和 `go vet ./internal/...`，验证编译无错误

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1（Setup）**: 无依赖，可立即开始
- **Phase 2（Foundational）**: 依赖 Phase 1 完成；**阻塞所有用户场景**
- **Phase 3–10（用户场景）**: 均依赖 Phase 2 完成；US1–US8 可并行（不同场景组件独立）
- **Phase 11（Polish）**: 依赖 Phase 3–10 完成

### 用户场景并行机会

| 场景 | 组件 | 可与哪些场景并行 |
|------|------|-----------------|
| US1 场景0 全链路 | `main.go` 组合逻辑 | US2–US8 均独立 |
| US2 TLS 1.2 | `tlskeylog/tls12_verifier.go` | US3, US5, US6, US7 |
| US3 TLS 1.3 | `tlskeylog/tls13_verifier.go` | US2, US5, US6, US7 |
| US4 CONNECT | `connectproxy/proxy.go` | US2, US3, US5, US7, US8 |
| US5 证书 | `tlsterminate/cert.go`, `scripts/gen_test_certs.sh` | US2, US3, US4, US6, US7 |
| US6 明文采集 | `plaintextcapture/capture.go` | US2, US3, US4, US5, US7 |
| US7 STARTTLS | `starttls/assessor.go` | US2, US3, US4, US5, US6 |
| US8 报告 | `tlskeylog/reporter.go` | US2, US3, US4, US5, US6, US7 |

### 关键串行依赖

```
Phase 1 (Setup)
    ↓
Phase 2 (Foundational) ← 所有 US 共用，阻塞所有场景
    ↓
US1 ─┬─ US2 ─┬─ US3 ─┬─ US4 ─┬─ US5 ─┬─ US6 ─┬─ US7 ─┬─ US8
     └────────┴────────┴────────┴────────┴────────┴────────┘ (全部并行)
              ↓
          Phase 11 (Polish)
```

---

## 并行执行示例

```bash
# Phase 2 完成后，同时启动多个场景实现：

# 场景 2 (TLS 1.2) 和 场景 3 (TLS 1.3) 独立实现：
Task: "实现 internal/tlskeylog/tls12_verifier.go（T021, T022）"
Task: "实现 internal/tlskeylog/tls13_verifier.go（T025, T026）"

# 同时实现 HTTP CONNECT 和 STARTTLS：
Task: "实现 internal/connectproxy/proxy.go（T012, T029）"
Task: "实现 internal/starttls/assessor.go（T016, T038, T039）"
```

---

## Implementation Strategy

### MVP First（US1 场景 0）

1. Phase 1 → Phase 2 → **US1 场景 0**
2. 验证端到端全链路明文采集
3. **STOP，验证后再继续**

### 增量交付

1. Phase 1 + Phase 2 → 基础就绪
2. US1 → 验证全链路 → **第一个可演示成果**
3. US2 + US3 → 验证 TLS 密钥导出能力
4. US4 → 验证 HTTP CONNECT
5. US5 → 验证证书生成
6. US6 → 验证明文采集关联
7. US7 → 验证 STARTTLS
8. US8 → 生成最终报告
9. Phase 11 → 清理 + 收尾

---

## Notes

- `[P]` 标记的任务可在同一阶段内并行执行
- Phase 001 原型不修改 `tests/` 和 `third_party/go-smtp/` 下任何文件
- TLS 密钥日志（keylog）不得提交版本库
- 所有测试 SMTP 数据和认证凭证均为虚构
