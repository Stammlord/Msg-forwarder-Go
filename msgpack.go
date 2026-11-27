package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// Minimal MessagePack codec. Only the subset used by MAX is implemented:
// nil/bool/int/string/bin/array/map/float and extension type 1 (int64 ids).

const (
	mpMaxDepth     = 16
	mpMaxItems     = 4096
	mpMaxItemBytes = 1 << 20
	mpAllocCap     = 64
	mpBudgetItems  = 2 * mpMaxItems
)

// mpState limits the total number of allocations while decoding one message.
type mpState struct {
	items int
}

func (s *mpState) addItems(n int) error {
	if n <= 0 {
		return nil
	}
	if n > mpBudgetItems-s.items {
		return fmt.Errorf("msgpack: too many items")
	}
	s.items += n
	return nil
}

func decodeMsgpack(b []byte) (any, error) {
	st := &mpState{}
	v, off, err := mpValueState(b, 0, 0, st)
	if err != nil {
		return nil, err
	}
	if off != len(b) {
		return nil, fmt.Errorf("msgpack: trailing bytes")
	}
	return v, nil
}

func mpNeed(b []byte, off, n int) error {
	if n < 0 || off < 0 || off > len(b) || n > len(b)-off {
		return fmt.Errorf("msgpack: unexpected eof")
	}
	return nil
}

func mpReadUint(b []byte, off, n int) (uint64, int, error) {
	if err := mpNeed(b, off, n); err != nil {
		return 0, off, err
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(b[off+i])
	}
	return v, off + n, nil
}

func mpString(b []byte, off, n int) (string, int, error) {
	return mpStringState(b, off, n, &mpState{})
}

func mpStringState(b []byte, off, n int, st *mpState) (string, int, error) {
	if n > mpMaxItemBytes {
		return "", off, fmt.Errorf("msgpack: string too large")
	}
	if err := mpNeed(b, off, n); err != nil {
		return "", off, err
	}
	if err := st.addItems(1); err != nil {
		return "", off, err
	}
	return string(b[off : off+n]), off + n, nil
}

func mpBin(b []byte, off, n int) ([]byte, int, error) {
	return mpBinState(b, off, n, &mpState{})
}

func mpBinState(b []byte, off, n int, st *mpState) ([]byte, int, error) {
	if n > mpMaxItemBytes {
		return nil, off, fmt.Errorf("msgpack: bin too large")
	}
	if err := mpNeed(b, off, n); err != nil {
		return nil, off, err
	}
	if err := st.addItems(1); err != nil {
		return nil, off, err
	}
	out := make([]byte, n)
	copy(out, b[off:off+n])
	return out, off + n, nil
}

func mpArray(b []byte, off, n, depth int) ([]any, int, error) {
	return mpArrayState(b, off, n, depth, &mpState{})
}

func mpArrayState(b []byte, off, n, depth int, st *mpState) ([]any, int, error) {
	// n can become negative on 32-bit platforms after converting a large
	// uint32 length; checking only the upper bound would then panic in make.
	if n < 0 || n > mpMaxItems {
		return nil, off, fmt.Errorf("msgpack: too many items")
	}
	if off < 0 || off > len(b) || n > len(b)-off {
		return nil, off, fmt.Errorf("msgpack: unexpected eof")
	}
	if err := st.addItems(n); err != nil {
		return nil, off, err
	}
	// A frame cannot hold more elements than bytes; never trust the wire
	// length for the initial allocation.
	out := make([]any, 0, minInt(n, mpAllocCap))
	for i := 0; i < n; i++ {
		v, noff, err := mpValueState(b, off, depth+1, st)
		if err != nil {
			return nil, off, err
		}
		out = append(out, v)
		off = noff
	}
	return out, off, nil
}

func mpMap(b []byte, off, n, depth int) (map[string]any, int, error) {
	return mpMapState(b, off, n, depth, &mpState{})
}

func mpMapState(b []byte, off, n, depth int, st *mpState) (map[string]any, int, error) {
	if n < 0 || n > mpMaxItems {
		return nil, off, fmt.Errorf("msgpack: too many items")
	}
	// Every pair needs at least one byte for the key and one for the value.
	if off < 0 || off > len(b) || n > (len(b)-off)/2 {
		return nil, off, fmt.Errorf("msgpack: unexpected eof")
	}
	if err := st.addItems(n); err != nil {
		return nil, off, err
	}
	out := make(map[string]any, minInt(n, mpAllocCap))
	for i := 0; i < n; i++ {
		k, noff, err := mpValueState(b, off, depth+1, st)
		if err != nil {
			return nil, off, err
		}
		off = noff
		v, noff, err := mpValueState(b, off, depth+1, st)
		if err != nil {
			return nil, off, err
		}
		off = noff
		out[mpKey(k)] = v
	}
	return out, off, nil
}

func mpKey(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return ""
	}
}

