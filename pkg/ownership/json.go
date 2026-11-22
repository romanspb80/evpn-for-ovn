package ownership

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// RejectDuplicateJSONKeys rejects ambiguous resource records before Go's
// case-insensitive struct decoder can silently replace an earlier field.
func RejectDuplicateJSONKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[strings.ToLower(key)] {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[strings.ToLower(key)] = true
				if err := value(); err != nil {
					return err
				}
			}
			token, err = d.Token()
			if err != nil || token != json.Delim('}') {
				return fmt.Errorf("unterminated JSON object")
			}
		case json.Delim('['):
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
			token, err = d.Token()
			if err != nil || token != json.Delim(']') {
				return fmt.Errorf("unterminated JSON array")
			}
		default:
			if _, delimiter := token.(json.Delim); delimiter {
				return fmt.Errorf("unexpected JSON delimiter")
			}
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}
