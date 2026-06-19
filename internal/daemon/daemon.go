// Package daemon implements the `claude-status daemon` subcommand — the
// long-lived reconciler. It maintains an in-memory niri window/workspace model
// (from the niri event stream), polls the sessions DB, advances decay buckets,
// reaps dead sessions, and is the sole mutator of niri workspace names.
//
// The real logic is built in Stage B (agent B2). This is the frozen contract.
package daemon

import "errors"

// Run executes the daemon subcommand. args is os.Args[2:]. It blocks until the
// process is signaled.
func Run(args []string) error {
	return errors.New("not implemented")
}
