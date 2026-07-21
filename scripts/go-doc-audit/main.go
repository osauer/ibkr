// Command go-doc-audit reports missing, duplicated, and malformed Go
// documentation comments without evaluating build constraints.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("go-doc-audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "exit nonzero when documentation findings are present")
	strict := flags.Bool("strict", false, "alias for -check")
	reportOnly := flags.Bool("report-only", false, "print findings and exit successfully")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: go-doc-audit [-check|-strict|-report-only] [file.go ...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *reportOnly && (*check || *strict) {
		fmt.Fprintln(stderr, "go-doc-audit: -report-only cannot be combined with -check or -strict")
		return 2
	}

	paths := flags.Args()
	if len(paths) == 0 {
		var err error
		paths, err = gitGoFiles(".")
		if err != nil {
			fmt.Fprintf(stderr, "go-doc-audit: %v\n", err)
			return 2
		}
	}

	findings, err := audit(paths)
	if err != nil {
		fmt.Fprintf(stderr, "go-doc-audit: %v\n", err)
		return 2
	}
	for _, finding := range findings {
		fmt.Fprintln(stdout, finding)
	}
	if len(findings) > 0 && (*check || *strict) {
		return 1
	}
	return 0
}
