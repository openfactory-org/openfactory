// A port of the parts of Go's net/url.Parse that decide whether an MCP
// server URL is valid: the error cases and the scheme and host results.

function isAlnum(c: string): boolean {
  return (c >= "a" && c <= "z") || (c >= "A" && c <= "Z") || (c >= "0" && c <= "9");
}

function isHex(c: string | undefined): boolean {
  return c !== undefined && /^[0-9a-fA-F]$/.test(c);
}

function hasCTL(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const b = s.charCodeAt(i);
    if (b < 0x20 || b === 0x7f) return true;
  }
  return false;
}

function hostCharOK(c: string): boolean {
  if (c.charCodeAt(0) >= 0x80) return true;
  if (isAlnum(c)) return true;
  return `!$&'()*+,;=:[]<>"-_.~`.includes(c);
}

/** unescapeOK reports whether s has valid %-escapes (and only host characters in host mode). */
function unescapeOK(s: string, host: boolean): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s[i]!;
    if (c === "%") {
      if (!isHex(s[i + 1]) || !isHex(s[i + 2])) return false;
      if (host && parseInt(s[i + 1]!, 16) < 8 && s.slice(i, i + 3) !== "%25") return false;
      i += 2;
      continue;
    }
    if (host && !hostCharOK(c)) return false;
  }
  return true;
}

function validOptionalPort(port: string): boolean {
  if (port === "") return true;
  if (port[0] !== ":") return false;
  return /^[0-9]*$/.test(port.slice(1));
}

function parseHost(host: string): string | null {
  if (host.startsWith("[")) {
    const i = host.lastIndexOf("]");
    if (i < 0) return null;
    if (!validOptionalPort(host.slice(i + 1))) return null;
    const zone = host.slice(0, i).indexOf("%25");
    if (zone >= 0) {
      if (
        !unescapeOK(host.slice(0, zone), true) ||
        !unescapeOK(host.slice(zone, i), false) ||
        !unescapeOK(host.slice(i), true)
      ) {
        return null;
      }
      return host;
    }
  } else {
    const i = host.lastIndexOf(":");
    if (i !== -1 && !validOptionalPort(host.slice(i))) return null;
  }
  return unescapeOK(host, true) ? host : null;
}

function validUserinfo(s: string): boolean {
  for (const c of s) {
    if (isAlnum(c)) continue;
    if (`-._:~!$&'()*+,;=%@`.includes(c)) continue;
    return false;
  }
  return true;
}

export interface GoURL {
  scheme: string;
  host: string;
}

/** goParseURL returns the scheme (lowercase) and host of raw, or null when Go's url.Parse fails. */
export function goParseURL(raw: string): GoURL | null {
  const hashAt = raw.indexOf("#");
  const u = hashAt >= 0 ? raw.slice(0, hashAt) : raw;
  const frag = hashAt >= 0 ? raw.slice(hashAt + 1) : "";
  if (hasCTL(u)) return null;
  if (u === "*") return frag === "" || unescapeOK(frag, false) ? { scheme: "", host: "" } : null;
  let scheme = "";
  let rest = u;
  for (let i = 0; i < u.length; i++) {
    const c = u[i]!;
    if ((c >= "a" && c <= "z") || (c >= "A" && c <= "Z")) continue;
    if ((c >= "0" && c <= "9") || c === "+" || c === "-" || c === ".") {
      if (i === 0) break;
      continue;
    }
    if (c === ":") {
      if (i === 0) return null;
      scheme = u.slice(0, i);
      rest = u.slice(i + 1);
    }
    break;
  }
  scheme = scheme.toLowerCase();
  if (rest.endsWith("?") && rest.split("?").length === 2) rest = rest.slice(0, -1);
  else {
    const q = rest.indexOf("?");
    if (q >= 0) rest = rest.slice(0, q);
  }
  let host = "";
  if (!rest.startsWith("/")) {
    if (scheme !== "") return frag === "" || unescapeOK(frag, false) ? { scheme, host: "" } : null;
    const colon = rest.indexOf(":");
    const slash = rest.indexOf("/");
    if (colon >= 0 && (slash < 0 || colon < slash)) return null;
  }
  if ((scheme !== "" || !rest.startsWith("///")) && rest.startsWith("//")) {
    let authority = rest.slice(2);
    rest = "";
    const slash = authority.indexOf("/");
    if (slash >= 0) {
      rest = authority.slice(slash);
      authority = authority.slice(0, slash);
    }
    const at = authority.lastIndexOf("@");
    const h = parseHost(at < 0 ? authority : authority.slice(at + 1));
    if (h === null) return null;
    if (at >= 0) {
      const userinfo = authority.slice(0, at);
      if (!validUserinfo(userinfo) || !unescapeOK(userinfo, false)) return null;
    }
    host = h;
  }
  if (!unescapeOK(rest, false)) return null;
  if (frag !== "" && !unescapeOK(frag, false)) return null;
  return { scheme, host };
}
