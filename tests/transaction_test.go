package integration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type transactionBackend struct {
	mu     sync.Mutex
	bodies []string
}

func (b *transactionBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &transactionSession{backend: b}, nil
}

type transactionSession struct{ backend *transactionBackend }

func (s *transactionSession) Mail(from string, opts *smtp.MailOptions) error { return nil }
func (s *transactionSession) Rcpt(to string, opts *smtp.RcptOptions) error   { return nil }
func (s *transactionSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.bodies = append(s.backend.bodies, string(body))
	s.backend.mu.Unlock()
	return nil
}
func (s *transactionSession) Reset()        {}
func (s *transactionSession) Logout() error { return nil }

func TestMultipleTransactionsAndConcurrentConnections(t *testing.T) {
	backend := &transactionBackend{}
	observer := &plainCapture{}
	server := smtp.NewServer(backend)
	server.Domain = "mailshield.test"
	server.TrafficObserver = observer.observe
	server.ReadTimeout = 5 * time.Second
	server.WriteTimeout = 5 * time.Second

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Log("server shutdown timed out")
		}
	}()

	// One SMTP connection has one greeting/EHLO exchange, then two mail transactions.
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := newTransactionSMTPClient(conn)
	if err := client.start(); err != nil {
		t.Fatalf("same connection SMTP greeting/EHLO: %v", err)
	}
	if err := client.sendMail("same-connection-1", "first-body-marker"); err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	if err := client.sendMail("same-connection-2", "second-body-marker"); err != nil {
		t.Fatalf("second transaction on same connection: %v", err)
	}
	if err := client.quit(); err != nil {
		t.Fatalf("QUIT on same connection: %v", err)
	}
	_ = conn.Close()

	// Two independent connections run concurrently with unique envelope/body markers.
	markers := []string{"parallel-a-body-marker", "parallel-b-body-marker"}
	var wg sync.WaitGroup
	errCh := make(chan error, len(markers))
	for i, marker := range markers {
		wg.Add(1)
		go func(i int, marker string) {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			defer c.Close()
			cl := newTransactionSMTPClient(c)
			if err := cl.start(); err != nil {
				errCh <- fmt.Errorf("parallel connection %d start: %w", i, err)
				return
			}
			if err := cl.sendMail(fmt.Sprintf("parallel-%d", i), marker); err != nil {
				errCh <- fmt.Errorf("parallel connection %d mail: %w", i, err)
				return
			}
			if err := cl.quit(); err != nil {
				errCh <- fmt.Errorf("parallel connection %d QUIT: %w", i, err)
			}
		}(i, marker)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		count := len(backend.bodies)
		backend.mu.Unlock()
		if count == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	backend.mu.Lock()
	bodies := append([]string(nil), backend.bodies...)
	backend.mu.Unlock()
	if len(bodies) != 4 {
		t.Fatalf("backend captured %d DATA transactions, want 4: %#v", len(bodies), bodies)
	}
	for _, marker := range []string{"first-body-marker", "second-body-marker", markers[0], markers[1]} {
		found := false
		for _, body := range bodies {
			if strings.Contains(body, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("backend did not capture transaction body marker %q; bodies=%q", marker, bodies)
		}
	}

	clientBytes := observer.joined(smtp.TrafficClientToServer)
	for _, marker := range []string{"first-body-marker", "second-body-marker", markers[0], markers[1]} {
		if !strings.Contains(clientBytes, marker) {
			t.Errorf("client->server observer missed %q", marker)
		}
	}

	// Group observed client->server bytes by the stable go-smtp Conn pointer.
	// Two transactions on one SMTP connection must share an owner; concurrent
	// connections must have separate owners.
	observer.mu.Lock()
	perConn := make(map[*smtp.Conn]string)
	for _, event := range observer.events {
		if event.direction == string(smtp.TrafficClientToServer) {
			perConn[event.conn] += string(event.data)
		}
	}
	observer.mu.Unlock()
	owners := make(map[string]*smtp.Conn)
	for _, marker := range []string{"first-body-marker", "second-body-marker", markers[0], markers[1]} {
		for connID, transcript := range perConn {
			if strings.Contains(transcript, marker) {
				owners[marker] = connID
				break
			}
		}
		if owners[marker] == nil {
			t.Errorf("no connection owner found for marker %q", marker)
		}
	}
	if owners["first-body-marker"] != owners["second-body-marker"] {
		t.Errorf("two transactions on the same SMTP connection were attributed to different connections")
	}
	if owners[markers[0]] == owners[markers[1]] {
		t.Errorf("two independent concurrent SMTP connections were attributed to the same connection")
	}
	if owners[markers[0]] == owners["first-body-marker"] || owners[markers[1]] == owners["first-body-marker"] {
		t.Errorf("concurrent SMTP connection was incorrectly attributed to the persistent SMTP connection")
	}
	t.Logf("backend DATA transactions captured: %d; unique transaction body markers: all present", len(bodies))
	t.Logf("connection-scoped observer attribution: same-connection transactions grouped; concurrent connections isolated")
}

type transactionSMTPClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func newTransactionSMTPClient(conn net.Conn) *transactionSMTPClient {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return &transactionSMTPClient{conn: conn, r: bufio.NewReader(conn)}
}

func (c *transactionSMTPClient) readCode(want string) error {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, want) {
		return fmt.Errorf("response %q, want prefix %q", line, want)
	}
	return nil
}

func (c *transactionSMTPClient) write(line string) error {
	_, err := fmt.Fprintf(c.conn, "%s\r\n", line)
	return err
}

func (c *transactionSMTPClient) start() error {
	if err := c.readCode("220 "); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if err := c.write("EHLO client.test"); err != nil {
		return err
	}
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("EHLO response: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "250") {
			return fmt.Errorf("EHLO response %q", line)
		}
		if len(line) < 4 || line[3] != '-' {
			break
		}
	}
	return nil
}

func (c *transactionSMTPClient) sendMail(id, bodyMarker string) error {
	commands := []struct{ command, response string }{
		{fmt.Sprintf("MAIL FROM:<%s@example.test>", id), "250 "},
		{"RCPT TO:<recipient@example.test>", "250 "},
		{"DATA", "354 "},
	}
	for _, item := range commands {
		if err := c.write(item.command); err != nil {
			return err
		}
		if err := c.readCode(item.response); err != nil {
			return fmt.Errorf("after %s: %w", item.command, err)
		}
	}
	for _, line := range []string{"Subject: " + id, "", bodyMarker, "."} {
		if err := c.write(line); err != nil {
			return err
		}
	}
	if err := c.readCode("250 "); err != nil {
		return fmt.Errorf("DATA completion: %w", err)
	}
	return nil
}

func (c *transactionSMTPClient) quit() error {
	if err := c.write("QUIT"); err != nil {
		return err
	}
	return c.readCode("221 ")
}
