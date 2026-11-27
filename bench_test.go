package main

import (
	"encoding/binary"
	"testing"
)

func benchMaxFrame() []byte {
	payload := map[string]any{
		"chatId": int64(123456789),
		"message": map[string]any{
			"id":     int64(987654321),
			"sender": int64(555000111),
			"time":   int64(1700000000000),
			"cid":    int64(42),
			"type":   "TEXT",
			"text":   "Привет, это тестовое сообщение длиной примерно сто символов, чтобы декодер работал как в реальности.",
		},
	}
	p := mpEncode(payload)
	frame := make([]byte, 10+len(p))
	frame[0] = maxProtoVersion
	binary.BigEndian.PutUint16(frame[2:4], 7)
	binary.BigEndian.PutUint16(frame[4:6], 128)
	putUint24(frame[7:10], len(p))
	copy(frame[10:], p)
	return frame
}

func BenchmarkParseMaxFrame(b *testing.B) {
	frame := benchMaxFrame()
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		if _, err := parseMaxFrame(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVKIncoming(b *testing.B) {
	ev := []any{int64(4), int64(100), int64(0), int64(555), int64(1700000000), "hello"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, _, ok := vkIncoming(ev); !ok {
			b.Fatal("event rejected")
		}
	}
}