func mpExt(b []byte, off, size int, typ byte, depth int) (any, int, error) {
	return mpExtState(b, off, size, typ, depth, &mpState{})
}

func mpExtState(b []byte, off, size int, typ byte, depth int, st *mpState) (any, int, error) {
	if size < 0 || size > mpMaxItemBytes {
		return nil, off, fmt.Errorf("msgpack: ext too large")
	}
	if err := mpNeed(b, off, size+1); err != nil {
		return nil, off, err
	}
	if err := st.addItems(1); err != nil {
		return nil, off, err
	}
	typ = b[off]
	off++
	data := b[off : off+size]
	off += size
	if typ == 1 {
		v, n, err := mpValueState(data, 0, depth+1, st)
		if err != nil || n != len(data) {
			return nil, off, fmt.Errorf("msgpack: invalid int64 ext")
		}
		return v, off, nil
	}
	out := make([]byte, size)
	copy(out, data)
	return out, off, nil
}

func mpValue(b []byte, off, depth int) (any, int, error) {
	return mpValueState(b, off, depth, &mpState{})
}

func mpValueState(b []byte, off, depth int, st *mpState) (any, int, error) {
	if depth > mpMaxDepth {
		return nil, off, fmt.Errorf("msgpack: depth limit")
	}
	if off >= len(b) {
		return nil, off, fmt.Errorf("msgpack: eof")
	}
	c := b[off]
	off++

	switch {
	case c <= 0x7f:
		return int64(c), off, nil
	case c >= 0xe0:
		return int64(int8(c)), off, nil
	case c >= 0xa0 && c <= 0xbf:
		return mpStringState(b, off, int(c&0x1f), st)
	case c >= 0x90 && c <= 0x9f:
		return mpArrayState(b, off, int(c&0x0f), depth, st)
	case c >= 0x80 && c <= 0x8f:
		return mpMapState(b, off, int(c&0x0f), depth, st)
	}

	switch c {
	case 0xc0:
		return nil, off, nil
	case 0xc2:
		return false, off, nil
	case 0xc3:
		return true, off, nil

	case 0xc4, 0xc5, 0xc6:
		nbytes := 1
		if c == 0xc5 {
			nbytes = 2
		} else if c == 0xc6 {
			nbytes = 4
		}
		n, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return mpBinState(b, noff, int(n), st)

	case 0xca, 0xcb:
		nbytes := 4
		if c == 0xcb {
			nbytes = 8
		}
		v, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		if nbytes == 4 {
			return float64(math.Float32frombits(uint32(v))), noff, nil
		}
		return math.Float64frombits(v), noff, nil

	case 0xcc, 0xcd, 0xce, 0xcf:
		nbytes := 1 << (c - 0xcc)
		v, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return v, noff, nil

	case 0xd0, 0xd1, 0xd2, 0xd3:
		nbytes := 1 << (c - 0xd0)
		v, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		var signed int64
		switch nbytes {
		case 1:
			signed = int64(int8(byte(v)))
		case 2:
			signed = int64(int16(uint16(v)))
		case 4:
			signed = int64(int32(uint32(v)))
		default:
			signed = int64(v)
		}
		return signed, noff, nil

	case 0xd9, 0xda, 0xdb:
		nbytes := 1
		if c == 0xda {
			nbytes = 2
		} else if c == 0xdb {
			nbytes = 4
		}
		n, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return mpStringState(b, noff, int(n), st)

	case 0xdc, 0xdd:
		nbytes := 2
		if c == 0xdd {
			nbytes = 4
		}
		n, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return mpArrayState(b, noff, int(n), depth, st)

	case 0xde, 0xdf:
		nbytes := 2
		if c == 0xdf {
			nbytes = 4
		}
		n, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return mpMapState(b, noff, int(n), depth, st)

	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8:
		size := 1 << (c - 0xd4)
		return mpExtState(b, off, size, 0, depth, st)

	case 0xc7, 0xc8, 0xc9:
		nbytes := 1
		if c == 0xc8 {
			nbytes = 2
		} else if c == 0xc9 {
			nbytes = 4
		}
		n, noff, err := mpReadUint(b, off, nbytes)
		if err != nil {
			return nil, off, err
		}
		return mpExtState(b, noff, int(n), 0, depth, st)
	}

	return nil, off, fmt.Errorf("msgpack: unsupported byte 0x%02x", c)
}

