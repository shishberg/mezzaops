package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/shishberg/mezzaops/task"
)

var (
	tokenFile = flag.String("token", "token.txt", "file containing the bot token")
	guildID   = flag.String("guild-id", "", "Guild ID, or empty to register globally")
	tasksYAML = flag.String("tasks", "tasks.yaml", "task config YAML file")
)

func subCommand(name, desc string) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Name:        name,
		Description: desc,
		Type:        discordgo.ApplicationCommandOptionSubCommand,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "task",
				Description: "Task name",
				Type:        discordgo.ApplicationCommandOptionString,
				Required:    true,
			},
		},
	}
}

var (
	defaultMemberPermissions int64 = discordgo.PermissionManageServer

	commands = []*discordgo.ApplicationCommand{
		{
			Name:        "ops",
			Description: "MezzaOps",
			Type:        discordgo.ChatApplicationCommand,
			Options: []*discordgo.ApplicationCommandOption{
				subCommand("start", "Start"),
				subCommand("stop", "Stop"),
				subCommand("restart", "Restart"),
				subCommand("logs", "Logs"),
			},
		},
	}
)

type stdoutMessager struct{}

func (s stdoutMessager) Send(format string, args ...any) {
	log.Println(fmt.Sprintf(format, args...))
}

func main() {
	flag.Parse()

	yaml, err := ioutil.ReadFile(*tasksYAML)
	if err != nil {
		log.Fatal(err)
	}
	tasks, err := task.ParseYAML(yaml)
	if err != nil {
		log.Fatal(err)
	}
	tasks.StartAll(stdoutMessager{})

	token, err := ioutil.ReadFile("token.txt")
	if err != nil {
		log.Fatal(err)
	}
	session, err := discordgo.New("Bot " + strings.TrimSpace(string(token)))
	if err != nil {
		log.Fatal(err)
	}
	if err := session.Open(); err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.ApplicationCommandData().Name {
		case "ops":
			options := i.ApplicationCommandData().Options
			msg := "ops"
			var task string
			if len(options) != 0 {
				msg = options[0].Name
				for _, opt := range options[0].Options {
					if opt.Name == "task" {
						task = opt.StringValue()
					}
				}
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Hello %s %s!", msg, task),
				},
			})
		}
	})

	var cmds []*discordgo.ApplicationCommand
	for _, c := range commands {
		cmd, err := session.ApplicationCommandCreate(session.State.User.ID, *guildID, c)
		if err != nil {
			log.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}

	log.Println("Running.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	for _, cmd := range cmds {
		session.ApplicationCommandDelete(session.State.User.ID, *guildID, cmd.ID)
	}

	log.Println("Shutting down.")
}
