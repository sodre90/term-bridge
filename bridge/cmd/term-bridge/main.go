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
	// The hook runs inside every Claude Code session on the box and must
	// stay silent and exit 0 no matter what -- exit 2 is Claude Code's
	// "block this tool call" -- so it never goes through the config guard.
	if os.Args[1] == "hook" {
		os.Exit(runHook(os.Args[2:]))
	}
	if err := cli.RefuseLegacyConfigDir("cmux-bridge", "term-bridge"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "agent":
		os.Exit(runAgent(os.Args[2:]))
	case "pair-device":
		os.Exit(runPairDevice(os.Args[2:]))
	case "devices":
		os.Exit(runDevices(os.Args[2:]))
	case "status":
		os.Exit(runStatus(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("term-bridge", version.String())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: term-bridge <agent|pair-device|devices|status|hook|version> [flags]")
}
