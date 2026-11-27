package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// svcState is what both UIs show as the coloured circle.
type svcState struct {
	State  string `json:"state"` // on | off | error
	Detail string `json:"detail"`
}

var serviceNames = []string{"vk", "max", "tg", "bark"}

func validService(s string) bool {
	for _, n := range serviceNames {
		if s == n {
			return true
		}
	}
	return false
}

func initStates(a *app) {
	a.states = make(map[string]*svcState, len(serviceNames))
	a.cancels = make(map[string]context.CancelFunc, 2)
	for _, n := range serviceNames {
		a.states[n] = &svcState{State: "off", Detail: "не запущено"}
	}
}

// safeDetail never lets a token into the UI: net/http errors embed the full
// URL (access_token=... or /bot<token>/) and must be reduced to an error kind.
func safeDetail(err error) string {
	if err == nil {
		return ""
	}
	// fmt.Errorf("proxy: %w", err) wraps *url.Error, so a plain type
	// assertion would miss it and leak the full proxy URL (with password).
	var ue *url.Error
	if errors.As(err, &ue) {
		return errKind(err)
	}
	// Other errors may contain a server-controlled string (for example the
	// MAX "error" field). Mask every configured secret before showing it in
	// the panel or returning it from /test and /status.
	return truncateText(debugLog.redact(err.Error()), 160)
}

func (a *app) outputEnabled(name string) bool {
	switch name {
	case "tg":
		return a.tg != nil && a.tg.isEnabled()
	case "bark":
		return a.bark != nil && a.bark.isEnabled()
	default:
		return false
	}
}

// stateErr records an error only if the service is still supposed to run.
func (a *app) stateErr(name string, err error) {
	if err == nil {
		return
	}
	detail := safeDetail(err)
	debugf("[ERR] %s: %s", name, detail)
	a.mu.Lock()
	_, running := a.cancels[name]
	pollService := name != "tg" && name != "bark" && running
	if pollService || a.outputEnabled(name) {
		a.states[name] = &svcState{State: "error", Detail: detail}
	}
	a.mu.Unlock()
}

func (a *app) setState(name, state, detail string) {
	debugf("[STATE] %s -> %s (%s)", name, state, detail)
	a.mu.Lock()
	a.states[name] = &svcState{State: state, Detail: detail}
	a.mu.Unlock()
}

func (a *app) configSnapshot() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *app) setUIServer(srv *http.Server) {
	a.mu.Lock()
	a.uiServer = srv
	a.mu.Unlock()
}

func (a *app) requestRestart() {
	a.restartOnce.Do(func() { close(a.restartCh) })
}

// setTestOK must not paint a green circle for a service that is actually off.
func (a *app) setTestOK(name string) {
	a.mu.Lock()
	_, running := a.cancels[name]
	enabled := name != "tg" && name != "bark" && running || a.outputEnabled(name)
	if enabled {
		a.states[name] = &svcState{State: "on", Detail: "проверка пройдена"}
	} else {
		a.states[name] = &svcState{State: "off", Detail: "проверка пройдена (выключено)"}
	}
	a.mu.Unlock()
}

func (a *app) snapshotStates() map[string]svcState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]svcState, len(a.states))
	for k, v := range a.states {
		out[k] = *v
	}
	return out
}

func (a *app) start(name string) string {
	if !validService(name) {
		return "неизвестная служба"
	}
	cfg := a.configSnapshot()

	switch name {
	case "vk":
		if cfg.VKToken == "" {
			a.setState("vk", "off", "не настроено: нет vk_token")
			return "FAIL: нет vk_token"
		}
		ctx, cancel := context.WithCancel(context.Background())
		// Check and insert under one lock: two concurrent clicks must not
		// start two long-poll goroutines.
		a.mu.Lock()
		if _, already := a.cancels["vk"]; already {
			a.mu.Unlock()
			cancel()
			return "VK уже запущен"
		}
		a.cancels["vk"] = cancel
		a.mu.Unlock()
		a.setState("vk", "on", "подключение…")
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runVK(ctx)
		}()
		return "VK запущен"

	case "max":
		if cfg.MaxToken == "" || cfg.MaxDeviceID == "" {
			a.setState("max", "off", "не настроено: нет max_token/max_device_id")
			return "FAIL: нет max_token или max_device_id"
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.mu.Lock()
		if _, already := a.cancels["max"]; already {
			a.mu.Unlock()
			cancel()
			return "MAX уже запущен"
		}
		a.cancels["max"] = cancel
		a.mu.Unlock()
		a.setState("max", "on", "подключение…")
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runMAX(ctx)
		}()
		return "MAX запущен"

	case "tg":
		// Serialise with applyConfig so a click cannot restore an old token or
		// proxy right after the form was saved.
		a.cfgMu.Lock()
		defer a.cfgMu.Unlock()
		cfg = a.configSnapshot()
		if !a.tg.configured() {
			a.setState("tg", "off", "не настроено: нет tg_bot_token/tg_chat_id")
			return "FAIL: нет tg_bot_token или tg_chat_id"
		}
		hc, err := newTelegramHTTPClient(cfg)
		if err != nil {
			a.tg.setEnabled(false)
			a.tg.stopWorker()
			a.setState("tg", "error", "proxy: "+safeDetail(err))
			return fmt.Sprintf("FAIL: %s", safeDetail(err))
		}
		a.tg.update(cfg, hc)
		a.tg.setEnabled(true)
		a.setState("tg", "on", "готов к отправке")
		return "Telegram включён"

	case "bark":
		a.cfgMu.Lock()
		defer a.cfgMu.Unlock()
		cfg = a.configSnapshot()
		if a.bark == nil {
			a.setState("bark", "off", "не инициализировано")
			return "FAIL: bark не инициализирован"
		}
		if problem := a.bark.configProblem(); problem != "" {
			a.bark.setEnabled(false)
			a.bark.stopWorker()
			a.setState("bark", "off", "не настроено: "+problem)
			return "FAIL: " + problem
		}
		a.bark.update(cfg, a.client)
		a.bark.setEnabled(true)
		a.setState("bark", "on", "готов к отправке")
		return "Bark включён"
	}
	return ""
}

