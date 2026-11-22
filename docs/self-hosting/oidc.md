# OIDC login

The dashboard supports single sign-on with any OpenID Connect provider, for example Okta, Microsoft Entra ID, Google Workspace, Keycloak, Dex, Authentik or GitLab. It is part of the free core. Every install has one team and the roles admin, editor and user.

## Register the client

Create an OIDC client ("web application") in your identity provider:

| Setting | Value |
|---|---|
| Redirect URI | `${OF_PUBLIC_URL}/auth/oidc/callback`, for example `https://factories.example.com/auth/oidc/callback` |
| Grant type | Authorization code (PKCE is used) |
| Scopes | `openid`, `profile`, `email`, and a scope that adds a groups claim if you map groups to roles |

Note the issuer URL, client ID and client secret. A public client with PKCE needs no secret.

## Configure the control plane

Set the variables in `wrangler.jsonc` and the secret with Wrangler:

```jsonc
"vars": {
  "OF_OIDC_ISSUER_URL": "https://login.example.com/realms/acme",
  "OF_OIDC_CLIENT_ID": "openfactory",
  "OF_OIDC_ADMIN_GROUPS": "platform-admins",
  "OF_OIDC_EDITOR_GROUPS": "developers"
}
```

```bash
npx wrangler secret put OF_OIDC_CLIENT_SECRET -c apps/control-plane/wrangler.jsonc
```

Deploy again after the change. For every setting, see [Configuration](configuration.md#secrets).

| Variable | Default | Description |
|---|---|---|
| `OF_OIDC_ISSUER_URL` | none | Issuer URL. Setting it turns single sign-on on. |
| `OF_OIDC_CLIENT_ID` | none | Required with an issuer. |
| `OF_OIDC_CLIENT_SECRET` (secret) | none | Optional for PKCE public clients. |
| `OF_OIDC_REDIRECT_URL` | `${OF_PUBLIC_URL}/auth/oidc/callback` | Redirect URL registered with the identity provider. |
| `OF_OIDC_SCOPES` | `openid,profile,email` | `openid` is always added. |
| `OF_OIDC_ALLOWED_DOMAINS` | any | Comma-separated email domains that may sign in. |
| `OF_OIDC_GROUPS_CLAIM` | `groups` | ID token claim with group names. |
| `OF_OIDC_ADMIN_GROUPS`, `OF_OIDC_EDITOR_GROUPS` | none | Comma-separated groups mapped to admin and editor. |
| `OF_OIDC_DEFAULT_ROLE` | `user` | Role for users in no mapped group: `admin`, `editor` or `user`. |
| `OF_OIDC_DISPLAY_NAME` | `Single sign-on` | Login button label. |
| `OF_OIDC_REQUIRE_VERIFIED_EMAIL` | `true` | Reject ID tokens with an unverified email. |

## Roles

At each login, the server maps the groups in `OF_OIDC_GROUPS_CLAIM` to a role:

1. A member of an `OF_OIDC_ADMIN_GROUPS` group is an admin.
2. Else a member of an `OF_OIDC_EDITOR_GROUPS` group is an editor.
3. Else the user gets `OF_OIDC_DEFAULT_ROLE` (default `user`).

The identity provider sets only the team-wide role. An admin can add a per-factory role with `PATCH /api/v1/users/{id}`; a login does not change it. See [Roles and permissions](../security.md#roles-and-permissions).

`OF_OIDC_ALLOWED_DOMAINS` limits sign-in to email domains. By default, ID tokens with an unverified email are rejected (`OF_OIDC_REQUIRE_VERIFIED_EMAIL`).

Dashboard sessions are signed with a key derived from the signing key, so they survive deploys. See [Dashboard sign-in](../security.md#dashboard-sign-in).

API keys keep working next to OIDC login. Use them for the CLI and for workers.

See also [Security](../security.md) and [Dashboard](../dashboard.md).
