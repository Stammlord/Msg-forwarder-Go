package main

import (
	"encoding/binary"
	"testing"
)

func TestMsgpackRoundTrip(t *testing.T) {
	in := map[string]any{
		"token": "abc",
		"n":     int64(42),
		"ok":    true,
		"user": map[string]any{
			"id": int64(9000000000003),
		},
		"items": []any{int64(1), "two", false},
	}
	decoded, err := decodeMsgpack(mpEncode(in))
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(decoded)
	if asString(m["token"]) != "abc" {
		t.Fatalf("bad token: %v", m["token"])
	}
	if v, ok := toInt64(m["n"]); !ok || v != 42 {
		t.Fatalf("bad n: %v", m["n"])
	}
	if v, ok := toInt64(nested(m, "user", "id")); !ok || v != 9000000000003 {
		t.Fatalf("bad nested id: %v", m["user"])
	}
	if got := asSlice(m["items"]); len(got) != 3 || asString(got[1]) != "two" {
		t.Fatalf("bad array: %v", got)
	}
}

func TestMsgpackInt64Ext(t *testing.T) {
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], 9000000000004)
	// ext8, size=9, type=1, payload = msgpack uint64 (0xcf + 8 bytes)
	data := []byte{0xc7, 9, 1, 0xcf}
	data = append(data, num[:]...)
	v, err := decodeMsgpack(data)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := toInt64(v)
	if !ok || got != 9000000000004 {
		t.Fatalf("bad ext int64: %v (%v)", v, ok)
	}
}

func TestLZ4LiteralAndMatch(t *testing.T) {
	got, err := lz4Decompress([]byte{0x50, 'h', 'e', 'l', 'l', 'o'}, 64)
	if err != nil || string(got) != "hello" {
		t.Fatalf("literals: %q err=%v", got, err)
	}

	// 1 literal 'a', then a match of length 7 at offset 1 => 8 x 'a'.
	got, err = lz4Decompress([]byte{0x13, 'a', 0x01, 0x00}, 64)
	if err != nil || string(got) != "aaaaaaaa" {
		t.Fatalf("match: %q err=%v", got, err)
	}
}

func TestUint24(t *testing.T) {
	b := make([]byte, 3)
	putUint24(b, 0x010203)
	if getUint24(b) != 0x010203 {
		t.Fatalf("bad uint24: %x", b)
	}
}
