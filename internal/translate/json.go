package translate

import (
	"bytes"
	"encoding/json"
	"slices"
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

// bytesContain reports whether body holds a quoted key, which can only match
// a real key, since quotes inside a JSON string are escaped.
func bytesContain(body []byte, key string) bool {
	return bytes.Contains(body, []byte(`"`+key+`"`))
}

// isSet reports whether a field asks for something: present, and not null,
// false or empty.
func isSet(value gjson.Result) bool {
	switch {
	case !value.Exists(), value.Type == gjson.Null, value.Type == gjson.False:
		return false
	case value.IsArray():
		return len(value.Array()) > 0
	case value.Type == gjson.String:
		return value.String() != ""
	}
	return true
}

// reportSettings reports the settings of an object field other than those
// translated, as field.setting.
func reportSettings(gone *droppedFields, field string, value gjson.Result, translated ...string) {
	value.ForEach(func(setting, v gjson.Result) bool {
		if s := setting.String(); !slices.Contains(translated, s) && v.Type != gjson.Null {
			gone.add(field + "." + s)
		}
		return true
	})
}

// setPresent sets key to value, if the request gave one.
func setPresent(dst, key string, value gjson.Result) string {
	if !value.Exists() || value.Type == gjson.Null {
		return dst
	}
	return setRaw(dst, key, value.Raw)
}

func rawItems(list gjson.Result) []json.RawMessage {
	var items []json.RawMessage
	for _, item := range list.Array() {
		items = append(items, json.RawMessage(item.Raw))
	}
	return items
}
