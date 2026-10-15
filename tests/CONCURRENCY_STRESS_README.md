# MailShield 并发连接与事务隔离验证

将 `concurrency_stress_test.go` 复制到现有 `mailshield-go-smtp-integration` 项目根目录（如果该文件已存在则直接使用，不要覆盖其他测试文件）。该测试复用项目中已有的 `transactionBackend`、`plainCapture` 和 SMTP 测试客户端。

运行：

```bash
go test -v -run TestConcurrentSMTPStressAndCaptureIsolation -count=1
```

建议随后执行竞态检测：

```bash
go test -race -run TestConcurrentSMTPStressAndCaptureIsolation -count=1
```

测试使用本机 loopback，不连接外部服务。默认同时建立 32 个 SMTP 连接，每个连接发送 3 笔邮件事务，共验证 96 笔事务；检查上游 backend 收件数量、每封邮件的唯一正文标记，以及每个连接的采集事件归属。

注意：测试代码使用当前验证包的 `go-smtp` connection-scoped `TrafficObserver` 补丁和现有测试辅助类型。运行环境需具备 `go.mod` 指定的 Go 版本及依赖。
