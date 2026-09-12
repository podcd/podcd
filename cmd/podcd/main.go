package main

import (
	"os"

	"github.com/podcd/podcd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
