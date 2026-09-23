package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Repository declarations are desired state only. Loading them never calls
// a provider, mints credentials, or mutates a remote.

const (
	repositoryProviderGitHub = "github"
	lifecycleRemoveRetain    = "retain"
	lifecycleExistingAdopt   = "adopt"
	lifecycleExistingRefuse  = "refuse"
	visibilityPublic         = "public"
	visibilityPrivate        = "private"
	actionsAllowedAll        = "all"
	actionsAllowedLocal      = "local_only"
	actionsAllowedSelected   = "selected"
	roleManager              = "manager"
	roleProgrammer           = "programmer"
	roleReviewer             = "reviewer"
	roleDevops               = "devops"
	maxDescriptionRunes      = 350
	maxReviewCount           = 6
)

var (
	githubOrgPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	gitBranchPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	closedNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	secretNamePattern   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	checkContextPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	actionPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:@[A-Za-z0-9_.*-]+)?$`)
	repositoryRoles     = map[string]struct{}{
		roleManager:    {},
		roleProgrammer: {},
		roleReviewer:   {},
		roleDevops:     {},
	}
	secretMaterialMarkers = []string{
		"-----begin",
		"ghp_",
		"github_pat_",
		"gho_",
		"ghu_",
		"ghs_",
		"ghr_",
		"x-access-token",
	}
)

type repositoryDefinition struct {
	ID         string                `json:"id" yaml:"-"`
	Provider   string                `json:"provider" yaml:"provider"`
	Org        string                `json:"org" yaml:"org"`
	Name       string                `json:"name" yaml:"name"`
	Lifecycle  repositoryLifecycle   `json:"lifecycle" yaml:"lifecycle"`
	Settings   repositorySettings    `json:"settings" yaml:"settings"`
	Bootstrap  *repositoryBootstrap  `json:"bootstrap,omitempty" yaml:"bootstrap,omitempty"`
	Actions    repositoryActions     `json:"actions" yaml:"actions"`
	Secrets    *repositorySecrets    `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Protection *repositoryProtection `json:"protection,omitempty" yaml:"protection,omitempty"`
	Identities []repositoryIdentity  `json:"identities,omitempty" yaml:"identities,omitempty"`
}

type repositoryLifecycle struct {
	Remove   string `json:"remove" yaml:"remove"`
	Existing string `json:"existing" yaml:"existing"`
}

type repositorySettings struct {
	Visibility    string             `json:"visibility" yaml:"visibility"`
	Description   string             `json:"description,omitempty" yaml:"description,omitempty"`
	DefaultBranch string             `json:"default_branch" yaml:"default_branch"`
	Features      repositoryFeatures `json:"features" yaml:"features"`
	Merge         repositoryMerge    `json:"merge" yaml:"merge"`
}

type repositoryFeatures struct {
	Issues   *bool `json:"issues" yaml:"issues"`
	Wiki     *bool `json:"wiki" yaml:"wiki"`
	Projects *bool `json:"projects" yaml:"projects"`
}

type repositoryMerge struct {
	AllowSquash         *bool `json:"allow_squash" yaml:"allow_squash"`
	AllowMergeCommit    *bool `json:"allow_merge_commit" yaml:"allow_merge_commit"`
	AllowRebase         *bool `json:"allow_rebase" yaml:"allow_rebase"`
	DeleteBranchOnMerge *bool `json:"delete_branch_on_merge" yaml:"delete_branch_on_merge"`
}

type repositoryBootstrap struct {
	Template string `json:"template" yaml:"template"`
}

type repositoryActions struct {
	Enabled  *bool    `json:"enabled" yaml:"enabled"`
	Allowed  string   `json:"allowed" yaml:"allowed"`
	Selected []string `json:"selected,omitempty" yaml:"selected,omitempty"`
}

type repositorySecrets struct {
	Repository   []string                `json:"repository,omitempty" yaml:"repository,omitempty"`
	Environments []repositoryEnvironment `json:"environments,omitempty" yaml:"environments,omitempty"`
}

