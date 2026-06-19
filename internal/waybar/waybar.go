// Package waybar implements the `claude-status gen-waybar` subcommand — it
// emits the paste-ready waybar format-icons JSON and style.css fragments,
// derived purely from the constants in internal/state (the single source of
// truth for the name grammar and render tables).
package waybar

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Run executes the gen-waybar subcommand. args is os.Args[2:] and may carry
// --icons / --css to select which fragment to emit (default: both).
func Run(args []string) error {
	fs := flag.NewFlagSet("gen-waybar", flag.ContinueOnError)
	icons := fs.Bool("icons", false, "print only the niri/workspaces format-icons map")
	css := fs.Bool("css", false, "print only the style.css fragments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *icons && *css {
		// Both flags == default behavior (print both).
		*icons, *css = false, false
	}
	return run(os.Stdout, *icons, *css)
}

// run writes the requested fragments to w. With neither icons nor css set it
// prints both, with comment headers explaining where each goes.
func run(w io.Writer, icons, css bool) error {
	both := !icons && !css
	if icons || both {
		if both {
			fmt.Fprintln(w, "// ===== format-icons — paste as the \"format-icons\" value of niri/workspaces in config.jsonc =====")
		}
		if err := writeIcons(w); err != nil {
			return err
		}
	}
	if both {
		fmt.Fprintln(w)
		fmt.Fprintln(w)
	}
	if css || both {
		if both {
			fmt.Fprintln(w, "/* ===== style.css — paste these rules into style.css ===== */")
		}
		if err := writeCSS(w); err != nil {
			return err
		}
	}
	return nil
}
