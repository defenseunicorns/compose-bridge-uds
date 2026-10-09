package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func testSettings() settings {
	return settings{
		dialTimeout:   50 * time.Millisecond,
		retryInterval: 5 * time.Millisecond,
		dialContext:   (&net.Dialer{}).DialContext,
	}
}

func TestEndpoint(t *testing.T) {
	for _, address := range []string{"localhost:80", "db:5432", "127.0.0.1:1", "host:65535", "host:00080", "[::1]:443", "[fe80::1%lo0]:80"} {
		t.Run(address, func(t *testing.T) {
			got, err := endpoint([]string{address})
			if err != nil || got != address {
				t.Fatalf("endpoint = %q, %v", got, err)
			}
		})
	}
	for _, args := range [][]string{
		nil, {"host:80", "extra"}, {""}, {"host"}, {":80"}, {"host:"},
		{"host:http"}, {"host:0"}, {"host:65536"}, {"host:-1"}, {"host:+80"},
		{"host: 80"}, {"host:８０"}, {"host:999999999999999999999"},
		{" host:80"}, {"host\n:80"}, {"tcp://host:80"}, {"host/path:80"},
		{"::1:80"}, {"[::1]"}, {"[not-ip]:80"}, {"host:80:90"},
	} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			cfg := testSettings()
			cfg.dialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("invalid arguments reached dial")
				return nil, nil
			}
			if err := run(context.Background(), args, io.Discard, cfg); err == nil {
				t.Fatal("expected argument error")
			}
		})
	}
}

func TestAvailableEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := run(ctx, []string{listener.Addr().String()}, &output, testSettings()); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("successful connection was not closed: %v", err)
	}
	if !strings.Contains(output.String(), "waiting for") || !strings.Contains(output.String(), "is available") {
		t.Fatalf("missing logs: %s", &output)
	}
}

func TestDelayedEndpoint(t *testing.T) {
	// Start the listener only after the first real connection attempt fails.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	var listener net.Listener
	defer func() {
		if listener != nil {
			listener.Close()
		}
	}()
	cfg := testSettings()
	dial := cfg.dialContext
	attempts := 0
	cfg.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts++
		conn, err := dial(ctx, network, address)
		if attempts == 1 {
			if err == nil {
				conn.Close()
				t.Fatal("endpoint unexpectedly available")
			}
			listener, err = net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("endpoint not started yet")
		}
		return conn, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := run(ctx, []string{address}, &output, cfg); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 || !strings.Contains(output.String(), "retrying in") {
		t.Fatalf("expected retries, attempts=%d, logs=%s", attempts, &output)
	}
}

func TestDialTimeoutRetriesUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testSettings()
	cfg.dialTimeout = 5 * time.Millisecond
	attempts := 0
	cfg.dialContext = func(attempt context.Context, network, address string) (net.Conn, error) {
		attempts++
		if network != "tcp" || address != "db:80" {
			t.Fatalf("dial(%q, %q)", network, address)
		}
		deadline, ok := attempt.Deadline()
		if !ok || time.Until(deadline) > cfg.dialTimeout {
			t.Fatal("dial has no bounded deadline")
		}
		<-attempt.Done()
		if !errors.Is(attempt.Err(), context.DeadlineExceeded) {
			t.Fatalf("attempt error = %v", attempt.Err())
		}
		if attempts == 3 {
			cancel()
		}
		return nil, attempt.Err()
	}
	if err := run(ctx, []string{"db:80"}, io.Discard, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestCancellationDuringDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	cfg := testSettings()
	cfg.dialTimeout = time.Hour
	cfg.dialContext = func(attempt context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-attempt.Done()
		return nil, attempt.Err()
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"db:80"}, io.Discard, cfg) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancel()
	assertCanceled(t, done)
}

type retryWriter struct{ logged chan struct{} }

func (w retryWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("retrying in")) {
		close(w.logged)
	}
	return len(p), nil
}

func TestCancellationDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testSettings()
	cfg.retryInterval = time.Hour
	cfg.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unavailable")
	}
	logged := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"db:80"}, retryWriter{logged}, cfg) }()
	select {
	case <-logged:
	case <-time.After(time.Second):
		t.Fatal("did not reach retry sleep")
	}
	cancel()
	assertCanceled(t, done)
}

func TestAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testSettings()
	cfg.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("canceled wait reached dial")
		return nil, nil
	}
	if err := run(ctx, []string{"db:80"}, io.Discard, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
}

func assertCanceled(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt wait")
	}
}
