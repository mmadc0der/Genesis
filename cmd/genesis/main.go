package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	listen := flag.String("listen", "127.0.0.1:8787", "HTTP listen address")
	rulesDir := flag.String("rules", "rules.d", "directory containing YAML rules")
	flag.Parse()
	if flag.NArg() != 0 {
		logger.Error("unexpected arguments", "arguments", flag.Args())
		os.Exit(2)
	}

	absoluteRulesDir, err := filepath.Abs(*rulesDir)
	if err != nil {
		fail(logger, "resolve rules directory", err)
	}
	rules, err := loadRules(absoluteRulesDir)
	if err != nil {
		fail(logger, "load rules", err)
	}

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		fail(logger, "find python3 in PATH", err)
	}
	pythonPath, err = filepath.Abs(pythonPath)
	if err != nil {
		fail(logger, "resolve python3 path", err)
	}

	runner := processRunner{pythonPath: pythonPath, source: embeddedPythonRunner, logger: logger}
	server := &http.Server{
		Addr: *listen,
		Handler: &eventServer{
			rulesDir: absoluteRulesDir,
			runner:   runner,
			newRunID: newGenesisRunID,
			logger:   logger,
		},
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("genesis listening",
		"address", *listen,
		"rules", len(rules),
		"python", pythonPath,
	)
	if err := server.ListenAndServe(); err != nil {
		fail(logger, "serve", err)
	}
}

func fail(logger *slog.Logger, message string, err error) {
	logger.Error(message, "error", err)
	os.Exit(1)
}
