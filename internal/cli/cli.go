package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// ServiceManager matches the interface used by Discord/Mattermost frontends.
type ServiceManager interface {
	Do(name, op string) string
	RequestDeploy(name string) error
	StartAll()
	StopAll()
	Reload() error
	ServiceNames() []string
	CountRunning() (int, int)
}

// Run starts an interactive CLI reading from stdin.
func Run(ctx context.Context, manager ServiceManager) error {
	return RunWithReader(ctx, manager, os.Stdin)
}

// RunWithReader starts an interactive CLI reading commands from r.
func RunWithReader(ctx context.Context, manager ServiceManager, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	fmt.Println("MezzaOps CLI. Type 'help' for commands.")

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		cmd := fields[0]
		var svc string
		if len(fields) >= 2 {
			svc = fields[1]
		}

		switch cmd {
		case "help":
			fmt.Println("Commands:")
			fmt.Println("  status <service>    Show service status")
			fmt.Println("  start <service>     Start a service")
			fmt.Println("  stop <service>      Stop a service")
			fmt.Println("  restart <service>   Restart a service")
			fmt.Println("  logs <service>      Show service logs")
			fmt.Println("  pull <service>      Git pull in service dir")
			fmt.Println("  deploy <service>    Request a deploy")
			fmt.Println("  reload              Reload config")
			fmt.Println("  start-all           Start all services")
			fmt.Println("  stop-all            Stop all services")
			fmt.Println("  list                List all services")
			fmt.Println("  count               Show running/total count")
			fmt.Println("  quit                Exit the CLI")

		case "quit", "exit":
			return nil

		case "status", "start", "stop", "restart", "logs", "pull":
			if svc == "" {
				fmt.Printf("usage: %s <service>\n", cmd)
				continue
			}
			fmt.Println(manager.Do(svc, cmd))

		case "deploy":
			if svc == "" {
				fmt.Println("usage: deploy <service>")
				continue
			}
			if err := manager.RequestDeploy(svc); err != nil {
				fmt.Println("error:", err)
			} else {
				fmt.Println("deploy requested for", svc)
			}

		case "reload":
			if err := manager.Reload(); err != nil {
				fmt.Println("error:", err)
			} else {
				fmt.Println("config reloaded")
			}

		case "start-all":
			manager.StartAll()
			fmt.Println("starting all services")

		case "stop-all":
			manager.StopAll()
			fmt.Println("stopping all services")

		case "list":
			for _, name := range manager.ServiceNames() {
				fmt.Println(" ", name)
			}

		case "count":
			r, total := manager.CountRunning()
			fmt.Printf("%d/%d running\n", r, total)

		default:
			fmt.Printf("unknown command: %s (type 'help' for commands)\n", cmd)
		}
	}
	return scanner.Err()
}
