package assets

import (
	"encoding/binary"
	"errors"
	"strings"
)

var le = binary.LittleEndian

// cstr returns the NUL-terminated, lowercased string in b.
func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToLower(string(b))
}

// parseBSP reads a GoldSrc BSP v30: lump 0 is entity text, lump 2 is miptex; a texture whose
// four mip offsets are all 0 has its pixels in a WAD (external).
func parseBSP(b []byte) (ents []map[string]string, external []string, err error) {
	const hdr = 4 + 15*8
	if len(b) < hdr || le.Uint32(b) != 30 {
		return nil, nil, errors.New("not a BSP v30")
	}
	lump := func(i int) ([]byte, error) {
		off, n := int(le.Uint32(b[4+8*i:])), int(le.Uint32(b[8+8*i:]))
		if off < 0 || n < 0 || off > len(b) || n > len(b)-off {
			return nil, errors.New("bsp lump out of range")
		}
		return b[off : off+n], nil
	}
	et, err := lump(0)
	if err != nil {
		return nil, nil, err
	}
	ents = parseEntities(string(et))
	tl, err := lump(2)
	if err != nil {
		return nil, nil, err
	}
	if len(tl) < 4 {
		return ents, nil, nil
	}
	n := int(le.Uint32(tl))
	if n < 0 || 4+4*n > len(tl) {
		return nil, nil, errors.New("bsp miptex directory out of range")
	}
	for i := 0; i < n; i++ {
		o := int32(le.Uint32(tl[4+4*i:]))
		if o < 0 { // -1: texture absent
			continue
		}
		if int(o)+40 > len(tl) {
			return nil, nil, errors.New("bsp miptex out of range")
		}
		m := tl[o:]
		// an unnamed entry is no texture at all, nothing to look up
		if name := cstr(m[:16]); name != "" && le.Uint32(m[24:]) == 0 && le.Uint32(m[28:]) == 0 && le.Uint32(m[32:]) == 0 && le.Uint32(m[36:]) == 0 {
			external = append(external, name)
		}
	}
	return ents, external, nil
}

// parseEntities reads the `{ "key" "value" ... }` text. Strings have no escapes in GoldSrc.
func parseEntities(s string) (out []map[string]string) {
	var cur map[string]string
	var key string
	haveKey := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			cur, haveKey = map[string]string{}, false
		case '}':
			if cur != nil {
				out = append(out, cur)
			}
			cur = nil
		case '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return out
			}
			v := s[i+1 : i+1+j]
			i += j + 1
			if cur == nil {
				continue
			}
			if haveKey {
				cur[key] = v
			} else {
				key = v
			}
			haveKey = !haveKey
		}
	}
	return out
}

// wadLumps lists the lowercase texture names in a WAD3 directory.
func wadLumps(b []byte) (map[string]struct{}, error) {
	if len(b) < 12 || string(b[:4]) != "WAD3" {
		return nil, errors.New("not a WAD3")
	}
	n, dir := int(le.Uint32(b[4:])), int(le.Uint32(b[8:]))
	if n < 0 || dir < 0 || dir > len(b) || n > (len(b)-dir)/32 {
		return nil, errors.New("wad directory out of range")
	}
	names := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		names[cstr(b[dir+32*i+16:dir+32*i+32])] = struct{}{}
	}
	return names, nil
}
