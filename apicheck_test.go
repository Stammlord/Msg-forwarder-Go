package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func useTestBotAPI(t *testing.T, base string) {
	t.Helper()
	old := botAPIBase
	botAPIBase = base
	t.Cleanup(func() { botAPIBase = old })
}

func TestCheckBotAPINeedsNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot0:0/getMe" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized: invalid token specified"}`)
	}))
	defer srv.Close()
	useTestBotAPI(t, srv.URL)

	if err := checkBotAPI(Config{}); err != nil {
		t.Fatalf("valid 401 reply rejected: %v", err)
	}
	if err := testService(Config{}, "api"); err != nil {
		t.Fatalf("testService(api) rejected: %v", err)
	}
}

func TestCheckBotAPIRejectsUnexpectedReply(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
	}{
		{"success reply", http.StatusOK, `{"ok":true}`},
		{"html error page", http.StatusUnauthorized, `<html>401</html>`},
		{"wrong error code", http.StatusUnauthorized, `{"ok":false,"error_code":400}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			useTestBotAPI(t, srv.URL)
			if err := checkBotAPI(Config{}); err == nil {
				t.Fatal("unexpected reply accepted")
			}
		})
	}
}

func TestCheckBotAPIRejectsBadProxy(t *testing.T) {
	useTestBotAPI(t, "http://127.0.0.1:1")
	if err := checkBotAPI(Config{TelegramProxy: "ftp://127.0.0.1:21"}); err == nil {
		t.Fatal("unsupported proxy accepted")
	}
}

func TestSOCKS5DialerUsesCredentials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			done <- fmt.Errorf("greeting: %w", err)
			return
		}
		if greeting[0] != 0x05 || greeting[2] != 0x02 {
			done <- fmt.Errorf("unexpected SOCKS5 greeting: % x", greeting)
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x02}) // select username/password auth
		var ulen, plen byte
		if err := readSOCKS5Auth(conn, &ulen, &plen); err != nil {
			done <- fmt.Errorf("auth: %w", err)
			return
		}
		_, _ = conn.Write([]byte{0x01, 0x00}) // auth OK
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
		// Consume the connect request before closing. On Windows closing a
		// socket with unread input sends RST and can discard the reply we just
		// wrote, making this check fail at random.
		_, _ = io.ReadFull(conn, make([]byte, 10))
		done <- nil
	}()

	u, err := url.Parse("socks5://user:pass@" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	d, err := newSOCKS5Dialer(u)
	if err != nil {
		t.Fatal(err)
	}
	if d.user != "user" || d.pass != "pass" {
		t.Fatalf("credentials not parsed: %q / %q", d.user, d.pass)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = d.DialContext(ctx, "tcp", "127.0.0.1:443")
	proxyErr := <-done
	if err == nil || !strings.Contains(err.Error(), "connect rejected") {
		t.Fatalf("unexpected handshake result: %v (proxy side: %v)", err, proxyErr)
	}
	if proxyErr != nil {
		t.Fatalf("proxy side: %v", proxyErr)
	}
}

func readSOCKS5Auth(conn net.Conn, ulen, plen *byte) error {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x01 {
		return fmt.Errorf("bad auth version")
	}
	*ulen = hdr[1]
	user := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return err
	}
	if string(user) != "user" {
		return fmt.Errorf("bad username")
	}
	if _, err := io.ReadFull(conn, hdr[:1]); err != nil {
		return err
	}
	*plen = hdr[0]
	pass := make([]byte, hdr[0])
	if _, err := io.ReadFull(conn, pass); err != nil {
		return err
	}
	if string(pass) != "pass" {
		return fmt.Errorf("bad password")
	}
	return nil
}
