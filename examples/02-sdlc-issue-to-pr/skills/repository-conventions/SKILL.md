---
name: repository-conventions
description: Coding, testing and pull request conventions shared by the web app and API repositories. Use before changing code or opening a pull request.
---

# Repository conventions

- Branches: `factory/<short-slug>`. One logical change per pull request.
- Commits: imperative subject under 72 characters, body explains why.
- Web app (`acme/web-app`): TypeScript strict mode, `pnpm lint`, `pnpm test`,
  `pnpm build` must pass. Components live in `src/components/<Name>/`.
- API service (`acme/api-service`): TypeScript strict mode, `pnpm lint`,
  `pnpm test`. Integration tests live in `test/integration/`.
- Pull request description sections: Summary, Changes, Validation, Screenshots,
  Risks.
- Never commit generated files listed in `.gitignore` or any credentials.
