import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { useMutate } from "../lib/mutate";
import { Button, Card, TimeAgo } from "./ui";

const KEY = ["mcp-sign-ins"];

/** McpSignIns lists the active Factory MCP browser sign-ins, with a control that ends one. */
export function McpSignIns() {
  const { me } = useAuth();
  const isAdmin = me?.role === "admin";
  const q = useQuery({
    queryKey: [...KEY, isAdmin ? "team" : "own"],
    queryFn: () => unwrap(api.GET("/mcp-sign-ins", { params: { query: isAdmin ? { scope: "team" } : {} } })).then((r) => r.items ?? []),
    retry: false,
  });
  const revoke = useMutate((id: string) => unwrap(api.DELETE("/mcp-sign-ins/{id}", { params: { path: { id } } })), {
    invalidate: [KEY],
    success: "Sign-in ended.",
  });
  if (q.isError) return null; // browser sign-in is not configured on this server
  const items = q.data ?? [];
  return (
    <Card title="Factory MCP sign-ins">
      <p className="muted small">
        MCP clients that signed in through the browser. Each one acts as its user. End a sign-in to stop its access at once. API keys are not listed here.
      </p>
      {items.length === 0 ? (
        <p className="muted small">No active sign-ins.</p>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Client</th>
                {isAdmin && <th scope="col">User</th>}
                <th scope="col">Signed in</th>
                <th scope="col">Last used</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((g) => (
                <tr key={g.id}>
                  <td>{g.client_name}</td>
                  {isAdmin && <td className="mono small">{g.user_id === me?.user?.id ? "you" : g.user_id}</td>}
                  <td>
                    <TimeAgo t={g.created_at} />
                  </td>
                  <td>{g.last_used_at ? <TimeAgo t={g.last_used_at} /> : <span className="muted">never</span>}</td>
                  <td>
                    <Button variant="danger" busy={revoke.isPending && revoke.variables === g.id} onClick={() => revoke.mutate(g.id)}>
                      End sign-in
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
