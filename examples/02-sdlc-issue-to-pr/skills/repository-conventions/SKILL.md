---
name: repository-conventions
description: Coding, testing and pull request conventions shared by the web and iOS repositories. Use before changing code or opening a pull request.
---

# Repository conventions

- Branches: `factory/<short-slug>`. One logical change per pull request.
- Commits: imperative subject under 72 characters, body explains why.
- Web app (`acme/web-app`): TypeScript strict mode, `pnpm lint`, `pnpm test`,
  `pnpm build` must pass. Components live in `src/components/<Name>/`.
- iOS app (`acme/ios-app`): Swift 6, `xcodebuild test -scheme App`, SwiftLint
  clean. Snapshot tests live in `AppTests/Snapshots`.
- Pull request description sections: Summary, Changes, Validation, Screenshots,
  Risks.
- Never commit generated files listed in `.gitignore` or any credentials.
