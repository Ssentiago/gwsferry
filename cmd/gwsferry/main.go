package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()
	Execute()
}
