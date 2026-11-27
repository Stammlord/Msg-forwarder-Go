package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeDetailHidesWrappedProxyURL(t *testing.T) {
	inner := &url.Error{
		Op:  "Get",
		URL: "socks5://user:SUPERSECRET@127.0.0.1:1080",
		Err: errors.New("connection refused"),
	}
	detail := safeDetail(fmt.Errorf("proxy: %w", inner))
	if strings.Contains(detail, "SUPERSECRET") || strings.Contains(detail, "user") {
		t.Fatalf("proxy credentials leaked: %q", detail)
	}
}

func TestSafeDetailRedactsNonURLErrors(t *testing.T) {
	debugLog.setSecrets(Config{MaxToken: "MAXSECRET-TOKEN"})
	defer debugLog.setSecrets(Config{})
	detail := safeDetail(fmt.Errorf("max error opcode 9: MAXSECRET-TOKEN"))
	if strings.Contains(detail, "MAXSECRET-TOKEN") {
		t.Fatalf("server-controlled error leaked a secret: %q", detail)
	}
}

func TestCheckRedirectBlocksOtherHostOrPort(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	hc := &http.Client{CheckRedirect: checkRedirect}
	if _, err := hc.Get(source.URL); err == nil {
		t.Fatal("cross-port redirect was allowed")
	}
}

func TestTruncateTextKeepsValidUTF8(t *testing.T) {
	if got := truncateText("привет", 3); got != "при…" {
		t.Fatalf("cyrillic: %q", got)
	}
	if got := truncateText("😀😀😀", 2); got != "😀😀…" {
		t.Fatalf("emoji: %q", got)
	}
	if got := truncateText("abc", 3); got != "abc" {
		t.Fatalf("exact length changed: %q", got)
	}
}

func TestConfigFromFormRejectsBadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, Config{VKToken: "keep"}); err != nil {
		t.Fatal(err)
	}
	s := &uiServer{configPath: path}

	bad := url.Values{"incoming_opcode": {"0"}, "ui_present": {"1"}}
	r := httptest.NewRequest("POST", "/save", strings.NewReader(bad.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := s.configFromForm(r); err == nil {
		t.Fatal("opcode 0 accepted")
	}

	bad = url.Values{"ui_listen": {"not-a-listen-addr"}, "ui_present": {"1"}}
	r = httptest.NewRequest("POST", "/save", strings.NewReader(bad.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := s.configFromForm(r); err == nil {
		t.Fatal("invalid ui_listen accepted")
	}

	good := url.Values{"ui_present": {"1"}, "ui_enabled": {"on"}}
	r = httptest.NewRequest("POST", "/save", strings.NewReader(good.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c, err := s.configFromForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.VKToken != "keep" {
		t.Fatalf("token lost: %q", c.VKToken)
	}
	if c.IncomingOpcode != 128 || c.MaxWSURL == "" || c.UIListen == "" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestWebSocketRejectsMaskedServerFrame(t *testing.T) {
	// FIN|text (0x81), masked with 1-byte payload. A compliant server never
	// masks, so this must be rejected before touching the payload.
	raw := []byte{0x81, 0x81, 1, 2, 3, 4, 'x' ^ 1}
	w := &wsConn{br: bufio.NewReader(bytes.NewReader(raw)), maxMessage: 1024}
	if _, err := w.readMessage(); err == nil {
		t.Fatal("masked server frame accepted")
	}
}

func TestWebSocketRejectsOversizedControlFrame(t *testing.T) {
	raw := []byte{0x89, 126, 0, 126} // ping with 126-byte payload
	raw = append(raw, make([]byte, 126)...)
	w := &wsConn{br: bufio.NewReader(bytes.NewReader(raw)), maxMessage: 1024}
	if _, err := w.readMessage(); err == nil {
		t.Fatal("oversized control frame accepted")
	}
}
