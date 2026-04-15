package discord

import (
	"fmt"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock ServiceManager ---

type mockManager struct {
	doResult       string
	doName         string
	doOp           string
	deployErr      error
	deployName     string
	reloadErr      error
	reloadCalled   bool
	startAllCalled bool
	stopAllCalled  bool
	serviceNames   []string
	runningCount   int
	totalCount     int
	onChangeFn     func(string, string)
}

func (m *mockManager) Do(name, op string) string {
	m.doName = name
	m.doOp = op
	return m.doResult
}

func (m *mockManager) RequestDeploy(name string) error {
	m.deployName = name
	return m.deployErr
}

func (m *mockManager) StartAll() {
	m.startAllCalled = true
}

func (m *mockManager) StopAll() {
	m.stopAllCalled = true
}

func (m *mockManager) Reload() error {
	m.reloadCalled = true
	return m.reloadErr
}

func (m *mockManager) ServiceNames() []string {
	return m.serviceNames
}

func (m *mockManager) CountRunning() (int, int) {
	return m.runningCount, m.totalCount
}

func (m *mockManager) SetOnChange(fn func(name, event string)) {
	m.onChangeFn = fn
}

// --- buildCommands tests ---

func TestBuildCommands_Structure(t *testing.T) {
	names := []string{"api", "web", "worker"}
	cmds := buildCommands(names)

	require.Len(t, cmds, 1, "should produce exactly one /ops command")
	ops := cmds[0]
	assert.Equal(t, "ops", ops.Name)
	assert.Equal(t, discordgo.ChatApplicationCommand, ops.Type)

	// Expect: reload, start-all, stop-all (subcommands)
	//         start, stop, restart, logs, status, pull, deploy (subcommand groups)
	require.Len(t, ops.Options, 10)

	// Check the 3 subcommands
	assert.Equal(t, "reload", ops.Options[0].Name)
	assert.Equal(t, discordgo.ApplicationCommandOptionSubCommand, ops.Options[0].Type)
	assert.Equal(t, "start-all", ops.Options[1].Name)
	assert.Equal(t, discordgo.ApplicationCommandOptionSubCommand, ops.Options[1].Type)
	assert.Equal(t, "stop-all", ops.Options[2].Name)
	assert.Equal(t, discordgo.ApplicationCommandOptionSubCommand, ops.Options[2].Type)

	// Check groups
	groupNames := []string{"start", "stop", "restart", "logs", "status", "pull", "deploy"}
	for i, gn := range groupNames {
		opt := ops.Options[3+i]
		assert.Equal(t, gn, opt.Name, "group at position %d", 3+i)
		assert.Equal(t, discordgo.ApplicationCommandOptionSubCommandGroup, opt.Type)
		// Each group should have one subcommand per service
		require.Len(t, opt.Options, len(names), "group %q should have %d subcommands", gn, len(names))
		for j, sn := range names {
			assert.Equal(t, sn, opt.Options[j].Name)
			assert.Equal(t, discordgo.ApplicationCommandOptionSubCommand, opt.Options[j].Type)
		}
	}
}

func TestBuildCommands_EmptyServices(t *testing.T) {
	cmds := buildCommands(nil)
	require.Len(t, cmds, 1)
	ops := cmds[0]
	// Still 10 options (3 subcommands + 7 groups), but groups have 0 subcommands
	require.Len(t, ops.Options, 10)
	for _, opt := range ops.Options[3:] {
		assert.Empty(t, opt.Options, "group %q should be empty", opt.Name)
	}
}

func TestBuildCommands_IncludesDeployGroup(t *testing.T) {
	cmds := buildCommands([]string{"myapp"})
	ops := cmds[0]

	var found bool
	for _, opt := range ops.Options {
		if opt.Name == "deploy" {
			found = true
			assert.Equal(t, discordgo.ApplicationCommandOptionSubCommandGroup, opt.Type)
			require.Len(t, opt.Options, 1)
			assert.Equal(t, "myapp", opt.Options[0].Name)
		}
	}
	assert.True(t, found, "deploy group should exist")
}

// --- handleInteraction routing tests ---

// helper to build a fake InteractionCreate with subcommand (reload, start-all, stop-all)
func fakeSubcommandInteraction(subName string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "ops",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{
						Name: subName,
						Type: discordgo.ApplicationCommandOptionSubCommand,
					},
				},
			},
		},
	}
}

