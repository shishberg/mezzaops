package app

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/shishberg/mezzaops/internal/dashboard"
	"github.com/shishberg/mezzaops/internal/discord"
	"github.com/shishberg/mezzaops/internal/mattermost"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/shishberg/mezzaops/internal/webhook"
)

// App wires all components together.
type App struct {
	cfg           *config.Config
	env           *config.Env
	manager       *service.Manager
	confirmations *service.ConfirmationTracker
	discordBot    *discord.Bot
	mmBot         *mattermost.Bot
	webhookSrv    *http.Server
	dashboardSrv  *http.Server
	cancel        context.CancelFunc
}

// New loads config and env, creates all components, and returns a ready App.
// templatesFS must contain index.html at its root.
func New(configPath string, envPath string, templatesFS fs.FS) (*App, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	env, err := config.LoadEnv(envPath)
	if err != nil {
		return nil, fmt.Errorf("loading env: %w", err)
	}

	svcs, err := config.LoadServices(cfg.ServicesDir)
	if err != nil {
		return nil, fmt.Errorf("loading services: %w", err)
	}

	a := &App{
		cfg:           cfg,
		env:           env,
		confirmations: service.NewConfirmationTracker(10 * time.Minute),
	}

	// Create Manager with NopNotifier initially.
	a.manager, err = service.NewManager(cfg, svcs, service.NopNotifier{})
	if err != nil {
		return nil, fmt.Errorf("creating manager: %w", err)
	}

	// Build notifier list.
	var notifiers service.MultiNotifier

	// Discord: check env.DiscordToken, then fall back to token.txt.
	discordToken := env.DiscordToken
	if discordToken == "" {
		if data, readErr := os.ReadFile("token.txt"); readErr == nil {
			discordToken = strings.TrimSpace(string(data))
		}
	}

	if cfg.Discord != nil && discordToken != "" {
		dcfg := discord.Config{
			Token:     discordToken,
			GuildID:   cfg.Discord.GuildID,
			ChannelID: cfg.Discord.ChannelID,
		}
		a.discordBot = discord.New(dcfg, a.manager)
		notifiers = append(notifiers, a.discordBot.Notifier())
	}

	// Mattermost.
	if cfg.Mattermost != nil && env.MattermostToken != "" {
		mcfg := mattermost.Config{
			URL:     cfg.Mattermost.URL,
			Token:   env.MattermostToken,
			Channel: cfg.Mattermost.Channel,
		}
		a.mmBot = mattermost.New(mcfg, a.manager)
		a.mmBot.SetConfirmHandler(a)
		notifiers = append(notifiers, mattermost.NewNotifier(a.mmBot))
	}

	// Set the real notifier on the manager.
	if len(notifiers) > 0 {
		a.manager.SetNotifier(notifiers)
	}

	// Webhook server.
	if cfg.Webhook != nil && env.WebhookSecret != "" {
		whHandler := webhook.NewHandler(env.WebhookSecret, a)
		webhookMux := http.NewServeMux()
		webhookMux.Handle("POST /webhook/github", whHandler)

		a.webhookSrv = &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Webhook.Port),
			Handler:      webhookMux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
	}

	// Dashboard server.
	if cfg.Dashboard != nil {
		dash, dashErr := dashboard.New(a.manager, templatesFS)
		if dashErr != nil {
			a.manager.Stop()
			return nil, fmt.Errorf("creating dashboard: %w", dashErr)
		}

		a.dashboardSrv = &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Dashboard.Port),
			Handler:      dash,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
	}

	return a, nil
}

// Manager returns the service manager for use by the CLI frontend.
func (a *App) Manager() *service.Manager {
	return a.manager
}

// Run starts all enabled components and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	if a.webhookSrv != nil {
		go func() {
			log.Printf("webhook server listening on %s", a.webhookSrv.Addr)
			if err := a.webhookSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("webhook server error: %v", err)
			}
		}()
	}

	if a.dashboardSrv != nil {
		go func() {
			log.Printf("dashboard server listening on %s", a.dashboardSrv.Addr)
			if err := a.dashboardSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("dashboard server error: %v", err)
			}
		}()
	}

	if a.discordBot != nil {
		go func() {
			if err := a.discordBot.Run(ctx); err != nil {
				log.Printf("discord bot error: %v", err)
			}
		}()
	}

	if a.mmBot != nil {
		go func() {
			if err := a.mmBot.Run(ctx); err != nil {
				log.Printf("mattermost bot error: %v", err)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// Shutdown gracefully stops all components.
func (a *App) Shutdown() {
	if a.cancel != nil {
		a.cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if a.webhookSrv != nil {
		if err := a.webhookSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("webhook server shutdown: %v", err)
		}
	}

	if a.dashboardSrv != nil {
		if err := a.dashboardSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("dashboard server shutdown: %v", err)
		}
	}

	a.manager.Stop()
}

// HandlePush implements webhook.DeployTrigger.
func (a *App) HandlePush(repo, branch string) {
	svcName, ok := a.manager.FindServiceByRepo(repo, branch)
	if !ok {
		log.Printf("app: no service found for repo=%s branch=%s", repo, branch)
		return
	}

	svcCfg, _ := a.manager.GetServiceConfig(svcName)
	if svcCfg.RequireConfirmation {
		a.confirmations.AddPending(svcName, branch)
		if a.mmBot != nil {
			msg := fmt.Sprintf("Deploy queued for **%s** (repo: %s, branch: %s). "+
				"Reply `@mezzaops confirm %s` to proceed.", svcName, repo, branch, svcName)
			a.mmBot.PostMessage(context.Background(), msg)
		}
		return
	}

	if err := a.manager.RequestDeploy(svcName); err != nil {
		log.Printf("app: request deploy for %s: %v", svcName, err)
	}
}

// Confirm implements mattermost.ConfirmHandler.
func (a *App) Confirm(svc string) bool {
	if !a.confirmations.Confirm(svc) {
		return false
	}
	if err := a.manager.RequestDeploy(svc); err != nil {
		log.Printf("app: confirm deploy for %s: %v", svc, err)
	}
	return true
}