func (a *app) stop(name string) string {
	if !validService(name) {
		return "неизвестная служба"
	}
	a.mu.Lock()
	cancel := a.cancels[name]
	delete(a.cancels, name)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if name == "tg" {
		a.tg.setEnabled(false)
		a.tg.stopWorker()
	}
	if name == "bark" {
		a.bark.setEnabled(false)
		a.bark.stopWorker()
	}
	a.setState(name, "off", "выключено вручную")
	return name + " выключен"
}

func (a *app) restart(name string) string {
	a.stop(name)
	return a.start(name)
}

// applyConfig makes a saved config visible to the runtime immediately, so the
// test/send/start buttons do not keep using old tokens until the whole process
// is restarted. Active connections keep running until their own restart.
func (a *app) applyConfig(c Config) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()

	debugLog.setSecrets(c)
	hc, err := newTelegramHTTPClient(c)
	a.mu.Lock()
	a.cfg = c
	a.mu.Unlock()

	if err != nil {
		// Keep running: the user can fix the proxy in the UI. Telegram turns
		// red and stays disabled, while VK/MAX/Bark keep working.
		logf("telegram proxy error: %s", safeDetail(err))
		a.tg.update(c, nil)
		a.tg.setEnabled(false)
		a.tg.stopWorker()
		a.setState("tg", "error", "proxy: "+safeDetail(err))
		a.applyBarkConfig(c)
		return
	}
	a.tg.update(c, hc)
	if !a.tg.configured() {
		a.tg.setEnabled(false)
		a.tg.stopWorker()
		a.setState("tg", "off", "нет tg_bot_token или tg_chat_id")
	}
	a.applyBarkConfig(c)
}

func (a *app) applyBarkConfig(c Config) {
	if a.bark == nil {
		return
	}
	a.bark.update(c, a.client)
	if problem := a.bark.configProblem(); problem != "" {
		a.bark.setEnabled(false)
		a.bark.stopWorker()
		a.setState("bark", "off", "не настроено: "+problem)
	}
}

func (a *app) setAutostart(name string, v bool) error {
	if !validService(name) {
		return fmt.Errorf("неизвестная служба %q", name)
	}
	// The whole read-modify-write runs under the config lock, so a parallel
	// save from the UI cannot be lost.
	c, err := modifyConfig(a.cfgPath, func(c *Config) error {
		if _, statErr := os.Stat(a.cfgPath); os.IsNotExist(statErr) {
			*c = a.configSnapshot()
		}
		applyDefaults(c)
		switch name {
		case "vk":
			c.AutostartVK = &v
		case "max":
			c.AutostartMAX = &v
		case "tg":
			c.AutostartTG = &v
		case "bark":
			c.AutostartBark = &v
		}
		return nil
	})
	if err != nil {
		return err
	}
	debugf("[CTL] autostart %s=%t", name, v)
	a.applyConfig(c)
	return nil
}

func (a *app) shutdown() {
	a.shutdownOnce.Do(func() {
		a.mu.Lock()
		srv := a.uiServer
		a.uiServer = nil
		cancels := make([]context.CancelFunc, 0, len(a.cancels))
		for _, c := range a.cancels {
			cancels = append(cancels, c)
		}
		a.cancels = make(map[string]context.CancelFunc, 2)
		a.mu.Unlock()

		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := srv.Shutdown(ctx); err != nil {
				_ = srv.Close()
			}
			cancel()
		}
		for _, c := range cancels {
			c()
		}

		// Buffered so this goroutine can still exit after the 5 s timeout.
		done := make(chan struct{}, 1)
		go func() {
			a.wg.Wait()
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}

		a.tg.setEnabled(false)
		a.tg.stopWorker()
		a.bark.setEnabled(false)
		a.bark.stopWorker()
	})
}

