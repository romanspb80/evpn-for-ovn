package ownership

import "testing"

func TestRejectDuplicateJSONKeys(t *testing.T) {
	for _, raw := range []string{
		`{"Version":2,"Version":1}`,
		`{"Trust":{"Denial":2,"denial":0}}`,
		`{"Receipts":[{"PathUUID":"a","Path\u0055UID":"b"}]}`,
		`{"x":1} {"x":2}`,
		`{"x":[1,}`,
	} {
		if err := RejectDuplicateJSONKeys([]byte(raw)); err == nil {
			t.Fatalf("ambiguous or malformed record accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		`null`,
		`{"Version":2,"Trust":{"Denial":18446744073709551615},"Receipts":[]}`,
		`[{"Version":1},{"Version":2}]`,
	} {
		if err := RejectDuplicateJSONKeys([]byte(raw)); err != nil {
			t.Fatalf("valid record rejected: %s: %v", raw, err)
		}
	}
}
