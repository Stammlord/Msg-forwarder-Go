package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testLogger(dir string) *debugLogger {
	d := newDebugLogger()
	d.path = filepath.Join(dir, debugLogFile)
	d.prevPath = filepath.Join(dir, debugLogPrevFile)
	d.enabledPath = filepath.Join(dir, debugEnabledFile)
	return d
}

func TestDebugLogDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	d.logf("should not be written")
	if d.Enabled() {
		t.Fatal("debug log enabled by default")
	}
	if _, err := os.Stat(d.path); !os.IsNotExist(err) {
		t.Fatalf("log file created while disabled: %v", err)
	}
	if st := d.status(); st.Enabled || st.Bytes != 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestDebugLogWritesAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	d.setSecrets(Config{
		TGBotToken:    "SUPERSECRET-TOKEN",
		TelegramProxy: "socks5://user:PROXYPASS@127.0.0.1:1080",
	})
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	d.logf("tg error for %s with %s", "SUPERSECRET-TOKEN", "PROXYPASS")
	d.disable()

	b, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "tg error for *** with ***") {
		t.Fatalf("redaction failed: %s", s)
	}
	if strings.Contains(s, "SUPERSECRET-TOKEN") || strings.Contains(s, "PROXYPASS") {
		t.Fatalf("secret leaked: %s", s)
	}
	if st := d.status(); st.Enabled || st.Lines == 0 || st.Bytes == 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(d.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("bad log mode: %v", fi.Mode().Perm())
		}
	}
}

func TestDebugLogRecordsRSS(t *testing.T) {
	if rss := currentRSS(); rss <= 0 {
		t.Fatalf("currentRSS() = %d, want > 0", rss)
	}
	dir := t.TempDir()
	d := testLogger(dir)
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	d.logf("память %d", 42)
	d.disable()
	b, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "rss=") || !strings.Contains(s, "kB память 42") {
		t.Fatalf("no RSS snapshot in log: %s", s)
	}
}

func TestDebugLogRedactsBarkURLAndControlResult(t *testing.T) {
	dir := t.TempDir()
	old := debugLog
	defer func() { debugLog = old }()
	debugLog = testLogger(dir)
	debugLog.setSecrets(Config{BarkServerURL: "http://user:BARKPASS@192.168.1.10:8080"})
	if err := debugLog.enable(); err != nil {
		t.Fatal(err)
	}
	// Response bodies are never logged: chat_id must not reach the file.
	logControlResult("tg", "chatid", "chat_id: 123456789")
	// The whole configured URL is a secret even though callers log only host.
	debugLog.logf("server %s", "http://user:BARKPASS@192.168.1.10:8080")
	debugLog.disable()

	b, err := os.ReadFile(debugLog.path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "BARKPASS") || strings.Contains(s, "123456789") {
		t.Fatalf("secret or chat id leaked: %s", s)
	}
	if !strings.Contains(s, "action=chatid failed=false") {
		t.Fatalf("control outcome missing: %s", s)
	}
}

func TestDebugLogRotatesAndKeepsPredecessor(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	d.maxBytes = 2048
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		d.logf("line %03d %s", i, strings.Repeat("x", 40))
	}
	d.disable()

	cur, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(d.prevPath)
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if !strings.Contains(string(cur), "line 119") {
		t.Fatal("last line lost after rotation")
	}
	if len(prev) == 0 {
		t.Fatal("rotated file is empty")
	}
	st := d.status()
	if st.Bytes == 0 || st.PrevBytes == 0 {
		t.Fatalf("rotation not reflected in status: %+v", st)
	}
	if st.Bytes > d.maxBytes+int64(len("2026-01-02T15:04:05.000Z07:00 line 000 ")+600) {
		t.Fatalf("log grew without bound: %d", st.Bytes)
	}

	w := httptest.NewRecorder()
	d.serve(w)
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("download header missing")
	}
	body := w.Body.String()
	if !strings.Contains(body, "line 119") || !strings.Contains(body, "предыдущий файл") {
		t.Fatal("download did not include both files")
	}
}

