// Package logtest decodes the JSON events internal/logging writes, for the logging packages' tests.
package logtest

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Events decodes one JSON object per non-blank line, failing t on anything else.
func Events(t testing.TB, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("log line %d is not a JSON object: %v", i+1, err)
		}
		if event == nil {
			t.Fatalf("log line %d is not a JSON object", i+1)
		}
		events = append(events, event)
	}
	return events
}
