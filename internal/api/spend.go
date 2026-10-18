package api

import "net/http"

// handleGetSpend returns the model spend of a factory and its team, with each
// spend limit that is set. The body is a spend.Report.
func (s *Server) handleGetSpend(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Spend == nil {
		return featureUnavailable("spend")
	}
	rep, err := s.opts.Spend(ctx, f.TeamID, f.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}
