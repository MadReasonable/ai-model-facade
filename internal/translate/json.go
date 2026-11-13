package translate

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file holds the JSON helpers every translator uses: objects built from
// raw members in order, sjson setters for constant paths, and the checks on
// request values that translation decisions rest on.

// members is a JSON object built from raw keys and values, in order.
type members struct{ buf []byte }

func (m *members) add(rawKey, rawValue string) {
	if m.buf == nil {
		m.buf = []byte{'{'}
	} else {
		m.buf = append(m.buf, ',')
	}
	m.buf = append(m.buf, rawKey...)
	m.buf = append(m.buf, ':')
	m.buf = append(m.buf, rawValue...)
}

func (m *members) bytes() []byte {
	if m.buf == nil {
		return []byte("{}")
	}
	return append(m.buf, '}')
}

// setRaw and set build blocks at constant paths, which sjson can't fail on.
func setRaw(dst, path, raw string) string {
	out, _ := sjson.SetRaw(dst, path, raw)
	return out
}

func set(dst, path string, value any) string {
	out, _ := sjson.Set(dst, path, value)
	return out
}

// pathKey escapes a key from the request as one path segment. sjson also
// reads a leading ':' as its force-key marker, which gjson.Escape leaves.
func pathKey(key string) string {
	p := gjson.Escape(key)
	if strings.HasPrefix(p, ":") {
		p = `\` + p
	}
	return p
}

func base64DataURL(url string) (mediaType, data string, ok bool) {
	header, data, found := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	mediaType, encoding, _ := strings.Cut(header, ";")
	if !strings.HasPrefix(url, "data:") || !found || encoding != "base64" {
		return "", "", false
	}
	return mediaType, data, true
}
