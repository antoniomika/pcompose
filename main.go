// Package main represents the main entrypoint of the pcompose application.
package main

import (
	"log"

	"github.com/antoniomika/pcompose/cmd"
)

// main will start the pcompose command lifecycle and spawn the pcompose services.
func main() {
	err := cmd.Execute()
	if err != nil {
		log.Println("Unable to execute root command:", err)
	}
}
