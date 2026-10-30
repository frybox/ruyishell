package agent

// Test-only constructors for the types the interactive command layer
// renders but only the engine builds: a /jobs row formats a *Job, and a
// test that only exercises the formatting must be able to make one without
// spawning a process. This file is ordinary (non-_test) code because the
// consumer lives in another package — cmd/rysh's /jobs table test — where
// a _test.go declaration would not be visible. Nothing here is on the
// engine's path.

import "time"

// NewJobForTest returns an unstarted job record with the given command, as
// if it had just been launched and were running. No process is created, so
// Elapsed counts from now and Kill has nothing to signal; callers that need
// a real lifetime use JobManager.StartBackground instead.
func NewJobForTest(command string) *Job {
	return &Job{Command: command, Started: time.Now()}
}
