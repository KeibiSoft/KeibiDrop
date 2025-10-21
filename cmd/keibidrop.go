package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
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

	common.PrintBanner()

	// text output, level=DEBUG
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	logger := slog.New(handler).With("component", "cli")

	ui.Launch(logger)
}
