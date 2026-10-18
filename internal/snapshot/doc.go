// Package snapshot saves the state of a git working directory to files and
// restores it later.
//
// A snapshot part is a directory that holds a manifest.json file and the
// files that the manifest names: git bundles, patches and tar archives
// compressed with zstd.
// This package builds and reads these files on the local disk only.
// It does no network work and it does not upload or download anything.
// The caller moves the files to and from the object store.
package snapshot
