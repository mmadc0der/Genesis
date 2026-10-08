package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// formField is one editable agent parameter. The list is reflected from
// agentDefinition so a new YAML field shows up without a UI change.
type formField struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required,omitempty"`
	Enum        []string    `json:"enum,omitempty"`
	Format      string      `json:"format,omitempty"`
	Description string      `json:"description,omitempty"`
	Items       string      `json:"items,omitempty"`
	Fields      []formField `json:"fields,omitempty"`
}

type formHint struct {
	required    bool
	format      string
	description string
	enum        []string
}

func agentFormSchema() []formField {
	return reflectForm(reflect.TypeOf(agentDefinition{}), "")
}

func reflectForm(t reflect.Type, prefix string) []formField {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var fields []formField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		tag := sf.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fields = append(fields, formFieldFor(path, name, sf.Type, agentFormHints[path]))
	}
	return fields
}

func formFieldFor(path, name string, t reflect.Type, hint formHint) formField {
	field := formField{
		Name:        name,
		Required:    hint.required,
		Enum:        hint.enum,
		Format:      hint.format,
		Description: hint.description,
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		field.Type = "string"
	case reflect.Int, reflect.Int32, reflect.Int64:
		field.Type = "integer"
	case reflect.Bool:
		field.Type = "boolean"
	case reflect.Slice:
		field.Type = "array"
		field.Items = scalarKind(t.Elem())
	case reflect.Map:
		field.Type = "map"
		field.Items = scalarKind(t.Elem())
	case reflect.Struct:
		field.Type = "object"
		field.Fields = reflectForm(t, path)
	default:
		field.Type = "string"
	}
	return field
}

func scalarKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Bool:
		return "boolean"
	default:
		return "string"
	}
}

// agentFormHints adds constraints the struct tags cannot express. Keys are
// YAML paths. A field with no hint still appears from reflection.
var agentFormHints = map[string]formHint{
	"instructions": {
		required:    true,
		format:      "markdown",
		description: "System prompt. Required.",
	},
	"cwd":  {required: true, format: "path", description: "Absolute working directory, distinct from home."},
	"home": {required: true, format: "path", description: "Absolute home. A dedicated user must use /home/<user>."},
	"user": {description: "Dedicated OS user. Empty runs as the listener account."},
	"setup": {
		description: "Host contract. Requires user.",
	},
	"setup.groups": {description: "Extra groups. Repeated names are rejected."},
	"setup.workspace": {
		description: "Workspace mode. Empty means private.",
		enum:        []string{"", workspacePrivate, workspaceSharedRead, workspaceSharedWrite},
	},
	"env":     {description: "Extra environment. Reserved and secret names are rejected."},
	"secrets": {description: "Secret names copied from the Genesis process. Values are not stored here."},
	"github":  {description: "Optional repository grant. No secret values."},
	"github.repository":  {description: "repos.d id."},
	"github.identity":    {description: "Provider identity name."},
	"github.git":         {description: "Git access mode."},
	"github.permissions": {description: "Permission map."},
	"max_parallel": {
		description: "Most runs of this agent at once. Empty means 1. Minimum 1.",
	},
	"reasoning_effort": {
		description: "Harness thinking level. Empty keeps the default, high.",
		enum:        []string{"", "off", "low", "high", "max"},
	},
}

func readAgentDocument(dir, id string) (map[string]any, error) {
	contents, err := os.ReadFile(agentPath(dir, id))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := decodeYAMLBytes(id+".yaml", contents, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func saveAgentDocument(dir, id string, doc map[string]any) error {
	if doc == nil {
		return fmt.Errorf("document is required")
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	var loaded agentDefinition
	if err := decodeYAMLBytes(id+".yaml", raw, &loaded); err != nil {
		return err
	}
	loaded.id = id
	if loaded.Env == nil {
		loaded.Env = map[string]string{}
	}
	if _, present := doc["secrets"]; !present || loaded.Secrets == nil {
		loaded.Secrets = []string{deepSeekAPIKey}
	}
	if err := loaded.validate(); err != nil {
		return err
	}
	others, err := loadAgents(dir)
	if err != nil && !os.IsNotExist(err) {
		// A sibling file can be invalid. Still enforce this file, and unique
		// users when the rest of the directory loads.
		if !strings.Contains(err.Error(), id+".yaml") {
			return err
		}
	}
	if err == nil {
		others[id] = loaded
		if err := validateUniqueAgentUsers(others); err != nil {
			return err
		}
	}
	encoded, err := yaml.Marshal(loaded)
	if err != nil {
		return err
	}
	return os.WriteFile(agentPath(dir, id), encoded, 0o644)
}

func agentPath(dir, id string) string {
	return filepath.Join(dir, id+".yaml")
}

func agentDocumentFromJSON(body []byte) (map[string]any, error) {
	var wrapper struct {
		Document map[string]any `json:"document"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wrapper); err != nil {
		return nil, err
	}
	if wrapper.Document == nil {
		return nil, fmt.Errorf("document is required")
	}
	return wrapper.Document, nil
}
