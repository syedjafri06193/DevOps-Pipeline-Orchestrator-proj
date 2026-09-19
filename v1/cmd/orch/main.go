// Command orch is the deployment CLI (design.md section 17.3).
//
// The verbs are the ones the document lists, and two properties of section
// 17.4 shape every one of them:
//
//   - `--output=json` on everything that lists or shows, "because the first
//     thing anyone does with a deployment tool is script it".
//   - Errors say what to do next. A tool whose failure mode is a stack trace
//     gets used once.
//
// The CLI decides nothing. Whether a rollback is safe, who may approve, which
// version a promotion ships: all of it is the server's, so that a stale binary
// on someone's laptop cannot be a way around a gate.
package main

import (
	"errors"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		// errSilent means the command has already said everything useful, and
		// only the exit status is left to communicate.
		if !errors.Is(err, errSilent) {
			fmt.Fprintf(os.Stderr, "%s %v\n", paint(red, "error:"), err)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}
	switch args[0] {
	case "deploy":
		return cmdDeploy(args[1:])
	case "rollback":
		return cmdRollback(args[1:])
	case "promote":
		return cmdPromote(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "history":
		return cmdHistory(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	case "abort":
		return cmdAbort(args[1:])
	case "approve":
		return cmdApprove(args[1:])
	case "freeze":
		return cmdFreeze(args[1:], false)
	case "unfreeze":
		return cmdFreeze(args[1:], true)
	case "config":
		return cmdConfig(args[1:])
	case "audit":
		return cmdAudit(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `orch -- deploy things, and put them back when they go wrong

  orch deploy    --app A --env E --version V   start a deployment
  orch rollback  --app A --env E               roll back to the last good version
  orch promote   --app A --env E               ship the version from the source environment
  orch status                                  what is running where
  orch history   --app A --env E               past deployments
  orch logs      DEPLOYMENT                    a deployment's output
  orch abort     DEPLOYMENT                    stop a running deployment
  orch approve   DEPLOYMENT                    approve a deployment waiting on a gate
  orch freeze    --app A --env E --reason R    block deploys to a target
  orch unfreeze  --app A --env E               lift a freeze
  orch config validate [FILE]                  check a config file without a server
  orch audit verify                            check the audit log's hash chain
  orch doctor                                  check this installation

Global flags (any command):
  --server URL     orchd's address        (or $ORCH_SERVER)
  --token TOKEN    API token              (or $ORCH_TOKEN / $ORCH_TOKEN_FILE)
  --output json    machine-readable output

Run "orch <command> -h" for a command's own flags.
`)
}
