package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type uiServer struct {
	app        *app
	configPath string

	tokenMu sync.RWMutex
	token   string

	// Heavy endpoints perform network calls or read the whole debug log.
	// Without a cap, many parallel polls of LuCI would exhaust RAM on a router.
	heavyOnce sync.Once
	heavy     chan struct{}
}

func (s *uiServer) heavySlot() chan struct{} {
	s.heavyOnce.Do(func() { s.heavy = make(chan struct{}, 2) })
	return s.heavy
}

func heavyRequest(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/test/") || strings.HasPrefix(p, "/auth/max/") || p == "/send" || p == "/debug/log" {
		return true
	}
	if p == "/control" {
		switch strings.ToLower(strings.TrimSpace(r.FormValue("action"))) {
		case "test", "ping", "chatid":
			return true
		}
	}
	return false
}

var uiTokenMu sync.Mutex

// writeUITokenFile writes the token atomically and keeps it owner-readable
// only. Both creation and rotation use the same path so there is no window
// with a missing or half-written token file.
func writeUITokenFile(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ui-token-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func readUIToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 32 {
		return "", fmt.Errorf("ui token слишком короткий")
	}
	// A world-readable token file would make the whole panel and local API
	// accessible to any local user. Fail closed instead of ignoring this.
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	return t, nil
}

func ensureUIToken(path string) (string, error) {
	uiTokenMu.Lock()
	defer uiTokenMu.Unlock()

	if t, err := readUIToken(path); err == nil {
		return t, nil
	}

	// O_EXCL makes the creation atomic across processes: a parallel -ctl or
	// -ui-token invocation either claims the path or reads the winner's value.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			f.Close()
			os.Remove(path)
			return "", err
		}
		tok := hex.EncodeToString(raw)
		if _, err := f.WriteString(tok + "\n"); err != nil {
			f.Close()
			os.Remove(path)
			return "", err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(path)
			return "", err
		}
		if err := f.Close(); err != nil {
			os.Remove(path)
			return "", err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return "", err
		}
		return tok, nil
	}
	if !os.IsExist(err) {
		return "", err
	}
	for i := 0; i < 50; i++ {
		if t, err := readUIToken(path); err == nil {
			return t, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", fmt.Errorf("не удалось прочитать ui token, созданный другим процессом")
}

// rotateUIToken makes a token used in the address bar single-use. It replaces
// the persisted value only if it still matches the caller's token, so a
// second process cannot invalidate a session started by the first one.
func rotateUIToken(path, expected string) (string, error) {
	uiTokenMu.Lock()
	defer uiTokenMu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !sameToken(strings.TrimSpace(string(b)), expected) {
		return "", fmt.Errorf("ui token был заменён")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := writeUITokenFile(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *uiServer) uiToken() string {
	s.tokenMu.RLock()
	defer s.tokenMu.RUnlock()
	return s.token
}

func (s *uiServer) setUIToken(t string) {
	s.tokenMu.Lock()
	s.token = t
	s.tokenMu.Unlock()
}

func newUIServer(a *app, configPath, token string) *http.Server {
	s := &uiServer{app: a, configPath: configPath, token: token}
	listen := a.configSnapshot().UIListen
	logf("[UI] listening on %s (get token: Msg-forwarder-Go -config %s -ui-token)", listen, configPath)
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if host != "" && host != "127.0.0.1" && host != "::1" && host != "localhost" {
			logf("WARNING: UI слушает не только loopback (%s): панель и редактор конфига доступны в сети", listen)
		}
	}
	return &http.Server{
		Addr:              listen,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func serveUI(a *app, configPath, token string) {
	srv := newUIServer(a, configPath, token)
	a.setUIServer(srv)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf("[UI] server error: %s", errKind(err))
	}
}

func sameToken(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *uiServer) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browser requests normally send either Origin or Sec-Fetch-Site. Keep
		// headerless clients working, but block cross-site browser requests.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			return false
		}
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) && (u.Scheme == "http" || u.Scheme == "https")
}

