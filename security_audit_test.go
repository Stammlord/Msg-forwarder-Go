package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func assertUIStatus(t *testing.T, s *uiServer, method, target, bearer string, want int) {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if bearer != "" {
		r.Header.Set("Authorization", bearer)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d, want %d (%s)", method, target, w.Code, want, w.Body.String())
	}
}

func TestUILoginTokenIsSingleUseAndIndexOnly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	tokenPath := filepath.Join(dir, "ui.token")
	if err := writeConfig(configPath, Config{}); err != nil {
		t.Fatal(err)
	}
	tok, err := ensureUIToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: Config{}, cfgPath: configPath}
	initStates(a)
	s := &uiServer{app: a, configPath: configPath, token: tok}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?token="+tok, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login status %d: %s", w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "Msg-forwarder-Go" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no auth cookie issued")
	}
	newTok := cookie.Value
	if newTok == tok {
		t.Fatal("token was not rotated on login")
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != newTok {
		t.Fatal("rotated token was not persisted")
	}

	assertUIStatus(t, s, http.MethodGet, "/status", "Bearer "+tok, http.StatusUnauthorized)
	assertUIStatus(t, s, http.MethodGet, "/status", "Bearer "+newTok, http.StatusOK)
	assertUIStatus(t, s, http.MethodGet, "/status?token="+newTok, "", http.StatusUnauthorized)
	assertUIStatus(t, s, http.MethodPost, "/?token="+newTok, "", http.StatusUnauthorized)
}

func TestSOCKS5HandshakeHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	u, err := url.Parse("socks5://127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	d, err := newSOCKS5Dialer(u)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", "api.telegram.org:443")
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded against a silent proxy")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("handshake ignored context deadline: %v", elapsed)
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never accepted the connection")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// oneByteBody makes the decoder stop at the end of the JSON value instead of
// slurping the whole response, so draining can be observed.
type oneByteBody struct {
	r      *strings.Reader
	sawEOF bool
	closed bool
}

func (b *oneByteBody) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.sawEOF = true
	}
	return n, err
}

func (b *oneByteBody) Close() error {
	b.closed = true
	return nil
}

func TestVKPollDrainsResponseBody(t *testing.T) {
	body := &oneByteBody{r: strings.NewReader(`{"ts":123,"failed":0,"updates":[]}`)}
	a := &app{client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}}
	upd, err := a.vkPoll(context.Background(), vkLongPoll{Server: "https://vk.com", Key: "k"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Ts != 123 {
		t.Fatalf("unexpected ts %d", upd.Ts)
	}
	if !body.sawEOF {
		t.Fatal("response body was not drained to EOF")
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestLuCICSRFGuardFailsClosed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("luci", "controller", "Msg-forwarder-Go.lua"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, `if expected == "" then`) ||
		!strings.Contains(src, "Не удалось получить CSRF-токен формы") {
		t.Fatal("LuCI CSRF guard must reject a missing authtoken")
	}
	for _, fn := range []string{"action_test", "action_control", "action_send", "action_restart", "action_raw_save"} {
		if !strings.Contains(src, "function "+fn+"()") {
			t.Fatalf("missing action %s", fn)
		}
	}
	if n := strings.Count(src, "require_post_action()"); n < 5 {
		t.Fatalf("only %d guarded actions found", n)
	}
}

func TestErrKindCanceled(t *testing.T) {
	if got := errKind(context.Canceled); got != "canceled" {
		t.Fatalf("cancel: got %q", got)
	}
	if got := errKind(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline: got %q", got)
	}
}
