import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes, useParams } from "react-router-dom";
import { ApiError } from "./api/client";
import { AuthProvider, useAuth } from "./auth/AuthContext";
import { FactoryLayout } from "./components/FactoryLayout";
import { Layout } from "./components/Layout";
import { ConfirmHost } from "./components/overlays";
import { ToastProvider } from "./components/Toast";
import { ErrorAlert, Loading } from "./components/ui";
import { AboutPage, NotFoundPage } from "./pages/About";
import { AgentPage, AgentsPage } from "./pages/Agents";
import { AutomationPage, AutomationsPage } from "./pages/Automations";
import { DashboardPage } from "./pages/Dashboard";
import { DefinitionPage, VersionPage, VersionsPage } from "./pages/Definition";
import { ChatPage, FactoriesPage, NewFactoryPage } from "./pages/Factories";
import { InboxPage } from "./pages/Inbox";
import { McpServerPage, McpServersPage } from "./pages/McpServers";
import { isServerPath, LoginPage, safeReturnTo } from "./pages/LoginPage";
import { BenchmarkPage, BenchmarksPage, ScorersPage } from "./pages/Measure";
import { RunPage, RunsPage } from "./pages/Runs";
import { FactoryIntegrationsPage, SettingsPage } from "./pages/Settings";
import { EnvironmentPage, EnvironmentsPage } from "./pages/Environments";
import { IntegrationsPage } from "./pages/Integrations";
import { SecretsPage } from "./pages/Secrets";
import { WorkersPage } from "./pages/Workers";
import { WebhookPage, WebhooksPage } from "./pages/Webhooks";
import { WorkItemPage, WorkItemRedirect, WorkItemsPage } from "./pages/Work";

/** Opt in to React Router v7 behaviour now (silences the v6 deprecation warnings). */
export const ROUTER_FUTURE = { v7_startTransition: true, v7_relativeSplatPath: true } as const;

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        refetchOnWindowFocus: true,
        retry: (count, err) => !(err instanceof ApiError && err.status >= 400 && err.status < 500) && count < 2,
      },
    },
  });
}

function FactoryInbox() {
  const { fid = "" } = useParams();
  return <InboxPage factoryID={fid} />;
}

function FactoryRuns() {
  const { fid = "" } = useParams();
  return <RunsPage factoryID={fid} />;
}

/** Route table. URLs match the links of @openfactory/core, so links in notifications resolve. */
export function AppRoutes() {
  const { me, loading, error } = useAuth();
  if (loading) return <Loading label="Loading Open Factory…" />;
  if (!me) {
    if (error) return <ErrorAlert error={error} title="Could not reach the server" />;
    return <LoginPage />;
  }
  return (
    <Routes>
      <Route path="/login" element={<LoginRedirect />} />
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/factories" replace />} />
        <Route path="factories" element={<FactoriesPage />} />
        <Route path="factories/new" element={<NewFactoryPage />} />
        <Route path="chat" element={<ChatPage />} />
        <Route path="inbox" element={<InboxPage />} />
        <Route path="runs" element={<RunsPage />} />
        <Route path="runs/:id" element={<RunPage />} />
        <Route path="work-items/:wid" element={<WorkItemRedirect />} />
        <Route path="secrets" element={<SecretsPage />} />
        <Route path="integrations" element={<IntegrationsPage />} />
        <Route path="environments" element={<EnvironmentsPage />} />
        <Route path="environments/:id" element={<EnvironmentPage />} />
        <Route path="mcp-servers" element={<McpServersPage />} />
        <Route path="mcp-servers/:id" element={<McpServerPage />} />
        <Route path="workers" element={<WorkersPage />} />
        <Route path="about" element={<AboutPage />} />
        <Route path="f/:fid" element={<FactoryLayout />}>
          <Route index element={<DashboardPage />} />
          <Route path="inbox" element={<FactoryInbox />} />
          <Route path="work" element={<WorkItemsPage />} />
          <Route path="work/:wid" element={<WorkItemPage />} />
          <Route path="runs" element={<FactoryRuns />} />
          <Route path="agents" element={<AgentsPage />} />
          <Route path="agents/:name" element={<AgentPage />} />
          <Route path="automations" element={<AutomationsPage />} />
          <Route path="automations/:name" element={<AutomationPage />} />
          <Route path="webhooks" element={<WebhooksPage />} />
          <Route path="webhooks/:id" element={<WebhookPage />} />
          <Route path="definition" element={<DefinitionPage />} />
          <Route path="definition/versions" element={<VersionsPage />} />
          <Route path="definition/versions/:vid" element={<VersionPage />} />
          <Route path="integrations" element={<FactoryIntegrationsPage />} />
          <Route path="scorers" element={<ScorersPage />} />
          <Route path="benchmarks" element={<BenchmarksPage />} />
          <Route path="benchmarks/:id" element={<BenchmarkPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}

function LoginRedirect() {
  const params = new URLSearchParams(window.location.search);
  const to = safeReturnTo(params.get("return_to") ?? params.get("next"));
  if (isServerPath(to)) {
    // The MCP sign-in page is served by the server: leave the dashboard.
    window.location.replace(to);
    return null;
  }
  return <Navigate to={to} replace />;
}

export function App({ client = makeQueryClient() }: { client?: QueryClient }) {
  return (
    <QueryClientProvider client={client}>
      <ToastProvider>
        <AuthProvider>
          <BrowserRouter future={ROUTER_FUTURE}>
            <AppRoutes />
          </BrowserRouter>
          <ConfirmHost />
        </AuthProvider>
      </ToastProvider>
    </QueryClientProvider>
  );
}
