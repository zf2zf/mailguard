# MailShield SOCKS4 E2E 验证

将 `socks4_test.go` 复制到现有 `mailshield-go-smtp-integration` 项目根目录。不要覆盖或重新应用现有文件/补丁。

运行功能测试：

```bash
go test -v -run '^TestSMTPProxyThroughSOCKS4$' -count=1
```

通过后运行竞态检测：

```bash
go test -race -run '^TestSMTPProxyThroughSOCKS4$' -count=1
```

测试使用本地模拟 SOCKS4 代理，只验证 IPv4 目标地址，不使用外部网络。检查 SOCKS4 CONNECT 成功、SMTP 邮件完整到达上游，以及出站双向原始字节采集。
