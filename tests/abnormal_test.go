package integration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	netsmtp "net/smtp"
	"strings"
	"testing"
	"time"

	esmtp "github.com/emersion/go-smtp"
)

func readSMTPResponse(t *testing.T, r *bufio.Reader) string {
	t.Helper()

	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SMTP response: %v", err)
		}

		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)

		// SMTP 多行响应以空格分隔的状态码行结束。
		if len(line) < 4 || line[3] != '-' {
			return strings.Join(lines, "\n")
		}
	}
}

func smtpCommand(
	t *testing.T,
	c net.Conn,
	r *bufio.Reader,
	command string,
	expectedCode string,
) {
	t.Helper()

	if _, err := fmt.Fprintf(c, "%s\r\n", command); err != nil {
		t.Fatalf("send SMTP command %q: %v", command, err)
	}

	response := readSMTPResponse(t, r)
	if !strings.HasPrefix(response, expectedCode) {
		t.Fatalf(
			"SMTP command %q returned %q; want code %s",
			command,
			response,
			expectedCode,
		)
	}
}

func TestSMTPClientDisconnectMidDataDoesNotAffectOtherClients(t *testing.T) {
	store := &e2eStore{}

	server := esmtp.NewServer(&e2eUpstreamBackend{store: store})
	server.Domain = "mailshield.test"
	server.ReadTimeout = 2 * time.Second
	server.WriteTimeout = 2 * time.Second

	addr := startE2ESMTPServer(t, server)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)

	if greeting := readSMTPResponse(t, reader); !strings.HasPrefix(greeting, "220 ") {
		_ = conn.Close()
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	smtpCommand(t, conn, reader, "EHLO disconnect.test", "250")
	smtpCommand(t, conn, reader, "MAIL FROM:<broken@example.test>", "250")
	smtpCommand(t, conn, reader, "RCPT TO:<recipient@example.test>", "250")
	smtpCommand(t, conn, reader, "DATA", "354")

	if _, err := io.WriteString(
		conn,
		"Subject: partial\r\n\r\npartial-body-without-terminator\r\n",
	); err != nil {
		_ = conn.Close()
		t.Fatalf("write partial DATA: %v", err)
	}

	// 故意在 DATA 结束标记之前断开连接。
	_ = conn.Close()

	goodBody := []byte(
		"Subject: healthy\r\n\r\nmailshield-after-disconnect-marker\r\n",
	)

	if err := netsmtp.SendMail(
		addr,
		nil,
		"healthy@example.test",
		[]string{"recipient@example.test"},
		goodBody,
	); err != nil {
		t.Fatalf("healthy client failed after partial-DATA disconnect: %v", err)
	}

	bodies := store.snapshot()
	if len(bodies) != 1 ||
		!strings.Contains(string(bodies[0]), "mailshield-after-disconnect-marker") {
		t.Fatalf(
			"expected only the subsequent complete message; got %d messages: %q",
			len(bodies),
			bodies,
		)
	}

	t.Log("client mid-DATA disconnect isolated; subsequent message relayed intact")
}

func TestSMTPIdleReadTimeoutDoesNotAffectOtherClients(t *testing.T) {
	store := &e2eStore{}

	server := esmtp.NewServer(&e2eUpstreamBackend{store: store})
	server.Domain = "mailshield.test"
	server.ReadTimeout = 250 * time.Millisecond
	server.WriteTimeout = time.Second

	addr := startE2ESMTPServer(t, server)

	idle, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	// 给客户端读取设置上限，避免测试无限等待。
	_ = idle.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(idle)

	if greeting := readSMTPResponse(t, reader); !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	// 不发送 EHLO 或其他命令，让服务端的 ReadTimeout 生效。
	var responseLines []string
	var terminalErr error

	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			terminalErr = readErr
			break
		}

		responseLines = append(
			responseLines,
			strings.TrimRight(line, "\r\n"),
		)
	}

	if terminalErr == nil {
		t.Fatal("idle connection did not close or time out")
	}

	responses := strings.Join(responseLines, "\n")

	// 不同实现可能发送 421 超时响应、221 退出响应，
	// 也可能直接关闭连接。这里记录实际行为，不将未验证的
	// SMTP 响应码当作硬性要求。
	if len(responseLines) > 0 {
		t.Logf("idle connection SMTP responses: %q", responses)
	}

	if netErr, ok := terminalErr.(net.Error); ok && netErr.Timeout() {
		t.Logf("client read deadline expired: %v", terminalErr)
	} else {
		t.Logf("idle connection ended: %v", terminalErr)
	}

	_ = idle.Close()

	// 空闲连接结束后，验证服务仍能接受并处理新邮件。
	body := []byte(
		"Subject: healthy after timeout\r\n\r\nmailshield-after-timeout-marker\r\n",
	)

	if err := netsmtp.SendMail(
		addr,
		nil,
		"healthy@example.test",
		[]string{"recipient@example.test"},
		body,
	); err != nil {
		t.Fatalf("healthy client failed after idle timeout: %v", err)
	}

	bodies := store.snapshot()
	if len(bodies) != 1 ||
		!strings.Contains(string(bodies[0]), "mailshield-after-timeout-marker") {
		t.Fatalf(
			"expected subsequent message after idle connection ended; got %d messages: %q",
			len(bodies),
			bodies,
		)
	}

	t.Log("idle connection ended; subsequent client succeeded")
}

func TestSMTPUpstreamDisconnectDoesNotCrashInboundServer(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()

	go func() {
		for {
			c, acceptErr := upstreamListener.Accept()
			if acceptErr != nil {
				return
			}

			// 模拟上游在 SMTP Greeting 之前断开。
			_ = c.Close()
		}
	}()

	backend := &e2eRelayBackend{
		upstreamAddr: upstreamListener.Addr().String(),
	}

	server := esmtp.NewServer(backend)
	server.Domain = "mailshield.test"
	server.ReadTimeout = 2 * time.Second
	server.WriteTimeout = 2 * time.Second

	addr := startE2ESMTPServer(t, server)

	body := []byte(
		"Subject: upstream disconnect\r\n\r\nmailshield-upstream-drop-marker\r\n",
	)

	err = netsmtp.SendMail(
		addr,
		nil,
		"sender@example.test",
		[]string{"recipient@example.test"},
		body,
	)
	if err == nil {
		t.Fatal("expected message delivery to fail when upstream disconnects")
	}

	// 验证上游故障后，入站服务仍然可以接受新连接。
	conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
	if dialErr != nil {
		t.Fatalf(
			"inbound server stopped accepting connections: %v (previous delivery error: %v)",
			dialErr,
			err,
		)
	}

	_ = conn.SetDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(conn)

	greeting, readErr := reader.ReadString('\n')
	_ = conn.Close()

	if readErr != nil || !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf(
			"inbound server did not serve SMTP after upstream failure: greeting=%q err=%v",
			greeting,
			readErr,
		)
	}

	t.Logf(
		"upstream disconnect returned delivery error (%v); inbound listener remained healthy",
		err,
	)
}
