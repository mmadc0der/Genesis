package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// repositorySchemaImagePath is the world-readable file the designer reads.
// The image installs schema/repository-declaration.txt there. Provider files,
// secret material, and root-only policy are not installed beside it.
const repositorySchemaImagePath = "/usr/share/genesis/schema/repository-declaration.txt"

const repositorySchemaExampleBegin = "BEGIN EXAMPLE\n"
const repositorySchemaExampleEnd = "END EXAMPLE\n"

// repositoryDeclarationSchema is the repos.d contract, rendered from the
// loader structs and the same constants validate() uses.
func repositoryDeclarationSchema() string {
	var b strings.Builder
	b.WriteString(repositorySchemaHeader())
	writeSchemaFields(&b, repositorySchemaTree(), repositoryFieldNotes(), 0)
	b.WriteString(repositorySchemaFooter())
	return b.String()
}

func repositorySchemaHeader() string {
	return fmt.Sprintf(`Repository declaration schema

This file is the contract for one repository declaration in repos.d.
It is generated from the loader structs. Read it when writing
repos.d/*.yaml or repos.d/*.yml. Do not inspect the genesis binary.
Do not run strings, grep, or readelf on the genesis binary.
Sample YAML and the lab-widget template are examples, not this contract.

Installed at %s.

File rules
- One repository per .yaml or .yml file. The filename stem is the id.
- The id matches %s and cannot contain "..".
- id is not a YAML field. A key named id is rejected.
- One YAML document. Unknown fields and duplicate keys are rejected.
- The repos directory and the file must not be symlinks. The file must be a regular file of at most %d bytes.
- Two files cannot declare the same org and name, ignoring case.
- Non-YAML names in the directory are ignored. A directory whose name ends in .yaml is rejected.
- Do not put secret values in any string. Rejected substrings, matched case-insensitively: %s.
- Writing this file records desired state only. It does not create an organization and it does not call a provider.

Fields
`, repositorySchemaImagePath, agentIDPattern.String(), maxRepositoryFileBytes, strings.Join(secretMaterialMarkers, ", "))
}

func repositorySchemaFooter() string {
	return `
Not part of this file
- There is no grants field. Agent github grants are not written here.
- Do not put provider identity files, secret values, or root-only policy in this file.

Example
` + repositorySchemaExampleBegin + repositorySchemaExample() + repositorySchemaExampleEnd
}

func repositorySchemaExample() string {
	return `provider: github
org: octo-org
name: lab-widget
lifecycle:
  remove: retain
  existing: adopt
settings:
  visibility: private
  description: Schema example widget. Desired state only.
  default_branch: main
  features:
    issues: true
    wiki: false
    projects: false
  merge:
    allow_squash: true
    allow_merge_commit: false
    allow_rebase: false
    delete_branch_on_merge: true
bootstrap:
  template: lab-widget
actions:
  enabled: true
  allowed: selected
  selected:
    - actions/checkout@v4
secrets:
  repository:
    - EXAMPLE_TOKEN
  environments:
    - name: ci
      secrets:
        - CI_BOT_TOKEN
protection:
  ruleset:
    name: protect-main
    required_approving_reviews: 1
    dismiss_stale_reviews: true
    required_checks:
      - ci
    strict_checks: true
identities:
  - name: programmer
    role: programmer
  - name: reviewer
    role: reviewer
`
}

type schemaField struct {
	Path     string
	TypeName string
	Presence string
	Children []schemaField
}

func repositorySchemaTree() []schemaField {
	return schemaFieldsOf(reflect.TypeOf(repositoryDefinition{}), "")
}

func schemaFieldsOf(t reflect.Type, prefix string) []schemaField {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []schemaField
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		presence := "required"
		if strings.Contains(opts, "omitempty") {
			presence = "optional"
		}
		node := schemaField{Path: path, TypeName: yamlKind(field.Type), Presence: presence}
		child := field.Type
		for child.Kind() == reflect.Pointer || child.Kind() == reflect.Slice {
			child = child.Elem()
		}
		if child.Kind() == reflect.Struct {
			node.Children = schemaFieldsOf(child, path)
		}
		out = append(out, node)
	}
	return out
}

func flattenSchemaFields(fields []schemaField) []schemaField {
	var out []schemaField
	for _, field := range fields {
		out = append(out, field)
		out = append(out, flattenSchemaFields(field.Children)...)
	}
	return out
}

func yamlKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		elem := t.Elem()
		switch elem.Kind() {
		case reflect.Bool:
			return "boolean"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return "integer"
		case reflect.Struct:
			return "object"
		default:
			return yamlKind(elem)
		}
	case reflect.Slice:
		elem := t.Elem()
		if elem.Kind() == reflect.String {
			return "list of strings"
		}
		if elem.Kind() == reflect.Struct || (elem.Kind() == reflect.Pointer && elem.Elem().Kind() == reflect.Struct) {
			return "list of objects"
		}
		return "list"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Struct:
		return "object"
	default:
		return t.Kind().String()
	}
}

func writeSchemaFields(b *strings.Builder, fields []schemaField, notes map[string]string, depth int) {
	indent := strings.Repeat("  ", depth)
	for i, field := range fields {
		if depth == 0 && i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "%s- %s: %s, %s\n", indent, field.Path, field.TypeName, field.Presence)
		if note := notes[field.Path]; note != "" {
			fmt.Fprintf(b, "%s  %s\n", indent, note)
		}
		if len(field.Children) > 0 {
			writeSchemaFields(b, field.Children, notes, depth+1)
		}
	}
}

