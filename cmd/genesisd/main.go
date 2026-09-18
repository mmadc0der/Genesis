package main

import (
	"flag"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8787", "HTTP listen address")
	rulesDir := flag.String("rules", "rules.d", "directory containing YAML rules")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}

	absoluteRulesDir, err := filepath.Abs(*rulesDir)
	if err != nil {
		log.Fatalf("resolve rules directory: %v", err)
	}
	rules, err := loadRules(absoluteRulesDir)
	if err != nil {
		log.Fatalf("load rules: %v", err)
	}

	dshPath, err := exec.LookPath("dsh")
	if err != nil {
		log.Fatalf("find dsh in PATH: %v", err)
	}
	dshPath, err = filepath.Abs(dshPath)
	if err != nil {
		log.Fatalf("resolve dsh path: %v", err)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           &eventServer{rulesDir: absoluteRulesDir, dshPath: dshPath},
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("genesisd listening on %s with %d rule(s); dsh=%s", *listen, len(rules), dshPath)
	log.Fatal(server.ListenAndServe())
}
