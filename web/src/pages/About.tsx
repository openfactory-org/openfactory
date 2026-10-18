import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { Card, KV, PageHeader, Query } from "../components/ui";

export function AboutPage() {
  const { me } = useAuth();
  const v = useQuery({ queryKey: ["version"], queryFn: () => unwrap(api.GET("/version")), staleTime: Infinity });
  return (
    <>
      <PageHeader title="About Open Factory" />
      <div className="stack narrow">
        <Card title="Server">
          <Query q={v}>
            {(info) => (
              <KV
                items={[
                  ["Version", <code key="v">{info.version}</code>],
                  ["Commit", info.commit || "—"],
                  ["Built", info.date || "—"],
                  [
                    "API reference",
                    <a key="api" href="/api/v1/openapi.json">
                      /api/v1/openapi.json
                    </a>,
                  ],
                ]}
              />
            )}
          </Query>
        </Card>
        {me && (
          <Card title="You">
            <KV
              items={[
                ["Name", me.user.name || "—"],
                ["Email", me.user.email],
                ["Team", me.team.name],
                ["Role", me.role],
              ]}
            />
          </Card>
        )}
      </div>
    </>
  );
}

export function NotFoundPage() {
  return (
    <>
      <PageHeader title="Page not found" />
      <p>
        That page does not exist. <Link to="/factories">Go to your factories</Link>.
      </p>
    </>
  );
}
