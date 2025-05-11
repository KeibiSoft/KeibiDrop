package main

import (
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/ui"
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

	ui.Launch()

	// TODO: Alice flow begins here
	// - Generate keypair (or load existing)
	// - Create fingerprint
	// - Encode public keys as base64
	// - POST to relay server
	// - Log/display the returned fingerprint for Bob
}
