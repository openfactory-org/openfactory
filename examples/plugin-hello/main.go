// Command openfactory-plugin-hello is an example Open Factory plugin built
// in a separate Go module on the public plugin SDK (pkg/plugin/sdk) only.
//
// It serves two components:
//
//   - a responder for the "hello" provider, which logs every outbound
//     message through the host;
//   - a webhook ingress at POST /plugins/hello/webhook, which turns a JSON
//     body {"team_id": "...", "factory_id": "...", "text": "..."} into an
//     inbound event of provider "hello", type "received".
//
// Build it and point the server at the directory:
//
//	go build -o /tmp/plugins/openfactory-plugin-hello .
//	OF_PLUGIN_DIR=/tmp/plugins openfactory serve
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
)

const provider = "hello"

// responder logs outbound messages instead of posting them anywhere.
type responder struct{ host sdk.Host }

func (responder) Provider() string { return provider }

func (r responder) Reply(ctx context.Context, teamID string, msg sdk.OutboundMessage) error {
	r.host.Log(ctx, "info", "hello reply", map[string]string{
		"team_id": teamID, "kind": msg.Kind, "text": msg.Text,
	})
	return nil
}

type webhookBody struct {
	TeamID    string `json:"team_id"`
	FactoryID string `json:"factory_id"`
	Text      string `json:"text"`
}

// webhook turns a JSON body into one inbound event.
func webhook(w http.ResponseWriter, r *http.Request) {
	var b webhookBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&b); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if b.TeamID == "" || b.Text == "" {
		http.Error(w, "team_id and text are required", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	sdk.Emit(r.Context(), &sdk.Event{
		ID:         fmt.Sprintf("hello-%d", now.UnixNano()),
		TeamID:     b.TeamID,
		FactoryID:  b.FactoryID,
		Provider:   provider,
		Type:       "received",
		Actor:      sdk.Actor{Provider: provider, ExternalID: "hello", Handle: "hello"},
		Source:     sdk.SourceRef{Provider: provider},
		Attributes: map[string]any{"text": b.Text},
		Text:       b.Text,
		ReceivedAt: now,
	})
	w.WriteHeader(http.StatusAccepted)
}

func main() {
	host := &sdk.DeferredHost{}
	sdk.Serve(sdk.Plugin{
		Name:       "hello",
		Version:    "0.1.0",
		Host:       host,
		Responders: []sdk.Responder{responder{host: host}},
		Ingress:    http.HandlerFunc(webhook),
	})
}
