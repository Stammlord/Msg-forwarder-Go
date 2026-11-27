package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// testService is shared by the CLI, the built-in UI and the LuCI page.
// Returned errors are short and never contain tokens.
func testService(cfg Config, mode string) error {
	switch mode {
	case "vk":
		hc := &http.Client{Transport: baseTransport(), Timeout: 20 * time.Second, CheckRedirect: checkRedirect}
		defer closeIdleClient(hc)
		return vkValidate(cfg.VKToken, cfg.VKAPIVersion, hc)

	case "tg":
		hc, err := newTelegramHTTPClient(cfg)
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		defer closeIdleClient(hc)
		return newTelegram(cfg, hc).test()

	case "max":
		return maxValidate(cfg.MaxToken, cfg.MaxDeviceID, cfg.MaxUserAgent, cfg.MaxAppVersion, cfg.MaxWSURL)

	case "bark":
		hc := &http.Client{Transport: baseTransport(), Timeout: 20 * time.Second, CheckRedirect: checkRedirect}
		defer closeIdleClient(hc)
		return newBark(cfg, hc).test()

	case "api":
		// Token-free check: DNS/TCP/TLS/HTTPS to api.telegram.org.
		return checkBotAPI(cfg)

	default:
		return fmt.Errorf("неизвестная служба %q", mode)
	}
}

func runTest(cfg Config, mode string) {
	if err := testService(cfg, mode); err != nil {
		fmt.Printf("FAIL: %s: %s\n", mode, err)
		os.Exit(1)
	}
	fmt.Printf("OK: %s\n", mode)
}

// pingService is a cheap TCP reachability check, not an API call.
// controlURL always targets loopback: it carries the UI token in a header and
// must never send it over the LAN, even if the UI itself is bound externally.
func controlURL(cfg Config, path string) string {
	listen := cfg.UIListen
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		port = "8080"
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + path
}

// runControl is a tiny HTTP client for LuCI. It uses the same token file as
// the browser UI, so the Lua controller does not depend on curl being
// installed on the router.
func runControl(configPath string, cfg Config, mode, service, action, value, text string) {
	tok, err := ensureUIToken(filepath.Join(filepath.Dir(configPath), "ui.token"))
	if err != nil {
		fmt.Printf("FAIL: %s\n", err)
		os.Exit(1)
	}

	var req *http.Request
	switch mode {
	case "status":
		req, err = http.NewRequest(http.MethodGet, controlURL(cfg, "/status"), nil)
	case "control":
		if !validService(service) {
			fmt.Println("FAIL: неизвестная служба")
			os.Exit(1)
		}
		form := url.Values{"service": {service}, "action": {action}}
		if value != "" {
			form.Set("value", value)
		}
		req, err = http.NewRequest(http.MethodPost, controlURL(cfg, "/control"), strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	case "send":
		if service == "" {
			service = "tg"
		}
		if !validService(service) {
			fmt.Println("FAIL: неизвестная служба")
			os.Exit(1)
		}
		form := url.Values{"service": {service}, "text": {text}}
		req, err = http.NewRequest(http.MethodPost, controlURL(cfg, "/send"), strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	case "debug":
		// Only whitelisted actions are turned into URL paths.
		act := strings.ToLower(strings.TrimSpace(action))
		switch act {
		case "status", "log":
			req, err = http.NewRequest(http.MethodGet, controlURL(cfg, "/debug/"+act), nil)
		case "on", "off":
			form := url.Values{"action": {act}}
			req, err = http.NewRequest(http.MethodPost, controlURL(cfg, "/debug/"+act), strings.NewReader(form.Encode()))
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		default:
			fmt.Println("FAIL: неизвестное действие отладки (status, on, off, log)")
			os.Exit(1)
		}
	default:
		fmt.Printf("FAIL: неизвестная команда %q (status, control, send, debug)\n", mode)
		os.Exit(1)
	}
	if err != nil {
		fmt.Printf("FAIL: %s\n", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	hc := &http.Client{Transport: baseTransport(), Timeout: 40 * time.Second, CheckRedirect: checkRedirect}
	defer closeIdleClient(hc)
	resp, err := hc.Do(req)
	if err != nil {
		fmt.Printf("FAIL: %s\n", errKind(err))
		os.Exit(1)
	}
	defer drainResponse(resp)
	// A debug log download contains two rotated 1 MiB files; the usual tiny
	// control-response limit would silently truncate it.
	limit := int64(64 << 10)
	if mode == "debug" && strings.TrimSpace(action) == "log" {
		limit = 4 << 20
	}
	_, _ = io.Copy(os.Stdout, io.LimitReader(resp.Body, limit))
}

func pingService(cfg Config, mode string) (string, error) {
	var host, label string
	switch mode {
	case "vk":
		host, label = "api.vk.com:443", "api.vk.com"
	case "tg":
		host, label = "api.telegram.org:443", "api.telegram.org"
		// Telegram is the only service that may use a proxy: ping the proxy,
		// not the API, otherwise the result would be misleading.
		if cfg.TelegramProxy != "" {
			u, err := url.Parse(cfg.TelegramProxy)
			if err != nil || u.Hostname() == "" {
				return "", fmt.Errorf("proxy: неверный адрес")
			}
			port := u.Port()
			if port == "" {
				port = "1080"
				if u.Scheme == "http" {
					port = "80"
				} else if u.Scheme == "https" {
					port = "443"
				}
			}
			host = net.JoinHostPort(u.Hostname(), port)
			label = "прокси " + u.Scheme
		}
	case "max":
		host = "api.oneme.ru:443"
		if u, err := url.Parse(cfg.MaxWSURL); err == nil && u.Hostname() != "" {
			port := u.Port()
			if port == "" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
	case "bark":
		target, err := barkPingTarget(cfg)
		if err != nil {
			return "", err
		}
		host = target
		server, _ := normalizeBarkServerURL(cfg.BarkServerURL)
		u, _ := url.Parse(server)
		label = u.Hostname()
	default:
		return "", fmt.Errorf("неизвестная служба %q", mode)
	}
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("net: %s", errKind(err))
	}
	_ = conn.Close()
	return fmt.Sprintf("OK: %s (%s) за %d мс", label, host, time.Since(start).Milliseconds()), nil
}
