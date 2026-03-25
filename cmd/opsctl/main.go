// opsctl is a CLI interface to mezzaops for interactive testing.
// It provides the same task management commands as the Discord bot
// but driven from stdin, so you can test without Discord.
//
// Usage:
//
//	go run ./cmd/opsctl --tasks tasks.yaml
//
// Commands:
//
//	start <task>      Start a task
//	stop <task>       Stop a task
//	restart <task>    Restart a task
//	status <task>     Check task status
//	logs <task>       Show task logs
//	pull <task>       Git pull in task directory
//	start-all         Start all tasks
//	stop-all          Stop all tasks
//	reload            Reload config
//	list              List all configured tasks
//	quit              Exit (tasks keep running)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/shishberg/mezzaops/task"
)

var (
	tasksYAML = flag.String("tasks", "tasks.yaml", "task config YAML file")
	logDir    = flag.String("log-dir", "logs", "directory for task log files")
	stateDir  = flag.String("state-dir", "state", "directory for task PID state files")
)

type cliMessager struct{}

func (c cliMessager) Send(format string, args ...any) {
	fmt.Printf("  [msg] %s\n", fmt.Sprintf(format, args...))
}

func main() {
	flag.Parse()

	tasks, err := task.StartFromConfig(*tasksYAML, *logDir, *stateDir, cliMessager{})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("opsctl ready. Type 'help' for commands.")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		cmd := parts[0]
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}

		switch cmd {
		case "help":
			fmt.Println("Commands:")
			fmt.Println("  start <task>      Start a task")
			fmt.Println("  stop <task>       Stop a task")
			fmt.Println("  restart <task>    Restart a task")
			fmt.Println("  status <task>     Check task status")
			fmt.Println("  logs <task>       Show task logs")
			fmt.Println("  pull <task>       Git pull in task directory")
			fmt.Println("  start-all         Start all tasks")
			fmt.Println("  stop-all          Stop all tasks")
			fmt.Println("  reload            Reload config")
			fmt.Println("  list              List configured tasks")
			fmt.Println("  quit              Exit (tasks keep running)")

		case "start", "stop", "restart", "status", "logs", "pull":
			if arg == "" {
				fmt.Printf("usage: %s <task>\n", cmd)
				continue
			}
			t := tasks.Get(arg)
			if t == nil {
				fmt.Printf("unknown task: %s\n", arg)
				continue
			}
			fmt.Println(t.Do(cmd))

		case "start-all":
			tasks.StartAll()
			fmt.Println("all tasks starting")

		case "stop-all":
			tasks.StopAll()
			fmt.Println("all tasks stopping")

		case "reload":
			if err := tasks.Reload(); err != nil {
				fmt.Printf("reload error: %v\n", err)
			} else {
				fmt.Println("config reloaded")
			}

		case "list":
			for name, t := range tasks.Tasks {
				fmt.Printf("  %s: %s\n", name, t.Do("status"))
			}

		case "quit", "exit":
			fmt.Println("exiting (tasks will keep running)")
			return

		default:
			fmt.Printf("unknown command: %s (type 'help')\n", cmd)
		}
	}
}
