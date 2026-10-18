package core

import (
	"errors"
	"testing"
)

type keyed string

func (k keyed) OutboxPartitionKey() string { return string(k) }

func TestOutboxPartitionKey(t *testing.T) {
	pr := SourceRef{Provider: "forge", Kind: "pull_request", Repo: "acme/api", Number: 7}
	comment := &Event{TeamID: "team1", Type: "review_comment", Source: pr}
	merge := Event{TeamID: "team1", Type: "pull_request_merged", Source: pr}
	other := &Event{TeamID: "team2", Source: pr}

	k := OutboxPartitionKey(TopicInboundEvent, comment)
	if k == "" {
		t.Fatal("an event with a source has a key")
	}
	if got := OutboxPartitionKey(TopicInboundEvent, merge); got != k {
		t.Fatalf("two events of one pull request: keys %q and %q", k, got)
	}
	if got := OutboxPartitionKey(TopicInboundEvent, other); got == k {
		t.Fatal("the same source in another team must have another key")
	}
	if got := OutboxPartitionKey(TopicInboundEvent, &Event{TeamID: "team1"}); got != "" {
		t.Fatalf("an event without a source has no key, got %q", got)
	}

	n := OutboxPartitionKey(TopicOutboundNotice, &OutboundMessage{WorkItemID: "wi1", Source: pr})
	if n == "" || n == k {
		t.Fatalf("notice key %q must exist and differ from the inbound key %q", n, k)
	}
	if got := OutboxPartitionKey(TopicOutboundNotice, OutboundMessage{WorkItemID: "wi1"}); got != n {
		t.Fatalf("notices of one work item: keys %q and %q", n, got)
	}
	if got := OutboxPartitionKey(TopicOutboundNotice, &OutboundMessage{Source: pr}); got == "" {
		t.Fatal("a notice without a work item uses its source")
	}

	if got := OutboxPartitionKey("jobs", keyed("abc")); got != "jobs|abc" {
		t.Fatalf("PartitionKeyer: got %q", got)
	}
	if got := OutboxPartitionKey("jobs", []byte("{}")); got != "" {
		t.Fatalf("raw payload has no key, got %q", got)
	}
}

func TestErrStaleIsConflict(t *testing.T) {
	if !errors.Is(ErrStale, ErrConflict) {
		t.Fatal("ErrStale must wrap ErrConflict")
	}
	if errors.Is(ErrConflict, ErrStale) {
		t.Fatal("a plain conflict is not stale")
	}
}
