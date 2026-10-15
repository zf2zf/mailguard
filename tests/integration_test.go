package integration

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type capture struct {
	mu          sync.Mutex
	events      [][]byte
	readEvents  [][]byte // server-side Read: client -> MailShield
	writeEvents [][]byte // server-side Write: MailShield -> client
}

func (c *capture) addRead(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copyOfP := append([]byte(nil), p...)
	c.events = append(c.events, copyOfP)
	c.readEvents = append(c.readEvents, copyOfP)
}

func (c *capture) addWrite(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copyOfP := append([]byte(nil), p...)
	c.events = append(c.events, copyOfP)
	c.writeEvents = append(c.writeEvents, copyOfP)
}

func (c *capture) directionCounts() (reads, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.readEvents), len(c.writeEvents)
}

func (c *capture) directionHasTLSRecord(events [][]byte) bool {
	for _, p := range events {
		for i := 0; i+5 <= len(p); i++ {
			if p[i] >= 20 && p[i] <= 23 && p[i+1] == 3 && p[i+2] <= 4 {
				return true
			}
		}
	}
	return false
}

func (c *capture) hasTLSRecord() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.directionHasTLSRecord(c.events)
}

type tappedConn struct {
	net.Conn
	c *capture
}

func (c *tappedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.c.addRead(p[:n])
	}
	return n, err
}

func (c *tappedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.c.addWrite(p[:n])
	}
	return n, err
}

type tappedListener struct {
	net.Listener
	c *capture
}

func (l *tappedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &tappedConn{Conn: conn, c: l.c}, nil
}

type plainEvent struct {
	conn      *smtp.Conn
	direction string
	data      []byte
}

type plainCapture struct {
	mu     sync.Mutex
	events []plainEvent
}

func (p *plainCapture) observe(conn *smtp.Conn, direction smtp.TrafficDirection, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, plainEvent{conn: conn, direction: string(direction), data: append([]byte(nil), data...)})
}

func (p *plainCapture) joined(direction smtp.TrafficDirection) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, event := range p.events {
		if event.direction == string(direction) {
			b.Write(event.data)
		}
	}
	return b.String()
}

func (p *plainCapture) joinedForConn(conn *smtp.Conn, direction smtp.TrafficDirection) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, event := range p.events {
		if event.conn == conn && event.direction == string(direction) {
			b.Write(event.data)
		}
	}
	return b.String()
}

type backend struct {
	mu         sync.Mutex
	tlsSeen    bool
	tlsVersion uint16
	peerCerts  int
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{conn: c, backend: b}, nil
}

type session struct {
	conn    *smtp.Conn
	backend *backend
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	state, ok := s.conn.TLSConnectionState()
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.tlsSeen = ok && state.HandshakeComplete
	if ok {
		s.backend.tlsVersion = state.Version
		s.backend.peerCerts = len(state.PeerCertificates)
	}
	return nil
}

