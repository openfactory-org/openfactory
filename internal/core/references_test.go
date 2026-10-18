package core

import "testing"

func TestWorkItemRefs(t *testing.T) {
	w := &WorkItem{
		Source:       SourceRef{Provider: "slack", Channel: "C1", ThreadID: "1.2"},
		PullRequests: []PullRequestRef{{Forge: "GITHUB", Repo: "acme/api", Number: 4}},
	}
	if !w.AddRef(WorkItemRef{Kind: RefIssue, Source: SourceRef{Provider: "linear", ExternalID: "ENG-1"}}) {
		t.Fatal("add issue")
	}
	if w.AddRef(WorkItemRef{Kind: RefIssue, Source: SourceRef{Provider: "linear", ExternalID: "ENG-1"}}) {
		t.Fatal("second add of the same issue")
	}
	if w.AddRef(WorkItemRef{Kind: RefIssue}) {
		t.Fatal("add without a key")
	}
	refs := w.Refs()
	if len(refs) != 3 || refs[0].Kind != RefSource || refs[1].Key != "linear:ENG-1" || refs[2].Key != "github:acme/api#4" {
		t.Fatalf("refs = %+v", refs)
	}
	for _, k := range []string{"slack:C1:1.2", "linear:ENG-1", "github:acme/api#4"} {
		if !w.HasRef(k) {
			t.Errorf("HasRef(%q) = false", k)
		}
	}
	if w.HasRef("") || w.HasRef("github:acme/api#5") {
		t.Fatal("HasRef matched a foreign key")
	}
}

func TestThreadRefIsNotPartOfKey(t *testing.T) {
	a := SourceRef{Provider: "github", Repo: "acme/api", Number: 4}
	b := a
	b.ThreadRef = "99"
	if a.Key() != b.Key() {
		t.Fatalf("keys differ: %q %q", a.Key(), b.Key())
	}
}

func TestIsTrackerIssue(t *testing.T) {
	cases := []struct {
		src  SourceRef
		want bool
	}{
		{SourceRef{Provider: "linear", ExternalID: "ENG-1"}, true},
		{SourceRef{Provider: "jira", Kind: "issue", ExternalID: "OPS-2"}, true},
		{SourceRef{Provider: "azure_devops", Kind: "work_item", Repo: "proj", Number: 3}, true},
		{SourceRef{Provider: "github", Kind: "issue", Repo: "a/b", Number: 3}, false},
		{SourceRef{Provider: "slack", Channel: "C1"}, false},
	}
	for _, c := range cases {
		if got := c.src.IsTrackerIssue(); got != c.want {
			t.Errorf("%+v: got %v", c.src, got)
		}
	}
}