func (s *uiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	tok := s.uiToken()
	if qtok := r.URL.Query().Get("token"); qtok != "" {
		// A token in the URL is only a login handshake for the index page.
		// Control/save endpoints require the cookie or the Bearer header, so a
		// leaked link cannot be replayed against them directly.
		if r.Method != http.MethodGet || r.URL.Path != "/" || !sameToken(qtok, tok) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tokenPath := filepath.Join(filepath.Dir(s.configPath), "ui.token")
		newTok, err := rotateUIToken(tokenPath, tok)
		if err != nil {
			logf("[UI] token rotation failed: %s", errKind(err))
			http.Error(w, "token rotation failed", http.StatusInternalServerError)
			return
		}
		s.setUIToken(newTok)
		cookie := &http.Cookie{
			Name:     "Msg-forwarder-Go",
			Value:    newTok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		}
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			cookie.Secure = true
		}
		http.SetCookie(w, cookie)
		// Drop the secret from the address bar and from the next request.
		u := *r.URL
		q := u.Query()
		q.Del("token")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}

	authorized := false
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		authorized = sameToken(strings.TrimSpace(h[7:]), tok)
	}
	if !authorized {
		if c, err := r.Cookie("Msg-forwarder-Go"); err == nil {
			authorized = sameToken(c.Value, tok)
		}
	}
	if !authorized {
		w.Header().Set("WWW-Authenticate", `Bearer`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodPost && !s.originOK(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	// Every action endpoint mutates state or talks to external APIs; only the
	// page and the read-only status endpoint may be fetched with GET.
	readOnly := r.URL.Path == "/" || r.URL.Path == "/status" ||
		r.URL.Path == "/debug/status" || r.URL.Path == "/debug/log"
	if !readOnly && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		// All legitimate forms are tiny; this bounds memory on the router.
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	if heavyRequest(r) {
		select {
		case s.heavySlot() <- struct{}{}:
			defer func() { <-s.heavySlot() }()
		default:
			http.Error(w, "слишком много одновременных операций", http.StatusServiceUnavailable)
			return
		}
	}

	switch r.URL.Path {
	case "/":
		s.handleIndex(w, r)
	case "/status":
		s.app.handleStatus(w, r)
	case "/debug/status":
		s.app.handleDebugStatus(w, r)
	case "/debug/on":
		s.app.handleDebugSwitch(w, r, true)
	case "/debug/off":
		s.app.handleDebugSwitch(w, r, false)
	case "/debug/log":
		s.app.handleDebugLog(w, r)
	case "/control":
		s.app.handleControl(w, r)
	case "/send":
		s.app.handleSend(w, r)
	case "/save":
		s.handleSave(w, r)
	case "/restart":
		s.handleRestart(w, r)
	case "/test/vk":
		s.handleTestVK(w, r)
	case "/test/tg":
		s.handleTestTG(w, r)
	case "/test/max":
		s.handleTestMAX(w, r)
	case "/test/bark":
		s.handleTestBark(w, r)
	case "/test/api":
		s.handleTestAPI(w, r)
	case "/config/save":
		s.handleRawConfigSave(w, r)
	case "/auth/max/captcha":
		s.handleMaxCaptcha(w, r)
	case "/auth/max/start":
		s.handleMaxStart(w, r)
	case "/auth/max/finish":
		s.handleMaxFinish(w, r)
	default:
		http.NotFound(w, r)
	}
}

func field(r *http.Request, name, def string) string {
	if err := r.ParseForm(); err != nil {
		return def
	}
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return def
	}
	return v
}

func validListen(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil || port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func applyFormToConfig(r *http.Request, c *Config) error {
	applySecret := func(name string, dst *string) {
		if r.FormValue("change_"+name) == "on" {
			*dst = r.FormValue(name)
		}
	}
	applySecret("vk_token", &c.VKToken)
	applySecret("tg_bot_token", &c.TGBotToken)
	applySecret("max_token", &c.MaxToken)
	applySecret("bark_key", &c.BarkKey)

	c.VKAppID = r.FormValue("vk_app_id")
	c.VKAPIVersion = field(r, "vk_api_version", c.VKAPIVersion)
	c.TGChatID = flexString(field(r, "tg_chat_id", string(c.TGChatID)))
	c.TelegramProxy = r.FormValue("telegram_proxy")
	c.MaxDeviceID = field(r, "max_device_id", c.MaxDeviceID)
	c.MaxWSURL = field(r, "max_ws_url", c.MaxWSURL)
	c.MaxUserAgent = field(r, "max_user_agent", c.MaxUserAgent)
	c.MaxAppVersion = field(r, "max_app_version", c.MaxAppVersion)

	if v := r.FormValue("incoming_opcode"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32767 {
			return fmt.Errorf("bad incoming_opcode")
		}
		c.IncomingOpcode = n
	}
	c.BarkServerURL = field(r, "bark_server_url", c.BarkServerURL)
	c.BarkDeviceKey = field(r, "bark_device_key", c.BarkDeviceKey)
	c.BarkTitle = field(r, "bark_title", c.BarkTitle)
	c.BarkSound = field(r, "bark_sound", c.BarkSound)
	c.BarkGroup = field(r, "bark_group", c.BarkGroup)
	c.BarkLevel = field(r, "bark_level", c.BarkLevel)
	c.BarkIcon = field(r, "bark_icon", c.BarkIcon)
	c.BarkClickURL = field(r, "bark_click_url", c.BarkClickURL)
	if r.FormValue("bark_present") == "1" {
		c.BarkEncrypt = r.FormValue("bark_encrypt") == "on"
	}
	_, barkDeviceSent := r.Form["bark_device_key"]
	_, barkServerSent := r.Form["bark_server_url"]
	barkForm := r.FormValue("bark_present") == "1" || barkDeviceSent || barkServerSent ||
		r.FormValue("bark_encrypt") == "on" || r.FormValue("change_bark_key") == "on"
	if barkForm && (c.BarkDeviceKey != "" || c.BarkEncrypt || c.BarkKey != "") {
		if err := validateBarkConfig(*c); err != nil {
			return err
		}
	}

	if v := r.FormValue("ui_listen"); v != "" {
		if !validListen(v) {
			return fmt.Errorf("bad ui_listen (нужен host:port)")
		}
		c.UIListen = v
	}
	if r.FormValue("ui_present") == "1" {
		enabled := r.FormValue("ui_enabled") == "on"
		c.UIEnabled = &enabled
		if r.FormValue("autostart_present") == "1" {
			vk := r.FormValue("autostart_vk") == "on"
			max := r.FormValue("autostart_max") == "on"
			tg := r.FormValue("autostart_tg") == "on"
			bark := r.FormValue("autostart_bark") == "on"
			c.AutostartVK, c.AutostartMAX, c.AutostartTG, c.AutostartBark = &vk, &max, &tg, &bark
		}
	}
	// Fill optional fields for a config created from scratch, so the saved
	// file is complete and applyConfig sees the same values as a restart.
	applyDefaults(c)
	return validateConfig(*c)
}

func (s *uiServer) configFromForm(r *http.Request) (Config, error) {
	if err := r.ParseForm(); err != nil {
		return Config{}, err
	}
	c, err := readConfigRaw(s.configPath)
	if err != nil {
		return c, err
	}
	if err := applyFormToConfig(r, &c); err != nil {
		return c, err
	}
	return c, nil
}

func pageField(label, name, value, placeholder string) string {
	return fmt.Sprintf(`<div class="row"><label>%s</label><input name="%s" value="%s" placeholder="%s"></div>`,
		html.EscapeString(label), html.EscapeString(name), html.EscapeString(value), html.EscapeString(placeholder))
}

func pageSecret(label, name string, set bool) string {
	// The checkbox must stay unchecked by default: saving other fields with an
	// empty password input must never erase an already stored token.
	note := "не задано"
	if set {
		note = "установлено (сохранено)"
	}
	return fmt.Sprintf(`<div class="row"><label>%s</label><input type="password" name="%s" autocomplete="off" placeholder="%s"><label class="chk"><input type="checkbox" name="change_%s"> применить новое значение</label></div>`,
		html.EscapeString(label), html.EscapeString(name), html.EscapeString(note), html.EscapeString(name))
}

// rawConfigText returns the exact file contents so the JSON editor shows the
// config as it is on disk, including secret values. Missing files get a
// complete default skeleton so the textarea is immediately usable.
func rawConfigText(path string, c Config) string {
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return string(b)
	}
	fallback := c
	applyDefaults(&fallback)
	out, err := json.MarshalIndent(fallback, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(out)
}

func (s *uiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.app.configSnapshot().uiEnabled() {
		http.NotFound(w, r) // control API stays available for LuCI
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := readConfigRaw(s.configPath)
	if err != nil {
		http.Error(w, "config read error", http.StatusInternalServerError)
		return
	}
	rawCfg := rawConfigText(s.configPath, c)
	uiListen := c.UIListen
	if uiListen == "" {
		uiListen = "127.0.0.1:8080"
	}
	uiChecked := ""
	if c.uiEnabled() {
		uiChecked = " checked"
	}
	debugPath := html.EscapeString(filepath.Join(filepath.Dir(s.configPath), debugLogFile))
	vkVersion := html.EscapeString(c.VKAPIVersion)
	if vkVersion == "" {
		vkVersion = "5.131"
	}
	oauthHint := `<p class="hint">Для VK нужен пользовательский токен с правом <code>messages</code>. Укажите <code>vk_app_id</code>, откройте ссылку ниже, разрешите доступ и вставьте токен из адресной строки в поле токена.</p>`
	if c.VKAppID != "" {
		link := fmt.Sprintf("https://oauth.vk.com/authorize?client_id=%s&display=page&redirect_uri=https://oauth.vk.com/blank.html&scope=messages&response_type=token&v=%s", url.QueryEscape(c.VKAppID), url.QueryEscape(vkVersion))
		oauthHint += fmt.Sprintf(`<p><a href="%s" target="_blank" rel="noopener">Открыть OAuth ВКонтакте</a></p>`, html.EscapeString(link))
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="ru"><head><meta charset="utf-8"><title>Msg-forwarder-Go</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font:14px system-ui,sans-serif;max-width:820px;margin:24px auto;padding:0 16px;color:#222}
h1{font-size:20px}h2{font-size:16px;margin-top:24px;border-bottom:1px solid #ddd;padding-bottom:4px}
.row{margin:8px 0}.row label{display:block;font-weight:600;margin-bottom:3px}
input[type=text],input[type=password],input[type=number]{width:100%;box-sizing:border-box;padding:6px 8px;border:1px solid #bbb;border-radius:4px}
.chk{font-weight:400!important;font-size:12px;color:#666;margin-top:3px}
button{padding:7px 12px;margin:4px 4px 4px 0;border:1px solid #999;border-radius:4px;background:#f6f6f6;cursor:pointer}
.hint{color:#666;font-size:12px}#status{white-space:pre-wrap;background:#f7f7f7;border:1px solid #ddd;padding:8px;margin-top:12px;min-height:20px}
code{background:#eee;padding:1px 4px;border-radius:3px}
.dot{display:inline-block;width:11px;height:11px;border-radius:50%;margin-right:6px;vertical-align:middle}
.dot.on{background:#28a745}.dot.off{background:#aaa}.dot.error{background:#dc3545}
table.svc{width:100%;border-collapse:collapse;margin-top:8px}
table.svc td{padding:6px 4px;border-bottom:1px solid #eee;vertical-align:middle}
table.svc td:first-child{white-space:nowrap;font-weight:600}.det{color:#666;font-size:12px}
</style></head><body>
<h1>Msg-forwarder-Go — настройки</h1>
<p class="hint">По умолчанию интерфейс доступен только с loopback. Если в поле ниже указан не-loopback адрес, панель и редактор конфига окажутся в сети — используйте HTTPS или <code>ssh -L 8080:127.0.0.1:8080 root@router</code>. Токены сохраняются в конфиг с правами 0600 и не выводятся в логи; редактор JSON показывает их открыто.</p>
<form method="post" id="mainform">
<input type="hidden" name="ui_present" value="1">
<input type="hidden" name="cfg_mtime" value="`)
	b.WriteString(html.EscapeString(configVersion(s.configPath)))
	b.WriteString(`">
<h2>VK</h2>`)
	b.WriteString(pageSecret("VK user token", "vk_token", c.VKToken != ""))
	b.WriteString(pageField("VK app ID (для OAuth)", "vk_app_id", c.VKAppID, "1234567"))
	b.WriteString(pageField("VK API version", "vk_api_version", vkVersion, "5.131"))
	b.WriteString(oauthHint)

	b.WriteString(`<h2>Telegram</h2>`)
	b.WriteString(pageSecret("Bot token", "tg_bot_token", c.TGBotToken != ""))
	b.WriteString(pageField("Chat ID", "tg_chat_id", string(c.TGChatID), "123456789"))
	b.WriteString(pageField("Прокси для Telegram (необязательно)", "telegram_proxy", c.TelegramProxy, "оставьте пустым, если доступ есть"))
	b.WriteString(`<button type="button" onclick="act('/test/api',this)">Проверить доступ к Bot API</button>`)
	b.WriteString(`<p class="hint">Проверки используют сохранённый конфиг: после изменения прокси сначала нажмите «Сохранить». Обычно прокси не нужен: проверка выше обращается к <code>api.telegram.org</code> без токена и ждёт ответ 401. MTProto и TG WS Proxy для Bot API не подходят — они туннелируют только MTProto. Если без прокси не работает, укажите <code>http://</code>, <code>https://</code>, <code>socks5://</code> или <code>socks5h://</code>; последний не раскрывает имя хоста локальному резолверу. VK, MAX и Bark всегда идут напрямую.</p>`)
	b.WriteString(`<p class="hint">Бот не прописан в программе — создайте своего у <a href="https://t.me/BotFather" target="_blank" rel="noopener">@BotFather</a> (/newbot), вставьте токен выше, напишите боту сообщение и нажмите «Узнать chat_id» в панели соединений ниже.</p>`)

	b.WriteString(`<h2>MAX</h2>`)
	b.WriteString(pageSecret("Access token", "max_token", c.MaxToken != ""))
	b.WriteString(pageField("Device ID (UUID из браузера)", "max_device_id", c.MaxDeviceID, "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"))
	b.WriteString(pageField("WebSocket URL", "max_ws_url", c.MaxWSURL, "wss://api.oneme.ru/websocket"))
	b.WriteString(`<p class="hint">Свой адрес получает токен MAX, deviceId и текст сообщений. Для внешнего сервера используйте <code>wss://</code>; проверки берут адрес из сохранённого конфига.</p>`)
	b.WriteString(pageField("User-Agent", "max_user_agent", c.MaxUserAgent, "Mozilla/5.0 ..."))
	b.WriteString(pageField("App version", "max_app_version", c.MaxAppVersion, "26.11.9"))
	b.WriteString(pageField("Опкод входящих сообщений", "incoming_opcode", strconv.Itoa(c.IncomingOpcode), "128"))

	b.WriteString(`<h2>Bark</h2>
<input type="hidden" name="bark_present" value="1">`)
	b.WriteString(pageField("Сервер Bark", "bark_server_url", c.BarkServerURL, "https://api.day.app или http://192.168.1.10:8080"))
	b.WriteString(`<p class="hint">Свой адрес получает device key, имя контакта и текст сообщений. Для внешнего сервера используйте <code>https://</code>; открытый <code>http://</code> оставляйте только для локальной сети.</p>`)
	b.WriteString(pageField("Device key (кому отправлять)", "bark_device_key", c.BarkDeviceKey, "ключ устройства из приложения Bark"))
	b.WriteString(pageSecret("Пароль/ключ шифрования", "bark_key", c.BarkKey != ""))
	b.WriteString(`<div class="row"><label class="chk"><input type="checkbox" name="bark_encrypt"`)
	if c.BarkEncrypt {
		b.WriteString(" checked")
	}
	b.WriteString(`> шифровать сообщения (AES-128-CBC)</label></div>
<p class="hint">Для шифрования ключ должен быть <b>ровно 16 символов</b> и совпадать с KEY в настройках «Push Encryption» приложения Bark. IV создаётся случайно и передаётся в параметре <code>iv</code>.</p>`)
	b.WriteString(pageField("Заголовок", "bark_title", c.BarkTitle, "Msg-forwarder-Go"))
	b.WriteString(pageField("Звук", "bark_sound", c.BarkSound, "birdsong"))
	b.WriteString(pageField("Группа", "bark_group", c.BarkGroup, "Msg-forwarder-Go"))
	b.WriteString(pageField("Уровень", "bark_level", c.BarkLevel, "active / timeSensitive / passive / critical"))
	b.WriteString(pageField("Иконка (URL)", "bark_icon", c.BarkIcon, "https://..."))
	b.WriteString(pageField("Ссылка при нажатии", "bark_click_url", c.BarkClickURL, "https://..."))
	b.WriteString(`<button type="button" onclick="act('/test/bark',this)">Проверить Bark</button>`)

	b.WriteString(`<h2>Авторизация MAX (SMS, опционально)</h2>
<p class="hint">Если сервер запросит капчу — сначала получите ссылку, решите её и вставьте captchaToken. Затем запросите код и введите его. Итоговый access token появится в статусе — скопируйте его в поле выше, нажмите «Сохранить» и обновите страницу, чтобы убрать токен из статуса.</p>
<div class="row"><label>Телефон MAX</label><input name="auth_phone" placeholder="+79991234567"></div>
<div class="row"><label>captchaToken (если нужен)</label><input name="auth_captcha"></div>
<div class="row"><label>Verification token (от запроса кода)</label><input name="auth_verify"></div>
<div class="row"><label>Код из SMS</label><input name="auth_code" autocomplete="one-time-code"></div>
<button type="button" onclick="act('/auth/max/captcha',this)">Ссылка на капчу</button>
<button type="button" onclick="act('/auth/max/start',this)">Запросить код</button>
<button type="button" onclick="act('/auth/max/finish',this)">Получить access token</button>

<h2>Веб-интерфейс</h2>
<div class="row"><label>Адрес прослушивания (требует перезапуска)</label><input name="ui_listen" value="` + html.EscapeString(uiListen) + `"></div>
<div class="row"><label class="chk"><input type="checkbox" name="ui_enabled"` + uiChecked + `> включить веб-интерфейс</label></div>

<h2>Управление соединениями</h2>
<p class="hint"><span class="dot off"></span> серый — выключено, <span class="dot on"></span> зелёный — работает, <span class="dot error"></span> красный — ошибка. Кнопки «вкл/выкл/перезапуск» действуют до перезапуска процесса; «автозапуск» сохраняется в конфиг.</p>
<table class="svc">
<tr><td><span id="dot-vk" class="dot off"></span> VK</td><td id="det-vk" class="det">—</td><td>
<button type="button" onclick="ctl('vk','on')">Включить</button>
<button type="button" onclick="ctl('vk','off')">Выключить</button>
<button type="button" onclick="ctl('vk','restart')">Перезапуск</button>
<button type="button" onclick="ctl('vk','autostart')">Автозапуск: <span id="as-vk">—</span></button>
<button type="button" onclick="ctl('vk','ping')">Пинг</button>
<button type="button" onclick="ctl('vk','test')">Тест</button>
</td></tr>
<tr><td><span id="dot-max" class="dot off"></span> MAX</td><td id="det-max" class="det">—</td><td>
<button type="button" onclick="ctl('max','on')">Включить</button>
<button type="button" onclick="ctl('max','off')">Выключить</button>
<button type="button" onclick="ctl('max','restart')">Перезапуск</button>
<button type="button" onclick="ctl('max','autostart')">Автозапуск: <span id="as-max">—</span></button>
<button type="button" onclick="ctl('max','ping')">Пинг</button>
<button type="button" onclick="ctl('max','test')">Тест</button>
</td></tr>
<tr><td><span id="dot-tg" class="dot off"></span> Telegram</td><td id="det-tg" class="det">—</td><td>
<button type="button" onclick="ctl('tg','on')">Включить</button>
<button type="button" onclick="ctl('tg','off')">Выключить</button>
<button type="button" onclick="ctl('tg','restart')">Перезапуск</button>
<button type="button" onclick="ctl('tg','autostart')">Автозапуск: <span id="as-tg">—</span></button>
<button type="button" onclick="ctl('tg','ping')">Пинг</button>
<button type="button" onclick="ctl('tg','test')">Тест</button>
<button type="button" onclick="ctl('tg','chatid')">Узнать chat_id</button>
</td></tr>
<tr><td><span id="dot-bark" class="dot off"></span> Bark</td><td id="det-bark" class="det">—</td><td>
<button type="button" onclick="ctl('bark','on')">Включить</button>
<button type="button" onclick="ctl('bark','off')">Выключить</button>
<button type="button" onclick="ctl('bark','restart')">Перезапуск</button>
<button type="button" onclick="ctl('bark','autostart')">Автозапуск: <span id="as-bark">—</span></button>
<button type="button" onclick="ctl('bark','ping')">Пинг</button>
<button type="button" onclick="ctl('bark','test')">Тест</button>
</td></tr>
</table>

<h2>Отладочный лог</h2>
<p class="hint">Подробная история трафика, соединений и ошибок: тип события, размеры, статусы, длительности, коды сбоев. Токены, имена контактов и текст сообщений не пишутся, а любые совпадения с секретами маскируются. Файл <code>` + debugPath + `</code>, не более 1 МБ плюс 1 МБ предыдущего файла. Включение сохраняется: отладка поднимется и после перезапуска процесса, пока не нажмёте «Выключить». Файл остаётся на диске.</p>
<table class="svc">
<tr><td><span id="dot-debug" class="dot off"></span> Лог</td><td id="det-debug" class="det">—</td><td>
<button type="button" onclick="debugCtl('on')">Включить</button>
<button type="button" onclick="debugCtl('off')">Выключить</button>
<button type="button" onclick="loadDebugStatus()">Статус</button>
<a id="debug-download" href="/debug/log" download="Msg-forwarder-Go-debug.log">Скачать лог</a>
</td></tr>
</table>
<div class="row"><label>Тестовое сообщение</label><input id="testtext" name="testtext" placeholder="Проверка доставки"></div>
<button type="button" onclick="sendTest('tg')">Отправить в Telegram</button>
<button type="button" onclick="sendTest('bark')">Отправить в Bark</button>

<h2>Действия</h2>
<button type="button" onclick="act('/test/vk',this)">Проверить VK</button>
<button type="button" onclick="act('/test/tg',this)">Проверить Telegram</button>
<button type="button" onclick="act('/test/max',this)">Проверить MAX</button>
<button type="button" onclick="act('/test/bark',this)">Проверить Bark</button>
<button type="submit">Сохранить</button>
<button type="button" onclick="act('/restart',this)">Перезапустить сервис</button>
<pre id="status">Готово.</pre>
</form>
<h2>Файл конфигурации (JSON)</h2>
<p class="hint">Текст взят из конфига как есть — токены видны открыто. Поставьте галочку «разрешить редактирование», поправьте JSON и нажмите «Сохранить JSON». «Отмена» возвращает последний загруженный текст. Неизвестные поля при сохранении удаляются.</p>
<form method="post" id="rawform">
<div class="row"><label class="chk"><input type="checkbox" id="raw-edit" name="raw_edit" onchange="rawToggle()"> разрешить редактирование</label></div>
<div class="row"><textarea id="raw-config" name="rawconfig" rows="16" spellcheck="false" style="width:100%;box-sizing:border-box;padding:6px 8px;border:1px solid #bbb;border-radius:4px;font-family:ui-monospace,monospace" readonly>` + html.EscapeString(rawCfg) + `</textarea></div>
<button type="button" onclick="saveRaw(this)">Сохранить JSON</button>
<button type="button" onclick="rawCancel()">Отмена</button>
<button type="button" onclick="act('/restart',this)">Перезапустить сервис</button>
<pre id="rawstatus">Готово.</pre>
</form>
<script>
async function act(url, btn){
  const fd = new FormData(document.getElementById('mainform'));
  fd.set('action', btn.textContent.trim());
  const out = document.getElementById('status');
  out.textContent = '...';
  try {
    const r = await fetch(url, {method:'POST', body:fd, headers:{'X-Requested-With':'fetch'}});
    out.textContent = await r.text();
  } catch(e) { out.textContent = 'Ошибка запроса: ' + e; }
}
function rawToggle(){
  document.getElementById('raw-config').readOnly = !document.getElementById('raw-edit').checked;
}
let rawInitial = document.getElementById('raw-config').value;
function rawCancel(){
  document.getElementById('raw-config').value = rawInitial;
  document.getElementById('raw-edit').checked = false;
  rawToggle();
  document.getElementById('rawstatus').textContent = 'Отменено.';
}
async function saveRaw(btn){
  const fd = new FormData(document.getElementById('rawform'));
  const out = document.getElementById('rawstatus');
  out.textContent = '...';
  try {
    const r = await fetch('/config/save', {method:'POST', body:fd});
    out.textContent = await r.text();
    if (r.ok) rawInitial = document.getElementById('raw-config').value;
  } catch(e) { out.textContent = 'Ошибка запроса: ' + e; }
}
rawToggle();
</script>
`)
	fmt.Fprintf(&b, `<script>
const autostart = {vk:%t,max:%t,tg:%t,bark:%t};
function paint(st){
  for (const n of ['vk','max','tg','bark']) {
    const s = st[n] || {state:'off',detail:''};
    const d = document.getElementById('dot-'+n);
    if (d) d.className = 'dot ' + (s.state === 'on' ? 'on' : s.state === 'error' ? 'error' : 'off');
    const t = document.getElementById('det-'+n);
    if (t) t.textContent = s.detail || s.state;
  }
}
async function loadStatus(){
  try { const r = await fetch('/status'); paint(await r.json()); } catch(e) {}
  loadDebugStatus();
}
function fmtBytes(n){ return n < 1024 ? n + ' Б' : Math.round(n/1024) + ' КБ'; }
function paintDebug(d){
  const dot = document.getElementById('dot-debug');
  if (!dot) return;
  dot.className = 'dot ' + (d.enabled ? 'on' : 'off');
  document.getElementById('det-debug').textContent = d.enabled
    ? 'пишется: ' + fmtBytes(d.bytes) + ' (прошлый файл ' + fmtBytes(d.prev_bytes) + ')' +
      (d.persisted ? ' · после рестарта' : '') +
      (d.forced_flushes ? ' · переполнений буфера: ' + d.forced_flushes : '')
    : 'выключен';
}
async function loadDebugStatus(){
  try { const r = await fetch('/debug/status'); paintDebug(await r.json()); } catch(e) {}
}
async function debugCtl(action){
  const out = document.getElementById('status');
  out.textContent = '...';
  try {
    const r = await fetch('/debug/' + action, {method:'POST', body:new FormData()});
    out.textContent = await r.text();
  } catch(e) { out.textContent = 'Ошибка: ' + e; }
  loadDebugStatus();
}
async function ctl(service, action){
  const fd = new FormData();
  fd.set('service', service); fd.set('action', action);
  if (action === 'autostart') {
    autostart[service] = !autostart[service];
    fd.set('value', String(autostart[service]));
  }
  const out = document.getElementById('status');
  out.textContent = '...';
  try {
    const r = await fetch('/control', {method:'POST', body:fd});
    const res = await r.text();
    out.textContent = res;
    if (action === 'autostart') document.getElementById('as-'+service).textContent = autostart[service] ? 'вкл' : 'выкл';
    if (action === 'chatid' && res.startsWith('chat_id: ')) {
      document.querySelector('input[name="tg_chat_id"]').value = res.slice(9).trim();
    }
  } catch(e) { out.textContent = 'Ошибка: ' + e; }
  loadStatus();
}
async function sendTest(service){
  const out = document.getElementById('status');
  out.textContent = '...';
  const fd = new FormData();
  fd.set('service', service);
  fd.set('text', document.getElementById('testtext').value);
  try { const r = await fetch('/send', {method:'POST', body:fd}); out.textContent = await r.text(); }
  catch(e) { out.textContent = 'Ошибка: ' + e; }
  loadStatus();
}
for (const n of ['vk','max','tg','bark']) document.getElementById('as-'+n).textContent = autostart[n] ? 'вкл' : 'выкл';
loadStatus(); setInterval(loadStatus, 4000);
</script></body></html>`, c.autostartVK(), c.autostartMAX(), c.autostartTG(), c.autostartBark())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *uiServer) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := modifyConfig(s.configPath, func(c *Config) error {
		if err := checkConfigVersion(s.configPath, r.FormValue("cfg_mtime")); err != nil {
			return err
		}
		return applyFormToConfig(r, c)
	})
	if err != nil {
		http.Error(w, safeDetail(err), http.StatusBadRequest)
		return
	}
	s.app.applyConfig(c)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Сохранено. Токены применяются сразу; для прокси, адреса интерфейса и активных соединений нажмите «Перезапустить сервис»."))
}

// handleRawConfigSave accepts the whole config as JSON. The checkbox is a
// server-side guard too: a disabled textarea alone is not protection.
func (s *uiServer) handleRawConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.FormValue("raw_edit") != "on" {
		http.Error(w, "редактирование не разрешено", http.StatusForbidden)
		return
	}
	c, err := modifyConfig(s.configPath, func(c *Config) error {
		if err := checkConfigVersion(s.configPath, r.FormValue("cfg_mtime")); err != nil {
			return err
		}
		var parsed Config
		if err := json.Unmarshal([]byte(r.FormValue("rawconfig")), &parsed); err != nil {
			return fmt.Errorf("неверный JSON: %w", err)
		}
		*c = parsed
		applyDefaults(c)
		return validateConfig(*c)
	})
	if err != nil {
		http.Error(w, safeDetail(err), http.StatusBadRequest)
		return
	}
	s.app.applyConfig(c)
	textResponse(w, "Сохранено и применено. Для прокси, адреса интерфейса и активных соединений нажмите «Перезапустить сервис».")
}

func (s *uiServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, _ = w.Write([]byte("Перезапуск…"))
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.app.requestRestart()
	}()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func logTestResult(name string, err error) {
	if err != nil {
		debugf("[TEST] %s FAIL err=%s", name, safeDetail(err))
	} else {
		debugf("[TEST] %s OK", name)
	}
}

// Tests deliberately use only the saved config. Unsaved form values must not
// be able to turn a button into an arbitrary-host request with tokens.
func (s *uiServer) handleTestVK(w http.ResponseWriter, _ *http.Request) {
	c := s.app.configSnapshot()
	err := vkValidate(c.VKToken, c.VKAPIVersion, s.app.client)
	logTestResult("vk", err)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "VK token валиден"})
}

func (s *uiServer) handleTestTG(w http.ResponseWriter, _ *http.Request) {
	c := s.app.configSnapshot()
	hc, err := newTelegramHTTPClient(c)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	defer closeIdleClient(hc)
	err = newTelegram(c, hc).test()
	logTestResult("tg", err)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "Telegram bot доступен (через заданный прокси, если указан)"})
}

func (s *uiServer) handleTestMAX(w http.ResponseWriter, _ *http.Request) {
	c := s.app.configSnapshot()
	err := maxValidate(c.MaxToken, c.MaxDeviceID, c.MaxUserAgent, c.MaxAppVersion, c.MaxWSURL)
	logTestResult("max", err)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "MAX LOGIN успешен"})
}

func (s *uiServer) handleTestBark(w http.ResponseWriter, _ *http.Request) {
	c := s.app.configSnapshot()
	err := newBark(c, s.app.client).test()
	logTestResult("bark", err)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "Bark push доставлен"})
}

func (s *uiServer) handleTestAPI(w http.ResponseWriter, _ *http.Request) {
	c := s.app.configSnapshot()
	err := checkBotAPI(c)
	logTestResult("api", err)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	msg := "api.telegram.org доступен, прокси не нужен"
	if strings.TrimSpace(c.TelegramProxy) != "" {
		msg = "api.telegram.org доступен через заданный прокси"
	}
	writeJSON(w, map[string]any{"ok": true, "message": msg})
}

func (s *uiServer) handleMaxCaptcha(w http.ResponseWriter, r *http.Request) {
	c := s.app.configSnapshot()
	phone := field(r, "auth_phone", "")
	if phone == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "укажите телефон"})
		return
	}
	debugLog.setAuthSecrets(phone)
	defer debugLog.setAuthSecrets()
	link, err := maxCaptchaLink(phone, c.MaxDeviceID, c.MaxUserAgent, c.MaxAppVersion, c.MaxWSURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "link": link})
}

