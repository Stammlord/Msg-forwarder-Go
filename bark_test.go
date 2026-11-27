package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeBarkServerURL(t *testing.T) {
	got, err := normalizeBarkServerURL(" http://127.0.0.1:8080/ ")
	if err != nil || got != "http://127.0.0.1:8080" {
		t.Fatalf("normalize: %q, %v", got, err)
	}
	if _, err := normalizeBarkServerURL("ftp://example.com"); err == nil {
		t.Fatal("ftp server accepted")
	}
}

func TestValidateBarkConfig(t *testing.T) {
	cfg := Config{BarkServerURL: "https://api.day.app", BarkDeviceKey: "device"}
	if err := validateBarkConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.BarkEncrypt = true
	cfg.BarkKey = "short"
	if err := validateBarkConfig(cfg); err == nil {
		t.Fatal("short encryption key accepted")
	}
	cfg.BarkKey = "1234567890123456"
	if err := validateBarkConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.BarkLevel = "unknown"
	if err := validateBarkConfig(cfg); err == nil {
		t.Fatal("bad level accepted")
	}
}

func aes128CBCDecrypt(key, iv, encrypted []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize || len(encrypted)%aes.BlockSize != 0 {
		return nil, errBadCiphertext{}
	}
	out := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, encrypted)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errBadCiphertext{}
	}
	return out[:len(out)-pad], nil
}

type errBadCiphertext struct{}

func (errBadCiphertext) Error() string { return "bad ciphertext" }

func TestBarkEncryptedPush(t *testing.T) {
	const key = "1234567890123456"
	var received struct {
		DeviceKey  string `json:"device_key"`
		Body       string `json:"body"`
		Ciphertext string `json:"ciphertext"`
		IV         string `json:"iv"`
	}
	var receivedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" || r.Method != http.MethodPost {
			t.Errorf("bad request: %s %s", r.Method, r.URL.Path)
		}
		var got struct {
			DeviceKey  string `json:"device_key"`
			Body       string `json:"body"`
			Ciphertext string `json:"ciphertext"`
			IV         string `json:"iv"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		receivedMu.Lock()
		received = got
		receivedMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	cfg := Config{
		BarkServerURL: srv.URL,
		BarkDeviceKey: "device-key",
		BarkKey:       key,
		BarkEncrypt:   true,
		BarkTitle:     "Msg-forwarder-Go",
		BarkSound:     "birdsong",
		BarkGroup:     "test",
		BarkLevel:     "active",
	}
	if err := newBark(cfg, srv.Client()).sendText("VK", "Иван", "привет"); err != nil {
		t.Fatal(err)
	}
	receivedMu.Lock()
	got := received
	receivedMu.Unlock()
	if got.DeviceKey != "device-key" || len(got.IV) != aes.BlockSize {
		t.Fatalf("bad envelope: %+v", got)
	}
	if got.Body == "" || got.Ciphertext == "" {
		t.Fatalf("empty ciphertext: %+v", got)
	}
	raw, err := base64.StdEncoding.DecodeString(got.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := aes128CBCDecrypt([]byte(key), []byte(got.IV), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"body":"привет"`, `"subtitle":"Иван"`, `"title":"Msg-forwarder-Go • VK"`, `"sound":"birdsong"`} {
		if !strings.Contains(string(plain), want) {
			t.Fatalf("plaintext %q does not contain %q", plain, want)
		}
	}
}
