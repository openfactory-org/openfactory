import { Outlet, useOutletContext, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Factory } from "../api/client";
import { TitleScopeContext } from "../lib/title";
import { ErrorAlert, NotFound, Skeleton, isNotFound } from "./ui";

export function factoryKey(fid: string) {
  return ["factory", fid] as const;
}

export function useFactoryQuery(fid: string) {
  return useQuery({
    queryKey: factoryKey(fid),
    queryFn: () => unwrap(api.GET("/factories/{uid}", { params: { path: { uid: fid } } })) as Promise<Factory>,
  });
}

/** Loads /f/:fid and hands the factory to nested pages. */
export function FactoryLayout() {
  const { fid = "" } = useParams();
  const q = useFactoryQuery(fid);
  if (q.isLoading) return <Skeleton rows={6} />;
  if (isNotFound(q.error)) return <FactoryNotFound />;
  if (q.isError || !q.data) return <ErrorAlert error={q.error ?? new Error("Factory not found.")} />;
  return (
    <TitleScopeContext.Provider value={q.data.name}>
      <Outlet context={{ factory: q.data }} />
    </TitleScopeContext.Provider>
  );
}

export function FactoryNotFound() {
  return <NotFound title="Factory not found" text="This factory does not exist, or it was deleted." back={{ label: "Go to factories", to: "/factories" }} />;
}

export function useFactory(): Factory {
  return useOutletContext<{ factory: Factory }>().factory;
}

export function fpath(f: Factory | string, sub = ""): string {
  const id = typeof f === "string" ? f : f.id;
  return `/f/${encodeURIComponent(id)}${sub ? `/${sub}` : ""}`;
}
