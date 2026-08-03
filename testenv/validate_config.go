// Command validate_config checks the generated lab configuration with the same
// strict loader used by slusk itself.
package main

import (
	"fmt"
	"os"

	"github.com/samuelenocsson/slusk/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: validate_config <config.toml>")
		os.Exit(2)
	}
	if _, err := config.Load(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
