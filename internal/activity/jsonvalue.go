package activity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var errNotObject = errors.New("JSON value is not an object")

// skipValue consumes one JSON value that the caller does not need.
type skipValue struct{}

func (skipValue) UnmarshalJSON([]byte) error { return nil }

// objectFields returns the raw values of the requested keys of one JSON
// object. A key that occurs more than once is reported in duplicates instead
// of being silently resolved to its last value, as encoding/json would do.
// Other members are syntax-checked and skipped.
func objectFields(raw []byte, wanted func(string) bool) (fields map[string]json.RawMessage, duplicates []string, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, nil, errNotObject
	}
	fields = make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, errNotObject
		}
		if !wanted(key) {
			// Syntax-check without retaining a copy: args can be several MiB.
			if err := dec.Decode(&skipValue{}); err != nil {
				return nil, nil, err
			}
			continue
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, err
		}
		if _, seen := fields[key]; seen {
			duplicates = append(duplicates, key)
			continue
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil { // Closing '}'.
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("unexpected data after JSON object")
	}
	return fields, duplicates, nil
}

// exactString decodes a JSON string literal only if the result represents the
// recorded value exactly. encoding/json silently replaces invalid UTF-8 and
// unpaired UTF-16 surrogate escapes with U+FFFD, which could merge distinct
// client-supplied identifiers; such values are reported as not exact instead.
func exactString(raw json.RawMessage) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	body := raw[1 : len(raw)-1]
	var out strings.Builder
	out.Grow(len(body))
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == '\\':
			if i+1 >= len(body) {
				return "", false
			}
			switch e := body[i+1]; e {
			case '"', '\\', '/':
				out.WriteByte(e)
			case 'b':
				out.WriteByte('\b')
			case 'f':
				out.WriteByte('\f')
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'u':
				r, ok := hex4(body[i+2:])
				if !ok {
					return "", false
				}
				i += 6
				if utf16.IsSurrogate(r) {
					if r >= 0xDC00 || i+6 > len(body) || body[i] != '\\' || body[i+1] != 'u' {
						return "", false
					}
					low, ok := hex4(body[i+2:])
					if !ok || low < 0xDC00 || low > 0xDFFF {
						return "", false
					}
					r = utf16.DecodeRune(r, low)
					i += 6
				}
				out.WriteRune(r)
				continue
			default:
				return "", false
			}
			i += 2
		case c < 0x20 || c == '"':
			return "", false
		default:
			r, size := utf8.DecodeRune(body[i:])
			if r == utf8.RuneError && size == 1 {
				return "", false
			}
			out.Write(body[i : i+size])
			i += size
		}
	}
	return out.String(), true
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}
