package main

import (
	"testing"
	"unicode/utf8"
)

func TestVKIncomingPrivateOnly(t *testing.T) {
	base := []any{int64(4), int64(100), int64(0), int64(555), int64(1700000000), "hi"}

	if peer, id, text, ok := vkIncoming(base); !ok || peer != 555 || id != 100 || text != "hi" {
		t.Fatalf("plain incoming message rejected: %v %v %q %v", peer, id, text, ok)
	}

	important := append([]any(nil), base...)
	important[2] = int64(8) // important, still private
	if _, _, _, ok := vkIncoming(important); !ok {
		t.Fatal("important private message must not be filtered as a chat")
	}

	for _, flag := range []int64{2, 16, 128} { // outgoing, chat, deleted
		ev := append([]any(nil), base...)
		ev[2] = flag
		if _, _, _, ok := vkIncoming(ev); ok {
			t.Fatalf("event with flag %d must be skipped", flag)
		}
	}
}

func TestTruncateTelegramUnits(t *testing.T) {
	if got := truncateTelegram("hello", 5); got != "hello" {
		t.Fatalf("short text changed: %q", got)
	}
	got := truncateTelegram("😀😀😀", 4)
	if !utf8.ValidString(got) || got != "😀😀…" {
		t.Fatalf("bad astral truncation: %q", got)
	}
	got = truncateTelegram("привет", 3)
	if !utf8.ValidString(got) || got != "при…" {
		t.Fatalf("bad cyrillic truncation: %q", got)
	}
}

func TestMsgpackHugeLengthRejected(t *testing.T) {
	// array32 with 0xffffffff elements: must be rejected, not panic.
	if _, err := decodeMsgpack([]byte{0xdd, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Fatal("huge array length accepted")
	}
	// map32 with 0xffffffff pairs.
	if _, err := decodeMsgpack([]byte{0xdf, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Fatal("huge map length accepted")
	}
	// A short frame must not reserve memory for the declared item count.
	if _, err := decodeMsgpack([]byte{0x93, 0x01}); err == nil {
		t.Fatal("array length exceeding frame bytes accepted")
	}
	if _, err := decodeMsgpack([]byte{0x82, 0x01}); err == nil {
		t.Fatal("map length exceeding frame bytes accepted")
	}
}
