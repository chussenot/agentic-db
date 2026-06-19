// Package waybar implements the `claude-status gen-waybar` subcommand — it
// emits the paste-ready waybar format-icons JSON and style.css fragments,
// derived purely from the constants in internal/state (the single source of
// truth for the name grammar and render tables).
//
// The real logic is built in Stage B (agent B3). This is the frozen contract.
package waybar

import "errors"

// Run executes the gen-waybar subcommand. args is os.Args[2:] and may carry
// --icons / --css to select which fragment to emit (default: both).
func Run(args []string) error {
	return errors.New("not implemented")
}
