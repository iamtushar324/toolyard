// Command toolyard is the standalone CLI for the toolyard gateway.
//
// It is a thin REST client over the gateway's bearer-authenticated
// endpoints. Designed for one-shot use on servers / scripts that don't
// want to hold a long-lived MCP connection open.
//
// Usage:
//
//	toolyard auth login <enrollment-code> [--server https://host:port]
//	toolyard auth status
//	toolyard auth logout
//	toolyard server <url>
//	toolyard list
//	toolyard call <tool> [--arg key=value]... [--json '{...}'] [--approve-wait 30s]
//
// Auth flow: an operator generates an enrollment code from the toolyard
// dashboard (Agents → Enroll), runs `toolyard auth login <code>` on the
// target server, and the CLI swaps it for a long-lived agent token via
// POST /v1/agents/exchange. The token is stored in ~/.toolyard/config.json
// (mode 0600) and used as Authorization: Bearer <token> on every
// subsequent call.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "auth":
		err = runAuth(args)
	case "server":
		err = runServer(args)
	case "list":
		err = runList(args)
	case "call":
		err = runCall(args)
	case "version", "-v", "--version":
		fmt.Println("toolyard", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`toolyard ` + version + ` — CLI for the toolyard gateway

Commands:
  auth login <code>    swap an enrollment code for an agent token
  auth status          show current server + agent
  auth logout          delete the stored token
  server <url>         set the configured gateway URL
  list                 list available tools (name + description)
  call <tool> [...]    run a tool one-shot

Run 'toolyard <command> -h' for command-specific flags.

Config:
  ~/.toolyard/config.json  (mode 0600)`)
}
