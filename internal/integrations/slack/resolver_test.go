package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// namesAPI is a Slack API fake that lists channels and users.
type namesAPI struct {
	mu       sync.Mutex
	calls    map[string]int
	channels []string // "ID:name"
	users    []string // "ID:name:email[:deleted]"
	fail     string   // Slack error code for all calls
	noGroups bool     // conversations.list fails with missing_scope for private channels
	pages    bool     // return the channels in pages of one
}

func (a *namesAPI) count(m string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[m]
}

func (a *namesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	a.calls[method]++
	if r.Header.Get("Authorization") != "Bearer xoxb-test" {
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	if a.fail != "" {
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, a.fail)
		return
	}
	switch method {
	case "conversations.list":
		if a.noGroups && strings.Contains(r.URL.Query().Get("types"), "private_channel") {
			fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
			return
		}
		list := a.channels
		next := ""
		if a.pages {
			i := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				fmt.Sscanf(c, "p%d", &i)
			}
			list = a.channels[i : i+1]
			if i+1 < len(a.channels) {
				next = fmt.Sprintf("p%d", i+1)
			}
		}
		var items []string
		for _, c := range list {
			id, name, _ := strings.Cut(c, ":")
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q}`, id, name))
		}
		fmt.Fprintf(w, `{"ok":true,"channels":[%s],"response_metadata":{"next_cursor":%q}}`, strings.Join(items, ","), next)
	case "users.list":
		var items []string
		for _, u := range a.users {
			p := strings.Split(u, ":")
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"deleted":%v,"profile":{"email":%q}}`, p[0], p[1], len(p) > 3, p[2]))
		}
		fmt.Fprintf(w, `{"ok":true,"members":[%s]}`, strings.Join(items, ","))
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

type resolverFixture struct {
	api *namesAPI
	r   *NameResolver
	st  *forgetest.Store
	now time.Time
	mu  sync.Mutex
}

