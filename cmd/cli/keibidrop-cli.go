package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	prompt "github.com/c-bata/go-prompt"
	"github.com/fatih/color"
)

func executor(in string) {
	in = strings.TrimSpace(in)
	args := strings.Fields(in)
	if len(args) == 0 {
		return
	}

	switch args[0] {
	case "help":
		printHelp()
	case "show":
		if len(args) < 2 {
			fmt.Println("Usage: show <what>")
			return
		}
		handleShow(args[1])
	case "register":
		if len(args) < 3 || args[1] != "peer" {
			fmt.Println("Usage: register peer <fingerprint>")
			return
		}
		registerPeer(args[2])
	case "create":
		createRoom()
	case "join":
		if len(args) < 2 {
			fmt.Println("Usage: join <peer fingerprint>")
			return
		}
		joinRoom(args[1])
	case "reset":
		resetSession()
	case "add":
		if len(args) < 2 {
			fmt.Println("Usage: add <filepath>")
			return
		}
		addFile(args[1])
	case "list":
		listFiles()
	case "pull":
		if len(args) < 3 {
			fmt.Println("Usage: pull <remote path> <local path>")
			return
		}
		pullFile(args[1], args[2])
	case "delete":
		if len(args) < 2 {
			fmt.Println("Usage: delete <filepath>")
			return
		}
		deleteFile(args[1])
	case "exit", "quit":
		fmt.Println("Goodbye.")
		os.Exit(0)
	default:
		color.Red("Unknown command: %s", args[0])
	}
}

func completer(d prompt.Document) []prompt.Suggest {
	s := []prompt.Suggest{
		{Text: "help", Description: "Show usage"},
		{Text: "show", Description: "Show info (ip, fingerprint, etc)"},
		{Text: "register", Description: "Register peer fingerprint"},
		{Text: "create", Description: "Create room"},
		{Text: "join", Description: "Join room"},
		{Text: "reset", Description: "Destroy and rotate"},
		{Text: "add", Description: "Add file/folder"},
		{Text: "list", Description: "List files"},
		{Text: "pull", Description: "Pull file/folder from peer"},
		{Text: "delete", Description: "Unshare file/folder"},
		{Text: "exit", Description: "Quit"},
	}
	return prompt.FilterHasPrefix(s, d.GetWordBeforeCursor(), true)
}

func printHelp() {
	color.Cyan(`
Available commands:
  help                         Show this help
  show fingerprint             Show your fingerprint
  show ip                      Show your IP
  show peer fingerprint        Show peer's fingerprint
  show peer ip                 Show peer's IP
  register peer <fingerprint> Register a peer's fingerprint
  create                       Create a room
  join <fingerprint>          Join a room by peer fingerprint
  reset                        Leave room and rotate keys
  add <filepath>               Share a file or directory
  list                         List shared files
  pull <remote> <local>        Copy file/folder from peer to local path
  delete <filepath>            Unshare a file or folder
  exit                         Quit
`)
}

// Placeholder stubs
func handleShow(what string)        { fmt.Println("Show", what) }
func registerPeer(fp string)        { fmt.Println("Registered peer:", fp) }
func createRoom()                   { fmt.Println("Created room") }
func joinRoom(fp string)            { fmt.Println("Joined room with", fp) }
func resetSession()                 { fmt.Println("Session reset") }
func addFile(p string)              { fmt.Println("Added", p) }
func listFiles()                    { fmt.Println("Listing files...") }
func pullFile(remote, local string) { fmt.Println("Pulled", remote, "→", local) }
func deleteFile(path string)        { fmt.Println("Deleted", path) }

func main() {
	common.PrintBanner()

	p := prompt.New(
		executor,
		completer,
		prompt.OptionPrefix("keibidrop> "),
		prompt.OptionTitle("keibidrop-cli"),
	)
	p.Run()
}
