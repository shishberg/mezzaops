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
	channelID = flag.String("channel-id", "", "Channel ID for broadcast messages")
	tasksYAML = flag.String("tasks", "tasks.yaml", "task config YAML file")
)

func subCommandGroup(name, desc string, tasks *task.Tasks) *discordgo.ApplicationCommandOption {
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

type channelMessager struct {
	session   *discordgo.Session
	channelID string
}

func (c channelMessager) Send(format string, args ...any) {
	_, err := c.session.ChannelMessageSend(c.channelID, fmt.Sprintf(format, args...))
	if err != nil {
		log.Println(err)
	}
}

func main() {
	flag.Parse()

	tasks, err := task.ReadConfig(*tasksYAML)
	if err != nil {
		log.Fatal(err)
	}

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
				subCommandGroup("status", "Status", tasks),
				subCommandGroup("pull", "git pull", tasks),
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

	var msgr task.Messager
	if *channelID != "" {
		msgr = channelMessager{session, *channelID}
	} else {
		msgr = stdoutMessager{}
	}
	tasks.StartAll(msgr)

	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.ApplicationCommandData().Name {
		case "ops":
			resp := func() string {
				var opOpt, taskOpt *discordgo.ApplicationCommandInteractionDataOption
				for _, opt := range i.ApplicationCommandData().Options {
					if opt.Type == discordgo.ApplicationCommandOptionSubCommandGroup {
						opOpt = opt
						break
					}
				}
				if opOpt == nil {
					return "operation required"
				}
				for _, opt := range opOpt.Options {
					if opt.Type == discordgo.ApplicationCommandOptionSubCommand {
						taskOpt = opt
						break
					}
				}
				if taskOpt == nil {
					return "task required"
				}
				task := tasks.Get(taskOpt.Name)
				if task == nil {
					return "unknown task " + taskOpt.Name
				}
				return fmt.Sprintf("%s: %s", taskOpt.Name, task.Do(opOpt.Name))
			}()
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: resp,
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

	tasks.StopAll()

	log.Println("Shutting down.")
}
