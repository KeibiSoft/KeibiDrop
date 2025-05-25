package main

import (
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/ui"
	"github.com/inconshreveable/log15"
)

func getenv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func main() {
	// Default to production relay unless overridden
	relayURL := getenv("KEIBIDROP_RELAY", "https://keibidroprelay.keibisoft.com")

	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		log.Fatalf("invalid KEIBIDROP_RELAY URL: %v", err)
	}

	fmt.Println("Connecting to relay:", parsedURL.String())

	common.PrintBanner()

	ui.Launch(log15.New("component", "GUI"))
}
