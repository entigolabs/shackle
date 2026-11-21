// Command shackle gives a CI/CD job access to a cloud's Kubernetes cluster,
// container registry and object storage with the job's own cloud identity,
// without the cloud's CLI.
package main

import (
	"os"

	"github.com/entigolabs/shackle/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], version, os.Stdout, os.Stderr))
}
