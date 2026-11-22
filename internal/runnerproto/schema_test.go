package runnerproto

import (
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/testkit/schematest"
)

const schemaDir = "../../schemas/runner/v1"

// enumValues gives Fill a valid value for each enum type.
var enumValues = map[reflect.Type]string{
	reflect.TypeOf(EventType("")):   string(EventMessage),
	reflect.TypeOf(TurnOutcome("")): string(OutcomeDone),
	reflect.TypeOf(Capability("")):  string(CapInterrupt),
	reflect.TypeOf(CommandType("")): string(CommandTurnStart),
	reflect.TypeOf(EndReason("")):   string(EndFatal),
	reflect.TypeOf(MessageKind("")): string(KindUser),
	reflect.TypeOf(UploadKind("")):  string(UploadArtifact),
}

func fillEgress(r *StartResponse) *StartResponse {
	r.EgressLock = EgressLockRequired
	return r
}

// TestSchemas marshals a filled value and a zero value of every message type
// and validates them against schemas/runner/v1.
func TestSchemas(t *testing.T) {
	v := schematest.New(t, schemaDir)
	cases := []struct {
		ref    string
		filled any
		zero   any
	}{
		{"session.json#/$defs/StartRequest", schematest.Fill(&StartRequest{}, enumValues), StartRequest{InstanceID: "i", RunnerVersion: "1"}},
		{"session.json#/$defs/StartResponse", fillEgress(schematest.Fill(&StartResponse{}, enumValues)), StartResponse{}},
		{"session.json#/$defs/SecretsResponse", schematest.Fill(&SecretsResponse{}, enumValues), SecretsResponse{}},
		{"session.json#/$defs/EndRequest", schematest.Fill(&EndRequest{}, enumValues), EndRequest{Reason: EndIdle}},
		{"session.json#/$defs/TokenResponse", schematest.Fill(&TokenResponse{}, enumValues), TokenResponse{}},
		{"session.json#/$defs/ErrorBody", schematest.Fill(&ErrorBody{}, enumValues), ErrorBody{Code: CodeRunnerVersionMismatch}},
		{"events.json#/$defs/EventBatch", schematest.Fill(&EventBatch{}, enumValues), EventBatch{}},
		{"events.json#/$defs/EventAck", schematest.Fill(&EventAck{}, enumValues), EventAck{}},
		{"events.json#/$defs/SetupStepData", schematest.Fill(&SetupStepData{}, enumValues), SetupStepData{}},
		{"events.json#/$defs/SnapshotRestoredData", schematest.Fill(&SnapshotRestoredData{}, enumValues), SnapshotRestoredData{}},
		{"events.json#/$defs/SessionReadyData", schematest.Fill(&SessionReadyData{}, enumValues), SessionReadyData{}},
		{"events.json#/$defs/InputAcceptedData", schematest.Fill(&InputAcceptedData{}, enumValues), InputAcceptedData{}},
		{"events.json#/$defs/MessageData", schematest.Fill(&MessageData{}, enumValues), MessageData{}},
		{"events.json#/$defs/ToolCallData", schematest.Fill(&ToolCallData{}, enumValues), ToolCallData{}},
		{"events.json#/$defs/ToolResultData", schematest.Fill(&ToolResultData{}, enumValues), ToolResultData{}},
		{"events.json#/$defs/HarnessSessionData", schematest.Fill(&HarnessSessionData{}, enumValues), HarnessSessionData{}},
		{"events.json#/$defs/UsageData", schematest.Fill(&UsageData{}, enumValues), UsageData{}},
		{"events.json#/$defs/ArtifactData", schematest.Fill(&ArtifactData{}, enumValues), ArtifactData{}},
		{"events.json#/$defs/TranscriptSavedData", schematest.Fill(&TranscriptSavedData{}, enumValues), TranscriptSavedData{}},
		{"events.json#/$defs/DiagnosticData", schematest.Fill(&DiagnosticData{}, enumValues), DiagnosticData{}},
		{"events.json#/$defs/TurnEndedData", schematest.Fill(&TurnEndedData{}, enumValues), TurnEndedData{Outcome: OutcomeError}},
		{"events.json#/$defs/IdleData", schematest.Fill(&IdleData{}, enumValues), IdleData{}},
		{"events.json#/$defs/SnapshotSavedData", schematest.Fill(&SnapshotSavedData{}, enumValues), SnapshotSavedData{}},
		{"commands.json#/$defs/CommandsResponse", schematest.Fill(&CommandsResponse{}, enumValues), CommandsResponse{}},
		{"commands.json#/$defs/TurnStartData", schematest.Fill(&TurnStartData{}, enumValues), TurnStartData{}},
		{"commands.json#/$defs/MessageDeliverData", schematest.Fill(&MessageDeliverData{}, enumValues), MessageDeliverData{}},
		{"commands.json#/$defs/TurnInterruptData", schematest.Fill(&TurnInterruptData{}, enumValues), TurnInterruptData{}},
		{"commands.json#/$defs/SessionStopData", schematest.Fill(&SessionStopData{}, enumValues), SessionStopData{}},
		{"transfer.json#/$defs/UploadRequest", schematest.Fill(&UploadRequest{}, enumValues), UploadRequest{Kind: UploadSnapshot}},
		{"transfer.json#/$defs/UploadResponse", schematest.Fill(&UploadResponse{}, enumValues), UploadResponse{}},
		{"transfer.json#/$defs/DownloadsResponse", schematest.Fill(&DownloadsResponse{}, enumValues), DownloadsResponse{}},
		{"transfer.json#/$defs/GitCredentialRequest", schematest.Fill(&GitCredentialRequest{}, enumValues), GitCredentialRequest{}},
		{"transfer.json#/$defs/GitCredentialResponse", schematest.Fill(&GitCredentialResponse{}, enumValues), GitCredentialResponse{}},
		{"transfer.json#/$defs/CloudTokenRequest", schematest.Fill(&CloudTokenRequest{}, enumValues), CloudTokenRequest{}},
		{"transfer.json#/$defs/CloudTokenResponse", schematest.Fill(&CloudTokenResponse{}, enumValues), CloudTokenResponse{}},
		{"conversation.json#/$defs/Conversation", schematest.Fill(&Conversation{}, enumValues), Conversation{}},
	}
	for _, tc := range cases {
		v.MustValidate(tc.ref, tc.filled)
		v.MustValidate(tc.ref, tc.zero)
	}
	// The schemas refuse an unknown field and a wrong enum value.
	if err := v.ValidateJSON("session.json#/$defs/StartRequest", []byte(`{"instance_id":"i","runner_version":"1","sandbox_id":"","platform":{"os":"linux","arch":"amd64"},"extra":1}`)); err == nil {
		t.Error("an unknown field passed")
	}
	if err := v.ValidateJSON("session.json#/$defs/EndRequest", []byte(`{"reason":"bored","last_event_seq":1}`)); err == nil {
		t.Error("an unknown end reason passed")
	}
	if err := v.ValidateJSON("session.json#/$defs/StartResponse", []byte(`{"session_token":"t","session_token_expires_at":"2026-01-01T00:00:00Z","run_id":"r","session":1,"conversation_id":"c","workspace":{"repositories":[],"setup_commands":[],"setup_timeout_seconds":0,"skills":[],"snapshot":{"restore":true,"enabled":true}},"harness":{"type":"claude","model":"m","reasoning_level":"","system_prompt":"","resume_session_id":"","mcp_servers":{}},"env":{},"idle_timeout_seconds":0,"limits":{"max_session_seconds":1,"event_batch_bytes":1},"egress_lock":"maybe"}`)); err == nil {
		t.Error("an unknown egress_lock passed")
	}
}

// TestSchemaEnums checks that every enum value of the schemas is valid in Go.
func TestSchemaEnums(t *testing.T) {
	for _, e := range []EventType{EventSetupStep, EventSnapshotRestored, EventSessionReady, EventTurnStarted, EventInputAccepted, EventMessage, EventToolCall, EventToolResult, EventHarnessSession, EventUsage, EventArtifact, EventTranscriptSaved, EventDiagnostic, EventTurnEnded, EventIdle, EventSnapshotSaved} {
		v := schematest.New(t, schemaDir)
		if err := v.Validate("events.json#/$defs/Event", Event{Seq: 1, Type: e}); err != nil {
			t.Errorf("event type %q: %v", e, err)
		}
	}
}
