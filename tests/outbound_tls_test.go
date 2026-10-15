package integration

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// This test models MailShield's outbound TLS leg to an upstream SMTP server.
// It uses Go's TLS APIs directly; it does not claim to test go-smtp's client API.
func TestOutboundImplicitTLSCapturesUpstreamCertificateAndRawDirections(t *testing.T) {
	cert, err := makeTestCertificateForCN("upstream.test")
	if err != nil {
		t.Fatal(err)
	}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()

	serverErr := make(chan error, 1)
	go func() {
		tlsListener := tls.NewListener(upstreamListener, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		conn, acceptErr := tlsListener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, writeErr := fmt.Fprint(conn, "220 upstream.test ESMTP ready\r\n"); writeErr != nil {
			serverErr <- writeErr
			return
		}
		line, readErr := bufio.NewReader(conn).ReadString('\n')
		if readErr != nil {
			serverErr <- readErr
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "EHLO ") {
			serverErr <- fmt.Errorf("unexpected SMTP command %q", strings.TrimSpace(line))
			return
		}
		_, writeErr := fmt.Fprint(conn, "250 upstream.test\r\n")
		serverErr <- writeErr
	}()

	rawCapture := &capture{}
	rawConn, err := net.DialTimeout("tcp", upstreamListener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(6 * time.Second))
	tapped := &tappedConn{Conn: rawConn, c: rawCapture}
	tlsConn := tls.Client(tapped, &tls.Config{
		ServerName:         "upstream.test",
		InsecureSkipVerify: true, // test-only self-signed certificate
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("upstream TLS handshake: %v", err)
	}
	state := tlsConn.ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("upstream TLS handshake was not complete")
	}
	if len(state.PeerCertificates) == 0 {
		t.Fatal("client TLS state did not expose upstream SMTP server certificate")
	}
	if got := state.PeerCertificates[0].Subject.CommonName; got != "upstream.test" {
		t.Fatalf("upstream certificate CN = %q, want upstream.test", got)
	}

	reader := bufio.NewReader(tlsConn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upstream greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("upstream greeting = %q", greeting)
	}
	if _, err := fmt.Fprint(tlsConn, "EHLO mailshield.test\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upstream EHLO response: %v", err)
	}
	if !strings.HasPrefix(response, "250 ") {
		t.Fatalf("upstream EHLO response = %q", response)
	}
	_ = tlsConn.Close()

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("upstream test server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream test server timed out")
	}

	rawCapture.mu.Lock()
	readHasTLS := rawCapture.directionHasTLSRecord(rawCapture.readEvents)
	writeHasTLS := rawCapture.directionHasTLSRecord(rawCapture.writeEvents)
	readCount := len(rawCapture.readEvents)
	writeCount := len(rawCapture.writeEvents)
	rawCapture.mu.Unlock()
	if !readHasTLS || !writeHasTLS {
		t.Fatalf("expected TLS records in both directions: upstream->MailShield=%v MailShield->upstream=%v", readHasTLS, writeHasTLS)
	}
	if readCount == 0 || writeCount == 0 {
		t.Fatalf("missing raw directional events: Read=%d Write=%d", readCount, writeCount)
	}

	t.Logf("upstream SMTP certificate observed: CN=%s", state.PeerCertificates[0].Subject.CommonName)
	t.Logf("upstream TLS state: complete=%v version=%#x", state.HandshakeComplete, state.Version)
	t.Logf("outbound raw capture: Read (upstream->MailShield)=%d events, Write (MailShield->upstream)=%d events", readCount, writeCount)
	t.Logf("TLS records by direction: upstream->MailShield=%v MailShield->upstream=%v", readHasTLS, writeHasTLS)
}

func makeTestCertificateForCN(cn string) (tls.Certificate, error) {
	// Create a dedicated certificate so the test proves the upstream identity.
	return makeCertificateWithCN(cn)
}
