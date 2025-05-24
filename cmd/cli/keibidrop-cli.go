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

	case "version":
		common.PrintBanner()

	case "show":
		if len(args) < 2 {
			fmt.Println("Usage: show <fingerprint|ip|peer fingerprint|peer ip>")
			return
		}
		handleShow(strings.Join(args[1:], " "))

	case "register":
		if len(args) != 3 || args[1] != "peer" {
			fmt.Println("Usage: register peer <fingerprint>")
			return
		}
		registerPeer(args[2])

	case "create":
		createRoom()

	case "join":
		if len(args) != 2 {
			fmt.Println("Usage: join <peer fingerprint>")
			return
		}
		joinRoom(args[1])

	case "reset":
		resetSession()

	case "add":
		if len(args) != 2 {
			fmt.Println("Usage: add <filepath>")
			return
		}
		addFile(args[1])

	case "list":
		listFiles()

	case "pull":
		if len(args) != 3 {
			fmt.Println("Usage: pull <remote path> <local path>")
			return
		}
		pullFile(args[1], args[2])

	case "delete":
		if len(args) != 2 {
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
		{Text: "help", Description: "Show help message"},
		{Text: "version", Description: "Show banner, version and commit hash"},
		{Text: "show", Description: "Show local or peer info"},
		{Text: "register", Description: "Register peer fingerprint"},
		{Text: "create", Description: "Create a room"},
		{Text: "join", Description: "Join a room by fingerprint"},
		{Text: "reset", Description: "Reset session, rotate keys"},
		{Text: "add", Description: "Add file or folder to share"},
		{Text: "list", Description: "List shared files"},
		{Text: "pull", Description: "Pull file/folder from peer"},
		{Text: "delete", Description: "Stop sharing a file/folder"},
		{Text: "exit", Description: "Exit the CLI"},
	}
	return prompt.FilterHasPrefix(s, d.GetWordBeforeCursor(), true)
}

func printHelp() {
	fmt.Println(`
help                         Show this help message
version 					 Show banner and version
show fingerprint             Show your fingerprint
show ip                      Show your IP
show peer fingerprint        Show peer's fingerprint
show peer ip                 Show peer's IP
register peer <fingerprint> Register a peer's fingerprint
create                       Create a room
join <fingerprint>           Join a room by peer fingerprint
reset                        Reset session and rotate keys
add <filepath>               Share a file or directory
list                         List shared files and their locations
pull <remote> <local>        Copy file/folder from peer to local path
delete <filepath>            Unshare a file or folder
exit                         Quit the CLI`)
}

func handleShow(what string) {
	switch what {
	case "fingerprint":
		fmt.Println("[TODO] Your fingerprint is: <fingerprint>")
	case "ip":
		fmt.Println("[TODO] Your IP is: <ip>")
	case "peer fingerprint":
		fmt.Println("[TODO] Peer fingerprint is: <peer fingerprint>")
	case "peer ip":
		fmt.Println("[TODO] Peer IP is: <peer ip>")
	default:
		fmt.Println("Unknown show command.")
	}
}

func registerPeer(fp string) { fmt.Println("[TODO] Registered peer fingerprint:", fp) }
func createRoom()            { fmt.Println("[TODO] Room created.") }
func joinRoom(fp string)     { fmt.Println("[TODO] Joined room with:", fp) }
func resetSession()          { fmt.Println("[TODO] Session reset and keys rotated.") }
func addFile(p string)       { fmt.Println("[TODO] Added to shared list:", p) }
func listFiles()             { fmt.Println("[TODO] Listing shared files...") }
func pullFile(remote, local string) {
	fmt.Printf("[TODO] Pulled '%s' to '%s'\n", remote, local)
}
func deleteFile(path string) { fmt.Println("[TODO] Unshared:", path) }

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
