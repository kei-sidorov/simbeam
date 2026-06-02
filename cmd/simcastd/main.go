// Command simcastd is the simcast daemon. For Phase 0 (Bootstrap) it exposes a
// single `list` subcommand that prints the real simulators on this machine,
// proving the Go-code -> idb_companion -> CoreSimulator chain end to end.
package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/kei-sidorov/simcast/internal/companion"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch args[0] {
	case "list":
		if err := runList(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "simcastd — simcast daemon (Phase 0 bootstrap)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  simcastd list    List available iOS simulators via idb_companion")
	fmt.Fprintln(w, "  simcastd help    Show this help")
}

func runList() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := companion.New()
	path, err := c.Resolve()
	if err != nil {
		return err
	}
	if v, err := c.Version(ctx); err == nil {
		fmt.Printf("idb_companion: %s (built %s)\n\n", path, v)
	} else {
		fmt.Printf("idb_companion: %s\n\n", path)
	}

	sims, err := c.List(ctx)
	if err != nil {
		return err
	}
	if len(sims) == 0 {
		fmt.Println("No simulators found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STATE\tNAME\tOS\tUDID")
	for _, s := range sims {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.State, s.Name, s.OSVersion, s.UDID)
	}
	w.Flush()

	fmt.Printf("\n%d simulator(s).\n", len(sims))
	return nil
}
