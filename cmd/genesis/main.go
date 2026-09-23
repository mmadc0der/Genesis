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
	case "control":
		runControl(logger, args)
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
	listen, agentsDir, rulesDir, reposDir, providersDir, dataDir, credentialsDir, syncToken, listenerUser := parseLaunchFlags(args, logger)
	runLaunch(logger, listen, agentsDir, rulesDir, reposDir, providersDir, dataDir, credentialsDir, syncToken, listenerUser)
}

func runListen(logger *slog.Logger, args []string) {
	listen, agentsDir, rulesDir, reposDir, providersDir, dataDir, syncToken := parseListenFlags("listen", args, logger)

	absoluteAgentsDir, err := filepath.Abs(agentsDir)
	if err != nil {
		fail(logger, "resolve agents directory", err)
	}
	absoluteRulesDir, err := filepath.Abs(rulesDir)
	if err != nil {
		fail(logger, "resolve rules directory", err)
	}
	absoluteReposDir, err := resolveOptionalDir(reposDir)
	if err != nil {
		fail(logger, "resolve repos directory", err)
	}
	absoluteProvidersDir, err := resolveOptionalDir(providersDir)
	if err != nil {
		fail(logger, "resolve providers directory", err)
	}
	absoluteDataDir, err := prepareDataDir(dataDir)
	if err != nil {
		fail(logger, "prepare data directory", err)
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

	store := newRunStore(absoluteDataDir, newEventBus(), logger)
	if err := store.Recover(); err != nil {
		fail(logger, "recover run journals", err)
	}

	handler := &eventServer{
		agentsDir:    absoluteAgentsDir,
		rulesDir:     absoluteRulesDir,
		reposDir:     absoluteReposDir,
		providersDir: absoluteProvidersDir,
		runner: processRunner{
			pythonPath: pythonPath,
			source:     embeddedPythonRunner,
			logger:     logger,
			store:      store,
			spawner:    asSpawner(coordinator),
		},
		newRunID:    newGenesisRunID,
		secrets:     inheritedEnvironment(),
		logger:      logger,
		syncToken:   syncToken,
		coordinator: coordinator,
		store:       store,
	}
	if err := handler.loadInitialGeneration(); err != nil {
		fail(logger, "load agents, rules, and repositories", err)
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
		"repositories", len(handler.generation.repos),
		"repositories_active", handler.generation.reposActive,
		"providers", len(handler.generation.providers),
		"providers_active", handler.generation.providersActive,
		"digest", handler.generation.digest,
		"python", pythonPath,
		"data", absoluteDataDir,
		"privileged_ipc", coordinator != nil,
		"sync_configured", syncToken != "",
	)
	if err := server.ListenAndServe(); err != nil {
		fail(logger, "serve", err)
	}
}

func parseListenFlags(command string, args []string, logger *slog.Logger) (listen, agentsDir, rulesDir, reposDir, providersDir, dataDir, syncToken string) {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	listenFlag := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	agentsFlag := flags.String("agents", "agents.d", "directory containing YAML agent definitions")
	rulesFlag := flags.String("rules", "rules.d", "directory containing YAML rules")
	reposFlag := flags.String("repos", "repos.d", "directory containing YAML repository declarations; missing directory leaves the layer inactive")
	providersFlag := flags.String("providers", defaultProvidersDir, "root-owned directory of provider identity files; missing directory leaves the layer inactive")
	dataFlag := flags.String("data", "genesis-data", "directory for per-run journals, stderr, results, and retained DeepSeek homes")
	tokenFlag := flags.String("sync-token", os.Getenv(syncTokenEnv), "bearer token required for POST /sync; empty disables /sync")
	parseFlagSet(flags, args, logger)
	return *listenFlag, *agentsFlag, *rulesFlag, *reposFlag, *providersFlag, *dataFlag, *tokenFlag
}

func parseLaunchFlags(args []string, logger *slog.Logger) (listen, agentsDir, rulesDir, reposDir, providersDir, dataDir, credentialsDir, syncToken, listenerUser string) {
	flags := flag.NewFlagSet("launch", flag.ExitOnError)
	listenFlag := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	agentsFlag := flags.String("agents", "agents.d", "directory containing YAML agent definitions")
	rulesFlag := flags.String("rules", "rules.d", "directory containing YAML rules")
	reposFlag := flags.String("repos", "repos.d", "directory containing YAML repository declarations; missing directory leaves the layer inactive")
	providersFlag := flags.String("providers", defaultProvidersDir, "root-owned directory of provider identity files; missing directory leaves the layer inactive")
	dataFlag := flags.String("data", "genesis-data", "directory for per-run journals, stderr, results, and retained DeepSeek homes")
	credentialsFlag := flags.String("credentials", defaultCredentialsDir, "root-only directory for SSH grant material; used when launch runs as root")
	tokenFlag := flags.String("sync-token", os.Getenv(syncTokenEnv), "bearer token required for POST /sync; empty disables /sync")
	listenerUserFlag := flags.String("listener-user", os.Getenv(listenerUserEnv), "OS user for the unprivileged listener; required when launch runs as root")
	parseFlagSet(flags, args, logger)
	return *listenFlag, *agentsFlag, *rulesFlag, *reposFlag, *providersFlag, *dataFlag, *credentialsFlag, *tokenFlag, *listenerUserFlag
}

func resolveOptionalDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	return filepath.Abs(path)
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
