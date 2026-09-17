// Command pay drives any Payload CMS 3.x project through its REST API.
//
// This file is deliberately tiny: it exists to receive the link-time build
// stamps (§16.4) and to be the one place in the program that calls os.Exit.
// Everything else lives behind internal/cli.Main, which returns the exit code
// instead of taking it, so the whole CLI is testable in-process.
package main

import (
	"os"

	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cli"
)

// Stamped by goreleaser with -X main.version / main.commit / main.date.
// When they are absent — `go install .../cmd/pay@latest`, or `go run` —
// buildinfo falls back to debug.ReadBuildInfo.
var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	buildinfo.Set(version, commit, date)
	os.Exit(cli.Main())
}
