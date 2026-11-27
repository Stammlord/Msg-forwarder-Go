package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	barkDefaultServer = "https://api.day.app"
	barkKeyLength     = 16
)

type barkMessage struct {
	app     string
	contact string
	text    string
	trace   uint64
}

type bark struct {
	mu           sync.Mutex
	cfg          Config
	client       *http.Client
	enabled      atomic.Bool
	queue        chan barkMessage
	workerMu     sync.Mutex
	workerActive bool
	stopCh       chan struct{}
}

func normalizeBarkServerURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = barkDefaultServer
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("неверный bark_server_url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("bark_server_url: нужен http или https")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("bark_server_url: нет хоста")
	}
	return strings.TrimRight(s, "/"), nil
}

func validateBarkConfig(c Config) error {
	if _, err := normalizeBarkServerURL(c.BarkServerURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.BarkDeviceKey) == "" {
		return fmt.Errorf("не задан bark_device_key")
	}
	if c.BarkEncrypt && len(c.BarkKey) != barkKeyLength {
		return fmt.Errorf("bark_key должен быть ровно %d символов", barkKeyLength)
	}
	switch c.BarkLevel {
	case "", "active", "timeSensitive", "passive", "critical":
	default:
		return fmt.Errorf("неверный bark_level")
	}
	return nil
}

func barkPingTarget(c Config) (string, error) {
	server, err := normalizeBarkServerURL(c.BarkServerURL)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("неверный bark_server_url")
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		} else {
			port = "443"
		}
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("bark_server_url: нет хоста")
	}
	return net.JoinHostPort(host, port), nil
}

func newBark(cfg Config, hc *http.Client) *bark {
	b := &bark{queue: make(chan barkMessage, 32)}
	b.update(cfg, hc)
	return b
}

func (b *bark) update(cfg Config, hc *http.Client) {
	if b == nil {
		return
	}
	b.mu.Lock()
	old := b.client
	b.cfg = cfg
	b.client = hc
	b.mu.Unlock()
	if old != nil && old != hc {
		closeIdleClient(old)
	}
}

func (b *bark) snapshot() (Config, *http.Client) {
	if b == nil {
		return Config{}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg, b.client
}

func (b *bark) setEnabled(v bool) {
	if b != nil {
		b.enabled.Store(v)
	}
}

func (b *bark) isEnabled() bool {
	return b != nil && b.enabled.Load()
}

func (b *bark) configProblem() string {
	cfg, hc := b.snapshot()
	if hc == nil {
		return "bark client не инициализирован"
	}
	if err := validateBarkConfig(cfg); err != nil {
		return err.Error()
	}
	return ""
}

func (b *bark) configured() bool {
	return b.configProblem() == ""
}

func (b *bark) worker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m := <-b.queue:
			if !b.isEnabled() {
				continue
			}
			if err := b.sendText(m.app, m.contact, m.text); err != nil {
				logf("[BARK] send failed: %s", err)
				debugf("[BARK] worker send failed trace=%d err=%s", m.trace, safeErrText(err))
			} else {
				debugf("[BARK] worker send ok trace=%d source=%s", m.trace, m.app)
			}
		}
	}
}

// ensureWorker checks "enabled" and starts the worker under one lock, so a
// concurrent stopWorker cannot be followed by a stale worker being resurrected.
func (b *bark) ensureWorker() bool {
	if b == nil {
		return false
	}
	b.workerMu.Lock()
	defer b.workerMu.Unlock()
	if !b.isEnabled() {
		return false
	}
	if b.workerActive {
		return true
	}
	b.stopCh = make(chan struct{})
	b.workerActive = true
	go b.worker(b.stopCh)
	return true
}

func (b *bark) stopWorker() {
	if b == nil {
		return
	}
	b.workerMu.Lock()
	defer b.workerMu.Unlock()
	if !b.workerActive {
		return
	}
	close(b.stopCh)
	b.workerActive = false
	b.stopCh = nil
	for {
		select {
		case <-b.queue:
		default:
			return
		}
	}
}

func (b *bark) send(app, contact, text string, trace uint64) {
	if !b.isEnabled() {
		logf("[BARK] выключен, сообщение пропущено")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "[вложение]"
	}
	if !b.ensureWorker() {
		logf("[BARK] выключен, сообщение пропущено")
		return
	}
	select {
	case b.queue <- barkMessage{app: truncateText(app, 64), contact: truncateText(contact, 96), text: truncateText(text, 2000), trace: trace}:
		debugf("[BARK] queued trace=%d source=%s textLen=%d depth=%d", trace, app, len(text), len(b.queue))
	default:
		logf("[BARK] очередь переполнена, сообщение пропущено")
		debugf("[BARK] queue full, dropped trace=%d source=%s textLen=%d", trace, app, len(text))
	}
}

