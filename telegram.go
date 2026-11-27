package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type tgMessage struct {
	app     string
	contact string
	text    string
	trace   uint64
}

type telegram struct {
	mu           sync.Mutex
	token        string
	chatID       string
	client       *http.Client
	enabled      atomic.Bool
	queue        chan tgMessage
	workerMu     sync.Mutex
	workerActive bool
	stopCh       chan struct{}
}

func newTelegram(cfg Config, hc *http.Client) *telegram {
	t := &telegram{queue: make(chan tgMessage, 32)}
	t.update(cfg, hc)
	return t
}

// worker starts lazily: test/validation instances never allocate a goroutine.
func (t *telegram) worker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m := <-t.queue:
			if !t.isEnabled() {
				continue // service was switched off while the message was queued
			}
			msg := fmt.Sprintf("[%s] %s:\n%s", m.app, m.contact, m.text)
			start := time.Now()
			if err := t.sendText(msg); err != nil {
				logf("[TG] send failed: %s", err)
				debugf("[TG] worker send failed trace=%d dur=%s err=%s", m.trace, time.Since(start), safeErrText(err))
			} else {
				debugf("[TG] worker send ok trace=%d source=%s textLen=%d dur=%s", m.trace, m.app, len(m.text), time.Since(start))
			}
		}
	}
}

// ensureWorker checks "enabled" and starts the worker under one lock, so a
// concurrent stopWorker cannot be followed by a stale worker being resurrected.
func (t *telegram) ensureWorker() bool {
	t.workerMu.Lock()
	defer t.workerMu.Unlock()
	if !t.isEnabled() {
		return false
	}
	if t.workerActive {
		return true
	}
	t.stopCh = make(chan struct{})
	t.workerActive = true
	go t.worker(t.stopCh)
	return true
}

// stopWorker releases the goroutine and drops messages queued while the
// service was enabled. A later enable call starts a fresh worker.
func (t *telegram) stopWorker() {
	t.workerMu.Lock()
	defer t.workerMu.Unlock()
	if !t.workerActive {
		return
	}
	close(t.stopCh)
	t.workerActive = false
	t.stopCh = nil
	for {
		select {
		case <-t.queue:
		default:
			return
		}
	}
}

func (t *telegram) update(cfg Config, hc *http.Client) {
	t.mu.Lock()
	old := t.client
	t.token = cfg.TGBotToken
	t.chatID = string(cfg.TGChatID)
	t.client = hc
	t.mu.Unlock()
	if old != nil && old != hc {
		closeIdleClient(old)
	}
}

func (t *telegram) setEnabled(v bool) { t.enabled.Store(v) }
func (t *telegram) isEnabled() bool   { return t.enabled.Load() }

func (t *telegram) snapshot() (string, string, *http.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.token, t.chatID, t.client
}

func (t *telegram) configured() bool {
	token, chat, _ := t.snapshot()
	return token != "" && chat != ""
}

func (t *telegram) send(app, contact, text string, trace uint64) {
	if !t.isEnabled() {
		logf("[TG] выключен, сообщение пропущено")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "[вложение]"
	}
	if !t.ensureWorker() {
		logf("[TG] выключен, сообщение пропущено")
		return
	}
	// Bound queue memory: a full queue drops new messages instead of growing
	// the heap. Telegram itself is rate-limited well below this burst size.
	select {
	case t.queue <- tgMessage{app: app, contact: contact, text: truncateText(text, 2000), trace: trace}:
		debugf("[TG] queued trace=%d source=%s textLen=%d depth=%d", trace, app, len(text), len(t.queue))
	default:
		logf("[TG] очередь переполнена, сообщение пропущено")
		debugf("[TG] queue full, dropped trace=%d source=%s textLen=%d", trace, app, len(text))
	}
}

