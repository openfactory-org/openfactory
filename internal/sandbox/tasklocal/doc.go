// Package tasklocal implements core.TaskBackend with one operating-system
// process on the host for each task.
//
// The backend gives no isolation. A task has the same user, file system and
// network as the process that runs the backend. Use it for development and
// tests only. Do not use it to run work that you do not trust.
//
// Each task gets a directory below the root of the backend. The process runs
// in the sub-directory "workspace" of that directory. Its output goes to the
// file "runner.log" in the task directory.
package tasklocal
