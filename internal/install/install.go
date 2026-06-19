// Package install implements the `claude-status install` and `uninstall`
// subcommands — idempotently merging our hook entries into
// ~/.claude/settings.json (preserving existing hooks like bd prime), creating
// the state dir, and printing the niri/waybar setup fragments.
//
// The real logic is built in Stage B (agent B4). This is the frozen contract.
package install

import "errors"

// Run executes the install subcommand. args is os.Args[2:].
func Run(args []string) error {
	return errors.New("not implemented")
}

// RunUninstall executes the uninstall subcommand. args is os.Args[2:] and may
// carry --purge to also drop the state dir + DB.
func RunUninstall(args []string) error {
	return errors.New("not implemented")
}
