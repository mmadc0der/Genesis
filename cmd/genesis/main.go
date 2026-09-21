package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	command, args := parseCommand(os.Args[1:])
	switch command {
	case "listen", "":
		runListen(logger, args)
	case "launch":
		runLaunchFromArgs(logger, args)
	default:
		logger.Error("unknown command", "command", command)
		os.Exit(2)
	}
}

func parseCommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "listen", args
	}
	return args[0], args[1:]
}

func runLaunchFromArgs(logger *slog.Logger, args []string) {
	listen, agentsDir, rulesDir, syncToken, listenerUser := parseLaunchFlags(args, logger)
	runLaunch(logger, listen, agentsDir, rulesDir, syncToken, listenerUser)
}

func runListen(logger *slog.Logger, args []string) {
	listen, agentsDir, rulesDir, syncToken := parseListenFlags("listen", args, logger)

	absoluteAgentsDir, err := filepath.Abs(agentsDir)
	if err != nil {
		fail(logger, "resolve agents directory", err)
	}
	absoluteRulesDir, err := filepath.Abs(rulesDir)
	if err != nil {
		fail(logger, "resolve rules directory", err)
	}

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		fail(logger, "find python3 in PATH", err)
	}
	pythonPath, err = filepath.Abs(pythonPath)
	if err != nil {
		fail(logger, "resolve python3 path", err)
	}

	coordinator, closeCoordinator, err := inheritedCoordinator()
	if err != nil {
		fail(logger, "inherit privileged ipc", err)
	}
	defer closeCoordinator()

	handler := &eventServer{
		agentsDir:   absoluteAgentsDir,
		rulesDir:    absoluteRulesDir,
		runner:      processRunner{pythonPath: pythonPath, source: embeddedPythonRunner, logger: logger},
		newRunID:    newGenesisRunID,
		secrets:     inheritedEnvironment(),
		logger:      logger,
		syncToken:   syncToken,
		coordinator: coordinator,
	}
	if err := handler.loadInitialGeneration(); err != nil {
		fail(logger, "load agents and rules", err)
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("genesis listening",
		"address", listen,
		"agents", len(handler.generation.agents),
		"rules", len(handler.generation.rules),
		"digest", handler.generation.digest,
		"python", pythonPath,
		"privileged_ipc", coordinator != nil,
		"sync_configured", syncToken != "",
	)
	if err := server.ListenAndServe(); err != nil {
		fail(logger, "serve", err)
	}
}

func parseListenFlags(command string, args []string, logger *slog.Logger) (listen, agentsDir, rulesDir, syncToken string) {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	listenFlag := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	agentsFlag := flags.String("agents", "agents.d", "directory containing YAML agent definitions")
	rulesFlag := flags.String("rules", "rules.d", "directory containing YAML rules")
	tokenFlag := flags.String("sync-token", os.Getenv(syncTokenEnv), "bearer token required for POST /sync; empty disables /sync")
	parseFlagSet(flags, args, logger)
	return *listenFlag, *agentsFlag, *rulesFlag, *tokenFlag
}

func parseLaunchFlags(args []string, logger *slog.Logger) (listen, agentsDir, rulesDir, syncToken, listenerUser string) {
	flags := flag.NewFlagSet("launch", flag.ExitOnError)
	listenFlag := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	agentsFlag := flags.String("agents", "agents.d", "directory containing YAML agent definitions")
	rulesFlag := flags.String("rules", "rules.d", "directory containing YAML rules")
	tokenFlag := flags.String("sync-token", os.Getenv(syncTokenEnv), "bearer token required for POST /sync; empty disables /sync")
	listenerUserFlag := flags.String("listener-user", os.Getenv(listenerUserEnv), "OS user for the unprivileged listener; required when launch runs as root")
	parseFlagSet(flags, args, logger)
	return *listenFlag, *agentsFlag, *rulesFlag, *tokenFlag, *listenerUserFlag
}

func parseFlagSet(flags *flag.FlagSet, args []string, logger *slog.Logger) {
	if err := flags.Parse(args); err != nil {
		fail(logger, "parse flags", err)
	}
	if flags.NArg() != 0 {
		logger.Error("unexpected arguments", "arguments", flags.Args())
		os.Exit(2)
	}
}

func fail(logger *slog.Logger, message string, err error) {
	logger.Error(message, "error", err)
	os.Exit(1)
}