func repositoryFieldNotes() map[string]string {
	explicitBool := "Explicit true or false. Omitting the key is rejected."
	closedName := fmt.Sprintf("Lowercase kebab name. Must match %s. No \"/\", \"\\\", or \"..\".", closedNamePattern.String())
	roles := sortedRoleNames()
	return map[string]string{
		"provider":                              fmt.Sprintf("Allowed value: %s. Not a URL or a token.", repositoryProviderGitHub),
		"org":                                   fmt.Sprintf("GitHub organization login. Must match %s and must not contain \"..\" or \"--\".", githubOrgPattern.String()),
		"name":                                  fmt.Sprintf("GitHub repository name. Must match %s. It cannot be \".\" or \"..\", cannot contain \"..\", and cannot end with \".git\" in any case.", githubRepoPattern.String()),
		"lifecycle":                             "How Genesis classifies the remote. Both keys are required. This file cannot archive or delete the repository.",
		"lifecycle.remove":                      fmt.Sprintf("Allowed value: %s. archive and delete are rejected.", lifecycleRemoveRetain),
		"lifecycle.existing":                    fmt.Sprintf("Allowed values: %s or %s.", lifecycleExistingAdopt, lifecycleExistingRefuse),
		"settings":                              "Visibility, description, default branch, features, and merge settings.",
		"settings.visibility":                   fmt.Sprintf("Allowed values: %s or %s.", visibilityPublic, visibilityPrivate),
		"settings.description":                  fmt.Sprintf("Optional. One trimmed UTF-8 line of at most %d characters. No carriage return, line feed, or NUL.", maxDescriptionRunes),
		"settings.default_branch":               fmt.Sprintf("Required branch name. Must match %s. No \"..\", no trailing \".\", not HEAD in any case, and not a name ending in \".lock\".", gitBranchPattern.String()),
		"settings.features":                     "issues, wiki, and projects are all required booleans.",
		"settings.features.issues":              explicitBool,
		"settings.features.wiki":                explicitBool,
		"settings.features.projects":            explicitBool,
		"settings.merge":                        "All four booleans are required. At least one of allow_squash, allow_merge_commit, and allow_rebase must be true.",
		"settings.merge.allow_squash":           explicitBool,
		"settings.merge.allow_merge_commit":     explicitBool,
		"settings.merge.allow_rebase":           explicitBool,
		"settings.merge.delete_branch_on_merge": explicitBool,
		"bootstrap":                             "Optional. When present, template is required. The value is a template id, not a path, URL, or file body.",
		"bootstrap.template":                    closedName,
		"actions":                               "Required. enabled and allowed are always required.",
		"actions.enabled":                       explicitBool,
		"actions.allowed":                       fmt.Sprintf("Allowed values: %s, %s, or %s.", actionsAllowedAll, actionsAllowedLocal, actionsAllowedSelected),
		"actions.selected":                      fmt.Sprintf("Required only when actions.allowed is %s, and then the list must be non-empty. Omit the key for %s and %s; a present empty list is rejected. Each pattern is owner/name, 2 to %d slash-separated segments, an optional @ref, and * wildcards. A pattern is at most %d bytes. Segments match %s. Refs match %s. No \"..\", backslash, space, NUL, or \"://\". Duplicates are rejected.", actionsAllowedSelected, actionsAllowedAll, actionsAllowedLocal, maxActionSegments, maxActionPatternBytes, actionSegmentPattern.String(), actionRefPattern.String()),
		"secrets":                               "Optional. When present, include at least one repository secret name or one environment. Names only. There is no value field.",
		"secrets.repository":                    fmt.Sprintf("Optional list of secret names. Each name matches %s. Duplicates are rejected.", secretNamePattern.String()),
		"secrets.environments":                  "Optional list. Each environment name is unique.",
		"secrets.environments.name":             closedName,
		"secrets.environments.secrets":          fmt.Sprintf("Optional secret names inside this environment. Each name matches %s. Duplicates inside one environment are rejected.", secretNamePattern.String()),
		"protection":                            "Optional. When present, ruleset is required.",
		"protection.ruleset":                    "The one ruleset this file declares. name, required_approving_reviews, dismiss_stale_reviews, and strict_checks are required when protection is present.",
		"protection.ruleset.name":               closedName,
		"protection.ruleset.required_approving_reviews": fmt.Sprintf("Required integer from 0 through %d.", maxReviewCount),
		"protection.ruleset.dismiss_stale_reviews":      explicitBool,
		"protection.ruleset.required_checks":            fmt.Sprintf("Optional list of check contexts. Each context is one trimmed line of at most %d characters. Allowed characters are letters, digits, space, and _./:()-,+. No \"..\", leading \"/\", backslash, or \"://\". Duplicates are rejected.", maxCheckContextRunes),
		"protection.ruleset.strict_checks":              "Explicit true or false. true requires at least one required_checks entry.",
		"identities":                                    "Optional. When present, the list must not be empty. Names are unique and roles are unique. This is not an agent github grant, and there is no grants key.",
		"identities.name":                               closedName,
		"identities.role":                               fmt.Sprintf("Allowed values: %s. Any other role is rejected.", strings.Join(roles, ", ")),
	}
}

func sortedRoleNames() []string {
	roles := make([]string, 0, len(repositoryRoles))
	for role := range repositoryRoles {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles
}
