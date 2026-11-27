package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var logger = log.New(os.Stderr, "", log.LstdFlags)

// checkRedirect blocks cross-host redirects. Host validation (VK long poll,
// custom API hosts) is done before the request; without this, a trusted host
// could answer 302 with an internal address and turn the client into an SSRF
// primitive against the router's LAN.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("слишком много редиректов")
	}
	// Compare host *including port*: a redirect from an external host to
	// 127.0.0.1:8080 is the SSRF case we must block.
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errors.New("редирект на другой хост запрещён")
	}
	return nil
}

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	rss := currentRSS()
	// stderr goes to syslog on OpenWrt and has no redaction layer of its own.
	// Mask configured secrets here as well: server-controlled error strings
	// must not leak tokens into syslog.
	if debugLog.Enabled() {
		msg = debugLog.redact(msg)
	}
	logger.Printf("rss=%dkB %s", rss, msg)
	// Every regular diagnostic is mirrored into the debug file when the
	// operator enables it from the panel. It stays a no-op otherwise.
	debugLog.write(msg, rss)
}

// errKind returns a short, non-sensitive description of an error.
// Never pass errors from requests that contain tokens to the log directly:
// net/http url.Error includes the full URL (with access_token or bot token).
func errKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "closed"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "timeout"
		}
		return "network error"
	}
	return fmt.Sprintf("%T", err)
}

// writeAll handles short writes from net.Conn without dropping bytes.
func writeAll(conn net.Conn, b []byte) error {
	for len(b) > 0 {
		n, err := conn.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrUnexpectedEOF
		}
		b = b[n:]
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

// backoffJitter adds up to 25% random delay so that parallel or repeatedly
// reconnecting clients do not follow a perfectly periodic pattern.
func backoffJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration(rand.Int63n(int64(d/4)+1))
}

func truncateText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i] + "…" // byte index of the first rune left out
		}
		n++
	}
	return s
}

// safeLogText подготавливает текст серверной ошибки для журнала: обрезает его
// и заменяет переводы строк и непечатаемые символы.
func safeLogText(s string) string {
	s = truncateText(s, 160)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// drainResponse consumes the rest of a finished response before closing it,
// so the connection can be reused. The read is bounded: a hostile server must
// not be able to keep the worker busy with an endless body.
func drainResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// truncateTelegram limits text by UTF-16 code units, which is what Telegram
// counts (emoji take two units, unlike runes).
func truncateTelegram(s string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		n := 1
		if r >= 0x10000 {
			n = 2 // astral code point = surrogate pair in UTF-16
		}
		if units+n > maxUnits {
			return s[:i] + "…"
		}
		units += n
	}
	return s
}

type chatInfo struct {
	title string
	typ   string
}

// cacheText bounds a single cache entry. A 512 KiB inbound frame must not be
// able to pin 512 KiB in the name/seen cache for a long time.
func cacheText(s string) string {
	if len(s) <= 256 {
		return s
	}
	b := s[:256]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

// idCache is a tiny in-memory bounded cache. Nothing is ever written to disk.
type idCache struct {
	mu        sync.Mutex
	names     map[int64]string
	nameOrder []int64
	chats     map[int64]chatInfo
	chatOrder []int64
	seen      map[string]struct{}
	seenOrder []string
	max       int
}

func newIDCache(max int) *idCache {
	if max < 16 {
		max = 16
	}
	return &idCache{
		names: make(map[int64]string, max),
		chats: make(map[int64]chatInfo, max),
		seen:  make(map[string]struct{}, max),
		max:   max,
	}
}

func (c *idCache) setName(id int64, name string) {
	if name == "" {
		return
	}
	name = cacheText(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.names[id]; !ok && len(c.names) >= c.max {
		old := c.nameOrder[0]
		c.nameOrder[0] = 0 // drop the reference before sliding the window
		c.nameOrder = c.nameOrder[1:]
		delete(c.names, old)
	}
	if _, ok := c.names[id]; !ok {
		c.nameOrder = append(c.nameOrder, id)
	}
	c.names[id] = name
}

func (c *idCache) getName(id int64) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.names[id]
	return v, ok
}

func (c *idCache) setChat(id int64, info chatInfo) {
	info.title = cacheText(info.title)
	info.typ = cacheText(info.typ)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.chats[id]; !ok && len(c.chats) >= c.max {
		old := c.chatOrder[0]
		c.chatOrder = c.chatOrder[1:]
		delete(c.chats, old)
	}
	if _, ok := c.chats[id]; !ok {
		c.chatOrder = append(c.chatOrder, id)
	}
	c.chats[id] = info
}

func (c *idCache) getChat(id int64) (chatInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.chats[id]
	return v, ok
}

// markSeen returns false if the key was already present. It is used for
// in-memory deduplication only (no state file, no history on disk).
func (c *idCache) markSeen(key string) bool {
	key = cacheText(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return false
	}
	if len(c.seen) >= c.max {
		old := c.seenOrder[0]
		c.seenOrder[0] = "" // drop the reference before sliding the window
		c.seenOrder = c.seenOrder[1:]
		delete(c.seen, old)
	}
	c.seenOrder = append(c.seenOrder, key)
	c.seen[key] = struct{}{}
	return true
}
