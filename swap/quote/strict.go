package quote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// CheckKeys reports whether a JSON value from the swap service can be
// decoded safely: every object key is lower-case snake case and appears
// once. Go's decoder matches keys in any case and keeps the last match,
// while JavaScript reads them exactly, so an answer carrying both
// "amount_out" and "AMOUNT_OUT" would show the user one amount and give the
// wallet another. Every key of this protocol is snake case.
func CheckKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := checkKeys(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err == nil {
		return errors.New("trailing data")
	}
	return nil
}

func checkKeys(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("nested too deep")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if !snakeKey(k) || seen[k] {
				return fmt.Errorf("unexpected field %q", k)
			}
			seen[k] = true
			if err := checkKeys(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for d.More() {
			if err := checkKeys(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	return nil
}

func snakeKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if c := k[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
