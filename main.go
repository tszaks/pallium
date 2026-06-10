package main

import (
	"fmt"
	"os"

	"github.com/tszaks/pallium/cmd"
)

func main() {
	app := cmd.NewApp(os.Stdout, os.Stderr)
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
