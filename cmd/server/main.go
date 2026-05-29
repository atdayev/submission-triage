// Server entry point. Runtime logic lives in internal/app.
package main

import (
	"log"

	"github.com/atdayev/submission-triage/internal/app"
)

func main() {
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
