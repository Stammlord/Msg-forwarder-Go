package main

import (
	"fmt"
)

// lz4Decompress decodes a raw LZ4 block (no frame header), as used by MAX
// when the MAX frame factor byte is > 0. The output is capped to protect RAM.
func lz4Decompress(src []byte, max int) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("lz4: invalid limit")
	}
	dst := make([]byte, 0, minInt(max, len(src)*8))
	if cap(dst) < 64 {
		dst = make([]byte, 0, 64)
	}

	for i := 0; i < len(src); {
		token := src[i]
		i++

		litLen := int(token >> 4)
		if litLen == 15 {
			for {
				if i >= len(src) {
					return nil, fmt.Errorf("lz4: truncated literal length")
				}
				b := src[i]
				i++
				litLen += int(b)
				if litLen > max || b != 255 {
					break
				}
			}
		}
		if litLen > len(src)-i {
			return nil, fmt.Errorf("lz4: truncated literals")
		}
		if len(dst)+litLen > max {
			return nil, fmt.Errorf("lz4: output limit")
		}
		dst = append(dst, src[i:i+litLen]...)
		i += litLen

		if i >= len(src) {
			break
		}
		if i+2 > len(src) {
			return nil, fmt.Errorf("lz4: truncated offset")
		}
		offset := int(src[i]) | int(src[i+1])<<8
		i += 2
		if offset <= 0 || offset > len(dst) {
			return nil, fmt.Errorf("lz4: invalid offset")
		}

		matchLen := int(token&0x0f) + 4
		if token&0x0f == 15 {
			for {
				if i >= len(src) {
					return nil, fmt.Errorf("lz4: truncated match length")
				}
				b := src[i]
				i++
				matchLen += int(b)
				if matchLen > max || b != 255 {
					break
				}
			}
		}
		if len(dst)+matchLen > max {
			return nil, fmt.Errorf("lz4: output limit")
		}
		for j := 0; j < matchLen; j++ {
			dst = append(dst, dst[len(dst)-offset])
		}
	}

	return dst, nil
}
