package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const defaultConfigPath = "/etc/Msg-forwarder-Go/config.json"

// flexString allows both "123" and 123 in JSON (useful for Telegram chat_id).
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*s = flexString(n.String())
		return nil
	}
	return fmt.Errorf("expected string or number")
}

type Config struct {
	VKToken        string     `json:"vk_token"`
	VKAppID        string     `json:"vk_app_id"`
	TGBotToken     string     `json:"tg_bot_token"`
	TGChatID       flexString `json:"tg_chat_id"`
	TelegramProxy  string     `json:"telegram_proxy"`
	MaxToken       string     `json:"max_token"`
	MaxDeviceID    string     `json:"max_device_id"`
	MaxWSURL       string     `json:"max_ws_url"`
	MaxUserAgent   string     `json:"max_user_agent"`
	MaxAppVersion  string     `json:"max_app_version"`
	VKAPIVersion   string     `json:"vk_api_version"`
	IncomingOpcode int        `json:"incoming_opcode"`

	// Bark push destination.
	BarkServerURL string `json:"bark_server_url"`
	BarkDeviceKey string `json:"bark_device_key"`
	BarkKey       string `json:"bark_key"`
	BarkEncrypt   bool   `json:"bark_encrypt"`
	BarkTitle     string `json:"bark_title"`
	BarkSound     string `json:"bark_sound"`
	BarkGroup     string `json:"bark_group"`
	BarkLevel     string `json:"bark_level"`
	BarkIcon      string `json:"bark_icon"`
	BarkClickURL  string `json:"bark_click_url"`

	// Per-service autostart. nil means "true" (backward compatible).
	AutostartVK   *bool `json:"autostart_vk"`
	AutostartMAX  *bool `json:"autostart_max"`
	AutostartTG   *bool `json:"autostart_tg"`
	AutostartBark *bool `json:"autostart_bark"`

	// Local web UI. Default: enabled on 127.0.0.1:8080.
	UIEnabled *bool  `json:"ui_enabled"`
	UIListen  string `json:"ui_listen"`
}