// logControlResult records only the outcome, never the response body: the
// "chatid" action returns a full chat ID, and ping results carry host details.
func logControlResult(service, action, out string) {
	debugf("[CTL] service=%s action=%s failed=%t resultLen=%d", service, action, strings.HasPrefix(out, "FAIL"), len(out))
}

func textResponse(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}

func (a *app) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.snapshotStates())
}

func (a *app) handleDebugStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, debugLog.status())
}

func (a *app) handleDebugSwitch(w http.ResponseWriter, r *http.Request, on bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if on {
		if err := debugLog.enable(); err != nil {
			textResponse(w, "FAIL: "+safeDetail(err))
			return
		}
		debugf("[DEBUG] лог включён из панели")
		textResponse(w, "OK: отладочный лог включён")
		return
	}
	debugf("[DEBUG] лог выключен из панели")
	debugLog.disable()
	textResponse(w, "OK: отладочный лог выключен, файл сохранён")
}

func (a *app) handleDebugLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	debugLog.serve(w)
}

func (a *app) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := strings.ToLower(strings.TrimSpace(r.FormValue("service")))
	action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
	if !validService(service) {
		textResponse(w, "FAIL: неизвестная служба")
		return
	}

	var out string
	switch action {
	case "on":
		out = a.start(service)
	case "off":
		out = a.stop(service)
	case "restart":
		out = a.restart(service)
	case "autostart":
		v, err := strconv.ParseBool(r.FormValue("value"))
		if err != nil {
			textResponse(w, "FAIL: неверное значение autostart")
			return
		}
		if err := a.setAutostart(service, v); err != nil {
			out = fmt.Sprintf("FAIL: %s", safeDetail(err))
		} else {
			out = fmt.Sprintf("autostart %s = %t (сохранено в конфиг)", service, v)
		}
	case "test":
		if err := testService(a.configSnapshot(), service); err != nil {
			a.stateErr(service, err)
			out = fmt.Sprintf("FAIL: %s", safeDetail(err))
		} else {
			a.setTestOK(service)
			out = fmt.Sprintf("OK: %s", service)
		}
	case "ping":
		msg, err := pingService(a.configSnapshot(), service)
		if err != nil {
			a.stateErr(service, err)
			out = fmt.Sprintf("FAIL: %s", safeDetail(err))
		} else {
			out = msg
		}
	case "chatid":
		if service != "tg" {
			out = "chat_id доступен только для Telegram"
			break
		}
		id, err := a.tg.lastChatID()
		if err != nil {
			out = fmt.Sprintf("FAIL: %s", safeDetail(err))
		} else {
			out = "chat_id: " + id
		}
	default:
		out = "FAIL: неизвестное действие"
	}

	logControlResult(service, action, out)
	textResponse(w, out)
}

func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := strings.ToLower(strings.TrimSpace(r.FormValue("service")))
	if service == "" {
		service = "tg"
	}
	// Manual test send is explicit: it works even when automatic forwarding is
	// off, as long as the destination is configured.
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		text = "проверка доставки Msg-forwarder-Go"
	}
	text = truncateText(text, 500)
	debugf("[SEND] service=%s textLen=%d", service, len(text))

	switch service {
	case "tg":
		if !a.tg.configured() {
			textResponse(w, "FAIL: не заданы tg_bot_token или tg_chat_id")
			return
		}
		if err := a.tg.sendText("[тест] " + text); err != nil {
			a.stateErr("tg", fmt.Errorf("%s", err))
			textResponse(w, fmt.Sprintf("FAIL: %s", safeDetail(err)))
			return
		}
		a.setState("tg", "on", "тестовое сообщение отправлено")
		textResponse(w, "OK: тестовое сообщение отправлено в Telegram")

	case "bark":
		if a.bark == nil || !a.bark.configured() {
			textResponse(w, "FAIL: Bark не настроен: "+a.bark.configProblem())
			return
		}
		if err := a.bark.sendText("тест", "", text); err != nil {
			a.stateErr("bark", fmt.Errorf("%s", err))
			textResponse(w, fmt.Sprintf("FAIL: %s", safeDetail(err)))
			return
		}
		a.setState("bark", "on", "тестовое сообщение отправлено")
		textResponse(w, "OK: тестовое сообщение отправлено в Bark")

	default:
		textResponse(w, "FAIL: тестовая отправка доступна только для tg и bark")
	}
}
