// CSRF check of cookie-authenticated unsafe requests (auth.Service.CSRFOK):
// "X-OF-CSRF: 1", or an Origin equal to the public origin or the request's
// own host, or Sec-Fetch-Site: same-origin.
export const CSRF_HEADER = "X-OF-CSRF";

export function csrfOk(req: Request, publicOrigin: string): boolean {
  if (req.headers.get(CSRF_HEADER) === "1") return true;
  const o = req.headers.get("origin");
  if (o && o !== "null") {
    if (publicOrigin && o.toLowerCase() === publicOrigin.toLowerCase()) return true;
    try {
      const u = new URL(o);
      const host = new URL(req.url).host;
      if (u.host !== "" && u.host.toLowerCase() === host.toLowerCase()) return true;
    } catch {
      /* not a URL */
    }
    return false;
  }
  return req.headers.get("sec-fetch-site") === "same-origin";
}