type repositoryEnvironment struct {
	Name    string   `json:"name" yaml:"name"`
	Secrets []string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

type repositoryProtection struct {
	Ruleset repositoryRuleset `json:"ruleset" yaml:"ruleset"`
}

type repositoryRuleset struct {
	Name                     string   `json:"name" yaml:"name"`
	RequiredApprovingReviews *int     `json:"required_approving_reviews" yaml:"required_approving_reviews"`
	DismissStaleReviews      *bool    `json:"dismiss_stale_reviews" yaml:"dismiss_stale_reviews"`
	RequiredChecks           []string `json:"required_checks,omitempty" yaml:"required_checks,omitempty"`
	StrictChecks             *bool    `json:"strict_checks" yaml:"strict_checks"`
}

type repositoryIdentity struct {
	Name string `json:"name" yaml:"name"`
	Role string `json:"role" yaml:"role"`
}

func loadRepositories(directory string) (map[string]repositoryDefinition, bool, error) {
	if strings.TrimSpace(directory) == "" {
		return map[string]repositoryDefinition{}, false, nil
	}
	info, err := os.Lstat(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]repositoryDefinition{}, false, nil
		}
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("repos directory must not be a symlink")
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("repos path %q is not a directory", directory)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, false, err
	}
	repos := map[string]repositoryDefinition{}
	claimed := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		extension := strings.ToLower(filepath.Ext(name))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		path := filepath.Join(directory, name)
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return nil, false, err
		}
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%s: repository file must not be a symlink", name)
		}
		if !fileInfo.Mode().IsRegular() {
			return nil, false, fmt.Errorf("%s: repository file must be a regular file", name)
		}
		id, err := agentIDFromFilename(name)
		if err != nil {
			return nil, false, fmt.Errorf("%s: invalid repository id %q", name, strings.TrimSuffix(name, filepath.Ext(name)))
		}
		if _, exists := repos[id]; exists {
			return nil, false, fmt.Errorf("%s: duplicate repository id %q", name, id)
		}
		var loaded repositoryDefinition
		if err := loadYAMLDocument(path, &loaded); err != nil {
			return nil, false, err
		}
		loaded.ID = id
		if err := loaded.validate(); err != nil {
			return nil, false, fmt.Errorf("%s: %w", name, err)
		}
		remote := strings.ToLower(loaded.Org) + "/" + strings.ToLower(loaded.Name)
		if other, exists := claimed[remote]; exists {
			return nil, false, fmt.Errorf("%s: org/name %s/%s is already declared by %s", name, loaded.Org, loaded.Name, other)
		}
		claimed[remote] = id
		repos[id] = loaded
	}
	return repos, true, nil
}

func (r *repositoryDefinition) validate() error {
	if r.Provider != repositoryProviderGitHub {
		return fmt.Errorf("provider must be %q", repositoryProviderGitHub)
	}
	if err := validateGitHubOrg(r.Org); err != nil {
		return err
	}
	if err := validateGitHubRepoName(r.Name); err != nil {
		return err
	}
	if err := r.Lifecycle.validate(); err != nil {
		return err
	}
	if err := r.Settings.validate(); err != nil {
		return err
	}
	if r.Bootstrap != nil {
		if err := r.Bootstrap.validate(); err != nil {
			return err
		}
	}
	if err := r.Actions.validate(); err != nil {
		return err
	}
	if r.Secrets != nil {
		if err := r.Secrets.validate(); err != nil {
			return err
		}
	}
	if r.Protection != nil {
		if err := r.Protection.validate(); err != nil {
			return err
		}
	}
	if r.Identities != nil {
		if err := r.validateIdentities(); err != nil {
			return err
		}
	}
	r.normalize()
	return nil
}

func (r *repositoryDefinition) normalize() {
	if r.Actions.Allowed != actionsAllowedSelected {
		r.Actions.Selected = nil
	} else {
		slices.Sort(r.Actions.Selected)
	}
	if r.Secrets != nil {
		slices.Sort(r.Secrets.Repository)
		if len(r.Secrets.Repository) == 0 {
			r.Secrets.Repository = nil
		}
		slices.SortFunc(r.Secrets.Environments, func(left, right repositoryEnvironment) int {
			return strings.Compare(left.Name, right.Name)
		})
		for i := range r.Secrets.Environments {
			slices.Sort(r.Secrets.Environments[i].Secrets)
			if len(r.Secrets.Environments[i].Secrets) == 0 {
				r.Secrets.Environments[i].Secrets = nil
			}
		}
		if len(r.Secrets.Environments) == 0 {
			r.Secrets.Environments = nil
		}
	}
	if r.Protection != nil {
		slices.Sort(r.Protection.Ruleset.RequiredChecks)
		if len(r.Protection.Ruleset.RequiredChecks) == 0 {
			r.Protection.Ruleset.RequiredChecks = nil
		}
	}
	slices.SortFunc(r.Identities, func(left, right repositoryIdentity) int {
		return strings.Compare(left.Name, right.Name)
	})
	if len(r.Identities) == 0 {
		r.Identities = nil
	}
}