func appendMPInt(b []byte, v int64) []byte {
	if v >= 0 {
		u := uint64(v)
		switch {
		case u <= 0x7f:
			return append(b, byte(u))
		case u <= 0xff:
			return append(b, 0xcc, byte(u))
		case u <= 0xffff:
			return append(b, 0xcd, byte(u>>8), byte(u))
		case u <= 0xffffffff:
			return append(b, 0xce, byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
		default:
			var t [8]byte
			binary.BigEndian.PutUint64(t[:], u)
			return append(b, append([]byte{0xcf}, t[:]...)...)
		}
	}
	switch {
	case v >= -32:
		return append(b, byte(int8(v)))
	case v >= -128:
		return append(b, 0xd0, byte(int8(v)))
	case v >= -32768:
		var t [2]byte
		binary.BigEndian.PutUint16(t[:], uint16(int16(v)))
		return append(b, append([]byte{0xd1}, t[:]...)...)
	case v >= -2147483648:
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], uint32(int32(v)))
		return append(b, append([]byte{0xd2}, t[:]...)...)
	default:
		var t [8]byte
		binary.BigEndian.PutUint64(t[:], uint64(v))
		return append(b, append([]byte{0xd3}, t[:]...)...)
	}
}

func appendMPString(b []byte, s string) []byte {
	n := len(s)
	switch {
	case n <= 31:
		b = append(b, byte(0xa0|n))
	case n <= 0xff:
		b = append(b, 0xd9, byte(n))
	case n <= 0xffff:
		b = append(b, 0xda, byte(n>>8), byte(n))
	default:
		b = append(b, 0xdb, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(b, s...)
}

func appendMP(b []byte, v any) []byte {
	return appendMPDepth(b, v, 0)
}

// appendMPDepth mirrors the decoder's depth and size limits so a pathological
// local payload cannot blow the stack or produce a frame larger than MAX
// accepts. Reaching a limit stops adding data instead of failing the send.
func appendMPDepth(b []byte, v any, depth int) []byte {
	if depth > mpMaxDepth || len(b) > 0xffffff {
		return b
	}
	switch x := v.(type) {
	case nil:
		return append(b, 0xc0)
	case bool:
		if x {
			return append(b, 0xc3)
		}
		return append(b, 0xc2)
	case int:
		return appendMPInt(b, int64(x))
	case int8:
		return appendMPInt(b, int64(x))
	case int16:
		return appendMPInt(b, int64(x))
	case int32:
		return appendMPInt(b, int64(x))
	case int64:
		return appendMPInt(b, x)
	case uint:
		return appendMPInt(b, int64(x))
	case uint8:
		return appendMPInt(b, int64(x))
	case uint16:
		return appendMPInt(b, int64(x))
	case uint32:
		return appendMPInt(b, int64(x))
	case uint64:
		if x > uint64(math.MaxInt64) {
			b = append(b, 0xcf, byte(x>>56), byte(x>>48), byte(x>>40), byte(x>>32),
				byte(x>>24), byte(x>>16), byte(x>>8), byte(x))
			return b
		}
		return appendMPInt(b, int64(x))
	case float32:
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], math.Float32bits(x))
		return append(b, append([]byte{0xca}, t[:]...)...)
	case float64:
		var t [8]byte
		binary.BigEndian.PutUint64(t[:], math.Float64bits(x))
		return append(b, append([]byte{0xcb}, t[:]...)...)
	case string:
		return appendMPString(b, x)
	case []byte:
		n := len(x)
		switch {
		case n <= 0xff:
			b = append(b, 0xc4, byte(n))
		case n <= 0xffff:
			b = append(b, 0xc5, byte(n>>8), byte(n))
		default:
			b = append(b, 0xc6, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		}
		return append(b, x...)
	case []any:
		n := len(x)
		if n <= 15 {
			b = append(b, byte(0x90|n))
		} else if n <= 0xffff {
			b = append(b, 0xdc, byte(n>>8), byte(n))
		} else {
			b = append(b, 0xdd, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		}
		for _, item := range x {
			b = appendMPDepth(b, item, depth+1)
		}
		return b
	case map[string]any:
		n := len(x)
		if n <= 15 {
			b = append(b, byte(0x80|n))
		} else if n <= 0xffff {
			b = append(b, 0xde, byte(n>>8), byte(n))
		} else {
			b = append(b, 0xdf, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		}
		// Map order is irrelevant to the server; Go map iteration is fine here.
		for k, val := range x {
			b = appendMPString(b, k)
			b = appendMPDepth(b, val, depth+1)
		}
		return b
	default:
		return appendMPString(b, fmt.Sprint(x))
	}
}

func mpEncode(v any) []byte {
	return appendMP(make([]byte, 0, 256), v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case uint64:
		if x <= uint64(math.MaxInt64) {
			return int64(x), true
		}
	case int:
		return int64(x), true
	case float64:
		if x > 9.2233720368547758e18 || x < -9.2233720368547758e18 || x != math.Trunc(x) {
			return 0, false
		}
		return int64(x), true
	case float32:
		f := float64(x)
		if f > 9.2233720368547758e18 || f < -9.2233720368547758e18 || f != math.Trunc(f) {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

func nested(v any, keys ...string) any {
	m := asMap(v)
	for i, k := range keys {
		if i == len(keys)-1 {
			return m[k]
		}
		m = asMap(m[k])
		if m == nil {
			return nil
		}
	}
	return nil
}
