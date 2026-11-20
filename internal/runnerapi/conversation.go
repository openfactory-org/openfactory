package runnerapi

import (
	"net/http"

	"github.com/openfactory-org/openfactory/internal/conversation"
)

func (s *Service) handleConversation(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	msgs, err := s.store.ListRunMessages(r.Context(), run.ID, 0)
	if err != nil {
		s.writeError(w, s.errInternal("list run messages", err))
		return
	}
	s.writeJSON(w, http.StatusOK, conversation.Build(run, msgs))
}