func (l repositoryLifecycle) validate() error {
	switch l.Remove {
	case lifecycleRemoveRetain:
	case "":
		return errors.New("lifecycle.remove is required")
	case "archive", "delete":
		return fmt.Errorf("lifecycle.remove %q is not allowed; this contract only retains the remote", l.Remove)
	default:
		return fmt.Errorf("lifecycle.remove must be %q", lifecycleRemoveRetain)
	}
	switch l.Existing {
	case lifecycleExistingAdopt, lifecycleExistingRefuse:
		return nil
	case "":
		return errors.New("lifecycle.existing is required")
	default:
		return fmt.Errorf("lifecycle.existing must be %q or %q", lifecycleExistingAdopt, lifecycleExistingRefuse)
	}
}

func (s repositorySettings) validate() error {
	switch s.Visibility {
	case visibilityPublic, visibilityPrivate:
	case "":
		return errors.New("settings.visibility is required")
	default:
		return fmt.Errorf("settings.visibility must be %q or %q", visibilityPrivate, visibilityPublic)
	}
	if s.Description != "" {
		if err := validateDescription(s.Description); err != nil {
			return err
		}
	}
	if err := validateBranch("settings.default_branch", s.DefaultBranch); err != nil {
		return err
	}
	if err := requireBool("settings.features.issues", s.Features.Issues); err != nil {
		return err
	}
	if err := requireBool("settings.features.wiki", s.Features.Wiki); err != nil {
		return err
	}
	if err := requireBool("settings.features.projects", s.Features.Projects); err != nil {
		return err
	}
	if err := requireBool("settings.merge.allow_squash", s.Merge.AllowSquash); err != nil {
		return err
	}
	if err := requireBool("settings.merge.allow_merge_commit", s.Merge.AllowMergeCommit); err != nil {
		return err
	}
	if err := requireBool("settings.merge.allow_rebase", s.Merge.AllowRebase); err != nil {
		return err
	}
	if err := requireBool("settings.merge.delete_branch_on_merge", s.Merge.DeleteBranchOnMerge); err != nil {
		return err
	}
	if !*s.Merge.AllowSquash && !*s.Merge.AllowMergeCommit && !*s.Merge.AllowRebase {
		return errors.New("settings.merge must allow at least one of squash, merge commit, or rebase")
	}
	return nil
}

func (b repositoryBootstrap) validate() error {
	if err := validateClosedName("bootstrap.template", b.Template); err != nil {
		return err
	}
	return rejectSecretMaterial("bootstrap.template", b.Template)
}

func (a repositoryActions) validate() error {
	if err := requireBool("actions.enabled", a.Enabled); err != nil {
		return err
	}
	switch a.Allowed {
	case actionsAllowedAll, actionsAllowedLocal, actionsAllowedSelected:
	case "":
		return errors.New("actions.allowed is required")
	default:
		return fmt.Errorf("actions.allowed must be %q, %q, or %q", actionsAllowedAll, actionsAllowedLocal, actionsAllowedSelected)
	}
	if a.Allowed != actionsAllowedSelected {
		if a.Selected != nil {
			return errors.New("actions.selected is only allowed when actions.allowed is \"selected\"")
		}
		return nil
	}
	if len(a.Selected) == 0 {
		return errors.New("actions.selected is required when actions.allowed is \"selected\"")
	}
	seen := map[string]struct{}{}
	for _, pattern := range a.Selected {
		if _, exists := seen[pattern]; exists {
			return fmt.Errorf("duplicate actions.selected %q", pattern)
		}
		seen[pattern] = struct{}{}
		if err := validateActionPattern(pattern); err != nil {
			return err
		}
	}
	return nil
}

