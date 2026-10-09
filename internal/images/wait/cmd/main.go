// Command wait waits indefinitely for a TCP endpoint to accept a connection.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	dialTimeout   = 5 * time.Second
	retryInterval = 2 * time.Second
)

type settings struct {
	dialTimeout   time.Duration
	retryInterval time.Duration
	dialContext   func(context.Context, string, string) (net.Conn, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr, settings{
		dialTimeout:   dialTimeout,
		retryInterval: retryInterval,
		dialContext:   (&net.Dialer{}).DialContext,
	})
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func endpoint(args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: /wait host:port")
	}
	address := args[0]
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || strings.IndexFunc(host, unicode.IsSpace) >= 0 || strings.ContainsAny(host, "/?#") {
		return "", fmt.Errorf("invalid endpoint %q: expected host:port (bracket IPv6 addresses)", address)
	}
	if strings.Contains(host, ":") || strings.HasPrefix(address, "[") {
		ip, _, _ := strings.Cut(host, "%")
		if net.ParseIP(ip) == nil {
			return "", fmt.Errorf("invalid endpoint %q: invalid IP address", address)
		}
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", fmt.Errorf("invalid endpoint %q: port must be numeric and in 1..65535", address)
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return "", fmt.Errorf("invalid endpoint %q: port must be in 1..65535", address)
	}
	return address, nil
}

func run(ctx context.Context, args []string, output io.Writer, cfg settings) error {
	address, err := endpoint(args)
	if err != nil {
		return err
	}
	logger := log.New(output, "wait: ", log.LstdFlags)
	logger.Printf("waiting for %s", address)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s canceled: %w", address, err)
		}
		// One deadline covers both DNS resolution and all TCP connection attempts.
		attempt, cancel := context.WithTimeout(ctx, cfg.dialTimeout)
		conn, err := cfg.dialContext(attempt, "tcp", address)
		cancel()
		if err == nil {
			if err := conn.Close(); err != nil {
				logger.Printf("closing connection to %s: %v", address, err)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("waiting for %s canceled: %w", address, err)
			}
			logger.Printf("%s is available", address)
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for %s canceled: %w", address, ctx.Err())
		}
		logger.Printf("%s unavailable: %v; retrying in %s", address, err, cfg.retryInterval)
		timer := time.NewTimer(cfg.retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for %s canceled: %w", address, ctx.Err())
		case <-timer.C:
		}
	}
}
