import pkg from "../package.json" with { type: "json" };

/** The CLI version, from package.json. */
export const VERSION: string = pkg.version;
