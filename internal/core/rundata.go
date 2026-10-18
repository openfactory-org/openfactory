package core

// Run data kept outside the database.

// ArtifactsDir is the directory, relative to the sandbox workspace
// (Sandbox.WorkDir), whose files are collected as run artifacts after every
// turn. Runs see its absolute path in $OF_ARTIFACTS_DIR.
const ArtifactsDir = ".openfactory/artifacts"

// EnvArtifactsDir names the environment variable holding the absolute
// artifacts directory of a run's sandbox.
const EnvArtifactsDir = "OF_ARTIFACTS_DIR"