func readConfigRaw(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil // first run: let the UI create it
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// applyDefaults fills optional fields so that a partially written config (for
// example, one created by an older version) remains usable after any save.
func applyDefaults(c *Config) []string {
	if c.MaxWSURL == "" {
		c.MaxWSURL = "wss://api.oneme.ru/websocket"
	}
	if c.MaxUserAgent == "" {
		c.MaxUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	}
	if c.MaxAppVersion == "" {
		c.MaxAppVersion = "26.11.9"
	}
	if c.VKAPIVersion == "" {
		c.VKAPIVersion = "5.131"
	}
	if c.IncomingOpcode < 1 || c.IncomingOpcode > 32767 {
		c.IncomingOpcode = 128
	}
	if c.UIListen == "" {
		c.UIListen = "127.0.0.1:8080"
	}
	if c.BarkServerURL == "" {
		c.BarkServerURL = barkDefaultServer
	}
	if c.BarkTitle == "" {
		c.BarkTitle = "Msg-forwarder-Go"
	}
	if c.BarkSound == "" {
		c.BarkSound = "birdsong"
	}
	if c.BarkGroup == "" {
		c.BarkGroup = "Msg-forwarder-Go"
	}
	if c.BarkLevel == "" {
		c.BarkLevel = "active"
	}

	missing := make([]string, 0, 5)
	if c.VKToken == "" {
		missing = append(missing, "vk_token")
	}
	if c.TGBotToken == "" {
		missing = append(missing, "tg_bot_token")
	}
	if c.TGChatID == "" {
		missing = append(missing, "tg_chat_id")
	}
	if c.MaxToken == "" {
		missing = append(missing, "max_token")
	}
	if c.MaxDeviceID == "" {
		missing = append(missing, "max_device_id")
	}
	return missing
}

func loadConfig(path string) (Config, error) {
	c, err := readConfigRaw(path)
	if err != nil {
		return c, err
	}
	if missing := applyDefaults(&c); len(missing) > 0 {
		logf("WARNING: missing config fields: %v (fill them in the local UI)", missing)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0077 != 0 {
		// Tokens must not stay readable by group or others. Fix it here instead
		// of merely warning, while keeping the warning for the administrator.
		if err := os.Chmod(path, 0600); err != nil {
			logf("WARNING: не удалось установить права 0600 на %s: %v", path, err)
		} else {
			logf("WARNING: конфиг был доступен group/others, права исправлены на 0600: %s", path)
		}
	}
	return c, nil
}

func (c Config) uiEnabled() bool {
	if c.UIEnabled == nil {
		return true
	}
	return *c.UIEnabled
}

func boolDefault(v *bool) bool {
	return v == nil || *v
}

func (c Config) autostartVK() bool   { return boolDefault(c.AutostartVK) }
func (c Config) autostartMAX() bool  { return boolDefault(c.AutostartMAX) }
func (c Config) autostartTG() bool   { return boolDefault(c.AutostartTG) }
func (c Config) autostartBark() bool { return boolDefault(c.AutostartBark) }

// configVersion is an optimistic lock token for a loaded config. LuCI writes
// the same file from another process, so the process-local mutex alone cannot
// prevent lost updates; the UI sends the version back on save.
func configVersion(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

func checkConfigVersion(path, version string) error {
	if version == "" {
		return nil // older clients without the field
	}
	if got := configVersion(path); got != "" && got != version {
		return fmt.Errorf("конфиг был изменён другой сессией — перезагрузите страницу")
	}
	return nil
}

// configWriteMu serialises the whole read-modify-write cycle. Without it two
// parallel saves (UI, -ctl and LuCI) can lose updates or truncate an already
// renamed config through a shared temporary name.
var configWriteMu sync.Mutex

// writeConfig writes atomically and keeps the file readable only by owner.
func writeConfig(path string, c Config) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	return writeConfigUnlocked(path, c)
}

// modifyConfig reads, changes and writes the config under one lock. The final
// config is returned so callers can apply it to the running service.
func modifyConfig(path string, fn func(*Config) error) (Config, error) {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	c, err := readConfigRaw(path)
	if err != nil {
		return c, err
	}
	if fn != nil {
		if err := fn(&c); err != nil {
			return c, err
		}
	}
	if err := writeConfigUnlocked(path, c); err != nil {
		return c, err
	}
	return c, nil
}

// backupConfig keeps the previous config as <path>.bak with mode 0600. It is
// best effort: a broken backup must not prevent a valid config from being saved.
func backupConfig(path string) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".config-*.bak")
	if err != nil {
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if f.Chmod(0600) != nil {
		f.Close()
		return
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return
	}
	_ = os.Rename(tmp, path+".bak")
}

func writeConfigUnlocked(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	// Flush to flash before the rename: OpenWRT devices lose power often and
	// an empty config would drop all tokens.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	backupConfig(path)
	return os.Rename(tmp, path)
}

// validateConfig is shared by the form editor and the raw JSON editor. Custom
// hosts stay allowed, but schemes and types must be valid so the saved file
// can always be loaded after a restart.
func validateConfig(c Config) error {
	if c.UIListen != "" && !validListen(c.UIListen) {
		return fmt.Errorf("неверный ui_listen (нужен host:port)")
	}
	if c.MaxWSURL != "" {
		u, err := url.Parse(c.MaxWSURL)
		if err != nil {
			return fmt.Errorf("неверный max_ws_url")
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "ws" && scheme != "wss" {
			return fmt.Errorf("max_ws_url: нужен ws или wss")
		}
		host := u.Hostname()
		if host == "" {
			return fmt.Errorf("max_ws_url: нет хоста")
		}
		// MAX sends the access token during LOGIN. Cleartext ws would put it
		// on the wire for any observer; allow it only for local test servers.
		if scheme == "ws" && host != "localhost" && !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("max_ws_url: ws без TLS разрешён только для localhost")
		}
	}
	hc, err := newTelegramHTTPClient(c)
	closeIdleClient(hc)
	if err != nil {
		return fmt.Errorf("bad telegram_proxy: %w", err)
	}
	// A bare bark_key without a device key is a valid "not configured yet"
	// state used by the form editor. Validate only when Bark can actually send.
	if c.BarkDeviceKey != "" || c.BarkEncrypt {
		if err := validateBarkConfig(c); err != nil {
			return err
		}
	}
	return nil
}
