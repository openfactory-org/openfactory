package github

import "testing"

// The events of one delivery share one action key, and
// the product's own app is marked.
func TestNormalizeActionKey(t *testing.T) {
	body := []byte(`{"action":"created","repository":{"full_name":"acme/api"},
		"sender":{"id":7,"login":"ourapp[bot]","type":"Bot"},
		"issue":{"number":3,"title":"t","user":{"login":"a"}},
		"comment":{"id":1,"body":"hi @payments","user":{"login":"ourapp[bot]"}}}`)
	evs, err := Normalizer{AppSlug: "ourapp"}.Normalize("issue_comment", "del-1", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d, want a comment and a mention", len(evs))
	}
	for _, ev := range evs {
		if ev.ActionKey != "github:del-1" {
			t.Errorf("%s: action key = %q", ev.Type, ev.ActionKey)
		}
		if ev.DeliveryID != "del-1:"+ev.Type {
			t.Errorf("%s: delivery id = %q", ev.Type, ev.DeliveryID)
		}
		if !ev.Actor.IsSelf {
			t.Errorf("%s: the own app is not marked", ev.Type)
		}
	}
}
