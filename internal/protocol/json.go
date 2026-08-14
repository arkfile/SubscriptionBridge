package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const MaxJSONBytes = 1 << 20

// DecodeStrict reads a bounded body and unmarshals it with unknown-field rejection.
func DecodeStrict(r io.Reader, dest any, allowed []string) error {
	limited := io.LimitReader(r, MaxJSONBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(raw) > MaxJSONBytes {
		return ErrBodyTooLarge
	}
	return UnmarshalStrict(raw, dest, allowed)
}

// UnmarshalStrict rejects unknown, duplicate, missing, and trailing JSON fields.
func UnmarshalStrict(raw []byte, dest any, allowed []string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ErrInvalidJSON
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("%w: expected object", ErrInvalidJSON)
	}
	seen := make(map[string]struct{}, len(allowed))
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("%w: non-string key", ErrInvalidJSON)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateField, key)
		}
		seen[key] = struct{}{}
		if _, ok := allow[key]; !ok {
			return fmt.Errorf("%w: %s", ErrUnknownField, key)
		}
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if endDelim, ok := end.(json.Delim); !ok || endDelim != '}' {
		return fmt.Errorf("%w: unterminated object", ErrInvalidJSON)
	}
	if dec.More() {
		return ErrTrailingJSON
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrTrailingJSON
		}
		if err != io.EOF {
			return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
	}
	for _, name := range allowed {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("%w: %s", ErrMissingField, name)
		}
	}
	if err := RejectIdentityFields(seen); err != nil {
		return err
	}
	replay := json.NewDecoder(bytes.NewReader(trimmed))
	replay.UseNumber()
	replay.DisallowUnknownFields()
	if err := replay.Decode(dest); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return fmt.Errorf("%w: %v", ErrUnknownField, err)
		}
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if replay.More() {
		return ErrTrailingJSON
	}
	return nil
}

// skipValue advances the decoder past one JSON value during the allow-list pass.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{', '[':
			for dec.More() {
				if delim == '{' {
					if _, err := dec.Token(); err != nil {
						return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
					}
				}
				if err := skipValue(dec); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
			}
			want := json.Delim('}')
			if delim == '[' {
				want = json.Delim(']')
			}
			if end != want {
				return ErrInvalidJSON
			}
		default:
			return ErrInvalidJSON
		}
	}
	return nil
}

// MarshalCompact encodes JSON without HTML escaping or a trailing newline.
func MarshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return out, nil
}