func (s *uiServer) handleMaxStart(w http.ResponseWriter, r *http.Request) {
	c := s.app.configSnapshot()
	phone := field(r, "auth_phone", "")
	if phone == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "укажите телефон"})
		return
	}
	debugLog.setAuthSecrets(phone, r.FormValue("auth_captcha"))
	defer debugLog.setAuthSecrets()
	vt, err := maxSMSStart(phone, r.FormValue("auth_captcha"), c.MaxDeviceID, c.MaxUserAgent, c.MaxAppVersion, c.MaxWSURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "verify_token": vt, "hint": "Введите код из SMS и нажмите «Получить access token»"})
}

func (s *uiServer) handleMaxFinish(w http.ResponseWriter, r *http.Request) {
	c := s.app.configSnapshot()
	vt := r.FormValue("auth_verify")
	code := r.FormValue("auth_code")
	if vt == "" || code == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "нужны verify_token и код"})
		return
	}
	debugLog.setAuthSecrets(vt, code)
	defer debugLog.setAuthSecrets()
	tok, err := maxSMSFinish(vt, code, c.MaxDeviceID, c.MaxUserAgent, c.MaxAppVersion, c.MaxWSURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeDetail(err)})
		return
	}
	writeJSON(w, map[string]any{
		"ok":           true,
		"access_token": tok,
		"device_id":    c.MaxDeviceID,
		"hint":         "Скопируйте access_token в поле MAX и нажмите «Сохранить»",
	})
}
