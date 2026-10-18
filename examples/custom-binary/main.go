// Command custom-openfactory is an example of a custom Open Factory binary
// built in a separate Go module on the public extension layer
// (pkg/openfactory). It adds an authorizer that keeps users read-only, an
// audit sink that writes JSON lines to stderr and an extra HTTP route, and
// otherwise behaves like `openfactory serve` (same OF_* configuration).
//
//	OF_DEV=true OF_DATABASE_URL=sqlite://:memory: go run .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/openfactory-org/openfactory/pkg/openfactory"
)

// readOnlyUsers is the built-in role policy, plus: users may not start
// work (operate), only read.
type readOnlyUsers struct{ openfactory.RoleAuthorizer }

func (a readOnlyUsers) Allow(ctx context.Context, p *openfactory.Principal, act openfactory.Action, res openfactory.Resource) error {
	if p != nil && p.Role == openfactory.RoleUser && act.Category() == openfactory.ActionOperate {
		return fmt.Errorf("%w: users are read-only in this installation", openfactory.ErrForbidden)
	}
	return a.RoleAuthorizer.Allow(ctx, p, act, res)
}

// jsonAuditSink writes each audit event as one JSON line to stderr.
type jsonAuditSink struct{ enc *json.Encoder }

func (s jsonAuditSink) Record(_ context.Context, e openfactory.AuditEvent) { _ = s.enc.Encode(e) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := openfactory.LoadConfig(os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := openfactory.New(ctx, cfg,
		openfactory.WithVersion("custom-example"),
		openfactory.WithAuthorizer(readOnlyUsers{}),
		openfactory.WithAuditSink(jsonAuditSink{enc: json.NewEncoder(os.Stderr)}),
		openfactory.WithRoutes(func(mux *http.ServeMux) {
			mux.HandleFunc("GET /custom/hello", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintln(w, "hello from a custom build")
			})
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
