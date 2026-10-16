# Quickstart: Phase 001 原型验证运行指南

**Phase**: Phase 1
**Spec**: [spec.md](./spec.md)

---

## 一、运行前提

### 环境要求

- Go 1.24.0+
- openssl（证书生成和 TLS 握手调试）
- 本地 SMTP 测试服务器（可用 Python smtpd 或 MailHog）

### 证书准备（仅用于测试）

```bash
# 生成测试用 CA 根证书（虚构）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -days 365 -nodes \
  -subj "/CN=TestCA/O=MailShield/Test" \
  -out testdata/testca.crt

# 用该 CA 为测试 SMTP 服务器签发证书（虚构）
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -subj "/CN=localhost/O=TestServer" \
  -out testdata/server.csr

# （以下为概念说明， Phase 001 使用脚本自动完成）
```

---

## 二、构造验证场景

### 场景 A：HTTP CONNECT 隧道验证（场景 3）

**目标**：验证 HTTP CONNECT 代理可建立 TCP 隧道并透传 SMTP 流量。

```bash
# 启动本地 SMTP 测试服务器（端口 8025）
python3 -c "
import smtpd, asyncio
class TestSMTPServer(smtpd.SMTPServer):
    def process_message(self, peer, mailfrom, rcpttos, data):
        print(f'MAIL FROM: {mailfrom}')
        print(f'RCPT TO: {rcpttos}')
        print(f'DATA: {data[:100]}')
        return '250 OK'
server = TestSMTPServer(('127.0.0.1', 8025), None)
asyncio.run(server.serve_forever())
" &
SMTP_PID=$!

# 运行原型 CONNECT 代理（端口 1080）
go run ./cmd/mailshield-phase1/ \
  -mode=connect \
  -listen=:1080 \
  -upstream=127.0.0.1:8025

# 使用 curl 验证 SMTP 命令可透传
curl -x http://127.0.0.1:1080 \
  --connect-to smtp.local:587:127.0.0.1:1080 \
  telnet://smtp.local:587

# 验证输出：TCP 隧道建立，SMTP 220 响应可到达
# 停止服务器
kill $SMTP_PID
```

**预期结果**：HTTP CONNECT 返回 `200 Connection Established`；SMTP `EHLO` 命令得到 250 响应。

---

### 场景 B：双向 TLS 终止明文采集验证（场景 0）

**目标**：验证完整双 TLS 终止架构的端到端明文采集。

```bash
# 运行原型（完整双 TLS 终止模式）
go run ./cmd/mailshield-phase1/ \
  -mode=full \
  -client-cert=testdata/client.crt \
  -client-key=testdata/client.key \
  -upstream-tls=true \
  -listen=:1443 \
  -upstream=127.0.0.1:8025

# 使用 openssl s_client 模拟客户端 TLS 连接
openssl s_client -connect 127.0.0.1:1443 \
  -CAfile testdata/testca.crt \
  -starttls smtp \
  -tls1_2

# 在 openssl 会话中发送 SMTP 命令
EHLO test
MAIL FROM:<test@example.com>
RCPT TO:<dest@example.com>
DATA
Hello World
.
QUIT

# 原型输出：双向 SMTP 明文采集结果（标记 client_to_server / server_to_client）
```

**预期结果**：原型控制台输出双向 SMTP 明文字节流，方向可区分，会话可关联。

---

### 场景 C：TLS 1.2 密钥导出验证（FR-001，场景 1）

**目标**：验证 Go KeyLogWriter 对 TLS 1.2 各字段的导出。

```bash
# 配置 KeyLogWriter 输出到临时文件
KEYLOG_FILE=$(mktemp)
chmod 600 $KEYLOG_FILE

go run ./cmd/mailshield-phase1/ \
  -mode=tls12-keylog \
  -keylog=$KEYLOG_FILE \
  -listen=:1443 \
  -upstream=127.0.0.1:8025

# 触发 TLS 1.2 连接
openssl s_client -connect 127.0.0.1:1443 \
  -tls1_2 \
  -CAfile testdata/testca.crt \
  -starttls smtp </dev/null

# 分析 KeyLogWriter 输出
echo "=== TLS 1.2 KeyLogWriter Output ==="
cat $KEYLOG_FILE

# 验证字段
echo "=== Field Verification ==="
python3 scripts/verify_keylog_tls12.py $KEYLOG_FILE

# 清理（删除前确认不含真实密钥）
rm $KEYLOG_FILE
```

