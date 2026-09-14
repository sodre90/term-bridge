package main

import (
	"fmt"
	"os"

	"github.com/sodre90/term-bridge/internal/cli"
	"github.com/sodre90/term-bridge/internal/logging"
	"github.com/sodre90/term-bridge/internal/version"
)

func main() {
	logging.Init()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := cli.RefuseLegacyConfigDir("cmux-relay", "term-bridge-relay"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "devices":
		os.Exit(runDevices(os.Args[2:]))
	case "tenants":
		os.Exit(runTenants(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("term-bridge-relay", version.String())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: term-bridge-relay <serve|devices|tenants|version> [flags]")
}
