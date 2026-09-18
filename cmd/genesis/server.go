package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	Cwd  string            `yaml:"cwd"`
	Args []string          `yaml:"args"`
	Env  map[string]string `yaml:"env"`
}

type eventServer struct {
	rulesDir string
	dshPath  string
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
		log.Printf("load rules: %v", err)
		http.Error(w, "rules are invalid", http.StatusInternalServerError)
		return
	}

	for _, candidate := range rules {
		if !candidate.matches(event) {
			continue
		}
		if err := s.launch(candidate); err != nil {
			log.Printf("launch rule %s: %v", candidate.name, err)
			http.Error(w, "failed to start dsh", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(struct {
			Rule string `json:"rule"`
		}{Rule: candidate.name})
		return
	}

	w.WriteHeader(http.StatusNoContent)
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

func (s *eventServer) launch(r rule) error {
	command := exec.Command(s.dshPath, r.Run.Args...)
	command.Dir = r.Run.Cwd
	command.Env = r.Run.environment()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}

	go func() {
		if err := command.Wait(); err != nil {
			log.Printf("rule %s: dsh exited: %v", r.name, err)
		}
	}()
	return nil
}

func (r runSpec) environment() []string {
	keys := make([]string, 0, len(r.Env))
	for key := range r.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+r.Env[key])
	}
	return environment
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
	for index, argument := range r.Run.Args {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("run.args[%d] must contain no NUL", index)
		}
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
