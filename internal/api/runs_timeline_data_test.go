package api

import (
	"encoding/json"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// A runner event whose data is JSON null must not panic (it once set the map to nil).
func TestEventDataNull(t *testing.T) {
	for _, data := range []string{"null", "", "[1]", `{"a":1}`} {
		got := eventData(core.RunnerEvent{TurnID: "t1", Data: json.RawMessage(data)})
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil || m["turn_id"] != "t1" {
			t.Fatalf("data %q: got %s (%v)", data, got, err)
		}
	}
}
