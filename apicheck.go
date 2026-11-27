package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// botAPIBase is a variable so tests can point it at httptest.
var botAPIBase = "https://api.telegram.org"

const (
	botAPICheckTimeout = 10 * time.Second
	maxBotAPIAddresses = 4
)

// checkBotAPI verifies DNS, TCP, TLS and HTTP reachability of the Bot API
// without any bot token. It deliberately uses an invalid token and expects
// HTTP 401: that proves the request really reached Telegram and got a valid
// JSON reply, which is exactly what a real send will do.
func checkBotAPI(cfg Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), botAPICheckTimeout)
	defer cancel()

	hc, err := newTelegramHTTPClient(cfg)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	// The shared context is the only timeout: the client itself must not
	// keep a request alive after the check has finished.
	hc.Timeout = 0
	defer closeIdleClient(hc)

	if strings.TrimSpace(cfg.TelegramProxy) == "" {
		// With no proxy, report which stage fails instead of a single
		// generic network error. This mirrors the checks ran on the router.
		if err := checkDirectTLSPathContext(ctx, botAPIBase); err != nil {
			return err
		}
	}
	return checkBotAPIWithClientContext(ctx, hc)
}

// checkDirectTLSPath resolves the host, connects to :443 and completes a TLS
// handshake. It is skipped when a proxy is configured: in that case the proxy
// performs DNS and connects to Telegram, so a direct check would mislead.
func checkDirectTLSPath(base string) error {
	ctx, cancel := context.WithTimeout(context.Background(), botAPICheckTimeout)
	defer cancel()
	return checkDirectTLSPathContext(ctx, base)
}

func checkDirectTLSPathContext(ctx context.Context, base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("api: неверный URL")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return nil // tests use a local http:// server
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}

	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fmt.Errorf("dns: %s", dnsErr.Error())
		}
		return fmt.Errorf("dns: %s", errKind(err))
	}
	if len(addrs) == 0 {
		return fmt.Errorf("dns: нет адресов для %s", host)
	}

	var d net.Dialer
	var lastErr error
	checked := 0
	for _, ip := range addrs {
		if checked >= maxBotAPIAddresses {
			break
		}
		checked++
		addr := net.JoinHostPort(ip, port)
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			lastErr = fmt.Errorf("tcp %s: %s", addr, errKind(err))
			continue
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		err = tc.HandshakeContext(ctx)
		_ = conn.Close()
		if err != nil {
			lastErr = fmt.Errorf("tls %s: %s", addr, err)
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("tcp: нет адресов для подключения")
	}
	return lastErr
}

func checkBotAPIWithClient(hc *http.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), botAPICheckTimeout)
	defer cancel()
	return checkBotAPIWithClientContext(ctx, hc)
}

func checkBotAPIWithClientContext(ctx context.Context, hc *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, botAPIBase+"/bot0:0/getMe", nil)
	if err != nil {
		return fmt.Errorf("api: неверный URL")
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Do not wrap err: the URL contains the fake bot token, and future
		// callers may reuse this path with a real one.
		return fmt.Errorf("https: %s", errKind(err))
	}
	defer drainResponse(resp)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return fmt.Errorf("https: %s", errKind(err))
	}
	var r struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if json.Unmarshal(body, &r) != nil {
		return fmt.Errorf("api: неожиданный ответ (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized && !r.OK && r.ErrorCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("api: неожиданный ответ (HTTP %d)", resp.StatusCode)
}