func (s repositorySecrets) validate() error {
	if len(s.Repository) == 0 && len(s.Environments) == 0 {
		return errors.New("secrets must name a repository secret or an environment")
	}
	seenRepo := map[string]struct{}{}
	for _, name := range s.Repository {
		if err := validateSecretName("secrets.repository", name); err != nil {
			return err
		}
		if _, exists := seenRepo[name]; exists {
			return fmt.Errorf("duplicate repository secret %q", name)
		}
		seenRepo[name] = struct{}{}
	}
	seenEnv := map[string]struct{}{}
	for _, environment := range s.Environments {
		if err := validateClosedName("secrets.environments.name", environment.Name); err != nil {
			return err
		}
		if _, exists := seenEnv[environment.Name]; exists {
			return fmt.Errorf("duplicate environment %q", environment.Name)
		}
		seenEnv[environment.Name] = struct{}{}
		seenSecret := map[string]struct{}{}
		for _, name := range environment.Secrets {
			if err := validateSecretName("secrets.environments.secrets", name); err != nil {
				return err
			}
			if _, exists := seenSecret[name]; exists {
				return fmt.Errorf("duplicate environment secret %q", name)
			}
			seenSecret[name] = struct{}{}
		}
	}
	return nil
}

func (p repositoryProtection) validate() error {
	ruleset := p.Ruleset
	if err := validateClosedName("protection.ruleset.name", ruleset.Name); err != nil {
		return err
	}
	if ruleset.RequiredApprovingReviews == nil {
		return errors.New("protection.ruleset.required_approving_reviews is required")
	}
	reviews := *ruleset.RequiredApprovingReviews
	if reviews < 0 || reviews > maxReviewCount {
		return fmt.Errorf("protection.ruleset.required_approving_reviews must be from 0 to %d", maxReviewCount)
	}
	if err := requireBool("protection.ruleset.dismiss_stale_reviews", ruleset.DismissStaleReviews); err != nil {
		return err
	}
	if err := requireBool("protection.ruleset.strict_checks", ruleset.StrictChecks); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, check := range ruleset.RequiredChecks {
		if _, exists := seen[check]; exists {
			return fmt.Errorf("duplicate protection.ruleset.required_checks %q", check)
		}
		seen[check] = struct{}{}
		if !checkContextPattern.MatchString(check) || strings.Contains(check, "..") {
			return fmt.Errorf("invalid protection.ruleset.required_checks %q", check)
		}
		if err := rejectSecretMaterial("protection.ruleset.required_checks", check); err != nil {
			return err
		}
	}
	if *ruleset.StrictChecks && len(ruleset.RequiredChecks) == 0 {
		return errors.New("protection.ruleset.strict_checks requires required_checks")
	}
	return nil
}

func (r *repositoryDefinition) validateIdentities() error {
	if len(r.Identities) == 0 {
		return errors.New("identities must not be empty")
	}
	seenName := map[string]struct{}{}
	seenRole := map[string]struct{}{}
	for _, identity := range r.Identities {
		if err := validateClosedName("identities.name", identity.Name); err != nil {
			return err
		}
		if _, exists := seenName[identity.Name]; exists {
			return fmt.Errorf("duplicate identity name %q", identity.Name)
		}
		seenName[identity.Name] = struct{}{}
		if _, ok := repositoryRoles[identity.Role]; !ok {
			return fmt.Errorf("identities.role %q must be manager, programmer, reviewer, or devops", identity.Role)
		}
		if _, exists := seenRole[identity.Role]; exists {
			return fmt.Errorf("duplicate identity role %q", identity.Role)
		}
		seenRole[identity.Role] = struct{}{}
	}
	return nil
}

func validateGitHubOrg(value string) error {
	if !githubOrgPattern.MatchString(value) || strings.Contains(value, "..") {
		return fmt.Errorf("invalid org %q", value)
	}
	return rejectSecretMaterial("org", value)
}

func validateGitHubRepoName(value string) error {
	if value == "." || value == ".." || !githubRepoPattern.MatchString(value) || strings.Contains(value, "..") {
		return fmt.Errorf("invalid name %q", value)
	}
	return rejectSecretMaterial("name", value)
}

