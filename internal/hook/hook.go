// Package hook implements the `claude-status hook` subcommand — the hot path
// invoked by every Claude Code hook. It reads a hook JSON event from stdin,
// derives the new session state (via internal/state), resolves the niri window
// on SessionStart (via internal/niri), and upserts one row (via internal/db).
//
// The real logic is built in Stage B (agent B1). This is the frozen contract.
package hook

import "errors"

// Run executes the hook subcommand. args is os.Args[2:]. The implementation
// must swallow all errors and exit 0 so it never blocks Claude; a non-nil error
// returned here is for the dispatcher's benefit only during development.
func Run(args []string) error {
	return errors.New("not implemented")
}
