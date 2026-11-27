package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	debugLogFile     = "debug.log"
	debugLogPrevFile = "debug.log.1"
	debugEnabledFile = "debug.enabled"
	// 1 MiB active + 1 MiB previous. Enough for a detailed history of a
	// low-volume bridge; bounded so a release build cannot fill router flash.
	debugLogMaxBytes = int64(1 << 20)
	debugLogLineMax  = 512
	// Lines are accumulated in RAM and flushed to flash at most once every
	// 10 minutes. The cap bounds RAM if traffic is unusually heavy.
	debugFlushInterval = 10 * time.Minute
	debugBufferMax     = 64 << 10
)

// debugStatus is the read-only state shown in both control panels.
type debugStatus struct {
	Enabled       bool   `json:"enabled"`
	Persisted     bool   `json:"persisted"`
	Session       string `json:"session"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	PrevBytes     int64  `json:"prev_bytes"`
	Lines         uint64 `json:"lines"`
	Dropped       uint64 `json:"dropped"`
	MaxBytes      int64  `json:"max_bytes"`
	Buffered      int64  `json:"buffered"`
	ForcedFlushes uint64 `json:"forced_flushes"`
}

// debugLogger is an append-only, size-bounded log with one rotated predecessor.
// It never stores tokens or message bodies: callers pass metadata only, and
// every line is additionally checked against the secrets from the config.
type debugLogger struct {
	mu            sync.Mutex
	path          string
	prevPath      string
	enabledPath   string
	f             *os.File
	size          int64
	maxBytes      int64
	lines         uint64
	dropped       uint64
	seq           uint64
	pending       []byte
	lastFlush     time.Time
	forcedFlushes uint64
	enabled       atomic.Bool
	secrets       []string
	authSecrets   []string
	sessionID     string
	cfgNote       string
}

var debugLog = newDebugLogger()

var sessionSeq atomic.Uint64

func newDebugLogger() *debugLogger {
	return &debugLogger{
		maxBytes:  debugLogMaxBytes,
		sessionID: fmt.Sprintf("%x-%x", time.Now().UnixNano(), sessionSeq.Add(1)),
	}
}

func initDebugLog(configPath string) {
	dir := filepath.Dir(configPath)
	debugLog.mu.Lock()
	debugLog.path = filepath.Join(dir, debugLogFile)
	debugLog.prevPath = filepath.Join(dir, debugLogPrevFile)
	debugLog.enabledPath = filepath.Join(dir, debugEnabledFile)
	debugLog.mu.Unlock()
}

// writeFileNoFollow replaces a small marker file without following symlinks.
func writeFileNoFollow(path string, b []byte, perm os.FileMode) error {
	f, err := openNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// restore enables logging at process start when the operator left it on.
// This is what makes startup failures visible in the released binary.
func (d *debugLogger) restore() bool {
	d.mu.Lock()
	marker := d.enabledPath
	d.mu.Unlock()
	if marker == "" {
		return false
	}
	if _, err := os.Stat(marker); err != nil {
		return false
	}
	return d.enable() == nil
}

func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := bi.Main.Version
	if v == "" {
		v = "dev"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) > 0 {
			v += "@" + s.Value[:minInt(len(s.Value), 8)]
		}
	}
	return v
}

var traceSeq atomic.Uint64

// nextTrace links an incoming event to the outgoing queue/worker lines.
func nextTrace() uint64 { return traceSeq.Add(1) }

// mapKeys exposes only the shape of a decoded payload, never its values.
func mapKeys(m map[string]any) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func logURLHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "-"
	}
	return u.Hostname()
}

func logURLScheme(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return "-"
	}
	return strings.ToLower(u.Scheme)
}

// setSecrets installs values that must never appear in the debug file, even
// if a caller accidentally formats a raw error containing a full URL.
func (d *debugLogger) setSecrets(cfg Config) {
	secrets := make([]string, 0, 8)
	// Do not trim: a secret with a trailing space must still be matched
	// byte-for-byte if it ever reaches a log line.
	add := func(v string) {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	add(cfg.VKToken)
	add(cfg.TGBotToken)
	add(cfg.MaxToken)
	add(cfg.BarkKey)
	add(cfg.BarkDeviceKey)
	add(cfg.MaxDeviceID)
	// Full URLs may carry user:password@host even though callers log hosts only.
	add(cfg.BarkServerURL)
	add(cfg.MaxWSURL)
	add(cfg.TelegramProxy)
	if u, err := url.Parse(strings.TrimSpace(cfg.TelegramProxy)); err == nil && u.User != nil {
		add(u.User.Username())
		if p, ok := u.User.Password(); ok {
			add(p)
		}
	}
	// A safe snapshot of the effective settings, written into the log header.
	// Contains no tokens: only operation mode, hosts and flags.
	cfgNote := fmt.Sprintf("opcode=%d proxy=%s/%s max=%s bark=%s autostart=%t/%t/%t/%t ui=%t/%s",
		cfg.IncomingOpcode,
		logURLScheme(cfg.TelegramProxy), logURLHost(cfg.TelegramProxy),
		logURLHost(cfg.MaxWSURL), logURLHost(cfg.BarkServerURL),
		boolDefault(cfg.AutostartVK), boolDefault(cfg.AutostartMAX),
		boolDefault(cfg.AutostartTG), boolDefault(cfg.AutostartBark),
		cfg.uiEnabled(), cfg.UIListen)
	d.mu.Lock()
	d.secrets = secrets
	d.cfgNote = cfgNote
	d.mu.Unlock()
}

func (d *debugLogger) Enabled() bool { return d.enabled.Load() }

func (d *debugLogger) enable() error {
	rss := currentRSS()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.enabled.Load() {
		return nil
	}
	if d.path == "" {
		return fmt.Errorf("путь лога не задан")
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil {
		return err
	}
	f, err := openNoFollow(d.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0600)
	d.f = f
	if fi, statErr := f.Stat(); statErr == nil {
		d.size = fi.Size()
	} else {
		d.size = 0
	}
	d.pending = make([]byte, 0, debugBufferMax)
	d.lastFlush = time.Now()
	d.enabled.Store(true)
	if d.enabledPath != "" {
		// Persistence marker: the next process starts with logging already on,
		// so failures during startup end up in the file too.
		_ = writeFileNoFollow(d.enabledPath, []byte(d.sessionID+"\n"), 0600)
	}
	d.writeLineLocked(fmt.Sprintf("=== session=%s pid=%d ver=%s ===", d.sessionID, os.Getpid(), buildVersion()), rss)
	if d.cfgNote != "" {
		d.writeLineLocked("=== config "+d.cfgNote+" ===", rss)
	}
	d.writeLineLocked("=== отладочный лог включён ===", rss)
	return nil
}

func (d *debugLogger) disable() {
	rss := currentRSS()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled.Load() {
		return
	}
	d.writeLineLocked("=== отладочный лог выключен ===", rss)
	d.flushLocked()
	if d.f != nil {
		_ = d.f.Close()
		d.f = nil
	}
	d.enabled.Store(false)
	if d.enabledPath != "" {
		_ = os.Remove(d.enabledPath)
	}
}

// closeFile flushes and closes the file on process exit without removing the
// persistence marker, so the next process restores logging automatically.
func (d *debugLogger) closeFile() {
	rss := currentRSS()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled.Load() {
		return
	}
	d.writeLineLocked("=== процесс завершён ===", rss)
	d.flushLocked()
	if d.f != nil {
		_ = d.f.Close()
		d.f = nil
	}
	d.enabled.Store(false)
}

// debugf is cheap when logging is off: arguments are not formatted at all.
func debugf(format string, args ...any) {
	debugLog.logf(format, args...)
}

// safeErrText keeps the useful part of an error for the debug file. HTTP
// errors embed a URL with tokens and are reduced to their kind; everything
// else is sanitised and then passed through secret redaction in writeLineLocked.
func safeErrText(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return errKind(err)
	}
	return safeLogText(err.Error())
}

func (d *debugLogger) logf(format string, args ...any) {
	if !d.enabled.Load() {
		return
	}
	// Read RSS before taking the lock: /proc I/O must not hold up other writers.
	d.write(fmt.Sprintf(format, args...), currentRSS())
}

func (d *debugLogger) write(s string, rss int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled.Load() {
		return
	}
	d.writeLineLocked(s, rss)
}

// redact masks configured secrets outside the logger (stderr and API errors).
func (d *debugLogger) redact(s string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.redactLocked(s)
}

// setAuthSecrets masks phone/verification/SMS values for the current MAX auth
// request without growing the secrets slice on every attempt.
func (d *debugLogger) setAuthSecrets(v ...string) {
	secrets := make([]string, 0, len(v))
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			secrets = append(secrets, s)
		}
	}
	d.mu.Lock()
	d.authSecrets = secrets
	d.mu.Unlock()
}

// currentRSS reports this process's resident set size in kB at call time.
// Linux publishes the real RSS in /proc; on other systems fall back to the
// memory reserved by the Go runtime so the field is always present.
func currentRSS() int64 {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "VmRSS:") {
				continue
			}
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return kb
				}
			}
		}
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.Sys / 1024)
}

// formatLine adds the monotonic number, timestamp and RSS at the moment the
// line is appended. Must be called with d.mu held.
func (d *debugLogger) formatLine(s string, rss int64) string {
	d.seq++
	return fmt.Sprintf("#%06d %s rss=%dkB %s\n",
		d.seq, time.Now().Format("2006-01-02T15:04:05.000Z07:00"), rss, s)
}

// writeLineLocked must be called with d.mu held.
func (d *debugLogger) writeLineLocked(s string, rss int64) {
	line := d.formatLine(d.redactLocked(sanitizeDebugLine(s)), rss)
	if d.size+int64(len(line)) > d.maxBytes {
		// Flush before rotation, otherwise pending lines would land in the
		// new file after the old one was renamed to .1.
		d.flushLocked()
		d.rotateLocked()
	}
	if d.f == nil {
		d.dropped++
		return
	}
	d.pending = append(d.pending, line...)
	d.size += int64(len(line))
	d.lines++
	if len(d.pending) >= debugBufferMax {
		// Leave a trace so the buffer size can be tuned after a release.
		// The note is formatted directly into the same batch, then flushed,
		// so it cannot trigger another pressure flush recursively.
		d.forcedFlushes++
		note := d.formatLine(fmt.Sprintf("[DEBUG] буфер полон: %d байт, внеочередной сброс №%d", len(d.pending), d.forcedFlushes), rss)
		d.pending = append(d.pending, note...)
		d.size += int64(len(note))
		d.lines++
	}
	if len(d.pending) >= debugBufferMax || time.Since(d.lastFlush) >= debugFlushInterval {
		d.flushLocked()
	}
}

// flushLocked writes accumulated lines to flash in a single syscall. It is
// called by the timer check, on size pressure, before download, and on exit.
func (d *debugLogger) flushLocked() {
	if len(d.pending) == 0 {
		return
	}
	if d.f == nil {
		d.dropped += uint64(bytes.Count(d.pending, []byte{'\n'}))
	} else if n, err := d.f.Write(d.pending); err != nil || n != len(d.pending) {
		d.dropped += uint64(bytes.Count(d.pending, []byte{'\n'}))
	}
	d.pending = d.pending[:0]
	d.lastFlush = time.Now()
}

// rotateLocked keeps at most two files: debug.log.1 holds the previous run.
func (d *debugLogger) rotateLocked() {
	// If a new file cannot be opened (full flash, bad permissions, symlink),
	// reset the size. Otherwise every following line would try to rotate again
	// and flood the system with remove/rename/open syscalls.
	defer func() {
		if d.f == nil {
			d.size = 0
		}
	}()
	if d.f != nil {
		_ = d.f.Close()
		d.f = nil
	}
	if d.path == "" {
		return
	}
	if d.prevPath != "" {
		_ = os.Remove(d.prevPath) // Windows cannot rename over an existing file
		if err := os.Rename(d.path, d.prevPath); err != nil {
			// Fallback: keep the same file but drop its contents.
			if f, ferr := openNoFollow(d.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); ferr == nil {
				_ = f.Chmod(0600)
				d.f = f
				d.size = 0
			}
			return
		}
	}
	f, err := openNoFollow(d.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		_ = f.Chmod(0600)
		d.f = f
		d.size = 0
	}
}

func (d *debugLogger) redactLocked(s string) string {
	for _, secret := range d.secrets {
		s = strings.ReplaceAll(s, secret, "***")
	}
	for _, secret := range d.authSecrets {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

// maskID keeps only the last four characters of an identifier, enough to
// correlate events without putting a full peer/chat ID into the log.
func maskID(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return "…" + id[len(id)-4:]
}

func sanitizeDebugLine(s string) string {
	n := 0
	var b strings.Builder
	// A caller may pass a large server-controlled string; never preallocate
	// more than the line limit we are about to enforce.
	b.Grow(minInt(len(s), debugLogLineMax+16))
	for _, r := range s {
		if n == debugLogLineMax-1 {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

func (d *debugLogger) status() debugStatus {
	d.mu.Lock()
	st := debugStatus{
		Enabled:       d.enabled.Load(),
		Persisted:     d.enabledPath != "" && fileExists(d.enabledPath),
		Session:       d.sessionID,
		Path:          d.path,
		MaxBytes:      d.maxBytes,
		Lines:         d.lines,
		Dropped:       d.dropped,
		Buffered:      int64(len(d.pending)),
		ForcedFlushes: d.forcedFlushes,
	}
	prev := d.prevPath
	d.mu.Unlock()
	if st.Path != "" {
		if fi, err := os.Stat(st.Path); err == nil {
			st.Bytes = fi.Size()
		}
	}
	if prev != "" {
		if fi, err := os.Stat(prev); err == nil {
			st.PrevBytes = fi.Size()
		}
	}
	return st
}

// serve writes the previous file followed by the current one, so a single
// download contains the whole retained history in chronological order.
func (d *debugLogger) serve(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="Msg-forwarder-Go-debug.log"`)

	d.mu.Lock()
	d.flushLocked() // include the last ten minutes in the download
	prev, cur := d.prevPath, d.path
	d.mu.Unlock()

	files := make([]string, 0, 2)
	for _, p := range []string{prev, cur} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		_, _ = io.WriteString(w, "Отладочный лог ещё не создан: включите его кнопкой «Включить».\n")
		return
	}
	buf := make([]byte, 32<<10)
	for i, p := range files {
		if i > 0 {
			_, _ = io.WriteString(w, "\n=== предыдущий файл (ротация) ===\n\n")
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_, _ = io.CopyBuffer(w, f, buf)
		_ = f.Close()
	}
}
