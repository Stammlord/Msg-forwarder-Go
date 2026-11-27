package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// baseTransport is shared by direct HTTPS calls. Proxy is deliberately nil:
// VK and MAX must never go through an environment proxy.
func baseTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 35 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
	}
}

// newTelegramHTTPClient applies the proxy only to Telegram traffic.
// Supported: http://, https://, socks5://, socks5h://.
func newTelegramHTTPClient(cfg Config) (*http.Client, error) {
	tr := baseTransport()
	proxy := strings.TrimSpace(cfg.TelegramProxy)
	if proxy == "" {
		return &http.Client{Transport: tr, Timeout: 40 * time.Second, CheckRedirect: checkRedirect}, nil
	}

	u, err := url.Parse(proxy)
	if err != nil {
		// Do not echo the raw URL: it may contain user:password.
		return nil, fmt.Errorf("proxy: неверный URL")
	}
	debugf("[PROXY] telegram scheme=%s host=%s", strings.ToLower(u.Scheme), u.Hostname())
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		// Для Telegram рекомендуется socks5h: DNS работает на стороне прокси,
		// локальный резолвер не используется.
		d, err := newSOCKS5Dialer(u)
		if err != nil {
			return nil, err
		}
		tr.DialContext = d.DialContext
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return &http.Client{Transport: tr, Timeout: 40 * time.Second, CheckRedirect: checkRedirect}, nil
}

// closeIdleClient releases idle connections kept by an HTTP client.
func closeIdleClient(hc *http.Client) {
	if hc != nil {
		hc.CloseIdleConnections()
	}
}

type socks5Dialer struct {
	addr      string
	user      string
	pass      string
	remoteDNS bool // socks5h:// resolves the target on the proxy side
	netDialer *net.Dialer
}

func newSOCKS5Dialer(u *url.URL) (*socks5Dialer, error) {
	addr := u.Host
	if u.Port() == "" {
		addr += ":1080"
	}
	d := &socks5Dialer{
		addr:      addr,
		remoteDNS: strings.EqualFold(u.Scheme, "socks5h"),
		netDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
	}
	if u.User != nil {
		d.user = u.User.Username()
		d.pass, _ = u.User.Password()
		if len(d.user) > 255 || len(d.pass) > 255 {
			return nil, fmt.Errorf("socks5: credentials too long")
		}
	}
	return d, nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	start := time.Now()
	debugf("[PROXY] socks5 connect addr=%s remoteDNS=%t auth=%t", d.addr, d.remoteDNS, d.user != "")
	conn, err := d.netDialer.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		debugf("[PROXY] socks5 tcp fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	// A malicious/hung proxy must not block Telegram sends forever: the HTTP
	// client timeout does not cover our manual SOCKS handshake reads. Use the
	// earlier of the caller's deadline and a fixed cap, and also check for
	// cancellation between reads so a cancelled request returns at once.
	deadline := time.Now().Add(15 * time.Second)
	if dl, has := ctx.Deadline(); has && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	checkCtx := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}

	greeting := []byte{0x05, 0x01, 0x00}
	if d.user != "" {
		greeting[2] = 0x02
	}
	if err := writeAll(conn, greeting); err != nil {
		return nil, err
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, err
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}
	if reply[0] != 0x05 {
		return nil, fmt.Errorf("socks5: bad version")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if d.user == "" {
			return nil, fmt.Errorf("socks5: server requires auth")
		}
		auth := []byte{0x01, byte(len(d.user))}
		auth = append(auth, d.user...)
		auth = append(auth, byte(len(d.pass)))
		auth = append(auth, d.pass...)
		if err := writeAll(conn, auth); err != nil {
			return nil, err
		}
		if err := checkCtx(); err != nil {
			return nil, err
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return nil, err
		}
		if err := checkCtx(); err != nil {
			return nil, err
		}
		if ar[0] != 0x01 || ar[1] != 0x00 {
			return nil, fmt.Errorf("socks5: auth failed")
		}
	default:
		return nil, fmt.Errorf("socks5: no acceptable auth method")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !d.remoteDNS && net.ParseIP(host) == nil {
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("socks5: dns error")
		}
		host = ips[0]
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad port")
	}

	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append([]byte{0x05, 0x01, 0x00, 0x01}, ip4...)
		} else {
			req = []byte{0x05, 0x01, 0x00, 0x04}
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("socks5: host too long")
		}
		req = []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if err := writeAll(conn, req); err != nil {
		return nil, err
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}

	rep := make([]byte, 4)
	if _, err := io.ReadFull(conn, rep); err != nil {
		return nil, err
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}
	if rep[0] != 0x05 || rep[1] != 0x00 || rep[2] != 0x00 {
		return nil, fmt.Errorf("socks5: connect rejected")
	}
	switch rep[3] {
	case 0x01:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return nil, err
		}
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(conn, make([]byte, int(n[0])+2)); err != nil {
			return nil, err
		}
	case 0x04:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("socks5: bad address type")
	}
	if err := checkCtx(); err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Time{})
	ok = true
	debugf("[PROXY] socks5 connected addr=%s dur=%s", d.addr, time.Since(start))
	return conn, nil
}
