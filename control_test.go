package main

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlURLAlwaysLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{"", "http://127.0.0.1:8080/status"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000/status"},
		{"127.0.0.1:18081", "http://127.0.0.1:18081/status"},
		{"192.168.1.10:8080", "http://127.0.0.1:8080/status"},
	}
	for _, tc := range cases {
		if got := controlURL(Config{UIListen: tc.listen}, "/status"); got != tc.want {
			t.Fatalf("listen %q: got %q, want %q", tc.listen, got, tc.want)
		}
	}
}

func TestAutostartDefaultsAndPersistence(t *testing.T) {
	var c Config
	if !c.autostartVK() || !c.autostartMAX() || !c.autostartTG() || !c.autostartBark() {
		t.Fatal("missing autostart flags must default to true")
	}
	v := false
	c.AutostartVK = &v
	if c.autostartVK() {
		t.Fatal("explicit false not honoured")
	}

	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, cfgPath: path, tg: newTelegram(cfg, nil)}
	initStates(a)
	if err := a.setAutostart("max", false); err != nil {
		t.Fatal(err)
	}
	saved, err := readConfigRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AutostartMAX == nil || *saved.AutostartMAX {
		t.Fatalf("autostart_max was not saved: %v", saved.AutostartMAX)
	}
	if got := a.snapshotStates()["max"]; got.State != "off" {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func TestSafeDetailHidesTokens(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://api.telegram.org/bot123456:SECRET/sendMessage?access_token=SECRET",
		Err: errors.New("connection refused"),
	}
	detail := safeDetail(err)
	if strings.Contains(detail, "SECRET") || strings.Contains(detail, "123456") {
		t.Fatalf("token leaked into status: %q", detail)
	}
}
