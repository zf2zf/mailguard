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

// TestSMTPProxyThroughSOCKS5 verifies SMTP delivery through a local SOCKS5
// proxy without authentication, plus bidirectional raw capture on the tunnel.
func TestSMTPProxyThroughSOCKS5(t *testing.T) {
	runSOCKS5SMTPProxyE2E(t, false)
}

// TestSMTPProxyThroughSOCKS5UsernamePassword verifies RFC 1929 username/password
// negotiation and SMTP delivery through the authenticated SOCKS5 tunnel.
func TestSMTPProxyThroughSOCKS5UsernamePassword(t *testing.T) {
	runSOCKS5SMTPProxyE2E(t, true)
}

func runSOCKS5SMTPProxyE2E(t *testing.T, auth bool) {
	t.Helper()

	upstreamStore := &e2eStore{}
	upstreamServer := esmtp.NewServer(&e2eUpstreamBackend{store: upstreamStore})
	upstreamServer.Domain = "upstream.test"
	upstreamServer.ReadTimeout = 5 * time.Second
	upstreamServer.WriteTimeout = 5 * time.Second
	upstreamAddr := startE2ESMTPServer(t, upstreamServer)

	var connectRequests atomic.Int32
	var authSuccesses atomic.Int32
	proxyAddr := startSOCKS5TestProxy(t, upstreamAddr, auth, &connectRequests, &authSuccesses)

	outboundRaw := &capture{}
	relayBackend := &socks5RelayBackend{
		proxyAddr:    proxyAddr,
		upstreamAddr: upstreamAddr,
		username:     "mailshield-test",
		password:     "test-secret",
		auth:         auth,
		outboundRaw:  outboundRaw,
	}
	observer := &plainCapture{}
	inboundServer := esmtp.NewServer(relayBackend)
	inboundServer.Domain = "mailshield.test"
	inboundServer.TrafficObserver = observer.observe
	inboundServer.ReadTimeout = 5 * time.Second
	inboundServer.WriteTimeout = 5 * time.Second
	inboundAddr := startE2ESMTPServer(t, inboundServer)

	marker := "mailshield-socks5-body-marker"
	body := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: SOCKS5 E2E\r\n\r\n" + marker + "\r\n")
	if err := netsmtp.SendMail(inboundAddr, nil, "sender@example.test", []string{"recipient@example.test"}, body); err != nil {
		t.Fatalf("send message through MailShield using SOCKS5: %v", err)
	}

	messages := upstreamStore.snapshot()
	if len(messages) != 1 {
		t.Fatalf("upstream received %d messages; want 1", len(messages))
	}
	if !bytes.Contains(messages[0], []byte(marker)) {
		t.Fatalf("upstream message missing body marker: %q", messages[0])
	}
	if got := connectRequests.Load(); got != 1 {
		t.Fatalf("SOCKS5 CONNECT requests=%d; want 1", got)
	}
	if auth {
		if got := authSuccesses.Load(); got != 1 {
			t.Fatalf("SOCKS5 authentication successes=%d; want 1", got)
		}
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

	if auth {
		t.Logf("SOCKS5 authenticated E2E passed: CONNECT requests=%d; auth successes=%d; upstream message verified; raw events upstream->MailShield=%d, MailShield->upstream=%d", connectRequests.Load(), authSuccesses.Load(), reads, writes)
	} else {
		t.Logf("SOCKS5 no-auth E2E passed: CONNECT requests=%d; upstream message verified; raw events upstream->MailShield=%d, MailShield->upstream=%d", connectRequests.Load(), reads, writes)
	}
}

type socks5RelayBackend struct {
	proxyAddr    string
	upstreamAddr string
	username     string
	password     string
	auth         bool
	outboundRaw  *capture
}

func (b *socks5RelayBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &socks5RelaySession{backend: b}, nil
}

type socks5RelaySession struct{ backend *socks5RelayBackend }

func (*socks5RelaySession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*socks5RelaySession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *socks5RelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", s.backend.proxyAddr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial SOCKS5 proxy: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	methods := []byte{0x00}
	if s.backend.auth {
		methods = []byte{0x00, 0x02}
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("write SOCKS5 greeting: %w", err)
	}
	var methodReply [2]byte
	if _, err := io.ReadFull(conn, methodReply[:]); err != nil {
		return fmt.Errorf("read SOCKS5 method selection: %w", err)
	}
	if methodReply[0] != 0x05 {
		return fmt.Errorf("invalid SOCKS5 method reply version: %d", methodReply[0])
	}
	if s.backend.auth {
		if methodReply[1] != 0x02 {
			return fmt.Errorf("SOCKS5 proxy selected method 0x%02x; want username/password", methodReply[1])
		}
		user := []byte(s.backend.username)
		pass := []byte(s.backend.password)
		if len(user) > 255 || len(pass) > 255 {
			return fmt.Errorf("SOCKS5 test credentials exceed 255 bytes")
		}
		request := []byte{0x01, byte(len(user))}
		request = append(request, user...)
		request = append(request, byte(len(pass)))
		request = append(request, pass...)
		if _, err := conn.Write(request); err != nil {
			return fmt.Errorf("write SOCKS5 username/password: %w", err)
		}
		var authReply [2]byte
		if _, err := io.ReadFull(conn, authReply[:]); err != nil {
			return fmt.Errorf("read SOCKS5 authentication reply: %w", err)
		}
		if authReply[0] != 0x01 || authReply[1] != 0x00 {
			return fmt.Errorf("SOCKS5 authentication rejected: version=%d status=%d", authReply[0], authReply[1])
		}
	} else if methodReply[1] != 0x00 {
		return fmt.Errorf("SOCKS5 proxy selected method 0x%02x; want no-auth", methodReply[1])
	}

	host, portText, err := net.SplitHostPort(s.backend.upstreamAddr)
	if err != nil {
		return fmt.Errorf("split upstream address: %w", err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return fmt.Errorf("test upstream host %q is not an IPv4 address", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid upstream port %q", portText)
	}
	request := []byte{0x05, 0x01, 0x00, 0x01}
	request = append(request, ip...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("write SOCKS5 CONNECT request: %w", err)
	}

	var replyHeader [4]byte
	if _, err := io.ReadFull(conn, replyHeader[:]); err != nil {
		return fmt.Errorf("read SOCKS5 CONNECT reply: %w", err)
	}
	if replyHeader[0] != 0x05 || replyHeader[1] != 0x00 {
		return fmt.Errorf("SOCKS5 CONNECT failed: version=%d status=%d", replyHeader[0], replyHeader[1])
	}
	if err := discardSOCKS5BoundAddress(conn, replyHeader[3]); err != nil {
		return fmt.Errorf("read SOCKS5 bound address: %w", err)
	}

	// Start capture only after SOCKS negotiation so counts represent the SMTP tunnel.
	if s.backend.outboundRaw != nil {
		conn = &tappedConn{Conn: conn, c: s.backend.outboundRaw}
	}
	_ = conn.SetDeadline(time.Time{})
	client, err := netsmtp.NewClient(conn, "upstream.test")
	if err != nil {
		return fmt.Errorf("create SMTP client over SOCKS5 tunnel: %w", err)
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

func (*socks5RelaySession) Reset()        {}
func (*socks5RelaySession) Logout() error { return nil }

func discardSOCKS5BoundAddress(r io.Reader, atyp byte) error {
	var n int
	switch atyp {
	case 0x01:
		n = 4
	case 0x04:
		n = 16
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return err
		}
		n = int(length[0])
	default:
		return fmt.Errorf("unsupported SOCKS5 address type 0x%02x", atyp)
	}
	buf := make([]byte, n+2) // address bytes plus port
	_, err := io.ReadFull(r, buf)
	return err
}

func startSOCKS5TestProxy(
	t *testing.T,
	allowedTarget string,
	requireAuth bool,
	connectRequests *atomic.Int32,
	authSuccesses *atomic.Int32,
) string {
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
			go handleSOCKS5TestProxyConn(client, allowedTarget, requireAuth, connectRequests, authSuccesses)
		}
	}()
	return listener.Addr().String()
}

func handleSOCKS5TestProxyConn(
	client net.Conn,
	allowedTarget string,
	requireAuth bool,
	connectRequests *atomic.Int32,
	authSuccesses *atomic.Int32,
) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	var greeting [2]byte
	if _, err := io.ReadFull(client, greeting[:]); err != nil || greeting[0] != 0x05 || greeting[1] == 0 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	selected := byte(0xff)
	for _, method := range methods {
		if requireAuth && method == 0x02 {
			selected = 0x02
			break
		}
		if !requireAuth && method == 0x00 {
			selected = 0x00
			break
		}
	}
	if _, err := client.Write([]byte{0x05, selected}); err != nil || selected == 0xff {
		return
	}

	if selected == 0x02 {
		var authHeader [2]byte
		if _, err := io.ReadFull(client, authHeader[:]); err != nil || authHeader[0] != 0x01 {
			return
		}
		user := make([]byte, int(authHeader[1]))
		if _, err := io.ReadFull(client, user); err != nil {
			return
		}
		var passLen [1]byte
		if _, err := io.ReadFull(client, passLen[:]); err != nil {
			return
		}
		pass := make([]byte, int(passLen[0]))
		if _, err := io.ReadFull(client, pass); err != nil {
			return
		}
		if string(user) != "mailshield-test" || string(pass) != "test-secret" {
			_, _ = client.Write([]byte{0x01, 0x01})
			return
		}
		authSuccesses.Add(1)
		if _, err := client.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}

	var req [4]byte
	if _, err := io.ReadFull(client, req[:]); err != nil || req[0] != 0x05 || req[1] != 0x01 || req[2] != 0x00 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		var ip [4]byte
		if _, err := io.ReadFull(client, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(client, length[:]); err != nil {
			return
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(client, name); err != nil {
			return
		}
		host = string(name)
	case 0x04:
		var ip [16]byte
		if _, err := io.ReadFull(client, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	default:
		_, _ = client.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(client, portBytes[:]); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes[:]))))
	if target != allowedTarget {
		_, _ = client.Write([]byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = client.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
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
