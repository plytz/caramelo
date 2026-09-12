package main

import (
	"os"

	"github.com/plytz/caramelo/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
