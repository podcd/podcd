// Command podcd is the debugging CLI for a host managed by podcd.
//
// It shares its whole implementation with the agent, so `podcd plan` shows
// exactly what the agent would do - not a second opinion about it.
package main

import (
	"os"

	"github.com/podcd/podcd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], ""))
}
