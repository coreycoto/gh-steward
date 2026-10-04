// Package contract provides the bounded machine-input and identity contracts.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Object = map[string]any

const MaxJSONBytes = 16 * 1024 * 1024
const maxDepth = 64

// Decode rejects duplicate keys, nonfinite numbers, multiple documents and
// oversized/deep inputs. Numbers retain their lexical representation.
func Decode(r io.Reader) (Object, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxJSONBytes {
		return nil, errors.New("JSON input exceeds the supported size")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("JSON input must contain exactly one document")
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("JSON input must be an object")
	}
	return o, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("JSON nesting exceeds the supported depth")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		o := Object{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("JSON object key must be a string")
			}
			if _, exists := o[name]; exists {
				return nil, fmt.Errorf("duplicate JSON object key %q", name)
			}
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			o[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid JSON object")
		}
		return o, nil
	case '[':
		a := []any{}
		for d.More() {
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid JSON array")
		}
		return a, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// Canonical uses the Go contract's UTF-8 JSON encoding. It is deliberately a
// new contract; Python-v1 plan hashes and mutation journals are not accepted.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func Digest(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func Clone(o Object) (Object, error) {
	b, err := Canonical(o)
	if err != nil {
		return nil, err
	}
	return Decode(bytes.NewReader(b))
}

func String(o Object, key string) (string, error) {
	v, ok := o[key].(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return v, nil
}

func Nonempty(o Object, key string) (string, error) {
	v, err := String(o, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s must be nonempty", key)
	}
	return v, nil
}

func ObjectAt(o Object, key string) (Object, error) {
	v, ok := o[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return v, nil
}

func Array(o Object, key string) ([]any, error) {
	v, ok := o[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	return v, nil
}

func Objects(o Object, key string) ([]Object, error) {
	a, err := Array(o, key)
	if err != nil {
		return nil, err
	}
	result := make([]Object, 0, len(a))
	for _, raw := range a {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s entries must be objects", key)
		}
		result = append(result, item)
	}
	return result, nil
}

func Bool(o Object, key string) (bool, error) {
	v, ok := o[key].(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
}

var integerPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func Integer(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		if !integerPattern.MatchString(string(n)) {
			break
		}
		return strconv.ParseInt(string(n), 10, 64)
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), nil
		}
	}
	return 0, errors.New("value must be an integer in supported range")
}

func PositiveInteger(v any) (int64, error) {
	n, err := Integer(v)
	if err != nil || n < 1 {
		return 0, errors.New("value must be a positive integer")
	}
	return n, nil
}

func Number(v any) (float64, error) {
	var n float64
	switch raw := v.(type) {
	case json.Number:
		parsed, err := raw.Float64()
		if err != nil {
			return 0, err
		}
		n = parsed
	case float64:
		n = raw
	case int:
		n = float64(raw)
	case int64:
		n = float64(raw)
	default:
		return 0, errors.New("value must be a number")
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("number must be finite")
	}
	return n, nil
}

func Strings(v any) ([]string, error) {
	a, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]string); ok {
			return append([]string{}, typed...), nil
		}
		return nil, errors.New("value must be an array of strings")
	}
	result := make([]string, 0, len(a))
	for _, value := range a {
		s, ok := value.(string)
		if !ok {
			return nil, errors.New("array entries must be strings")
		}
		result = append(result, s)
	}
	return result, nil
}

func StringSet(v any) ([]string, error) {
	a, err := Strings(v)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []string{}
	for _, s := range a {
		if strings.TrimSpace(s) == "" || seen[s] {
			return nil, errors.New("names must be nonempty and unique")
		}
		seen[s] = true
		result = append(result, s)
	}
	sort.Strings(result)
	return result, nil
}

type Repository struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var hostPart = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

func ParseRepository(o Object) (Repository, error) {
	name, ok := o["nameWithOwner"].(string)
	if _, present := o["nameWithOwner"]; present && !ok {
		return Repository{}, errors.New("repository nameWithOwner must be text")
	}
	if !ok {
		owner, oo := o["owner"].(string)
		repository, nn := o["name"].(string)
		if !oo || !nn {
			return Repository{}, errors.New("repository must identify owner/name")
		}
		name = owner + "/" + repository
	}
	rawURL, err := String(o, "url")
	if err != nil {
		return Repository{}, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Repository{}, errors.New("invalid repository URL")
	}
	parts := strings.Split(name, "/")
	if len(parts) != 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return Repository{}, errors.New("invalid repository owner/name")
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(strings.Trim(u.EscapedPath(), "/"), name) {
		return Repository{}, errors.New("repository URL must identify its exact HTTPS host and owner/name")
	}
	host, owner, repository := strings.ToLower(u.Hostname()), strings.ToLower(parts[0]), strings.ToLower(parts[1])
	validHost := len(host) <= 253
	for _, label := range strings.Split(host, ".") {
		validHost = validHost && len(label) <= 63 && hostPart.MatchString(label)
	}
	if !validHost {
		return Repository{}, errors.New("invalid repository hostname")
	}
	if raw, exists := o["host"]; exists {
		configured, ok := raw.(string)
		if !ok || !strings.EqualFold(configured, host) {
			return Repository{}, errors.New("repository host does not match its URL")
		}
	}
	if raw, exists := o["owner"]; exists {
		configured, textOwner := raw.(string)
		if !textOwner {
			providerOwner, objectOwner := raw.(Object)
			if !objectOwner || (providerOwner["__typename"] != "User" && providerOwner["__typename"] != "Organization") {
				return Repository{}, errors.New("repository owner must be text or a typed GitHub owner")
			}
			configured, textOwner = providerOwner["login"].(string)
		}
		if !textOwner || !strings.EqualFold(configured, owner) {
			return Repository{}, errors.New("repository owner conflicts with nameWithOwner")
		}
	}
	if raw, exists := o["name"]; exists {
		if configured, ok := raw.(string); !ok || !strings.EqualFold(configured, repository) {
			return Repository{}, errors.New("repository name conflicts with nameWithOwner")
		}
	}
	return Repository{Host: host, Owner: owner, Name: repository, URL: "https://" + host + "/" + owner + "/" + repository}, nil
}

func (r Repository) FullName() string { return r.Owner + "/" + r.Name }
func (r Repository) Object() Object {
	return Object{"host": r.Host, "owner": r.Owner, "name": r.Name, "url": r.URL, "nameWithOwner": r.FullName()}
}
