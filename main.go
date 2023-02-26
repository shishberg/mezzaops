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

func subCommandGroup(name, desc string, tasks task.Tasks) *discordgo.ApplicationCommandOption {
	aco := &discordgo.ApplicationCommandOption{
		Name:        name,
		Description: desc,
		Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
	}
	for _, t := range tasks.Tasks {
		aco.Options = append(aco.Options, &discordgo.ApplicationCommandOption{
			Name:        t.Name,
			Description: t.Name,
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		})
	}
	return aco
}

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

	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "ops",
			Description: "MezzaOps",
			Type:        discordgo.ChatApplicationCommand,
			Options: []*discordgo.ApplicationCommandOption{
				subCommandGroup("start", "Start", tasks),
				subCommandGroup("stop", "Stop", tasks),
				subCommandGroup("restart", "Restart", tasks),
				subCommandGroup("logs", "Logs", tasks),
			},
		},
	}

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
					task = opt.Name
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

	_, err = session.ApplicationCommandBulkOverwrite(session.State.User.ID, *guildID, commands)
	if err != nil {
		log.Fatal(err)
	}
	defer session.ApplicationCommandBulkOverwrite(session.State.User.ID, *guildID, nil)

	log.Println("Running.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	log.Println("Shutting down.")
}
