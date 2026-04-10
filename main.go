package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/shishberg/mezzaops/internal/app"
	"github.com/shishberg/mezzaops/internal/cli"
)

//go:embed templates
var templatesFS embed.FS

func main() {
	configPath := flag.String("config", "config.yaml", "config file path")
	envPath := flag.String("env", ".env", "env file path")
	interactive := flag.Bool("i", false, "interactive CLI mode")
	flag.Parse()

	templates, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		log.Fatalf("embedded templates: %v", err)
	}

	a, err := app.New(*configPath, *envPath, templates)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling.
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sc:
			log.Println("Shutting down...")
		case <-a.Manager().ShutdownCh():
			log.Println("Self-deploy complete, shutting down...")
		}
		a.Shutdown()
		cancel()
	}()

	if *interactive {
		// Run the app (servers, bots) in the background; CLI in the foreground.
		go func() {
			if err := a.Run(ctx); err != nil {
				log.Printf("app error: %v", err)
			}
		}()
		if err := cli.Run(ctx, a.Manager()); err != nil {
			log.Printf("cli error: %v", err)
		}
		a.Shutdown()
		return
	}

	if err := a.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
