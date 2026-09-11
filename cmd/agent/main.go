// Command podcd-agent reconciles this host against Git, forever.
//
// It is what the systemd user service runs. With no arguments it reconciles in
// a loop; it also accepts every podcd subcommand, so debugging on the host never
// needs a second binary.
package main

import (
	"os"

	"github.com/podcd/podcd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], "run"))
}
