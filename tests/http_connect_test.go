package integration

import (
	"bytes"
	"fmt"
	"io"
	"net"
	netsmtp "net/smtp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	esmtp "github.com/emersion/go-smtp"
)

// TestSMTPProxyThroughHTTPConnect verifies SMTP delivery through an HTTP CONNECT
// tunnel and confirms that MailShield-side raw capture observes both directions.
// The proxy is a local test fixture, not a production HTTP proxy implementation.
func TestSMTPProxyThroughHTTPConnect(t *testing.T) {
	upstreamStore := &e2eStore{}
	upstreamServer := esmtp.NewServer(&e2eUpstreamBackend{store: upstreamStore})
	upstreamServer.Domain = "upstream.test"
	upstreamServer.ReadTimeout = 5 * time.Second
	upstreamServer.WriteTimeout = 5 * time.Second
	upstreamAddr := startE2ESMTPServer(t, upstreamServer)

	var connectRequests atomic.Int32
	proxyAddr := startHTTPConnectTestProxy(t, &connectRequests)

	outboundRaw := &capture{}
	relayBackend := &httpConnectRelayBackend{
		proxyAddr:    proxyAddr,
		upstreamAddr: upstreamAddr,
		outboundRaw:  outboundRaw,
	}
	observer := &plainCapture{}
	inboundServer := esmtp.NewServer(relayBackend)
	inboundServer.Domain = "mailshield.test"
	inboundServer.TrafficObserver = observer.observe
	inboundServer.ReadTimeout = 5 * time.Second
	inboundServer.WriteTimeout = 5 * time.Second
	inboundAddr := startE2ESMTPServer(t, inboundServer)

	body := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: HTTP CONNECT E2E\r\n\r\nmailshield-http-connect-body-marker\r\n")
	if err := netsmtp.SendMail(inboundAddr, nil, "sender@example.test", []string{"recipient@example.test"}, body); err != nil {
		t.Fatalf("send message through MailShield using HTTP CONNECT: %v", err)
	}

	messages := upstreamStore.snapshot()
	if len(messages) != 1 {
		t.Fatalf("upstream received %d messages; want 1", len(messages))
	}
	if !bytes.Contains(messages[0], []byte("mailshield-http-connect-body-marker")) {
		t.Fatalf("upstream message missing body marker: %q", messages[0])
	}
	if got := connectRequests.Load(); got != 1 {
		t.Fatalf("HTTP CONNECT requests=%d; want 1", got)
	}

	clientToProxy := observer.joined(esmtp.TrafficClientToServer)
	proxyToClient := observer.joined(esmtp.TrafficServerToClient)
	if !strings.Contains(clientToProxy, "MAIL FROM:") || !strings.Contains(clientToProxy, "mailshield-http-connect-body-marker") {
		t.Fatalf("inbound plaintext capture missing SMTP command/body: %q", clientToProxy)
	}
	if !strings.Contains(proxyToClient, "220 ") || !strings.Contains(proxyToClient, "250 ") {
		t.Fatalf("inbound plaintext capture missing SMTP responses: %q", proxyToClient)
	}

	reads, writes := outboundRaw.directionCounts()
	if reads == 0 || writes == 0 {
		t.Fatalf("outbound raw capture incomplete: upstream->proxy reads=%d proxy->upstream writes=%d", reads, writes)
	}
	t.Logf("HTTP CONNECT E2E passed: CONNECT requests=%d; upstream message verified; outbound raw events upstream->MailShield=%d, MailShield->upstream=%d", connectRequests.Load(), reads, writes)
}

type httpConnectRelayBackend struct {
	proxyAddr    string
	upstreamAddr string
	outboundRaw  *capture
}

func (b *httpConnectRelayBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &httpConnectRelaySession{backend: b}, nil
}

type httpConnectRelaySession struct{ backend *httpConnectRelayBackend }

func (*httpConnectRelaySession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*httpConnectRelaySession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *httpConnectRelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", s.backend.proxyAddr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial HTTP proxy: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", s.backend.upstreamAddr, s.backend.upstreamAddr); err != nil {
		return fmt.Errorf("write CONNECT request: %w", err)
	}

	status, err := readHTTPLineBytewise(conn)
	if err != nil {
		return fmt.Errorf("read CONNECT status: %w", err)
	}
	if !strings.Contains(status, " 200 ") {
		return fmt.Errorf("HTTP CONNECT failed: %s", status)
	}
	for {
		line, err := readHTTPLineBytewise(conn)
		if err != nil {
			return fmt.Errorf("read CONNECT headers: %w", err)
		}
		if line == "" {
			break
		}
	}

	// The bytewise header reader ensures no tunneled SMTP greeting is buffered
	// beyond the HTTP CONNECT response headers.
	if s.backend.outboundRaw != nil {
		conn = &tappedConn{Conn: conn, c: s.backend.outboundRaw}
	}
	client, err := netsmtp.NewClient(conn, "upstream.test")
	if err != nil {
		return fmt.Errorf("create SMTP client over CONNECT tunnel: %w", err)
	}
	ok = true
	defer client.Close()
	_ = conn.SetDeadline(time.Time{})

	if err := client.Mail("sender@example.test"); err != nil {
		return err
	}
	if err := client.Rcpt("recipient@example.test"); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}
func (*httpConnectRelaySession) Reset()        {}
func (*httpConnectRelaySession) Logout() error { return nil }

func startHTTPConnectTestProxy(t *testing.T, requests *atomic.Int32) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go handleHTTPConnectTestProxyConn(client, requests)
		}
	}()
	return listener.Addr().String()
}

func handleHTTPConnectTestProxyConn(client net.Conn, requests *atomic.Int32) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	requestLine, err := readHTTPLineBytewise(client)
	if err != nil {
		return
	}
	parts := strings.Fields(requestLine)
	if len(parts) != 3 || parts[0] != "CONNECT" {
		_, _ = io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		return
	}
	target := parts[1]
	for {
		line, err := readHTTPLineBytewise(client)
		if err != nil {
			return
		}
		if line == "" {
			break
		}
	}

	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	requests.Add(1)
	_ = client.SetDeadline(time.Time{})

	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	if tcp, ok := client.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	<-done
}

func readHTTPLineBytewise(r io.Reader) (string, error) {
	var line []byte
	var one [1]byte
	for {
		_, err := io.ReadFull(r, one[:])
		if err != nil {
			return "", err
		}
		b := one[0]
		if b == '\n' {
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return string(line), nil
		}
		line = append(line, b)
		if len(line) > 16*1024 {
			return "", fmt.Errorf("HTTP line exceeds 16 KiB")
		}
	}
}