// helper to build a fake InteractionCreate with subcommand group + service subcommand
func fakeGroupInteraction(group, svc string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "ops",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{
					{
						Name: group,
						Type: discordgo.ApplicationCommandOptionSubCommandGroup,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{
							{
								Name: svc,
								Type: discordgo.ApplicationCommandOptionSubCommand,
							},
						},
					},
				},
			},
		},
	}
}

func TestHandleInteraction_Reload(t *testing.T) {
	mgr := &mockManager{
		serviceNames: []string{"api"},
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeSubcommandInteraction("reload"))
	assert.True(t, mgr.reloadCalled)
	assert.Equal(t, "Config reloaded", resp)
}

func TestHandleInteraction_ReloadError(t *testing.T) {
	mgr := &mockManager{
		serviceNames: []string{"api"},
		reloadErr:    fmt.Errorf("bad config"),
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeSubcommandInteraction("reload"))
	assert.True(t, mgr.reloadCalled)
	assert.Equal(t, "Config reload error: bad config", resp)
}

func TestHandleInteraction_StartAll(t *testing.T) {
	mgr := &mockManager{}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeSubcommandInteraction("start-all"))
	assert.True(t, mgr.startAllCalled)
	assert.Equal(t, "all tasks starting", resp)
}

func TestHandleInteraction_StopAll(t *testing.T) {
	mgr := &mockManager{}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeSubcommandInteraction("stop-all"))
	assert.True(t, mgr.stopAllCalled)
	assert.Equal(t, "all tasks stopping", resp)
}

func TestHandleInteraction_StartService(t *testing.T) {
	mgr := &mockManager{
		doResult:     "started",
		serviceNames: []string{"api"},
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("start", "api"))
	assert.Equal(t, "api", mgr.doName)
	assert.Equal(t, "start", mgr.doOp)
	assert.Equal(t, "api: started", resp)
}

func TestHandleInteraction_StopService(t *testing.T) {
	mgr := &mockManager{
		doResult: "stopped",
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("stop", "web"))
	assert.Equal(t, "web", mgr.doName)
	assert.Equal(t, "stop", mgr.doOp)
	assert.Equal(t, "web: stopped", resp)
}

func TestHandleInteraction_RestartService(t *testing.T) {
	mgr := &mockManager{
		doResult: "restarted",
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("restart", "worker"))
	assert.Equal(t, "worker", mgr.doName)
	assert.Equal(t, "restart", mgr.doOp)
	assert.Equal(t, "worker: restarted", resp)
}

func TestHandleInteraction_LogsService(t *testing.T) {
	mgr := &mockManager{
		doResult: "some log output",
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("logs", "api"))
	assert.Equal(t, "api", mgr.doName)
	assert.Equal(t, "logs", mgr.doOp)
	assert.Equal(t, "api: some log output", resp)
}

func TestHandleInteraction_StatusService(t *testing.T) {
	mgr := &mockManager{
		doResult: "running",
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("status", "api"))
	assert.Equal(t, "api", mgr.doName)
	assert.Equal(t, "status", mgr.doOp)
	assert.Equal(t, "api: running", resp)
}

func TestHandleInteraction_PullService(t *testing.T) {
	mgr := &mockManager{
		doResult: "Already up to date.",
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("pull", "api"))
	assert.Equal(t, "api", mgr.doName)
	assert.Equal(t, "pull", mgr.doOp)
	assert.Equal(t, "api: Already up to date.", resp)
}

func TestHandleInteraction_DeployService(t *testing.T) {
	mgr := &mockManager{}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("deploy", "api"))
	assert.Equal(t, "api", mgr.deployName)
	assert.Equal(t, "Deploy requested for api", resp)
}

func TestHandleInteraction_DeployError(t *testing.T) {
	mgr := &mockManager{
		deployErr: fmt.Errorf("service \"api\" not found"),
	}
	b := &Bot{manager: mgr}

	resp := b.routeInteraction(fakeGroupInteraction("deploy", "api"))
	assert.Equal(t, "api", mgr.deployName)
	assert.Contains(t, resp, "not found")
}

func TestHandleInteraction_NoOptions(t *testing.T) {
	mgr := &mockManager{}
	b := &Bot{manager: mgr}

	ic := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name:    "ops",
				Options: nil,
			},
		},
	}
	resp := b.routeInteraction(ic)
	assert.Equal(t, "operation required", resp)
}

