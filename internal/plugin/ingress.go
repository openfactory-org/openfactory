package plugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// WebhookPattern is the canonical plugin webhook route.
const WebhookPattern = "POST /plugins/{name}/webhook"

// Register mounts POST /plugins/{name}/webhook. Call it after Start.
func (h *Host) Register(mux *http.ServeMux) {
	mux.HandleFunc(WebhookPattern, func(w http.ResponseWriter, r *http.Request) {
		h.serveWebhook(w, r, r.PathValue("name"))
	})
}

// ServeWebhook forwards an HTTP request to the named plugin's Ingress service
// (exported for servers that mount routes themselves).
func (h *Host) ServeWebhook(w http.ResponseWriter, r *http.Request, name string) {
	h.serveWebhook(w, r, name)
}

func (h *Host) serveWebhook(w http.ResponseWriter, r *http.Request, name string) {
	in, ok := h.plugin(name)
	if !ok || !hasCap(in.pluginInfo(), wire.CapIngress) {
		forgeutil.WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no plugin %q receives webhooks", name))
		return
	}
	conn, err := in.conn()
	if err != nil {
		w.Header().Set("Retry-After", "5")
		forgeutil.WriteError(w, http.StatusServiceUnavailable, "plugin_unavailable", err.Error())
		return
	}
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	ctx, cancel := h.callCtx(r.Context())
	defer cancel()
	resp, err := pb.NewIngressClient(conn).HandleWebhook(ctx, &pb.WebhookRequest{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Headers: wire.HeadersToPB(r.Header),
		Body: body, RemoteAddr: r.RemoteAddr, Host: r.Host,
	})
	if err != nil {
		h.log.Error("plugin webhook failed", "plugin", name, "err", err)
		forgeutil.WriteError(w, http.StatusBadGateway, "plugin_error", wire.FromStatus(name, err).Error())
		return
	}
	h.publish(r.Context(), name, resp.Events)
	for k, vs := range wire.HeadersFromPB(resp.Headers) {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	st := int(resp.Status)
	if st == 0 {
		st = http.StatusOK
	}
	w.WriteHeader(st)
	_, _ = w.Write(resp.Body)
}

// publish links actors, resolves factory aliases the plugin left unresolved
// and hands each event to Options.Publish.
func (h *Host) publish(ctx context.Context, plugin string, evs []*pb.Event) {
	ctx = context.WithoutCancel(ctx)
	for _, pe := range evs {
		ev, err := wire.EventFromPB(pe)
		if err != nil {
			h.log.Warn("plugin returned an invalid event", "plugin", plugin, "err", err)
			continue
		}
		if ev.TeamID == "" {
			h.log.Warn("plugin event without team dropped", "plugin", plugin, "type", ev.Type)
			continue
		}
		if st := h.opts.Store; st != nil {
			forgeutil.LinkActor(ctx, st, &ev.Actor)
			if alias, _ := ev.Attributes[eventattr.Alias].(string); alias != "" && ev.FactoryID == "" {
				if f, err := st.GetFactoryByAlias(ctx, ev.TeamID, alias); err == nil {
					ev.FactoryID = f.ID
				}
			}
		}
		if h.opts.Publish == nil {
			h.log.Warn("plugin event dropped: no publisher configured", "plugin", plugin, "type", ev.Type)
			continue
		}
		if err := h.opts.Publish(ctx, *ev); err != nil {
			h.log.Error("publish plugin event", "plugin", plugin, "type", ev.Type, "err", err)
		}
	}
}
