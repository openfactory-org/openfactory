package orchestration

import "testing"

func TestParseIssueURL(t *testing.T) {
	cases := []struct {
		url, key, kind string
		ok             bool
	}{
		{"https://linear.app/acme/issue/ENG-123/fix-invoices", "linear:ENG-123", "issue", true},
		{"https://acme.atlassian.net/browse/OPS-7", "jira:OPS-7", "issue", true},
		{"https://acme.atlassian.net/jira/software/projects/OPS/boards/1?selectedIssue=OPS-9", "jira:OPS-9", "issue", true},
		{"https://github.com/acme/api/issues/12", "github:acme/api#12", "issue", true},
		{"https://gitlab.com/acme/group/api/-/issues/5", "gitlab:acme/group/api#5", "issue", true},
		{"https://dev.azure.com/acme/Payments/_workitems/edit/42", "azure_devops:Payments#42", "work_item", true},
		{"https://example.com/nothing/here", "", "", false},
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		src, ok := ParseIssueURL(c.url)
		if ok != c.ok {
			t.Errorf("%s: ok = %v", c.url, ok)
			continue
		}
		if ok && (src.Key() != c.key || src.Kind != c.kind) {
			t.Errorf("%s: key = %q kind = %q", c.url, src.Key(), src.Kind)
		}
	}
}

func TestIssueRef(t *testing.T) {
	src, err := issueRef(linkIssueInput{Provider: "Jira", Key: "OPS-1"})
	if err != nil || src.Key() != "jira:OPS-1" {
		t.Fatalf("src = %+v err = %v", src, err)
	}
	if _, err := issueRef(linkIssueInput{Key: "OPS-1"}); err == nil {
		t.Fatal("key without provider accepted")
	}
	if _, err := issueRef(linkIssueInput{URL: "https://example.com/x"}); err == nil {
		t.Fatal("unknown url accepted")
	}
}
