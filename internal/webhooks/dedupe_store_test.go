package webhooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// TestHandlerRecordsDuplicateInStore checks that a repeated delivery gets its
// own Duplicate entry in a real store, whose unique index covers only the
// accepted entry.
func TestHandlerRecordsDuplicateInStore(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	tm := &core.Team{Name: "team"}
	if err := st.CreateTeam(ctx, tm); err != nil {
		t.Fatal(err)
	}
	f := &core.Factory{TeamID: tm.ID, Name: "Factory", Alias: "bot", Source: core.DefinitionManaged}
	if err := st.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	wh := core.Webhook{ID: "wh_db", FactoryID: f.ID, Name: "CI", AuthMode: AuthToken, SecretName: "TOK", Enabled: true}
	if err := st.ReplaceFactoryResources(ctx, f.ID, nil, nil, nil, []core.Webhook{wh}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(st, &fakeSecrets{values: map[string]string{"TOK": "whsec_token-value"}}, &fakeBus{}, Options{})
	mux := http.NewServeMux()
	h.Register(mux)
	for i, want := range []string{"accepted", "duplicate", "duplicate"} {
		req := httptest.NewRequest("POST", "/webhooks/w/wh_db", strings.NewReader(`{"same":true}`))
		req.Header.Set("Authorization", "Bearer whsec_token-value")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"status":"`+want+`"`) {
			t.Fatalf("delivery %d: %d %s, want %s", i, rr.Code, rr.Body.String(), want)
		}
	}
	list, err := st.ListWebhookDeliveries(ctx, "wh_db", 10)
	if err != nil {
		t.Fatal(err)
	}
	dups := 0
	for _, d := range list {
		if d.Status == StatusDeduplicated && d.Reason == ReasonDuplicate {
			dups++
		}
	}
	if len(list) != 3 || dups != 2 {
		t.Fatalf("log has %d entries with %d duplicates, want 3 with 2", len(list), dups)
	}
}
