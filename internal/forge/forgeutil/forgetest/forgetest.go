// Package forgetest provides in-memory fakes of core interfaces for the tests
// of the forge, integrations and notify packages.
package forgetest

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Store is an in-memory subset of core.Store. Unimplemented methods panic (nil embedded interface).
type Store struct {
	core.Store

	mu           sync.Mutex
	Teams        []core.Team
	Users        []core.User
	Integrations []core.Integration
	Identities   map[string]core.ExternalIdentity // provider|externalID
	Factories    []core.Factory
	Versions     map[string]core.DefinitionVersion
	FactoryInts  map[string]map[string]map[string]string
	KV           map[string]string
	WorkItems    map[string]core.WorkItem
	Inbox        []core.InboxItem
}

// NewStore returns a store with one team "team_1".
func NewStore() *Store {
	return &Store{
		Teams:       []core.Team{{ID: "team_1", Name: "Team"}},
		Identities:  map[string]core.ExternalIdentity{},
		Versions:    map[string]core.DefinitionVersion{},
		FactoryInts: map[string]map[string]map[string]string{},
		KV:          map[string]string{},
		WorkItems:   map[string]core.WorkItem{},
	}
}

func (s *Store) ListTeams(context.Context) ([]core.Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Team(nil), s.Teams...), nil
}

func (s *Store) ListIntegrations(_ context.Context, teamID string) ([]core.Integration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Integration
	for _, in := range s.Integrations {
		if in.TeamID == teamID {
			out = append(out, in)
		}
	}
	return out, nil
}

func (s *Store) UpsertExternalIdentity(_ context.Context, x *core.ExternalIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Identities[x.Provider+"|"+x.ExternalID] = *x
	return nil
}

func (s *Store) FindExternalIdentity(_ context.Context, provider, externalID string) (*core.ExternalIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.Identities[provider+"|"+externalID]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &x, nil
}

func (s *Store) GetUser(_ context.Context, id string) (*core.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.Users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Store) GetUserByEmail(_ context.Context, email string) (*core.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.Users {
		if strings.EqualFold(u.Email, email) {
			return &u, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Store) GetFactory(_ context.Context, id string) (*core.Factory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.Factories {
		if f.ID == id {
			return &f, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Store) GetFactoryByAlias(_ context.Context, teamID, alias string) (*core.Factory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.Factories {
		if f.TeamID == teamID && strings.EqualFold(f.Alias, alias) {
			return &f, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Store) ListFactories(_ context.Context, teamID, _ string) ([]core.Factory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Factory
	for _, f := range s.Factories {
		if f.TeamID == teamID {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Store) GetDefinitionVersion(_ context.Context, id string) (*core.DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Versions[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &v, nil
}

func (s *Store) GetFactoryIntegrations(_ context.Context, factoryID string) (map[string]map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.FactoryInts[factoryID]
	if !ok {
		return map[string]map[string]string{}, nil
	}
	return m, nil
}

func (s *Store) KVGet(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.KV[key]
	if !ok {
		return "", core.ErrNotFound
	}
	return v, nil
}

func (s *Store) KVSet(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.KV[key] = value
	return nil
}

func (s *Store) GetWorkItem(_ context.Context, id string) (*core.WorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.WorkItems[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &w, nil
}

func (s *Store) ListInboxItems(_ context.Context, factoryID string, includeResolved bool) ([]core.InboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.InboxItem
	for _, it := range s.Inbox {
		if it.FactoryID == factoryID && (includeResolved || !it.Resolved) {
			out = append(out, it)
		}
	}
	return out, nil
}

// Secrets is an in-memory core.SecretStore keyed by name (team scope only).
type Secrets struct {
	mu     sync.Mutex
	Values map[string]string
}

// NewSecrets returns a secret store with the given name → value pairs.
func NewSecrets(kv map[string]string) *Secrets {
	if kv == nil {
		kv = map[string]string{}
	}
	return &Secrets{Values: kv}
}

func (s *Secrets) Put(_ context.Context, teamID, factoryID, name string, typ core.SecretType, value []byte) (*core.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Values[name] = string(value)
	return &core.Secret{ID: name, TeamID: teamID, FactoryID: factoryID, Name: name, Type: typ}, nil
}

func (s *Secrets) Get(_ context.Context, _, _, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Values[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return []byte(v), nil
}

func (s *Secrets) List(context.Context, string, string) ([]core.Secret, error) { return nil, nil }
func (s *Secrets) Delete(context.Context, string) error                        { return nil }
func (s *Secrets) Expand(_ context.Context, _, _, v string) (string, error)    { return v, nil }

// Bus records published payloads and supports subscriptions.
type Bus struct {
	mu        sync.Mutex
	Published []Published
	subs      map[string][]chan any
}

// Published is one recorded publication.
type Published struct {
	Topic   string
	Payload any
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[string][]chan any{}} }

func (b *Bus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	b.Published = append(b.Published, Published{topic, payload})
	subs := append([]chan any(nil), b.subs[topic]...)
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (b *Bus) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, 64)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[topic]
		for i, c := range list {
			if c == ch {
				b.subs[topic] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}

// Events returns the published inbound events.
func (b *Bus) Events() []*core.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*core.Event
	for _, p := range b.Published {
		if ev, ok := p.Payload.(*core.Event); ok && p.Topic == core.TopicInboundEvent {
			out = append(out, ev)
		}
	}
	return out
}

// EventTypes returns the types of published inbound events.
func (b *Bus) EventTypes() []string {
	var out []string
	for _, ev := range b.Events() {
		out = append(out, ev.Type)
	}
	return out
}

// Find returns the first published event of the given type.
func (b *Bus) Find(typ string) *core.Event {
	for _, ev := range b.Events() {
		if ev.Type == typ {
			return ev
		}
	}
	return nil
}

// Eventually polls cond until it returns true or the timeout passes.
func Eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func (s *Store) GetIntegration(_ context.Context, id string) (*core.Integration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.Integrations {
		if in.ID == id {
			cp := in
			return &cp, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Store) UpsertIntegration(_ context.Context, in *core.Integration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Integrations {
		if s.Integrations[i].ID == in.ID {
			s.Integrations[i] = *in
			return nil
		}
	}
	s.Integrations = append(s.Integrations, *in)
	return nil
}
