// The event context of a run message: envelope.json, payload.json and the
// files of the provider context builders, written by the runner under
// .openfactory/events/<message_id>/. Port of internal/eventctx.
//
//   types.ts             shared types and limits
//   bundle.ts            builds, encodes and describes a bundle
//   storage.ts           bundles in R2 by the SHA-256 of their JSON, and their expiry
//   builders/payload.ts  context the payload already has (issues, review comments)
//   builders/review.ts   review thread and file diff read from the forge
export * from "./types";
export * from "./bundle";
export * from "./storage";
export { defaultBuilders } from "./builders/payload";
export { diffFile, reviewBuilders, threadFile } from "./builders/review";
