package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	cloudEventsJSON = "application/cloudevents+json"
	maxEventBytes   = 1 << 20
)

type cloudEvent map[string]json.RawMessage

type rule struct {
	Match map[string]string `yaml:"match"`
	Run   runSpec           `yaml:"run"`
	name  string
}

type runSpec struct {
	Cwd string            `yaml:"cwd"`
	Env map[string]string `yaml:"env"`
}

type invocation struct {
	Event cloudEvent        `json:"event"`
	Rule  string            `json:"rule"`
	RunID string            `json:"run_id"`
	Cwd   string            `json:"cwd"`
	Env   map[string]string `json:"env"`
}

type acceptedRun struct {
	Rule  string `json:"rule"`
	RunID string `json:"run_id"`
}

type invocationRunner interface {
	Run(invocation)
}

type eventServer struct {
	rulesDir string
	runner   invocationRunner
	newRunID func() (string, error)
	logger   *slog.Logger
}

func (s *eventServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/events" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != cloudEventsJSON {
		http.Error(w, "Content-Type must be application/cloudevents+json", http.StatusUnsupportedMediaType)
		return
	}

	event, err := decodeCloudEvent(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rules, err := loadRules(s.rulesDir)
	if err != nil {
		s.log().Error("load rules", "error", err)
		http.Error(w, "rules are invalid", http.StatusInternalServerError)
		return
	}

	matches := make([]rule, 0, len(rules))
	for _, candidate := range rules {
		if candidate.matches(event) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.runner == nil {
		s.log().Error("runner is not configured")
		http.Error(w, "runner is unavailable", http.StatusInternalServerError)
		return
	}

	newRunID := s.newRunID
	if newRunID == nil {
		newRunID = newGenesisRunID
	}
	invocations := make([]invocation, 0, len(matches))
	accepted := make([]acceptedRun, 0, len(matches))
	for _, matched := range matches {
		runID, err := newRunID()
		if err != nil {
			s.log().Error("create Genesis run ID", "rule", matched.name, "error", err)
			http.Error(w, "failed to create run ID", http.StatusInternalServerError)
			return
		}
		invocations = append(invocations, invocation{
			Event: event,
			Rule:  matched.name,
			RunID: runID,
			Cwd:   matched.Run.Cwd,
			Env:   matched.Run.Env,
		})
		accepted = append(accepted, acceptedRun{Rule: matched.name, RunID: runID})
	}

	for _, document := range invocations {
		go s.runner.Run(document)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Runs []acceptedRun `json:"runs"`
	}{Runs: accepted})
}

func (s *eventServer) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func newGenesisRunID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "gen_" + hex.EncodeToString(random[:]), nil
}

func decodeCloudEvent(w http.ResponseWriter, r *http.Request) (cloudEvent, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBytes))
	var event cloudEvent
	if err := decoder.Decode(&event); err != nil {
		return nil, fmt.Errorf("invalid CloudEvent JSON: %w", err)
	}
	if event == nil {
		return nil, errors.New("CloudEvent must be a JSON object")
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("CloudEvent body must contain one JSON object")
		}
		return nil, fmt.Errorf("invalid trailing JSON: %w", err)
	}

	specVersion, ok := event.stringAttribute("specversion")
	if !ok || specVersion != "1.0" {
		return nil, errors.New(`CloudEvent "specversion" must be "1.0"`)
	}
	for _, attribute := range []string{"id", "source", "type"} {
		value, ok := event.stringAttribute(attribute)
		if !ok || value == "" {
			return nil, fmt.Errorf("CloudEvent %q must be a non-empty string", attribute)
		}
	}
	return event, nil
}

func (e cloudEvent) stringAttribute(name string) (string, bool) {
	raw, ok := e[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func (r rule) matches(event cloudEvent) bool {
	for attribute, expected := range r.Match {
		actual, ok := event.stringAttribute(attribute)
		if !ok || actual != expected {
			return false
		}
	}
	return true
}

func loadRules(directory string) ([]rule, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}

	rules := make([]rule, 0, len(entries))
	for _, entry := range entries {
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || (extension != ".yaml" && extension != ".yml") {
			continue
		}

		path := filepath.Join(directory, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}

		decoder := yaml.NewDecoder(bytes.NewReader(contents))
		decoder.KnownFields(true)
		var loaded rule
		if err := decoder.Decode(&loaded); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("%s: rule file must contain one YAML document", entry.Name())
			}
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}

		loaded.name = entry.Name()
		if loaded.Run.Env == nil {
			loaded.Run.Env = map[string]string{}
		}
		if err := loaded.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		rules = append(rules, loaded)
	}
	return rules, nil
}

func (r rule) validate() error {
	if len(r.Match) == 0 {
		return errors.New("match must contain at least one attribute")
	}
	for attribute := range r.Match {
		if attribute == "" || strings.ContainsRune(attribute, '\x00') {
			return errors.New("match attribute names must be non-empty and contain no NUL")
		}
	}

	if r.Run.Cwd == "" {
		return errors.New("run.cwd is required")
	}
	if !filepath.IsAbs(r.Run.Cwd) {
		return errors.New("run.cwd must be an absolute path")
	}
	if strings.ContainsRune(r.Run.Cwd, '\x00') {
		return errors.New("run.cwd must contain no NUL")
	}
	for key, value := range r.Run.Env {
		if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key, '\x00') {
			return fmt.Errorf("invalid run.env key %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("run.env[%q] must contain no NUL", key)
		}
	}
	return nil
}
