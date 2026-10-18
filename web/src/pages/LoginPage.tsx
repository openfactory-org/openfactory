import { useState, type FormEvent } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { errorMessage, loginWithAPIKey } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { BrandLogo } from "../components/BrandMark";

/** Path to return to after sign-in, restricted to same-origin paths. */
export function safeReturnTo(raw: string | null | undefined): string {
  if (!raw || !raw.startsWith("/") || raw.startsWith("//") || raw.startsWith("/\\")) return "/factories";
  if (raw.startsWith("/login")) return "/factories";
  return raw;
}

/** isServerPath tells that a return path is served by the server, not by the dashboard (the MCP sign-in at /oauth/). */
export function isServerPath(path: string): boolean {
  return path.startsWith("/oauth/");
}

export function LoginPage() {
  const { providers, refresh } = useAuth();
  const loc = useLocation();
  const navigate = useNavigate();
  const params = new URLSearchParams(loc.search);
  const returnTo = safeReturnTo(params.get("return_to") ?? (loc.pathname !== "/login" ? loc.pathname + loc.search : null));
  const [apiKey, setAPIKey] = useState("");
  const [error, setError] = useState<string | null>(params.get("error"));
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!apiKey.trim()) {
      setError("Enter an API key.");
      return;
    }
    setBusy(true);
    try {
      await loginWithAPIKey(apiKey.trim());
      setAPIKey("");
      await refresh();
      if (isServerPath(returnTo)) window.location.assign(returnTo);
      else navigate(returnTo, { replace: true });
    } catch (err) {
      setError(errorMessage(err) || "Sign-in failed.");
    } finally {
      setBusy(false);
    }
  }

  // The server lists the API key sign-in as a provider too. The form below covers it.
  const sso = providers.filter((p) => p.name !== "api_key");

  return (
    <main className="login" id="main">
      <div className="card login-card">
        <h1 className="login-title">
          <BrandLogo size={32} />
          <span>Sign in to Open Factory</span>
        </h1>
        {error && (
          <div className="alert alert-error" role="alert">
            {error}
          </div>
        )}
        {sso.length > 0 && (
          <div className="stack stack-sm" aria-label="Single sign-on">
            {sso.map((p) => {
              const sep = p.login_url.includes("?") ? "&" : "?";
              return (
                <a key={p.name} className="btn btn-primary btn-block" href={`${p.login_url}${sep}return_to=${encodeURIComponent(returnTo)}`}>
                  Continue with {p.display_name || p.name}
                </a>
              );
            })}
            <p className="divider">
              <span>or</span>
            </p>
          </div>
        )}
        <form onSubmit={submit} className="stack stack-sm" aria-label="Sign in with an API key">
          <div className="field">
            <label htmlFor="api-key">API key</label>
            <input
              id="api-key"
              name="api_key"
              type="password"
              autoComplete="current-password"
              placeholder="of_…"
              value={apiKey}
              onChange={(e) => setAPIKey(e.target.value)}
              required
            />
            <p className="hint">
              Use the key printed by <code>openfactory bootstrap</code>, or any team API key (<code>POST /api/v1/api-keys</code>).
            </p>
          </div>
          <button type="submit" className={`btn btn-block ${sso.length > 0 ? "" : "btn-primary"}`} disabled={busy} aria-busy={busy || undefined}>
            {busy ? "Signing in…" : "Sign in with API key"}
          </button>
        </form>
      </div>
    </main>
  );
}
