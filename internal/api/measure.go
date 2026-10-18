package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func (s *Server) handleListScorers(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	scs, err := s.opts.Store.ListScorers(r.Context(), f.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("scorers", scs, ""))
	return nil
}

func (s *Server) handleListScores(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	since, err := qSince(r, "since")
	if err != nil {
		return err
	}
	scorerID := r.URL.Query().Get("scorer_id")
	if slug := r.URL.Query().Get("scorer"); slug != "" && scorerID == "" {
		scs, err := s.opts.Store.ListScorers(ctx, f.ID)
		if err != nil {
			return err
		}
		idx := slices.IndexFunc(scs, func(sc core.Scorer) bool { return sc.Slug == slug || sc.ID == slug })
		if idx < 0 {
			return errNotFound("scorer " + slug)
		}
		scorerID = scs[idx].ID
	}
	scores, err := s.opts.Store.ListScores(ctx, f.ID, scorerID, since)
	if err != nil {
		return err
	}
	if passed := r.URL.Query().Get("passed"); passed != "" {
		want, err := qBool(r, "passed")
		if err != nil {
			return err
		}
		scores = slices.DeleteFunc(scores, func(sc core.Score) bool { return sc.Passed != want })
	}
	if agent := r.URL.Query().Get("agent"); agent != "" {
		scores = slices.DeleteFunc(scores, func(sc core.Score) bool { return sc.AgentName != agent })
	}
	page, next := paginate(scores, func(sc core.Score) (timeT, string) { return sc.CreatedAt, sc.ID }, pg)
	writeJSON(w, http.StatusOK, listResponse("scores", page, next))
	return nil
}

func (s *Server) handleListBenchmarkSuites(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	def, _, err := s.activeDefinition(ctx, f)
	if err != nil {
		return err
	}
	suites := []core.BenchmarkSuite{}
	if def != nil {
		suites = append(suites, def.Benchmarks...)
	}
	writeJSON(w, http.StatusOK, listResponse("benchmarks", suites, ""))
	return nil
}

// StartBenchmarkRequest is the body of POST /factories/{uid}/benchmarks/{suite}/runs.
type StartBenchmarkRequest struct {
	Configurations []core.BenchmarkConfiguration `json:"configurations"`
	Scorers        []string                      `json:"scorers,omitempty"`
	Repetitions    int                           `json:"repetitions,omitempty"`
}

func (s *Server) handleStartBenchmark(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.StartBenchmark == nil {
		return featureUnavailable("benchmarks")
	}
	suite := r.PathValue("suite")
	if def, _, err := s.activeDefinition(ctx, f); err != nil {
		return err
	} else if def != nil {
		if !slices.ContainsFunc(def.Benchmarks, func(b core.BenchmarkSuite) bool { return b.Slug == suite }) {
			return errNotFound("benchmark suite " + suite)
		}
	}
	body, err := readRaw(r)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		var req StartBenchmarkRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return errInvalid("malformed body: %v", err)
		}
		if req.Repetitions < 0 || req.Repetitions > 10 {
			return errInvalid("repetitions must be between 1 and 10")
		}
		for i, c := range req.Configurations {
			if c.Name == "" {
				return errInvalid("configurations[%d].name is required", i)
			}
		}
	} else {
		body = json.RawMessage(`{}`)
	}
	br, err := s.opts.StartBenchmark(ctx, f.ID, suite, body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, br)
	return nil
}

func readRaw(r *http.Request) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := decodeJSON(r, &raw, true); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(raw), nil
}

func (s *Server) handleListBenchmarkRuns(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	brs, err := s.opts.Store.ListBenchmarkRuns(ctx, f.ID)
	if err != nil {
		return err
	}
	if suite := firstNonEmpty(r.PathValue("suite"), r.URL.Query().Get("suite")); suite != "" {
		brs = slices.DeleteFunc(brs, func(b core.BenchmarkRun) bool { return b.Suite != suite })
	}
	if st := r.URL.Query().Get("state"); st != "" {
		brs = slices.DeleteFunc(brs, func(b core.BenchmarkRun) bool { return b.State != st })
	}
	page, next := paginate(brs, func(b core.BenchmarkRun) (timeT, string) { return b.CreatedAt, b.ID }, pg)
	writeJSON(w, http.StatusOK, listResponse("benchmark_runs", page, next))
	return nil
}

func (s *Server) handleGetBenchmarkRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	br, err := s.opts.Store.GetBenchmarkRun(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			return errNotFound("benchmark run " + r.PathValue("id"))
		}
		return err
	}
	if _, err := s.factoryFor(ctx, p, br.FactoryID); err != nil {
		return errNotFound("benchmark run " + r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, br)
	return nil
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Metrics == nil {
		return featureUnavailable("metrics")
	}
	since, err := qSince(r, "since")
	if err != nil {
		return err
	}
	if since == nil {
		t := s.now().Add(-30 * 24 * time.Hour)
		since = &t
	}
	m, err := s.opts.Metrics(ctx, f.ID, *since)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"factory_id": f.ID, "since": since.UTC(), "metrics": m})
	return nil
}

// POST /benchmark-runs/{id}/cancel
func (s *Server) handleCancelBenchmarkRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	br, err := s.opts.Store.GetBenchmarkRun(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			return errNotFound("benchmark run " + r.PathValue("id"))
		}
		return err
	}
	if _, err := s.factoryFor(ctx, p, br.FactoryID); err != nil {
		return errNotFound("benchmark run " + r.PathValue("id"))
	}
	if s.opts.CancelBenchmark == nil {
		return featureUnavailable("benchmarks")
	}
	br, err = s.opts.CancelBenchmark(ctx, br.ID, p.ActorID())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, br)
	return nil
}

// BenchmarkTaskRequest is the body of POST /factories/{uid}/benchmarks/{suite}/tasks.
type BenchmarkTaskRequest struct {
	FactoryID       string   `json:"-"`
	Suite           string   `json:"-"`
	FromRunID       string   `json:"from_run_id"`
	Slug            string   `json:"slug,omitempty"`
	Title           string   `json:"title,omitempty"`
	SuccessCriteria string   `json:"success_criteria"`
	Tags            []string `json:"tags,omitempty"`
}

// POST /factories/{uid}/benchmarks/{suite}/tasks makes a benchmark task from a
// finished run, with the commits that the run checked out. A managed
// definition is applied at once. For a git definition, the response has the
// files to commit and nothing is applied.
func (s *Server) handleBenchmarkTaskFromRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.BenchmarkTaskFromRun == nil {
		return featureUnavailable("benchmarks")
	}
	var req BenchmarkTaskRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	if req.FromRunID == "" {
		return errInvalid("from_run_id is required")
	}
	if _, err := s.runFor(ctx, p, req.FromRunID); err != nil {
		return err
	}
	req.FactoryID, req.Suite = f.ID, r.PathValue("suite")
	task, files, full, err := s.opts.BenchmarkTaskFromRun(ctx, req)
	if err != nil {
		return err
	}
	out := map[string]any{"task": task, "files": files, "applied": false}
	if f.Source != core.DefinitionGit {
		if s.opts.Engine == nil {
			return featureUnavailable("definition apply")
		}
		v, err := s.opts.Engine.ApplyDefinition(ctx, f.ID, full, p.ActorID(), "Add a benchmark task from run "+req.FromRunID, "")
		if err != nil {
			if v != nil && len(v.Errors) > 0 {
				return validationError("the definition with the new task is invalid", v.Errors)
			}
			return err
		}
		out["applied"] = true
		out["version_id"] = v.ID
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}