func TestDebugHandlersAndDownload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	old := debugLog
	defer func() { debugLog = old }()
	debugLog = newDebugLogger()
	initDebugLog(configPath)

	a := &app{}
	initStates(a)

	w := httptest.NewRecorder()
	a.handleDebugStatus(w, httptest.NewRequest(http.MethodGet, "/debug/status", nil))
	var st debugStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Fatal("debug log enabled before switch")
	}

	w = httptest.NewRecorder()
	a.handleDebugSwitch(w, httptest.NewRequest(http.MethodPost, "/debug/on", nil), true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "включён") {
		t.Fatalf("enable failed: %d %s", w.Code, w.Body.String())
	}
	debugLog.logf("проверка %d", 42)

	w = httptest.NewRecorder()
	a.handleDebugStatus(w, httptest.NewRequest(http.MethodGet, "/debug/status", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || (st.Bytes == 0 && st.Buffered == 0) || st.Path == "" {
		t.Fatalf("bad enabled status: %+v", st)
	}

	w = httptest.NewRecorder()
	a.handleDebugLog(w, httptest.NewRequest(http.MethodGet, "/debug/log", nil))
	if !strings.Contains(w.Body.String(), "проверка 42") {
		t.Fatalf("download missing log line: %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	a.handleDebugSwitch(w, httptest.NewRequest(http.MethodPost, "/debug/off", nil), false)
	if w.Code != http.StatusOK || debugLog.Enabled() {
		t.Fatalf("disable failed: %d %s", w.Code, w.Body.String())
	}

	// The log must remain available after the operator switches it off.
	w = httptest.NewRecorder()
	a.handleDebugLog(w, httptest.NewRequest(http.MethodGet, "/debug/log", nil))
	if !strings.Contains(w.Body.String(), "проверка 42") {
		t.Fatal("log vanished after disable")
	}
}

func TestDebugLogBuffersAndFlushesOnDownload(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	d.logf("буфер %d", 1)
	if st := d.status(); st.Buffered == 0 || st.Bytes != 0 {
		t.Fatalf("line should stay in RAM: %+v", st)
	}
	if b, err := os.ReadFile(d.path); err != nil || bytes.Contains(b, []byte("буфер 1")) {
		t.Fatalf("flushed before download: %q %v", b, err)
	}

	w := httptest.NewRecorder()
	d.serve(w)
	if !strings.Contains(w.Body.String(), "буфер 1") {
		t.Fatal("download missed buffered line")
	}
	if st := d.status(); st.Buffered != 0 || st.Bytes == 0 {
		t.Fatalf("buffer not flushed: %+v", st)
	}
	d.disable()
}

func TestDebugLogFlushesByTimeAndSize(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	// Simulate ten minutes passing without waiting.
	d.lastFlush = time.Now().Add(-debugFlushInterval)
	d.logf("по таймеру")
	if b, err := os.ReadFile(d.path); err != nil || !bytes.Contains(b, []byte("по таймеру")) {
		t.Fatalf("timer flush failed: %q %v", b, err)
	}

	// A heavy burst must not grow the RAM buffer without bound.
	for i := 0; i < 1500; i++ {
		d.logf("строка %04d %s", i, strings.Repeat("x", 60))
	}
	b, err := os.ReadFile(d.path)
	if err != nil || len(b) < debugBufferMax {
		t.Fatalf("size flush failed: %d bytes, %v", len(b), err)
	}
	if !bytes.Contains(b, []byte("буфер полон")) || !bytes.Contains(b, []byte("внеочередной сброс")) {
		t.Fatal("buffer pressure was not logged")
	}
	if st := d.status(); st.ForcedFlushes == 0 {
		t.Fatalf("forced flush counter not exposed: %+v", st)
	}
	d.disable()
}

func TestDebugLogPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first := testLogger(dir)
	if err := first.enable(); err != nil {
		t.Fatal(err)
	}
	first.logf("до рестарта")
	first.disable()
	if fileExists(first.enabledPath) {
		t.Fatal("marker survived disable")
	}

	// Re-enable, then imitate a new process reading the marker on startup.
	if err := first.enable(); err != nil {
		t.Fatal(err)
	}
	oldSession := first.sessionID
	second := testLogger(dir)
	if !fileExists(second.enabledPath) || !second.restore() {
		t.Fatal("logging was not restored at startup")
	}
	if second.sessionID == oldSession {
		t.Fatal("session id must be unique per process")
	}
	second.logf("после рестарта")
	second.disable()
	first.disable() // the old process finally exits and flushes its buffer

	b, err := os.ReadFile(second.path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "до рестарта") || !strings.Contains(s, "после рестарта") {
		t.Fatalf("history across sessions lost: %s", s)
	}
	// Two enables of the first instance (one to check that disable removes the
	// marker, one to leave it) plus one restore in the "new process".
	if n := strings.Count(s, "=== session="); n != 3 {
		t.Fatalf("want three session headers, got %d: %s", n, s)
	}
}

func TestDebugLogCloseFileKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	d.closeFile()
	if d.Enabled() {
		t.Fatal("still enabled after close")
	}
	if !fileExists(d.enabledPath) {
		t.Fatal("shutdown removed the persistence marker")
	}
	next := testLogger(dir)
	if !next.restore() {
		t.Fatal("logging not restored after clean shutdown")
	}
	next.disable()
}

func TestDebugLogHeaderAndSequence(t *testing.T) {
	dir := t.TempDir()
	d := testLogger(dir)
	d.setSecrets(Config{IncomingOpcode: 129, TelegramProxy: "socks5h://127.0.0.1:1080"})
	if err := d.enable(); err != nil {
		t.Fatal(err)
	}
	d.logf("первая строка")
	d.logf("вторая строка")
	d.disable()

	b, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"session=", "pid=", "ver=", "opcode=129", "proxy=socks5h/127.0.0.1", "#000001", "#000005"} {
		if !strings.Contains(s, want) {
			t.Fatalf("header/sequence missing %q: %s", want, s)
		}
	}
}