func (f *resolverFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func newResolverFixture(t *testing.T) *resolverFixture {
	t.Helper()
	api := &namesAPI{calls: map[string]int{},
		channels: []string{"C1:incidents", "C2:dev", "G3:secret"},
		users:    []string{"U1:alice:Alice@Example.com", "U2:bob:bob@example.com", "U3:carol:carol@example.com:deleted"}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	st := forgetest.NewStore()
	st.Integrations = []core.Integration{{ID: "int_s", TeamID: "team_1", Provider: "slack", SecretRef: "SLACK_BOT"}}
	f := &resolverFixture{api: api, st: st, now: time.Unix(1_700_000_000, 0)}
	f.r = NewNameResolver(NameResolverOptions{
		Store: st, Secrets: forgetest.NewSecrets(map[string]string{"SLACK_BOT": "xoxb-test"}), APIBaseURL: srv.URL,
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	})
	return f
}

func TestResolveChannelNames(t *testing.T) {
	f := newResolverFixture(t)
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"#incidents", " dev ", "Secret", "nope"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"#incidents": {"C1"}, " dev ": {"C2"}, "Secret": {"G3"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolveUserNamesAndEmails(t *testing.T) {
	f := newResolverFixture(t)
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "users", []string{"Alice", "bob@example.com", "alice@example.com", "carol"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"Alice": {"U1"}, "bob@example.com": {"U2"}, "alice@example.com": {"U1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (a deleted user does not match)", got, want)
	}
	// itemUsers uses the same list: no second API call.
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "itemUsers", []string{"bob"}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.api.count("users.list"); n != 1 {
		t.Fatalf("users.list called %d times, want 1", n)
	}
}

func TestResolverCachesAndRefreshesOnAMiss(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	resolve := func(name string) map[string][]string {
		got, err := f.r.ResolveNames(ctx, "team_1", "slack", "channels", []string{name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	resolve("dev")
	resolve("dev")
	if n := f.api.count("conversations.list"); n != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", n)
	}
	// A name that is missing does not read the list again within 30 seconds.
	f.api.mu.Lock()
	f.api.channels = append(f.api.channels, "C4:new")
	f.api.mu.Unlock()
	if got := resolve("new"); len(got) != 0 {
		t.Fatalf("found %v within the refresh limit", got)
	}
	if n := f.api.count("conversations.list"); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	// After 30 seconds, a miss reads the list again and finds the new channel.
	f.advance(time.Minute)
	if got := resolve("new"); !reflect.DeepEqual(got["new"], []string{"C4"}) {
		t.Fatalf("got %v", got)
	}
	if n := f.api.count("conversations.list"); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	// After the TTL a found name reads the list again.
	f.advance(10 * time.Minute)
	resolve("dev")
	if n := f.api.count("conversations.list"); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestResolverUsesTheOldListWhenTheAPIFails(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "channels", []string{"dev"}, nil); err != nil {
		t.Fatal(err)
	}
	f.api.mu.Lock()
	f.api.fail = "ratelimited"
	f.api.mu.Unlock()
	f.advance(10 * time.Minute)
	got, err := f.r.ResolveNames(ctx, "team_1", "slack", "channels", []string{"dev"}, nil)
	if err != nil || !reflect.DeepEqual(got["dev"], []string{"C2"}) {
		t.Fatalf("got %v, %v; want the cached list", got, err)
	}
	// An old list is not used for ever.
	f.advance(2 * time.Hour)
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "channels", []string{"dev"}, nil); err == nil || !strings.Contains(err.Error(), "ratelimited") {
		t.Fatalf("err = %v, want the API error", err)
	}
}

func TestResolverErrorsAreReportedAndBackedOff(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	f.api.fail = "missing_scope"
	_, err := f.r.ResolveNames(ctx, "team_1", "slack", "users", []string{"alice"}, nil)
	if err == nil || !strings.Contains(err.Error(), "users:read") {
		t.Fatalf("err = %v, want a hint about the scope", err)
	}
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "users", []string{"alice"}, nil); err == nil {
		t.Fatal("want the error again")
	}
	if n := f.api.count("users.list"); n != 1 {
		t.Fatalf("users.list called %d times, want 1 (back-off)", n)
	}
	f.api.mu.Lock()
	f.api.fail = ""
	f.api.mu.Unlock()
	f.advance(time.Minute)
	if got, err := f.r.ResolveNames(ctx, "team_1", "slack", "users", []string{"alice"}, nil); err != nil || len(got["alice"]) != 1 {
		t.Fatalf("got %v, %v after the back-off", got, err)
	}
}

func TestResolverPrivateChannelScopeFallback(t *testing.T) {
	f := newResolverFixture(t)
	f.api.noGroups = true
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"dev"}, nil)
	if err != nil || !reflect.DeepEqual(got["dev"], []string{"C2"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestResolverFollowsPages(t *testing.T) {
	f := newResolverFixture(t)
	f.api.pages = true
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"secret"}, nil)
	if err != nil || !reflect.DeepEqual(got["secret"], []string{"G3"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if n := f.api.count("conversations.list"); n != 3 {
		t.Fatalf("pages = %d, want 3", n)
	}
}

func TestResolverDuplicateNamesGiveAllIDs(t *testing.T) {
	f := newResolverFixture(t)
	f.api.channels = append(f.api.channels, "C9:dev")
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"dev"}, nil)
	if err != nil || !reflect.DeepEqual(got["dev"], []string{"C2", "C9"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestResolverNeedsAnActiveIntegration(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	f.st.Integrations[0].Status = core.IntegrationDisabled
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "channels", []string{"dev"}, nil); err == nil || !strings.Contains(err.Error(), "no active Slack integration") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.r.ResolveNames(ctx, "team_1", "linear", "teams", []string{"Eng"}, nil); err == nil {
		t.Fatal("want an error for another provider")
	}
	f.st.Integrations[0].Status = ""
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "bogus", []string{"x"}, nil); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
}

func TestResolverSecondIntegrationCoversForAFailingOne(t *testing.T) {
	f := newResolverFixture(t)
	f.st.Integrations = append(f.st.Integrations, core.Integration{ID: "int_bad", TeamID: "team_1", Provider: "slack", SecretRef: "MISSING"})
	got, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"dev"}, nil)
	if err != nil || !reflect.DeepEqual(got["dev"], []string{"C2"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}
