package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigVersionDetectsExternalWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, Config{}); err != nil {
		t.Fatal(err)
	}
	v := configVersion(path)
	if v == "" || checkConfigVersion(path, v) != nil {
		t.Fatal("fresh version must be accepted")
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if err := checkConfigVersion(path, v); err == nil {
		t.Fatal("external modification was not detected")
	}
}

func TestConfigFromFormPreservesSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	initial := Config{
		VKToken:       "vk-secret",
		TGBotToken:    "tg-secret",
		MaxToken:      "max-secret",
		BarkKey:       "1234567890123456",
		TGChatID:      flexString("111"),
		TelegramProxy: "socks5://127.0.0.1:1080",
	}
	if err := writeConfig(path, initial); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"vk_token":        {"should-be-ignored"},
		"tg_bot_token":    {"should-be-ignored"},
		"max_token":       {"should-be-ignored"},
		"bark_key":        {"should-be-ignored"},
		"tg_chat_id":      {"222"},
		"telegram_proxy":  {"http://127.0.0.1:3128"},
		"vk_app_id":       {"1234567"},
		"ui_present":      {"1"},
		"ui_enabled":      {"off"},
		"incoming_opcode": {"129"},
	}
	r := httptest.NewRequest("POST", "/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	s := &uiServer{configPath: path}
	c, err := s.configFromForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.VKToken != "vk-secret" || c.TGBotToken != "tg-secret" || c.MaxToken != "max-secret" || c.BarkKey != "1234567890123456" {
		t.Fatalf("secrets were overwritten: %+v", c)
	}
	if c.TGChatID != "222" || c.TelegramProxy != "http://127.0.0.1:3128" || c.IncomingOpcode != 129 {
		t.Fatalf("fields not updated: %+v", c)
	}
	if c.UIEnabled == nil || *c.UIEnabled {
		t.Fatalf("ui_enabled not applied: %v", c.UIEnabled)
	}
}

func TestConfigFromFormChangesSecretOnlyWhenChecked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeConfig(path, Config{VKToken: "old"}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"vk_token":        {"new"},
		"change_vk_token": {"on"},
		"ui_present":      {"1"},
	}
	r := httptest.NewRequest("POST", "/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c, err := (&uiServer{configPath: path}).configFromForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.VKToken != "new" {
		t.Fatalf("secret not changed: %q", c.VKToken)
	}
}

func TestWriteConfigPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, Config{TGChatID: "1"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports synthetic 0666 for regular files, so only assert on Unix.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
		t.Fatalf("bad config mode: %v", fi.Mode().Perm())
	}
}

func TestTelegramProxyValidation(t *testing.T) {
	cfg := Config{TelegramProxy: "ftp://127.0.0.1:21"}
	if _, err := newTelegramHTTPClient(cfg); err == nil {
		t.Fatal("expected error for unsupported proxy scheme")
	}
	cfg.TelegramProxy = "socks5://user:pass@127.0.0.1:1080"
	if _, err := newTelegramHTTPClient(cfg); err != nil {
		t.Fatalf("socks5 proxy rejected: %v", err)
	}
	cfg.TelegramProxy = ""
	if _, err := newTelegramHTTPClient(cfg); err != nil {
		t.Fatalf("empty proxy rejected: %v", err)
	}
}

func TestUILoadsWithEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatalf("first run must not fail: %v", err)
	}
	if !c.uiEnabled() {
		t.Fatal("UI must be enabled by default")
	}
}

func TestModifyConfigSerialisesUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, Config{BarkTitle: "0"}); err != nil {
		t.Fatal(err)
	}
	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := modifyConfig(path, func(c *Config) error {
				n, err := strconv.Atoi(c.BarkTitle)
				if err != nil {
					return err
				}
				c.BarkTitle = strconv.Itoa(n + 1)
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	saved, err := readConfigRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.BarkTitle != strconv.Itoa(workers) {
		t.Fatalf("lost update: got %q, want %q", saved.BarkTitle, strconv.Itoa(workers))
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}
