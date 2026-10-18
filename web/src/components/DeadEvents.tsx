import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { Button, Card, Query, TimeAgo } from "./ui";
import { useMutate } from "../lib/mutate";

export const DEAD_EVENTS_KEY = ["outbox", "dead"];

/**
 * Lists events and notices that failed every delivery attempt. A dead event
 * does not hold back later events of the same entity. An admin can deliver
 * it again; it then takes its old place in the order of its entity.
 */
export function DeadEvents() {
  const { isAdmin } = useAuth();
  const q = useQuery({
    queryKey: DEAD_EVENTS_KEY,
    queryFn: () => unwrap(api.GET("/outbox/dead")).then((r) => r.items ?? []),
    enabled: isAdmin,
    refetchInterval: 60_000,
  });
  const redrive = useMutate(
    (id: string) => unwrap(api.POST("/outbox/{id}/redrive", { params: { path: { id } } })),
    { invalidate: [DEAD_EVENTS_KEY], success: "The event will be delivered again." },
  );
  if (!isAdmin) return null;
  return (
    <Card title="Dead-lettered events">
      <Query q={q}>
        {(msgs) =>
          msgs.length === 0 ? (
            <p className="muted">No events failed every delivery attempt.</p>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Event</th>
                    <th scope="col">Attempts</th>
                    <th scope="col">Last error</th>
                    <th scope="col">Created</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {msgs.map((m) => (
                    <tr key={m.id}>
                      <td>
                        {m.summary || m.topic}
                        <br />
                        <code className="small muted">{m.id}</code>
                      </td>
                      <td>{m.attempts}</td>
                      <td className="small">{m.last_error}</td>
                      <td>
                        <TimeAgo t={m.created_at} />
                      </td>
                      <td>
                        <Button busy={redrive.isPending && redrive.variables === m.id} onClick={() => redrive.mutate(m.id)}>
                          Deliver again
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </Query>
    </Card>
  );
}
