package integration

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// TestConcurrentSMTPStressAndCaptureIsolation sends multiple transactions over
// many independent SMTP connections and verifies backend delivery plus
// connection-scoped observer attribution. It uses only loopback sockets.
func TestConcurrentSMTPStressAndCaptureIsolation(t *testing.T) {
	const (
		connections      = 32
		transactionsEach = 3
	)
	backend := &transactionBackend{}
	observer := &plainCapture{}
	server := smtp.NewServer(backend)
	server.Domain = "mailshield.test"
	server.TrafficObserver = observer.observe
	server.ReadTimeout = 10 * time.Second
	server.WriteTimeout = 10 * time.Second
	server.MaxMessageBytes = 1 << 20

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
		case <-time.After(3 * time.Second):
			t.Error("SMTP server shutdown timed out")
		}
	}()

	started := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, connections)
	for i := 0; i < connections; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("client %d dial: %w", clientID, err)
				return
			}
			defer conn.Close()
			client := newTransactionSMTPClient(conn)
			if err := client.start(); err != nil {
				errCh <- fmt.Errorf("client %d start: %w", clientID, err)
				return
			}
			for tx := 0; tx < transactionsEach; tx++ {
				marker := fmt.Sprintf("stress-client-%02d-tx-%02d-unique-body-marker", clientID, tx)
				id := fmt.Sprintf("stress-%02d-%02d", clientID, tx)
				if err := client.sendMail(id, marker); err != nil {
					errCh <- fmt.Errorf("client %d transaction %d: %w", clientID, tx, err)
					return
				}
			}
			if err := client.quit(); err != nil {
				errCh <- fmt.Errorf("client %d QUIT: %w", clientID, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Error(err)
		}
	}

	wantTransactions := connections * transactionsEach
	deadline := time.Now().Add(3 * time.Second)
	var bodies []string
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		bodies = append([]string(nil), backend.bodies...)
		backend.mu.Unlock()
		if len(bodies) == wantTransactions {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(bodies) != wantTransactions {
		t.Fatalf("backend received %d transactions, want %d", len(bodies), wantTransactions)
	}

	// Build a per-connection client->server transcript from copied observer events.
	observer.mu.Lock()
	perConn := make(map[*smtp.Conn]string)
	for _, event := range observer.events {
		if event.direction == string(smtp.TrafficClientToServer) {
			perConn[event.conn] += string(event.data)
		}
	}
	observer.mu.Unlock()
	if len(perConn) != connections {
		t.Fatalf("observer attributed traffic to %d SMTP connections, want %d", len(perConn), connections)
	}

	seenBody := make(map[string]bool, wantTransactions)
	for _, body := range bodies {
		for clientID := 0; clientID < connections; clientID++ {
			for tx := 0; tx < transactionsEach; tx++ {
				marker := fmt.Sprintf("stress-client-%02d-tx-%02d-unique-body-marker", clientID, tx)
				if strings.Contains(body, marker) {
					if seenBody[marker] {
						t.Errorf("backend received duplicate body marker %q", marker)
					}
					seenBody[marker] = true
				}
			}
		}
	}
	if len(seenBody) != wantTransactions {
		t.Errorf("backend body markers: got %d unique, want %d", len(seenBody), wantTransactions)
	}

	for clientID := 0; clientID < connections; clientID++ {
		var owner *smtp.Conn
		for tx := 0; tx < transactionsEach; tx++ {
			marker := fmt.Sprintf("stress-client-%02d-tx-%02d-unique-body-marker", clientID, tx)
			var found *smtp.Conn
			for conn, transcript := range perConn {
				if strings.Contains(transcript, marker) {
					if found != nil {
						t.Errorf("marker %q appears in multiple connection transcripts", marker)
					}
					found = conn
				}
			}
			if found == nil {
				t.Errorf("observer missed marker %q", marker)
				continue
			}
			if owner == nil {
				owner = found
			} else if owner != found {
				t.Errorf("transactions for client %d were attributed to different SMTP connections", clientID)
			}
		}
	}

	// Ensure all backend transactions include the marker expected in the body.
	for _, body := range bodies {
		if !strings.Contains(body, "unique-body-marker") {
			t.Errorf("unexpected body without a stress marker: %q", body)
		}
	}
	t.Logf("concurrency stress passed: %d connections, %d transactions, %d observer connection owners in %s", connections, wantTransactions, len(perConn), time.Since(started).Round(time.Millisecond))
}