func barkPayload(cfg Config, source, subtitle, body string) map[string]any {
	p := map[string]any{"body": body}
	title := strings.TrimSpace(cfg.BarkTitle)
	if title == "" {
		title = "Msg-forwarder-Go"
	}
	if source != "" {
		if title == "" {
			title = source
		} else {
			title = title + " • " + source
		}
	}
	p["title"] = title
	if subtitle = strings.TrimSpace(subtitle); subtitle != "" {
		p["subtitle"] = subtitle
	}
	if v := strings.TrimSpace(cfg.BarkSound); v != "" {
		p["sound"] = v
	}
	if v := strings.TrimSpace(cfg.BarkGroup); v != "" {
		p["group"] = v
	}
	if v := strings.TrimSpace(cfg.BarkLevel); v != "" {
		p["level"] = v
	}
	if v := strings.TrimSpace(cfg.BarkIcon); v != "" {
		p["icon"] = v
	}
	if v := strings.TrimSpace(cfg.BarkClickURL); v != "" {
		p["url"] = v
	}
	return p
}

func randomBarkIV() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, aes.BlockSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("не удалось создать IV: %s", errKind(err))
	}
	iv := make([]byte, aes.BlockSize)
	for i, v := range raw {
		iv[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(iv), nil
}

func aes128CBCEncrypt(key, iv []byte, plain []byte) ([]byte, error) {
	if len(key) != barkKeyLength || len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("неверный размер ключа или IV")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("bark cipher: %s", err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	out := make([]byte, len(plain)+pad)
	copy(out, plain)
	for i := len(plain); i < len(out); i++ {
		out[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, out)
	return out, nil
}

func (b *bark) sendText(source, subtitle, body string) error {
	if b == nil {
		return fmt.Errorf("bark client не инициализирован")
	}
	cfg, hc := b.snapshot()
	if err := validateBarkConfig(cfg); err != nil {
		return err
	}
	if hc == nil {
		return fmt.Errorf("bark client не инициализирован")
	}

	payload := barkPayload(cfg, source, subtitle, truncateText(strings.TrimSpace(body), 2000))
	out := map[string]any{"device_key": cfg.BarkDeviceKey}
	if cfg.BarkEncrypt {
		iv, err := randomBarkIV()
		if err != nil {
			return err
		}
		plain, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("bark json error")
		}
		ciphertext, err := aes128CBCEncrypt([]byte(cfg.BarkKey), []byte(iv), plain)
		if err != nil {
			return err
		}
		out["body"] = "Зашифрованное сообщение"
		out["ciphertext"] = base64.StdEncoding.EncodeToString(ciphertext)
		out["iv"] = iv
	} else {
		for k, v := range payload {
			out[k] = v
		}
	}

	server, err := normalizeBarkServerURL(cfg.BarkServerURL)
	if err != nil {
		return err
	}
	reqBody, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("bark json error")
	}
	req, err := http.NewRequest(http.MethodPost, server+"/push", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("bark request error")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	// Only the host goes to the log: a custom server URL may contain
	// user:password credentials in the userinfo part.
	barkHost := ""
	if u, uerr := url.Parse(server); uerr == nil {
		barkHost = u.Hostname()
	}
	debugf("[BARK] push start host=%s encrypted=%t bodyLen=%d", barkHost, cfg.BarkEncrypt, len(reqBody))
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		debugf("[BARK] push fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return fmt.Errorf("%s", errKind(err))
	}
	defer drainResponse(resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		debugf("[BARK] read fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return fmt.Errorf("%s", errKind(err))
	}
	err = parseBarkResponse(resp.StatusCode, raw)
	debugf("[BARK] push done status=%d dur=%s err=%s", resp.StatusCode, time.Since(start), safeErrText(err))
	return err
}

func parseBarkResponse(status int, raw []byte) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("bark http %d", status)
	}
	if len(raw) == 0 {
		return nil
	}
	var r struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	if r.Code != 0 && r.Code != 200 {
		return fmt.Errorf("bark api error %d", r.Code)
	}
	return nil
}

func (b *bark) test() error {
	return b.sendText("", "", "Проверка доставки Msg-forwarder-Go")
}