**预期结果**：
- `CLIENT_RANDOM <hex>` 行存在，32 字节十六进制
- `master_secret` 在同一行，48 字节十六进制
- `cipher_suite` 可从 `ConnectionState.CipherSuite` 读取并记录

---

### 场景 D：TLS 1.3 密钥导出验证（FR-002，场景 2）

**目标**：验证 Go KeyLogWriter 对 TLS 1.3 各字段的导出，区分可获取与不可获取字段。

```bash
KEYLOG_FILE=$(mktemp)
chmod 600 $KEYLOG_FILE

go run ./cmd/mailshield-phase1/ \
  -mode=tls13-keylog \
  -keylog=$KEYLOG_FILE \
  -listen=:1443 \
  -upstream=127.0.0.1:8025

# TLS 1.3 连接
openssl s_client -connect 127.0.0.1:1443 \
  -tls1_3 \
  -CAfile testdata/testca.crt \
  -starttls smtp </dev/null

echo "=== TLS 1.3 KeyLogWriter Output ==="
cat $KEYLOG_FILE

# 字段验证脚本
python3 scripts/verify_keylog_tls13.py $KEYLOG_FILE

# 预期：
# 可获取：CLIENT_HANDSHAKE_TRAFFIC_SECRET (x2), SERVER_HANDSHAKE_TRAFFIC_SECRET (x2)
# 可获取：CLIENT_TRAFFIC_SECRET_0 (x2), SERVER_TRAFFIC_SECRET_0 (x2)
# 不可获取：CLIENT_TRAFFIC_SECRET_N (N>0) —— 密钥更新无 keylog 条目
# 不可获取：exporter_master_secret, resumption_master_secret

rm $KEYLOG_FILE
```

---

### 场景 E：STARTTLS 评估（场景 6）

**目标**：评估隐式 TLS 与 STARTTLS 两种模式的行为差异。

```bash
# 隐式 TLS 模式验证
go run ./cmd/mailshield-phase1/ \
  -mode=implicit-tls \
  -listen=:1465 \
  -upstream=127.0.0.1:8025

openssl s_client -connect 127.0.0.1:1465 \
  -tls1_2 \
  -CAfile testdata/testca.crt

# STARTTLS 模式验证
go run ./cmd/mailshield-phase1/ \
  -mode=starttls \
  -listen=:1587 \
  -upstream=127.0.0.1:8025

# 先发送明文 SMTP，触发 STARTTLS 后升级
openssl s_client -connect 127.0.0.1:1587 \
  -starttls smtp \
  -tls1_2 \
  -CAfile testdata/testca.crt
```

**记录**：连接建立方式、TLS 握手完成后的 SMTP 命令处理、TrafficObserver 在 TLS 升级后的连续性。

---

## 三、验证报告生成

运行所有场景后，原型生成汇总报告：

```bash
go run ./cmd/mailshield-phase1/ \
  -mode=full \
  -report=phase001-verification-report.md \
  -keylog-analyze=true
```

报告包含：
- TLS 1.2/1.3 各字段可获取性分类（可直接获取/需其他机制/无法获取）
- HTTP CONNECT 隧道建立结果
- 双向 SMTP 明文采集结果
- STARTTLS vs 隐式 TLS 行为差异
- 已知限制
- 测试数据清单（均为虚构）

---

## 四、清理

验证完成后执行：

```bash
# 删除所有密钥日志临时文件
find . -name "*.keylog" -mtime +0 -delete 2>/dev/null

# 删除测试证书
rm -rf testdata/

# 停止所有后台 SMTP 服务器
pkill -f "python3 -c" 2>/dev/null
```

---

## 五、相关文档

- 数据模型：[data-model.md](./data-model.md)
- 技术研究：[research.md](./research.md)
- 需求规格：[spec.md](./spec.md)
