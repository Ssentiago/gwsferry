package main

import (
	"fmt"
	"os"

	"github.com/pterm/pterm"
)

func main() {
	pterm.EnableStyling()
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()
	Execute()
}
