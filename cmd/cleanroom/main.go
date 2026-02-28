package main

import (
	"fmt"
	"os"

	"github.com/buildkite/cleanroom/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Run(os.Args[1:], version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}