func validateBranch(field, value string) error {
	if !gitBranchPattern.MatchString(value) || strings.Contains(value, "..") {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return rejectSecretMaterial(field, value)
}

func validateClosedName(field, value string) error {
	if !closedNamePattern.MatchString(value) || strings.Contains(value, "..") || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return rejectSecretMaterial(field, value)
}

func validateSecretName(field, value string) error {
	if !secretNamePattern.MatchString(value) {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return rejectSecretMaterial(field, value)
}

func validateActionPattern(value string) error {
	if strings.Contains(value, "..") || strings.Contains(value, `\`) || strings.Contains(value, "://") || !actionPattern.MatchString(value) {
		return fmt.Errorf("invalid actions.selected %q", value)
	}
	return rejectSecretMaterial("actions.selected", value)
}

func validateDescription(value string) error {
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") || strings.ContainsRune(value, '\x00') {
		return errors.New("settings.description must be a single trimmed line")
	}
	if utf8.RuneCountInString(value) > maxDescriptionRunes {
		return fmt.Errorf("settings.description must be at most %d characters", maxDescriptionRunes)
	}
	return rejectSecretMaterial("settings.description", value)
}

func requireBool(field string, value *bool) error {
	if value == nil {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func rejectSecretMaterial(field, value string) error {
	lowered := strings.ToLower(value)
	for _, marker := range secretMaterialMarkers {
		if strings.Contains(lowered, marker) {
			return fmt.Errorf("%s must not contain secret material", field)
		}
	}
	return nil
}

func canonicalRepositories(repos map[string]repositoryDefinition) []repositoryDefinition {
	if len(repos) == 0 {
		return []repositoryDefinition{}
	}
	ids := make([]string, 0, len(repos))
	for id := range repos {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]repositoryDefinition, 0, len(ids))
	for _, id := range ids {
		out = append(out, repos[id].clone())
	}
	return out
}

func (r repositoryDefinition) clone() repositoryDefinition {
	cloned := r
	cloned.Settings = r.Settings.clone()
	cloned.Bootstrap = cloneBootstrap(r.Bootstrap)
	cloned.Actions = r.Actions.clone()
	cloned.Secrets = cloneSecrets(r.Secrets)
	cloned.Protection = cloneProtection(r.Protection)
	if r.Identities != nil {
		cloned.Identities = append([]repositoryIdentity(nil), r.Identities...)
	}
	return cloned
}

func (s repositorySettings) clone() repositorySettings {
	cloned := s
	cloned.Features.Issues = cloneBool(s.Features.Issues)
	cloned.Features.Wiki = cloneBool(s.Features.Wiki)
	cloned.Features.Projects = cloneBool(s.Features.Projects)
	cloned.Merge.AllowSquash = cloneBool(s.Merge.AllowSquash)
	cloned.Merge.AllowMergeCommit = cloneBool(s.Merge.AllowMergeCommit)
	cloned.Merge.AllowRebase = cloneBool(s.Merge.AllowRebase)
	cloned.Merge.DeleteBranchOnMerge = cloneBool(s.Merge.DeleteBranchOnMerge)
	return cloned
}

func (a repositoryActions) clone() repositoryActions {
	cloned := a
	cloned.Enabled = cloneBool(a.Enabled)
	if a.Selected != nil {
		cloned.Selected = append([]string(nil), a.Selected...)
	}
	return cloned
}

func cloneBootstrap(value *repositoryBootstrap) *repositoryBootstrap {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneSecrets(value *repositorySecrets) *repositorySecrets {
	if value == nil {
		return nil
	}
	cloned := repositorySecrets{}
	if value.Repository != nil {
		cloned.Repository = append([]string(nil), value.Repository...)
	}
	if value.Environments != nil {
		cloned.Environments = make([]repositoryEnvironment, len(value.Environments))
		for i, environment := range value.Environments {
			cloned.Environments[i] = environment
			if environment.Secrets != nil {
				cloned.Environments[i].Secrets = append([]string(nil), environment.Secrets...)
			}
		}
	}
	return &cloned
}

func cloneProtection(value *repositoryProtection) *repositoryProtection {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Ruleset.RequiredApprovingReviews = cloneInt(value.Ruleset.RequiredApprovingReviews)
	cloned.Ruleset.DismissStaleReviews = cloneBool(value.Ruleset.DismissStaleReviews)
	cloned.Ruleset.StrictChecks = cloneBool(value.Ruleset.StrictChecks)
	if value.Ruleset.RequiredChecks != nil {
		cloned.Ruleset.RequiredChecks = append([]string(nil), value.Ruleset.RequiredChecks...)
	}
	return &cloned
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func repositoryDirectoryUsable(directory string) (bool, error) {
	if strings.TrimSpace(directory) == "" {
		return false, nil
	}
	info, err := os.Lstat(directory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, nil
	}
	return true, nil
}
