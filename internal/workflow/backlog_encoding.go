package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Python's retained workflow uses json.dumps(sort_keys=True) with its default
// separators and ensure_ascii behavior for these durable content markers. Keep
// that encoding so a cutover continues to deduplicate markers already stored.
func backlogPythonMarkerDigest(value any) (string, error) {
	var encoded strings.Builder
	if err := backlogWritePythonJSON(&encoded, value); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(encoded.String()))
	return hex.EncodeToString(digest[:]), nil
}

func backlogWritePythonJSON(out *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		backlogWritePythonString(out, typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteString(", ")
			}
			backlogWritePythonString(out, key)
			out.WriteString(": ")
			if err := backlogWritePythonJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteString(", ")
			}
			if err := backlogWritePythonJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case []string:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteString(", ")
			}
			backlogWritePythonString(out, item)
		}
		out.WriteByte(']')
	case json.Number:
		if !strings.ContainsAny(string(typed), ".eE") {
			out.WriteString(string(typed))
			return nil
		}
		number, err := typed.Float64()
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("review marker input must contain finite JSON numbers")
		}
		text := strconv.FormatFloat(number, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		out.WriteString(text)
	case int:
		out.WriteString(strconv.Itoa(typed))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case int32:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return errors.New("review marker input must contain finite JSON numbers")
		}
		text := strconv.FormatFloat(typed, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		out.WriteString(text)
	default:
		return fmt.Errorf("review marker input contains unsupported type %T", value)
	}
	return nil
}

func backlogWritePythonString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if char < 0x20 {
				out.WriteString(fmt.Sprintf(`\u%04x`, char))
			} else if char > 0x7e {
				if char <= 0xffff {
					out.WriteString(fmt.Sprintf(`\u%04x`, char))
				} else {
					pair := utf16.Encode([]rune{char})
					out.WriteString(fmt.Sprintf(`\u%04x\u%04x`, pair[0], pair[1]))
				}
			} else {
				out.WriteRune(char)
			}
		}
	}
	out.WriteByte('"')
}
