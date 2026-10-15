package integration

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	netsmtp "net/smtp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	esmtp "github.com/emersion/go-smtp"
)

// TestSMTPProxyThroughSOCKS4 verifies SMTP delivery through a local SOCKS4
// proxy using an IPv4 target and checks both directions of raw tunnel capture.
func TestSMTPProxyThroughSOCKS4(t *testing.T) {
	upstreamStore := &e2eStore{}
	upstreamServer := esmtp.NewServer(&e2eUpstreamBackend{store: upstreamStore})
	upstreamServer.Domain = "upstream.test"
	upstreamServer.ReadTimeout = 5 * time.Second
	upstreamServer.WriteTimeout = 5 * time.Second
	upstreamAddr := startE2ESMTPServer(t, upstreamServer)

	var connectRequests atomic.Int32
	proxyAddr := startSOCKS4TestProxy(t, upstreamAddr, &connectRequests)
	outboundRaw := &capture{}
	relayBackend := &socks4RelayBackend{
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

	marker := "mailshield-socks4-body-marker"
	body := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: SOCKS4 E2E\r\n\r\n" + marker + "\r\n")
	if err := netsmtp.SendMail(inboundAddr, nil, "sender@example.test", []string{"recipient@example.test"}, body); err != nil {
		t.Fatalf("send message through MailShield using SOCKS4: %v", err)
	}

	messages := upstreamStore.snapshot()
	if len(messages) != 1 {
		t.Fatalf("upstream received %d messages; want 1", len(messages))
	}
	if !bytes.Contains(messages[0], []byte(marker)) {
		t.Fatalf("upstream message missing body marker: %q", messages[0])
	}
	if got := connectRequests.Load(); got != 1 {
		t.Fatalf("SOCKS4 CONNECT requests=%d; want 1", got)
	}

	clientToProxy := observer.joined(esmtp.TrafficClientToServer)
	proxyToClient := observer.joined(esmtp.TrafficServerToClient)
	if !strings.Contains(clientToProxy, "MAIL FROM:") || !strings.Contains(clientToProxy, marker) {
		t.Fatalf("inbound plaintext capture missing SMTP command/body: %q", clientToProxy)
	}
	if !strings.Contains(proxyToClient, "220 ") || !strings.Contains(proxyToClient, "250 ") {
		t.Fatalf("inbound plaintext capture missing SMTP responses: %q", proxyToClient)
	}

	reads, writes := outboundRaw.directionCounts()
	if reads == 0 || writes == 0 {
		t.Fatalf("outbound raw capture incomplete: upstream->MailShield reads=%d MailShield->upstream writes=%d", reads, writes)
	}
	t.Logf("SOCKS4 E2E passed: CONNECT requests=%d; IPv4 target verified; upstream message verified; raw events upstream->MailShield=%d, MailShield->upstream=%d", connectRequests.Load(), reads, writes)
}

type socks4RelayBackend struct {
	proxyAddr    string
	upstreamAddr string
	outboundRaw  *capture
}

func (b *socks4RelayBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &socks4RelaySession{backend: b}, nil
}

type socks4RelaySession struct{ backend *socks4RelayBackend }

func (*socks4RelaySession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*socks4RelaySession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *socks4RelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", s.backend.proxyAddr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial SOCKS4 proxy: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	host, portText, err := net.SplitHostPort(s.backend.upstreamAddr)
	if err != nil {
		return fmt.Errorf("split upstream address: %w", err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return fmt.Errorf("SOCKS4 requires an IPv4 target; got %q", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid upstream port %q", portText)
	}

	// SOCKS4 request: VN=4, CD=CONNECT, DSTPORT, DSTIP, USERID NUL.
	request := []byte{0x04, 0x01, 0x00, 0x00}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request[2], request[3] = portBytes[0], portBytes[1]
	request = append(request, ip...)
	request = append(request, []byte("mailshield-test")...)
	request = append(request, 0x00)
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("write SOCKS4 CONNECT request: %w", err)
	}

	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return fmt.Errorf("read SOCKS4 CONNECT reply: %w", err)
	}
	if reply[0] != 0x00 || reply[1] != 0x5a {
		return fmt.Errorf("SOCKS4 CONNECT failed: VN=%d status=0x%02x", reply[0], reply[1])
	}

	if s.backend.outboundRaw != nil {
		conn = &tappedConn{Conn: conn, c: s.backend.outboundRaw}
	}
	_ = conn.SetDeadline(time.Time{})
	client, err := netsmtp.NewClient(conn, "upstream.test")
	if err != nil {
		return fmt.Errorf("create SMTP client over SOCKS4 tunnel: %w", err)
	}
	ok = true
	defer client.Close()
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

func (*socks4RelaySession) Reset()        {}
func (*socks4RelaySession) Logout() error { return nil }

func startSOCKS4TestProxy(t *testing.T, allowedTarget string, connectRequests *atomic.Int32) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go handleSOCKS4TestProxyConn(client, allowedTarget, connectRequests)
		}
	}()
	return listener.Addr().String()
}

func handleSOCKS4TestProxyConn(client net.Conn, allowedTarget string, connectRequests *atomic.Int32) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	var header [8]byte
	if _, err := io.ReadFull(client, header[:]); err != nil || header[0] != 0x04 || header[1] != 0x01 {
		return
	}
	// Consume USERID through its NUL terminator.
	for {
		var b [1]byte
		if _, err := io.ReadFull(client, b[:]); err != nil {
			return
		}
		if b[0] == 0 {
			break
		}
	}
	host := net.IP(header[4:8]).String()
	port := binary.BigEndian.Uint16(header[2:4])
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	if target != allowedTarget {
		_, _ = client.Write([]byte{0x00, 0x5b, 0, 0, 0, 0, 0, 0})
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = client.Write([]byte{0x00, 0x5b, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{0x00, 0x5a, header[2], header[3], header[4], header[5], header[6], header[7]}); err != nil {
		return
	}
	connectRequests.Add(1)
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
