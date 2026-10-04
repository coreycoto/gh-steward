package main

import (
	"context"
	"fmt"
	"os"

	"github.com/coreycoto/gh-steward/internal/cli"
)

func main() {
	if err := (cli.Runner{Out: os.Stdout, Err: os.Stderr, Input: os.Stdin}).Run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gh-steward:", err)
		os.Exit(1)
	}
}