func TestSafeErrTextAndMapKeys(t *testing.T) {
	withURL := fmt.Errorf("request: %w", &url.Error{
		Op:  "Get",
		URL: "https://api.telegram.org/botSECRET/sendMessage",
		Err: errors.New("dial timeout"),
	})
	if got := safeErrText(withURL); strings.Contains(got, "SECRET") || got != errKind(withURL) {
		t.Fatalf("url error leaked or lost: %q", got)
	}
	if got := safeErrText(errors.New("tls: bad\r\ncertificate")); got != "tls: bad  certificate" {
		t.Fatalf("sanitised error: %q", got)
	}
	if got := safeErrText(nil); got != "" {
		t.Fatalf("nil error: %q", got)
	}
	if got := mapKeys(map[string]any{"z": 1, "a": 2, "secret": 3}); got != "a,secret,z" {
		t.Fatalf("keys: %q", got)
	}
	if got := mapKeys(nil); got != "-" {
		t.Fatalf("empty keys: %q", got)
	}
	if nextTrace() <= 0 {
		t.Fatal("trace counter must be positive")
	}
}

func TestUIDebugRoutesRequireAuth(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	old := debugLog
	defer func() { debugLog = old }()
	debugLog = newDebugLogger()
	initDebugLog(configPath)

	a := &app{}
	initStates(a)
	s := &uiServer{app: a, configPath: configPath, token: "test-token"}

	assertUIStatus(t, s, http.MethodGet, "/debug/status", "", http.StatusUnauthorized)
	assertUIStatus(t, s, http.MethodGet, "/debug/log", "", http.StatusUnauthorized)
	assertUIStatus(t, s, http.MethodGet, "/debug/status", "Bearer test-token", http.StatusOK)
	assertUIStatus(t, s, http.MethodGet, "/debug/log", "Bearer test-token", http.StatusOK)
	assertUIStatus(t, s, http.MethodPost, "/debug/log", "Bearer test-token", http.StatusMethodNotAllowed)
}

func TestDebugLogMirrorsLogf(t *testing.T) {
	dir := t.TempDir()
	old := debugLog
	defer func() { debugLog = old }()
	debugLog = testLogger(dir)
	if err := debugLog.enable(); err != nil {
		t.Fatal(err)
	}
	logf("mirror %s", "ok")
	debugLog.disable()

	b, err := os.ReadFile(debugLog.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "mirror ok") {
		t.Fatalf("logf not mirrored: %s", b)
	}
}

func TestSanitizeAndMask(t *testing.T) {
	if got := sanitizeDebugLine("a\r\nb\tc\x00d"); got != "a  b c?d" {
		t.Fatalf("sanitize: %q", got)
	}
	if got := sanitizeDebugLine(strings.Repeat("x", 1000)); len([]rune(got)) > debugLogLineMax {
		t.Fatalf("not truncated: %d", len([]rune(got)))
	}
	if got := sanitizeDebugLine(strings.Repeat("x", 1<<20)); len([]rune(got)) > debugLogLineMax {
		t.Fatalf("large input not truncated: %d", len([]rune(got)))
	}
	if got := maskID("123456789"); got != "…6789" {
		t.Fatalf("mask: %q", got)
	}
	if got := maskID("12"); got != "****" {
		t.Fatalf("short mask: %q", got)
	}
}