func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error { return nil }
func (s *session) Data(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (s *session) Reset()        {}
func (s *session) Logout() error { return nil }

func TestGoSMTPStartTLSDebugAndRawCapture(t *testing.T) {
	cert, err := makeTestCertificate()
	if err != nil {
		t.Fatal(err)
	}

	debugLog := &safeBuffer{}
	rawCapture := &capture{}
	plain := &plainCapture{}
	be := &backend{}
	server := smtp.NewServer(be)
	server.Domain = "mailshield.test"
	server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.Debug = debugLog
	server.TrafficObserver = plain.observe
	server.ReadTimeout = 5 * time.Second
	server.WriteTimeout = 5 * time.Second

	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &tappedListener{Listener: baseListener, c: rawCapture}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Log("server Serve goroutine did not return within timeout")
		}
	}()

	conn, err := net.DialTimeout("tcp", baseListener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	reader := bufio.NewReader(conn)

	if line := readLine(t, reader); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("greeting = %q", line)
	}
	writeLine(t, conn, "EHLO client.test")
	readMultilineResponse(t, reader, "250")
	writeLine(t, conn, "STARTTLS")
	if line := readLine(t, reader); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("STARTTLS response = %q", line)
	}

	tlsClient := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "mailshield.test", MinVersion: tls.VersionTLS12}) // test-only self-signed certificate
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	clientTLSState := tlsClient.ConnectionState()
	if len(clientTLSState.PeerCertificates) == 0 {
		t.Fatal("client TLS state did not expose the MailShield server certificate")
	}
	if got := clientTLSState.PeerCertificates[0].Subject.CommonName; got != "mailshield.test" {
		t.Errorf("server certificate CommonName = %q, want mailshield.test", got)
	}
	reader = bufio.NewReader(tlsClient)

	writeLine(t, tlsClient, "EHLO client.test")
	readMultilineResponse(t, reader, "250")
	writeLine(t, tlsClient, "MAIL FROM:<sender@example.test>")
	if line := readLine(t, reader); !strings.HasPrefix(line, "250 ") {
		t.Fatalf("MAIL response = %q", line)
	}
	writeLine(t, tlsClient, "RCPT TO:<recipient@example.test>")
	if line := readLine(t, reader); !strings.HasPrefix(line, "250 ") {
		t.Fatalf("RCPT response = %q", line)
	}
	writeLine(t, tlsClient, "DATA")
	if line := readLine(t, reader); !strings.HasPrefix(line, "354 ") {
		t.Fatalf("DATA response = %q", line)
	}
	writeLine(t, tlsClient, "Subject: mailshield integration test")
	writeLine(t, tlsClient, "")
	writeLine(t, tlsClient, "hello from STARTTLS")
	writeLine(t, tlsClient, ".")
	if line := readLine(t, reader); !strings.HasPrefix(line, "250 ") {
		t.Fatalf("DATA final response = %q", line)
	}
	writeLine(t, tlsClient, "QUIT")
	if line := readLine(t, reader); !strings.HasPrefix(line, "221 ") {
		t.Fatalf("QUIT response = %q", line)
	}
	_ = tlsClient.Close()

	debugText := debugLog.String()
	// Verify commands and server responses are both present in the same Debug
	// stream. Server.Debug is an unframed, direction-unlabelled byte stream;
	// these assertions prove presence, not per-direction attribution.
	for _, expected := range []string{
		// Client commands before and after STARTTLS.
		"EHLO client.test",
		"STARTTLS",
		"MAIL FROM:<sender@example.test>",
		"RCPT TO:<recipient@example.test>",
		"DATA",
		"QUIT",
		// Server response codes before and after STARTTLS.
		"220 ",
		"250-",
		"250 ",
		"354 ",
		"221 ",
		// Decrypted message content after STARTTLS.
		"Subject: mailshield integration test",
		"hello from STARTTLS",
	} {
		if !strings.Contains(debugText, expected) {
			t.Errorf("Server.Debug did not contain %q; captured debug output:\n%s", expected, debugText)
		}
	}
	if !rawCapture.hasTLSRecord() {
		t.Error("raw net.Conn boundary did not observe a TLS record")
	}
	rawCapture.mu.Lock()
	readHasTLS := rawCapture.directionHasTLSRecord(rawCapture.readEvents)
	writeHasTLS := rawCapture.directionHasTLSRecord(rawCapture.writeEvents)
	rawCapture.mu.Unlock()
	readCount, writeCount := rawCapture.directionCounts()
	if readCount == 0 || writeCount == 0 {
		t.Errorf("directional raw capture missing events: server Read events=%d, server Write events=%d", readCount, writeCount)
	}
	if !readHasTLS || !writeHasTLS {
		t.Errorf("expected TLS records in both directions: client->server=%v server->client=%v", readHasTLS, writeHasTLS)
	}

	be.mu.Lock()
	tlsSeen, tlsVersion := be.tlsSeen, be.tlsVersion
	be.mu.Unlock()
	if !tlsSeen {
		t.Error("smtp.Conn.TLSConnectionState() did not report a completed TLS session during MAIL")
	}
	if tlsVersion < tls.VersionTLS12 {
		t.Errorf("TLS version = %#x, want TLS 1.2 or newer", tlsVersion)
	}

	clientToServer := plain.joined(smtp.TrafficClientToServer)
	serverToClient := plain.joined(smtp.TrafficServerToClient)
	for _, expected := range []string{"EHLO client.test", "MAIL FROM:<sender@example.test>", "RCPT TO:<recipient@example.test>", "Subject: mailshield integration test", "hello from STARTTLS"} {
		if !strings.Contains(clientToServer, expected) {
			t.Errorf("client->server plaintext observer missing %q; captured=%q", expected, clientToServer)
		}
	}
	for _, expected := range []string{"220 ", "250-", "354 ", "221 "} {
		if !strings.Contains(serverToClient, expected) {
			t.Errorf("server->client plaintext observer missing %q; captured=%q", expected, serverToClient)
		}
	}
	t.Logf("directional plaintext capture: client->server=%d bytes, server->client=%d bytes", len(clientToServer), len(serverToClient))
	t.Logf("plaintext observer captures SMTP commands, responses, and DATA content across STARTTLS: true")

	t.Logf("Server.Debug captured SMTP protocol bytes: %d bytes", len(debugText))
	t.Logf("Server.Debug contains commands and response codes before/after STARTTLS: true")
	t.Logf("Server.Debug provides per-direction labels: false (single combined stream)")
	t.Logf("raw connection capture observed TLS records: %v", rawCapture.hasTLSRecord())
	t.Logf("directional raw capture: server Read events (client->MailShield)=%d, server Write events (MailShield->client)=%d", readCount, writeCount)
	t.Logf("TLS records by direction: client->MailShield=%v MailShield->client=%v", readHasTLS, writeHasTLS)
	t.Logf("client observed MailShield certificate: CN=%s", clientTLSState.PeerCertificates[0].Subject.CommonName)
	t.Logf("TLS state observed in backend: complete=%v version=%#x peerCerts=%d", tlsSeen, tlsVersion, be.peerCerts)
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read SMTP line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func readMultilineResponse(t *testing.T, r *bufio.Reader, code string) {
	t.Helper()
	for {
		line := readLine(t, r)
		if !strings.HasPrefix(line, code) {
			t.Fatalf("SMTP response line = %q, want prefix %s", line, code)
		}
		if len(line) < 4 || line[3] != '-' {
			return
		}
	}
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "%s\r\n", line); err != nil {
		t.Fatalf("write SMTP command %q: %v", line, err)
	}
}

func makeTestCertificate() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mailshield.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"mailshield.test"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}
