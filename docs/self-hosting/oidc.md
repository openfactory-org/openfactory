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

## Configure the server

With Helm:

```yaml
oidc:
  enabled: true
  issuer: https://login.example.com/realms/acme
  clientID: openfactory
  existingSecret: factory-oidc       # Secret with key "client-secret"
  allowedDomains: example.com
  groupsClaim: groups
  adminGroups: platform-admins
  editorGroups: developers
  defaultRole: user
```

Without Helm, set the variables:

```bash
export OF_OIDC_ISSUER=https://login.example.com/realms/acme
export OF_OIDC_CLIENT_ID=openfactory
export OF_OIDC_CLIENT_SECRET=...
export OF_OIDC_ADMIN_GROUPS=platform-admins
```

For every variable and Helm key, see [Configuration](configuration.md#oidc-login).

## Roles

At each login, the server maps the groups in `OF_OIDC_GROUPS_CLAIM` to a role:

1. A member of an `OF_OIDC_ADMIN_GROUPS` group is an admin.
2. Else a member of an `OF_OIDC_EDITOR_GROUPS` group is an editor.
3. Else the user gets `OF_OIDC_DEFAULT_ROLE` (default `user`).

The identity provider sets only the team-wide role. An admin can add a per-factory role with `PATCH /api/v1/users/{id}`; a login does not change it. See [Roles and permissions](../security.md#roles-and-permissions).

`OF_OIDC_ALLOWED_DOMAINS` limits sign-in to email domains. By default, ID tokens with an unverified email are rejected (`OF_OIDC_REQUIRE_VERIFIED_EMAIL`).

Dashboard sessions are signed with a key derived from the signing key, so they survive restarts and work on every replica. See [Dashboard sign-in](../security.md#dashboard-sign-in).

API keys keep working next to OIDC login. Use them for the CLI, SDKs and workers.

See also [Security](../security.md) and [Dashboard](../dashboard.md).
