package main

import "filet/internal/cli"

var version = "dev"

func main() {
	cli.Execute(version)
}
