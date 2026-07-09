package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/tszaks/pallium/cmd"
)

func main() {
	app := cmd.NewApp(os.Stdout, os.Stderr)
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A loop tick's non-success terminal state (no_op/blocked/exhausted/
		// stagnated/already_running) carries a SPECIFIC exit code so a
		// calling cron/agent can branch without parsing JSON — every other
		// command error still just exits 1, the existing generic behavior.
		var exitErr *cmd.LoopTickExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		os.Exit(1)
	}
}