// --- Notifier tests ---

func TestNotifier_ServiceEvent(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.ServiceEvent("api", "started")
	assert.Equal(t, "**api**: started", sent)
}

func TestNotifier_DeployStarted(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.DeployStarted("web")
	assert.Equal(t, "Deploying **web**...", sent)
}

func TestNotifier_DeploySucceeded(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.DeploySucceeded("web", "all good")
	assert.Equal(t, "Deploy of **web** succeeded.", sent)
}

func TestNotifier_DeployFailed(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.DeployFailed("web", "build", "exit code 1\nsome error")
	expected := "Deploy of **web** failed at step `build`.\n```\nexit code 1\nsome error\n```"
	assert.Equal(t, expected, sent)
}

func TestNotifier_DeployFailed_LargeOutputTruncated(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}

	const tailMarker = "===FINAL_ERROR_MARKER_AT_TAIL==="
	// Build ~50 KB of "x\n" plus a distinctive tail that must survive.
	buf := make([]byte, 0, 50001+len(tailMarker))
	for i := 0; i < 25000; i++ {
		buf = append(buf, 'x', '\n')
	}
	buf = append(buf, tailMarker...)
	output := string(buf)

	n.DeployFailed("slurp", "go test -short ./...", output)

	// Whole sent message must fit under Discord's 2000-char message limit.
	runes := len([]rune(sent))
	assert.LessOrEqual(t, runes, discordMessageRuneLimit,
		"sent message exceeds Discord rune limit: %d > %d", runes, discordMessageRuneLimit)

	// Truncation marker present and the tail of the original output survives.
	assert.Contains(t, sent, "truncated")
	assert.Contains(t, sent, tailMarker)

	// Code fence intact.
	assert.Contains(t, sent, "```\n")
	tail := sent
	if len(tail) > 30 {
		tail = tail[len(tail)-30:]
	}
	assert.True(t, len(sent) >= 4 && sent[len(sent)-4:] == "\n```",
		"message should end with closing code fence; got tail %q", tail)
}

func TestNotifier_WebhookReceived_Full(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.WebhookReceived("api", service.WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		Compare:   "https://github.com/acme/myapp/compare/abc...def",
		Pusher:    "alice",
		CommitID:  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		CommitMsg: "feat: add a thing\n\nmore details here",
		CommitURL: "https://github.com/acme/myapp/commit/deadbeef",
		Author:    "Alice Author",
		Timestamp: "2026-04-10T12:34:56Z",
	})
	assert.Contains(t, sent, "api")
	assert.Contains(t, sent, "acme/myapp")
	assert.Contains(t, sent, "main")
	assert.Contains(t, sent, "deadbee") // short sha
	assert.Contains(t, sent, "https://github.com/acme/myapp/commit/deadbeef")
	assert.Contains(t, sent, "feat: add a thing")
	assert.NotContains(t, sent, "more details here", "should only show first line of commit msg")
	assert.Contains(t, sent, "Alice Author")
	assert.Contains(t, sent, "https://github.com/acme/myapp/compare/abc...def")
}

func TestNotifier_WebhookReceived_NoHeadCommit(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	n.WebhookReceived("api", service.WebhookInfo{
		Repo:   "acme/myapp",
		Branch: "main",
		Pusher: "alice",
	})
	assert.Contains(t, sent, "api")
	assert.Contains(t, sent, "main")
	assert.Contains(t, sent, "alice")
}

func TestNotifier_WebhookReceived_LongCommitMsg(t *testing.T) {
	var sent string
	n := &Notifier{
		sendFunc: func(msg string) { sent = msg },
	}
	long := ""
	for i := 0; i < 400; i++ {
		long += "x"
	}
	n.WebhookReceived("api", service.WebhookInfo{
		Repo:      "acme/myapp",
		Branch:    "main",
		CommitID:  "deadbeef",
		CommitMsg: long,
	})
	assert.Contains(t, sent, "...")
	// Must not include the full 400 x's
	assert.NotContains(t, sent, long)
}

func TestNotifier_FallbackToLog(t *testing.T) {
	// When session is nil (sendFunc nil), should not panic
	n := NewNotifier(nil, "")
	// Should not panic — falls back to log.Println
	n.ServiceEvent("api", "started")
}
