# MailShield TLS E2E 增量验证

将 `e2e_tls_proxy_test.go` 复制到现有 `mailshield-go-smtp-integration` 项目根目录。不要覆盖已有测试或 `third_party/go-smtp` 修改。

执行：

```bash
go test -v -run TestSMTPProxyEndToEndSTARTTLSBothLegsAndCaptures
```

测试模拟：客户端 --STARTTLS--> MailShield SMTP 服务 --STARTTLS--> 上游 SMTP 服务。

检查入站/出站两段 TLS、两端原始 TLS 记录、MailShield 入站解密后的 SMTP 明文，以及上游收到完整邮件正文。测试使用自签名证书并在测试客户端关闭证书链校验，仅限本地测试；生产环境必须正常验证上游证书。