func (t *telegram) sendText(text string) error {
	token, chatID, hc := t.snapshot()
	if token == "" || chatID == "" {
		return fmt.Errorf("не заданы tg_bot_token или tg_chat_id")
	}
	if hc == nil {
		return fmt.Errorf("telegram client не инициализирован")
	}
	form := url.Values{
		"chat_id":                  {chatID},
		"text":                     {truncateTelegram(text, 4000)},
		"disable_web_page_preview": {"true"},
	}
	// The token is in the path. Never log the raw error from Do(): it contains
	// the full URL, including the bot token.
	endpoint := "https://api.telegram.org/bot" + token + "/sendMessage"
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		debugf("[TG] sendMessage request build failed")
		return fmt.Errorf("request error")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	debugf("[TG] sendMessage start chat#%s textLen=%d", maskID(chatID), len(text))
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		debugf("[TG] sendMessage fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return fmt.Errorf("%s", errKind(err))
	}
	defer drainResponse(resp)
	err = parseTelegramResponse(resp)
	debugf("[TG] sendMessage done status=%d dur=%s err=%s", resp.StatusCode, time.Since(start), safeErrText(err))
	return err
}

func (t *telegram) test() error {
	token, _, hc := t.snapshot()
	if token == "" {
		return fmt.Errorf("не задан tg_bot_token")
	}
	if hc == nil {
		return fmt.Errorf("telegram client не инициализирован")
	}
	endpoint := "https://api.telegram.org/bot" + token + "/getMe"
	debugf("[TG] getMe start")
	start := time.Now()
	resp, err := hc.Get(endpoint)
	if err != nil {
		debugf("[TG] getMe fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return fmt.Errorf("%s", errKind(err))
	}
	defer drainResponse(resp)
	err = parseTelegramResponse(resp)
	debugf("[TG] getMe done status=%d dur=%s err=%s", resp.StatusCode, time.Since(start), safeErrText(err))
	return err
}

// lastChatID helps the user fill tg_chat_id: send any message to the bot and
// press "Узнать chat_id" in the UI.
func (t *telegram) lastChatID() (string, error) {
	token, _, hc := t.snapshot()
	if token == "" {
		return "", fmt.Errorf("не задан tg_bot_token")
	}
	if hc == nil {
		return "", fmt.Errorf("telegram client не инициализирован")
	}
	endpoint := "https://api.telegram.org/bot" + token + "/getUpdates?limit=1&offset=-1"
	debugf("[TG] getUpdates start")
	start := time.Now()
	resp, err := hc.Get(endpoint)
	if err != nil {
		debugf("[TG] getUpdates fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return "", fmt.Errorf("%s", errKind(err))
	}
	defer drainResponse(resp)
	debugf("[TG] getUpdates status=%d dur=%s", resp.StatusCode, time.Since(start))

	var r struct {
		OK     bool `json:"ok"`
		Result []struct {
			Message struct {
				Chat struct {
					ID json.Number `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
		ErrorCode int `json:"error_code"`
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("%s", errKind(err))
	}
	if json.Unmarshal(body, &r) != nil || !r.OK {
		return "", fmt.Errorf("api error %d (status %d)", r.ErrorCode, resp.StatusCode)
	}
	if len(r.Result) == 0 || r.Result[0].Message.Chat.ID == "" {
		return "", fmt.Errorf("нет сообщений: напишите боту и повторите")
	}
	return r.Result[0].Message.Chat.ID.String(), nil
}

func parseTelegramResponse(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return fmt.Errorf("%s", errKind(err))
	}
	var r struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if json.Unmarshal(body, &r) != nil || !r.OK {
		debugf("[TG] api error code=%d status=%d bodyLen=%d", r.ErrorCode, resp.StatusCode, len(body))
		return fmt.Errorf("api error %d (status %d)", r.ErrorCode, resp.StatusCode)
	}
	debugf("[TG] api ok status=%d bodyLen=%d", resp.StatusCode, len(body))
	return nil
}
