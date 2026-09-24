package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	observationNone        = "none"
	observationUnavailable = "unavailable"
	observationObserved    = "observed"

	driftDrift        = "drift"
	driftUnobservable = "unobservable"
	driftUnsupported  = "unsupported"

	observationVersion  = 1
	observationDirName  = "observations"
	observationFileName = "repositories.json"
	observationDirMode  = 0o700
	observationFileMode = 0o600
	maxObservationPages = 10

	reasonSecretsUnobservable   = "repository and environment secrets are not observable with administration and metadata read; Genesis did not call the secrets or environments APIs"
	reasonBootstrapUnobservable = "bootstrap template is not a readable repository field; Genesis did not apply it"
	reasonIdentitiesUnsupported = "runtime identities are not repository settings; Genesis did not change collaborators or mint an agent token"
	reasonRetainUnsupported     = "remove retain is not a remote change; Genesis did not archive or delete the repository"
	reasonRefuseUnsupported     = "existing refuse is not observed or claimed; Genesis did not call GitHub"
	reasonActionsUnobservable   = "actions permissions are not available for this repository"
	reasonRulesetsUnobservable  = "repository rulesets are not available for this repository"
	reasonBranchUnobservable    = "branch rules are not available for this repository"
	reasonNoDefaultBranch       = "the repository has no default branch, so branch rules were not evaluated"
	reasonExtraRulesets         = "additional repository rulesets are observed and were not changed"
	reasonSHAPinning            = "actions sha pinning is enabled and this declaration cannot express it"
	reasonSHAPinningUnknown     = "actions sha pinning was not reported; Genesis did not assume it is off"
	reasonExtraActions          = "actions policy allows categories this declaration cannot express"
	reasonExtraReview           = "the ruleset has review requirements this declaration cannot express"
	reasonUnmodeledRules        = "rules this declaration cannot express were observed and were not changed"
	reasonDismissUnknown        = "dismiss stale reviews was not reported"
	reasonBranchName            = "the default branch name is not a single segment; Genesis did not request branch rules"
)

var (
	errRepositoryNotFound    = errors.New("github repository was not found")
	errRepositoryIdentity    = errors.New("github repository owner, name, or id does not match")
	errRepositoryPersisted   = errors.New("github repository id does not match the persisted repository")
	errRepositoryRetarget    = errors.New("repository declaration no longer matches the persisted repository identity")
	errRepositoryTruncated   = errors.New("github repository observation was truncated")
	errRepositoryAuthScope   = errors.New("github auth scope does not match")
	errRepositoryRate        = errors.New("github rate limit")
	errRepositoryMalformed   = errors.New("github repository response was malformed")
	errRepositoryUnavailable = errors.New("github repository observation is unavailable")
	errDesiredChanged        = errors.New("repository desired state changed during observation")
	errFeatureUnavailable    = errors.New("github feature is unavailable")
)

type repositoryDrift struct {
	ID       string `json:"id"`
	Field    string `json:"field"`
	Desired  string `json:"desired,omitempty"`
	Observed string `json:"observed,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

type repositoryBinding struct {
	ID           string `json:"id"`
	Org          string `json:"org"`
	Name         string `json:"name"`
	RepositoryID int64  `json:"repository_id"`
	NodeID       string `json:"node_id"`
}

type observedRuleset struct {
	ID                       int64    `json:"id"`
	Name                     string   `json:"name"`
	Target                   string   `json:"target,omitempty"`
	SourceType               string   `json:"source_type"`
	Enforcement              string   `json:"enforcement"`
	RequiredApprovingReviews *int     `json:"required_approving_reviews,omitempty"`
	DismissStaleReviews      *bool    `json:"dismiss_stale_reviews,omitempty"`
	RequiredChecks           []string `json:"required_checks,omitempty"`
	StrictChecks             *bool    `json:"strict_checks,omitempty"`
	CoversDefaultBranch      bool     `json:"covers_default_branch"`
	Include                  []string `json:"include,omitempty"`
	AdditionalReview         bool     `json:"additional_review,omitempty"`
	HasPullRequest           bool     `json:"has_pull_request"`
	HasStatusChecks          bool     `json:"has_status_checks"`
	Unmodeled                bool     `json:"unmodeled,omitempty"`
	BypassActors             bool     `json:"bypass_actors,omitempty"`
}

type repositoryObserved struct {
	ID              string             `json:"id"`
	Org             string             `json:"org"`
	Name            string             `json:"name"`
	RepositoryID    int64              `json:"repository_id"`
	NodeID          string             `json:"node_id"`
	ObservedAt      string             `json:"observed_at"`
	Visibility      string             `json:"visibility"`
	Description     string             `json:"description"`
	DefaultBranch   string             `json:"default_branch"`
	Archived        bool               `json:"archived"`
	Features        repositoryFeatures `json:"features"`
	Merge           repositoryMerge    `json:"merge"`
	ActionsStatus   string             `json:"actions_status"`
	ActionsReason   string             `json:"actions_reason,omitempty"`
	ActionsEnabled  *bool              `json:"actions_enabled,omitempty"`
	ActionsAllowed  string             `json:"actions_allowed,omitempty"`
	ActionsSelected []string           `json:"actions_selected,omitempty"`
	SelectedKnown   bool               `json:"selected_known,omitempty"`
	SHAPinning      bool               `json:"sha_pinning,omitempty"`
	SHAPinningKnown bool               `json:"sha_pinning_known,omitempty"`
	ActionsExtra    bool               `json:"actions_extra,omitempty"`
	RulesetStatus   string             `json:"ruleset_status"`
	RulesetReason   string             `json:"ruleset_reason,omitempty"`
	Rulesets        []observedRuleset  `json:"rulesets,omitempty"`
	BranchStatus    string             `json:"branch_status"`
	BranchReason    string             `json:"branch_reason,omitempty"`
	BranchRules     []observedRuleset  `json:"branch_rules,omitempty"`
}

type repositoryJournal struct {
	Version  int                  `json:"version"`
	Digest   string               `json:"digest,omitempty"`
	Bindings []repositoryBinding  `json:"bindings"`
	Observed []repositoryObserved `json:"observed"`
}

type repositoryObservationResult struct {
	Status         string                  `json:"status"`
	Digest         string                  `json:"digest,omitempty"`
	RemoteMutation string                  `json:"remote_mutation,omitempty"`
	Applied        []string                `json:"applied,omitempty"`
	Observed       []repositoryObserved    `json:"observed,omitempty"`
	Drift          []repositoryDrift       `json:"drift,omitempty"`
	Unsupported    []repositoryUnsupported `json:"unsupported,omitempty"`
	Journal        *repositoryJournal      `json:"journal,omitempty"`
}

func repositoryDigest(repos map[string]repositoryDefinition) (string, error) {
	return repositoryListDigest(canonicalRepositories(repos))
}

func repositoryListDigest(repos []repositoryDefinition) (string, error) {
	payload, err := json.Marshal(repos)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (s *privilegedState) observeDesiredRepositories(ctx context.Context, wantDigest string) (repositoryObservationResult, error) {
	if err := ctx.Err(); err != nil {
		return repositoryObservationResult{}, err
	}
	if strings.TrimSpace(wantDigest) == "" {
		return repositoryObservationResult{}, errors.New("repository observation digest is missing")
	}
	repos, active, err := loadRepositories(s.reposDir)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if !active {
		return repositoryObservationResult{}, errDesiredChanged
	}
	got, err := repositoryDigest(repos)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if got != wantDigest {
		return repositoryObservationResult{}, errDesiredChanged
	}
	prior, err := readRepositoryJournal(s.dataDir)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	ordered := canonicalRepositories(repos)
	if err := bindingsStillMatch(prior.Bindings, ordered); err != nil {
		return repositoryObservationResult{}, err
	}
	ready, err := s.observationReady(ctx, ordered)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if !ready {
		return repositoryObservationResult{Status: observationUnavailable, Digest: wantDigest}, nil
	}
	registrar := s.repoObserver()
	observed := make([]repositoryObserved, 0)
	bindings := append([]repositoryBinding(nil), prior.Bindings...)
	now := time.Now
	if registrar.now != nil {
		now = registrar.now
	}
	observedAt := now().UTC().Format(time.RFC3339)
	for _, definition := range ordered {
		if definition.Lifecycle.Existing != lifecycleExistingAdopt {
			continue
		}
		one, err := registrar.observeRepository(ctx, definition, bindingFor(bindings, definition.ID))
		if errors.Is(err, errReconcilerUnavailable) {
			return repositoryObservationResult{Status: observationUnavailable, Digest: wantDigest}, nil
		}
		if err != nil {
			return repositoryObservationResult{}, observationError(err)
		}
		one.ObservedAt = observedAt
		if err := rejectObservedSecrets(one); err != nil {
			return repositoryObservationResult{}, err
		}
		bindings, err = mergeRepositoryBinding(bindings, definition, one)
		if err != nil {
			return repositoryObservationResult{}, err
		}
		observed = append(observed, one)
	}
	again, active, err := loadRepositories(s.reposDir)
	if err != nil || !active {
		return repositoryObservationResult{}, errDesiredChanged
	}
	againDigest, err := repositoryDigest(again)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if againDigest != wantDigest {
		return repositoryObservationResult{}, errDesiredChanged
	}
	drift, unsupported := classifyRepositories(ordered, observedByID(observed))
	journal := &repositoryJournal{
		Version:  observationVersion,
		Digest:   wantDigest,
		Bindings: bindings,
		Observed: observed,
	}
	if err := journal.normalize(); err != nil {
		return repositoryObservationResult{}, err
	}
	return repositoryObservationResult{
		Status:      observationObserved,
		Digest:      wantDigest,
		Observed:    observed,
		Drift:       drift,
		Unsupported: unsupported,
		Journal:     journal,
	}, nil
}

func (s *privilegedState) observationReady(ctx context.Context, repos []repositoryDefinition) (bool, error) {
	if s == nil || s.secrets == nil || !s.canMutate() {
		return false, nil
	}
	sawAdopt := false
	for _, definition := range repos {
		if definition.Lifecycle.Existing != lifecycleExistingAdopt {
			continue
		}
		sawAdopt = true
		if _, err := s.resolveReconcilerRepository(ctx, definition); err != nil {
			if errors.Is(err, errReconcilerUnavailable) {
				return false, nil
			}
			return false, err
		}
	}
	if !sawAdopt {
		return true, nil
	}
	return true, nil
}

func (s *privilegedState) repoObserver() githubRegistrar {
	if s != nil && s.registrar != nil {
		if concrete, ok := s.registrar.(githubRegistrar); ok {
			if concrete.resolveRepo == nil {
				concrete.resolveRepo = s.resolveReconcilerRepository
			}
			return concrete
		}
		return githubRegistrar{resolveRepo: s.resolveReconcilerRepository}
	}
	return newGitHubRegistrar(s)
}

func (s *privilegedState) resolveReconcilerRepository(ctx context.Context, definition repositoryDefinition) (githubAppBinding, error) {
	if err := ctx.Err(); err != nil {
		return githubAppBinding{}, err
	}
	if s == nil || s.secrets == nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	if definition.Provider != repositoryProviderGitHub || definition.Lifecycle.Existing != lifecycleExistingAdopt {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	if err := validateGitHubOrg(definition.Org); err != nil || validateGitHubRepoName(definition.Name) != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	providers, providersActive, err := loadProviders(s.providersDir, s.agentsDir, s.rulesDir, s.reposDir)
	if err != nil || !providersActive {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	chosen, err := selectOrgReconciler(definition.Org, providers)
	if err != nil {
		return githubAppBinding{}, err
	}
	payload, err := s.secrets.read(chosen.Secret)
	if err != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	defer clear(payload)
	key, err := parseAppPrivateKey(payload)
	if err != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	return githubAppBinding{
		Org:            definition.Org,
		Repo:           definition.Name,
		AppID:          chosen.AppID,
		InstallationID: chosen.InstallationID,
		Key:            key,
	}, nil
}

func selectOrgReconciler(org string, providers map[string]providerDefinition) (providerIdentity, error) {
	var found providerIdentity
	count := 0
	for _, provider := range providers {
		if provider.Provider != repositoryProviderGitHub || !strings.EqualFold(provider.Org, org) {
			continue
		}
		for _, identity := range provider.Identities {
			if identity.Role != roleReconciler {
				continue
			}
			if identity.Credential != credentialApp || identity.Secret == "" || identity.AppID == "" || identity.InstallationID == "" {
				return providerIdentity{}, errReconcilerUnavailable
			}
			found = identity
			count++
		}
	}
	if count != 1 {
		return providerIdentity{}, errReconcilerUnavailable
	}
	return found, nil
}

func (g githubRegistrar) observeRepository(ctx context.Context, definition repositoryDefinition, prior *repositoryBinding) (repositoryObserved, error) {
	if g.resolveRepo == nil || g.transport == nil {
		return repositoryObserved{}, errReconcilerUnavailable
	}
	app, err := g.resolveRepo(ctx, definition)
	if err != nil {
		return repositoryObserved{}, err
	}
	if app.Key == nil || app.Org != definition.Org || app.Repo != definition.Name {
		return repositoryObserved{}, errReconcilerUnavailable
	}
	client, err := g.client(app)
	if err != nil {
		return repositoryObserved{}, err
	}
	readPerms := gitHubPermissions{Administration: "read", Metadata: "read"}
	discovery, err := client.mint(ctx, []string{definition.Name}, nil, readPerms)
	if err != nil {
		return repositoryObserved{}, err
	}
	document, err := client.getObservedRepo(ctx, &discovery)
	if err != nil {
		return repositoryObserved{}, err
	}
	if err := document.matchesDeclaration(definition); err != nil {
		return repositoryObserved{}, err
	}
	if prior != nil && (document.ID != prior.RepositoryID || document.NodeID != prior.NodeID) {
		return repositoryObserved{}, errRepositoryPersisted
	}
	pinned, err := client.mint(ctx, nil, []int64{document.ID}, readPerms)
	if err != nil {
		return repositoryObserved{}, err
	}
	confirmed, err := client.getObservedRepo(ctx, &pinned)
	if err != nil {
		return repositoryObserved{}, err
	}
	if err := confirmed.matchesDeclaration(definition); err != nil {
		return repositoryObserved{}, err
	}
	if confirmed.ID != document.ID || confirmed.NodeID != document.NodeID || confirmed.FullName != document.FullName || confirmed.Owner != document.Owner || confirmed.Name != document.Name {
		return repositoryObserved{}, errRepositoryIdentity
	}
	document = confirmed
	observed := document.view(definition.ID)
	actions, actionsErr := client.getActionsPolicy(ctx, &pinned)
	switch {
	case actionsErr == nil:
		observed.ActionsStatus = observationObserved
		enabled := actions.Enabled
		observed.ActionsEnabled = &enabled
		observed.ActionsAllowed = actions.Allowed
		observed.ActionsSelected = actions.Selected
		observed.SelectedKnown = actions.SelectedKnown
		observed.SHAPinning = actions.Pinning
		observed.SHAPinningKnown = actions.PinningKnown
		observed.ActionsExtra = actions.Extra
	case errors.Is(actionsErr, errFeatureUnavailable):
		observed.ActionsStatus = driftUnobservable
		observed.ActionsReason = reasonActionsUnobservable
	default:
		return repositoryObserved{}, actionsErr
	}
	rulesets, rulesErr := client.getRulesets(ctx, &pinned, observed.DefaultBranch)
	switch {
	case rulesErr == nil:
		observed.RulesetStatus = observationObserved
		observed.Rulesets = rulesets
	case errors.Is(rulesErr, errFeatureUnavailable):
		observed.RulesetStatus = driftUnobservable
		observed.RulesetReason = reasonRulesetsUnobservable
	default:
		return repositoryObserved{}, rulesErr
	}
	if observed.DefaultBranch == "" {
		observed.BranchStatus = driftUnobservable
		observed.BranchReason = reasonNoDefaultBranch
	} else if !branchNameObservable(observed.DefaultBranch) {
		observed.BranchStatus = driftUnobservable
		observed.BranchReason = reasonBranchName
	} else {
		branch, branchErr := client.getBranchRules(ctx, &pinned, observed.DefaultBranch)
		switch {
		case branchErr == nil:
			observed.BranchStatus = observationObserved
			observed.BranchRules = branch
		case errors.Is(branchErr, errFeatureUnavailable):
			observed.BranchStatus = driftUnobservable
			observed.BranchReason = reasonBranchUnobservable
		default:
			return repositoryObserved{}, branchErr
		}
	}
	return observed, nil
}

type observedRepoDocument struct {
	ID            int64
	NodeID        string
	Name          string
	FullName      string
	Owner         string
	Visibility    string
	Description   string
	DefaultBranch string
	Archived      bool
	Issues        bool
	Wiki          bool
	Projects      bool
	AllowSquash   bool
	AllowMerge    bool
	AllowRebase   bool
	DeleteBranch  bool
}

func (d observedRepoDocument) matchesDeclaration(definition repositoryDefinition) error {
	if d.Owner != definition.Org || d.Name != definition.Name || d.FullName != definition.Org+"/"+definition.Name {
		return errRepositoryIdentity
	}
	if d.ID <= 0 || d.NodeID == "" {
		return errRepositoryMalformed
	}
	return nil
}

func (d observedRepoDocument) view(id string) repositoryObserved {
	return repositoryObserved{
		ID:            id,
		Org:           d.Owner,
		Name:          d.Name,
		RepositoryID:  d.ID,
		NodeID:        d.NodeID,
		Visibility:    d.Visibility,
		Description:   d.Description,
		DefaultBranch: d.DefaultBranch,
		Archived:      d.Archived,
		Features: repositoryFeatures{
			Issues:   boolPtr(d.Issues),
			Wiki:     boolPtr(d.Wiki),
			Projects: boolPtr(d.Projects),
		},
		Merge: repositoryMerge{
			AllowSquash:         boolPtr(d.AllowSquash),
			AllowMergeCommit:    boolPtr(d.AllowMerge),
			AllowRebase:         boolPtr(d.AllowRebase),
			DeleteBranchOnMerge: boolPtr(d.DeleteBranch),
		},
	}
}

func (c *githubClient) getOnly(ctx context.Context, path string, tok *tokenResult) ([]byte, http.Header, error) {
	return c.call(ctx, http.MethodGet, path, tok, nil)
}

func (c *githubClient) getObservedRepo(ctx context.Context, tok *tokenResult) (observedRepoDocument, error) {
	body, _, err := c.getOnly(ctx, "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo), tok)
	if err != nil {
		return observedRepoDocument{}, err
	}
	document, err := parseObservedRepo(body)
	if err != nil {
		return observedRepoDocument{}, err
	}
	if err := c.requireScoped(*tok, tok.perms, document.ID); err != nil {
		return observedRepoDocument{}, err
	}
	if tok.perms.Administration != "read" || tok.perms.Metadata != "read" {
		return observedRepoDocument{}, githubErr("auth")
	}
	return document, nil
}

func parseObservedRepo(body []byte) (observedRepoDocument, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	required := []string{
		"id", "node_id", "name", "full_name", "owner", "visibility", "private",
		"description", "default_branch", "has_issues", "has_wiki", "has_projects",
		"allow_squash_merge", "allow_merge_commit", "allow_rebase_merge",
		"delete_branch_on_merge", "archived",
	}
	for _, key := range required {
		if _, ok := raw[key]; !ok {
			return observedRepoDocument{}, githubErr("partial")
		}
	}
	var owner struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw["owner"], &owner); err != nil || owner.Login == "" {
		return observedRepoDocument{}, githubErr("partial")
	}
	id, err := jsonInt(raw["id"])
	if err != nil || id <= 0 {
		return observedRepoDocument{}, githubErr("partial")
	}
	nodeID, err := jsonString(raw["node_id"], false)
	if err != nil || !validNodeID(nodeID) {
		return observedRepoDocument{}, githubErr("partial")
	}
	name, err := jsonString(raw["name"], false)
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	fullName, err := jsonString(raw["full_name"], false)
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	visibility, err := jsonString(raw["visibility"], false)
	if err != nil || (visibility != visibilityPublic && visibility != visibilityPrivate && visibility != "internal") {
		return observedRepoDocument{}, githubErr("partial")
	}
	private, err := jsonBool(raw["private"])
	if err != nil || private != (visibility != visibilityPublic) {
		return observedRepoDocument{}, githubErr("partial")
	}
	description, err := jsonString(raw["description"], true)
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	branch, err := jsonString(raw["default_branch"], true)
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	issues, err := jsonBool(raw["has_issues"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	wiki, err := jsonBool(raw["has_wiki"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	projects, err := jsonBool(raw["has_projects"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	squash, err := jsonBool(raw["allow_squash_merge"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	merge, err := jsonBool(raw["allow_merge_commit"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	rebase, err := jsonBool(raw["allow_rebase_merge"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	deleteBranch, err := jsonBool(raw["delete_branch_on_merge"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	archived, err := jsonBool(raw["archived"])
	if err != nil {
		return observedRepoDocument{}, githubErr("partial")
	}
	if fullName != owner.Login+"/"+name {
		return observedRepoDocument{}, githubErr("partial")
	}
	return observedRepoDocument{
		ID: id, NodeID: nodeID, Name: name, FullName: fullName, Owner: owner.Login,
		Visibility: visibility, Description: description, DefaultBranch: branch, Archived: archived,
		Issues: issues, Wiki: wiki, Projects: projects,
		AllowSquash: squash, AllowMerge: merge, AllowRebase: rebase, DeleteBranch: deleteBranch,
	}, nil
}

type actionsPolicy struct {
	Enabled       bool
	Allowed       string
	Selected      []string
	SelectedKnown bool
	Pinning       bool
	PinningKnown  bool
	Extra         bool
}

func (c *githubClient) getActionsPolicy(ctx context.Context, tok *tokenResult) (actionsPolicy, error) {
	body, _, err := c.getOptional(ctx, "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo)+"/actions/permissions", tok)
	if err != nil {
		return actionsPolicy{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return actionsPolicy{}, githubErr("partial")
	}
	if _, ok := raw["enabled"]; !ok {
		return actionsPolicy{}, githubErr("partial")
	}
	if _, ok := raw["allowed_actions"]; !ok {
		return actionsPolicy{}, githubErr("partial")
	}
	enabled, err := jsonBool(raw["enabled"])
	if err != nil {
		return actionsPolicy{}, githubErr("partial")
	}
	allowed, err := jsonString(raw["allowed_actions"], false)
	if err != nil || (allowed != actionsAllowedAll && allowed != actionsAllowedLocal && allowed != actionsAllowedSelected) {
		return actionsPolicy{}, githubErr("partial")
	}
	pinning := false
	pinningKnown := false
	if value, ok := raw["sha_pinning_required"]; ok && string(value) != "null" {
		pinning, err = jsonBool(value)
		if err != nil {
			return actionsPolicy{}, githubErr("partial")
		}
		pinningKnown = true
	}
	policy := actionsPolicy{Enabled: enabled, Allowed: allowed, Pinning: pinning, PinningKnown: pinningKnown, SelectedKnown: true}
	if allowed != actionsAllowedSelected {
		return policy, nil
	}
	selectedBody, _, err := c.getOptional(ctx, "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo)+"/actions/permissions/selected-actions", tok)
	if err != nil {
		if errors.Is(err, errFeatureUnavailable) {
			policy.SelectedKnown = false
			return policy, nil
		}
		return actionsPolicy{}, err
	}
	patterns, extra, err := parseSelectedActions(selectedBody)
	if err != nil {
		return actionsPolicy{}, err
	}
	policy.Selected = patterns
	policy.Extra = extra
	return policy, nil
}

func parseSelectedActions(body []byte) ([]string, bool, error) {
	var parsed struct {
		GitHubOwned     bool     `json:"github_owned_allowed"`
		Verified        bool     `json:"verified_allowed"`
		PatternsAllowed []string `json:"patterns_allowed"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, githubErr("partial")
	}
	if parsed.PatternsAllowed == nil {
		parsed.PatternsAllowed = []string{}
	}
	seen := map[string]struct{}{}
	for _, pattern := range parsed.PatternsAllowed {
		if pattern == "" {
			return nil, false, githubErr("partial")
		}
		if _, dup := seen[pattern]; dup {
			return nil, false, githubErr("partial")
		}
		seen[pattern] = struct{}{}
	}
	patterns := append([]string(nil), parsed.PatternsAllowed...)
	slices.Sort(patterns)
	return patterns, parsed.GitHubOwned || parsed.Verified, nil
}

func (c *githubClient) getRulesets(ctx context.Context, tok *tokenResult, defaultBranch string) ([]observedRuleset, error) {
	path := "/repos/" + url.PathEscape(c.app.Org) + "/" + url.PathEscape(c.app.Repo) + "/rulesets?per_page=100"
	expected := "/repos/" + url.PathEscape(c.app.Org) + "/" + url.PathEscape(c.app.Repo) + "/rulesets"
	pages, err := c.getJSONPages(ctx, tok, path, expected)
	if err != nil {
		return nil, err
	}
	summaries := make([]gitHubRulesetSummary, 0)
	seen := map[int64]struct{}{}
	for _, page := range pages {
		var batch []gitHubRulesetSummary
		if err := json.Unmarshal(page, &batch); err != nil {
			return nil, githubErr("partial")
		}
		for _, item := range batch {
			if item.ID <= 0 || item.Name == "" || item.SourceType == "" || item.Enforcement == "" {
				return nil, githubErr("partial")
			}
			if item.SourceType != "Repository" && item.SourceType != "Organization" {
				return nil, githubErr("partial")
			}
			if _, dup := seen[item.ID]; dup {
				return nil, githubErr("partial")
			}
			seen[item.ID] = struct{}{}
			summaries = append(summaries, item)
		}
	}
	slices.SortFunc(summaries, func(left, right gitHubRulesetSummary) int {
		if left.Name != right.Name {
			return strings.Compare(left.Name, right.Name)
		}
		return int(left.ID - right.ID)
	})
	names := map[string]struct{}{}
	for _, summary := range summaries {
		if summary.SourceType != "Repository" {
			continue
		}
		if _, dup := names[summary.Name]; dup {
			return nil, githubErr("partial")
		}
		names[summary.Name] = struct{}{}
	}
	out := make([]observedRuleset, 0, len(summaries))
	for _, summary := range summaries {
		detail, _, err := c.getOnly(ctx, expected+"/"+strconv.FormatInt(summary.ID, 10), tok)
		if githubKind(err, "missing") {
			return nil, githubErr("partial")
		}
		if err != nil {
			return nil, err
		}
		parsed, err := parseRulesetDetail(detail, defaultBranch)
		if err != nil {
			return nil, err
		}
		if parsed.ID != summary.ID || parsed.Name != summary.Name {
			return nil, githubErr("partial")
		}
		out = append(out, parsed)
	}
	return out, nil
}

type gitHubRulesetSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	SourceType  string `json:"source_type"`
	Enforcement string `json:"enforcement"`
}

func parseRulesetDetail(body []byte, defaultBranch string) (observedRuleset, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return observedRuleset{}, githubErr("partial")
	}
	for _, key := range []string{"id", "name", "source_type", "enforcement", "rules"} {
		if _, ok := raw[key]; !ok {
			return observedRuleset{}, githubErr("partial")
		}
	}
	id, err := jsonInt(raw["id"])
	if err != nil || id <= 0 {
		return observedRuleset{}, githubErr("partial")
	}
	name, err := jsonString(raw["name"], false)
	if err != nil {
		return observedRuleset{}, githubErr("partial")
	}
	sourceType, err := jsonString(raw["source_type"], false)
	if err != nil || (sourceType != "Repository" && sourceType != "Organization") {
		return observedRuleset{}, githubErr("partial")
	}
	enforcement, err := jsonString(raw["enforcement"], false)
	if err != nil {
		return observedRuleset{}, githubErr("partial")
	}
	target := ""
	if value, ok := raw["target"]; ok && string(value) != "null" {
		target, err = jsonString(value, false)
		if err != nil {
			return observedRuleset{}, githubErr("partial")
		}
	}
	var rules []struct {
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw["rules"], &rules); err != nil {
		return observedRuleset{}, githubErr("partial")
	}
	observed := observedRuleset{ID: id, Name: name, Target: target, SourceType: sourceType, Enforcement: enforcement}
	ruleTypes := map[string]struct{}{}
	for _, rule := range rules {
		if rule.Type == "" {
			return observedRuleset{}, githubErr("partial")
		}
		if _, dup := ruleTypes[rule.Type]; dup {
			return observedRuleset{}, githubErr("partial")
		}
		ruleTypes[rule.Type] = struct{}{}
		switch rule.Type {
		case "pull_request":
			var params struct {
				Count            *int  `json:"required_approving_review_count"`
				Dismiss          *bool `json:"dismiss_stale_reviews_on_push"`
				CodeOwners       *bool `json:"require_code_owner_review"`
				LastPush         *bool `json:"require_last_push_approval"`
				ThreadResolution *bool `json:"required_review_thread_resolution"`
			}
			if len(rule.Parameters) > 0 && string(rule.Parameters) != "null" {
				if err := json.Unmarshal(rule.Parameters, &params); err != nil || params.Count == nil {
					return observedRuleset{}, githubErr("partial")
				}
			}
			if params.Count == nil {
				return observedRuleset{}, githubErr("partial")
			}
			observed.HasPullRequest = true
			observed.RequiredApprovingReviews = params.Count
			observed.DismissStaleReviews = params.Dismiss
			if boolValue(params.CodeOwners) || boolValue(params.LastPush) || boolValue(params.ThreadResolution) {
				observed.AdditionalReview = true
			}
		case "required_status_checks":
			var params struct {
				Strict *bool `json:"strict_required_status_checks_policy"`
				Checks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			}
			if err := json.Unmarshal(rule.Parameters, &params); err != nil || params.Strict == nil {
				return observedRuleset{}, githubErr("partial")
			}
			checks := make([]string, 0, len(params.Checks))
			seen := map[string]struct{}{}
			for _, check := range params.Checks {
				if strings.TrimSpace(check.Context) == "" {
					return observedRuleset{}, githubErr("partial")
				}
				if _, dup := seen[check.Context]; dup {
					return observedRuleset{}, githubErr("partial")
				}
				seen[check.Context] = struct{}{}
				checks = append(checks, check.Context)
			}
			slices.Sort(checks)
			observed.HasStatusChecks = true
			observed.RequiredChecks = checks
			observed.StrictChecks = params.Strict
		default:
			observed.Unmodeled = true
		}
	}
	if value, ok := raw["bypass_actors"]; ok && string(value) != "null" && string(value) != "[]" {
		var actors []json.RawMessage
		if err := json.Unmarshal(value, &actors); err != nil {
			return observedRuleset{}, githubErr("partial")
		}
		if len(actors) > 0 {
			observed.BypassActors = true
		}
	}
	if value, ok := raw["conditions"]; ok && string(value) != "null" {
		var conditions struct {
			RefName *struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		}
		if err := json.Unmarshal(value, &conditions); err != nil {
			return observedRuleset{}, githubErr("partial")
		}
		if conditions.RefName != nil {
			observed.Include = append([]string(nil), conditions.RefName.Include...)
			slices.Sort(observed.Include)
			observed.CoversDefaultBranch = coversDefaultBranch(observed.Include, conditions.RefName.Exclude, defaultBranch)
		}
	}
	return observed, nil
}

func coversDefaultBranch(include, exclude []string, branch string) bool {
	if branch == "" {
		return false
	}
	ref := "refs/heads/" + branch
	excluded := false
	for _, item := range exclude {
		if item == ref || item == "~DEFAULT_BRANCH" || item == "~ALL" {
			excluded = true
		}
	}
	if excluded {
		return false
	}
	for _, item := range include {
		if item == ref || item == "~DEFAULT_BRANCH" || item == "~ALL" {
			return true
		}
	}
	return false
}

func (c *githubClient) getBranchRules(ctx context.Context, tok *tokenResult, branch string) ([]observedRuleset, error) {
	if branch == "" || strings.Contains(branch, "/") || strings.Contains(branch, "..") {
		return nil, githubErr("partial")
	}
	body, _, err := c.getOptional(ctx, "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo)+"/rules/branches/"+url.PathEscape(branch), tok)
	if err != nil {
		return nil, err
	}
	return parseBranchRules(body)
}

func parseBranchRules(body []byte) ([]observedRuleset, error) {
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '[' {
		return nil, githubErr("partial")
	}
	wrapped := append([]byte(`{"id":1,"name":"branch","source_type":"Repository","enforcement":"active","rules":`), append(bytes.TrimSpace(body), '}')...)
	parsed, err := parseRulesetDetail(wrapped, "")
	if err != nil {
		return nil, err
	}
	if !parsed.HasPullRequest && !parsed.HasStatusChecks {
		return []observedRuleset{}, nil
	}
	parsed.ID = 0
	parsed.Name = ""
	parsed.SourceType = ""
	parsed.Enforcement = ""
	return []observedRuleset{parsed}, nil
}

func (c *githubClient) getOptional(ctx context.Context, path string, tok *tokenResult) ([]byte, http.Header, error) {
	body, header, err := c.getOnly(ctx, path, tok)
	if err == nil {
		return body, header, nil
	}
	if githubKind(err, "missing") || githubKind(err, "auth") {
		return nil, nil, errFeatureUnavailable
	}
	return nil, nil, err
}

func (c *githubClient) getJSONPages(ctx context.Context, tok *tokenResult, path, expectedPath string) ([][]byte, error) {
	pages := make([][]byte, 0, 1)
	for page := 0; page < maxObservationPages; page++ {
		var body []byte
		var header http.Header
		var err error
		if page == 0 {
			body, header, err = c.getOptional(ctx, path, tok)
		} else {
			body, header, err = c.getOnly(ctx, path, tok)
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '[' {
			return nil, githubErr("partial")
		}
		pages = append(pages, body)
		next, err := nextRelativePage(c.origin, header, expectedPath)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return pages, nil
		}
		path = next
	}
	return nil, errRepositoryTruncated
}

func nextRelativePage(origin *url.URL, header http.Header, expectedPath string) (string, error) {
	if header == nil {
		return "", nil
	}
	raw := header.Get("Link")
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	next := ""
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.IndexByte(part, '<')
		end := strings.IndexByte(part, '>')
		if start < 0 || end <= start {
			return "", githubErr("partial")
		}
		next = part[start+1 : end]
	}
	if next == "" {
		return "", nil
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", githubErr("partial")
	}
	if parsed.IsAbs() {
		if origin == nil || parsed.Scheme != origin.Scheme || !strings.EqualFold(parsed.Host, origin.Host) {
			return "", githubErr("partial")
		}
	} else if parsed.Host != "" {
		return "", githubErr("partial")
	}
	if parsed.EscapedPath() != expectedPath {
		return "", githubErr("partial")
	}
	query := parsed.Query()
	for key := range query {
		switch key {
		case "page", "per_page":
		default:
			return "", githubErr("partial")
		}
	}
	path := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return path, nil
}

func classifyRepositories(repos []repositoryDefinition, observed map[string]repositoryObserved) ([]repositoryDrift, []repositoryUnsupported) {
	drift := make([]repositoryDrift, 0)
	unsupported := make([]repositoryUnsupported, 0)
	for _, definition := range repos {
		if definition.Lifecycle.Existing != lifecycleExistingAdopt {
			for _, intent := range repositoryIntents(definition) {
				unsupported = append(unsupported, repositoryUnsupported{Kind: intent.Kind, ID: definition.ID, Reason: reasonRefuseUnsupported})
			}
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "lifecycle.existing", Desired: definition.Lifecycle.Existing, Status: driftUnsupported, Reason: reasonRefuseUnsupported})
			continue
		}
		one, ok := observed[definition.ID]
		if !ok {
			continue
		}
		drift = append(drift, compareAdopted(definition, one)...)
		unsupported = append(unsupported, unsupportedAdopted(definition, one)...)
	}
	if drift == nil {
		drift = []repositoryDrift{}
	}
	return drift, unsupported
}

func compareAdopted(definition repositoryDefinition, observed repositoryObserved) []repositoryDrift {
	drift := make([]repositoryDrift, 0)
	addString(&drift, definition.ID, "settings.visibility", definition.Settings.Visibility, observed.Visibility)
	addString(&drift, definition.ID, "settings.description", definition.Settings.Description, observed.Description)
	addString(&drift, definition.ID, "settings.default_branch", definition.Settings.DefaultBranch, observed.DefaultBranch)
	addBool(&drift, definition.ID, "settings.features.issues", definition.Settings.Features.Issues, observed.Features.Issues)
	addBool(&drift, definition.ID, "settings.features.wiki", definition.Settings.Features.Wiki, observed.Features.Wiki)
	addBool(&drift, definition.ID, "settings.features.projects", definition.Settings.Features.Projects, observed.Features.Projects)
	addBool(&drift, definition.ID, "settings.merge.allow_squash", definition.Settings.Merge.AllowSquash, observed.Merge.AllowSquash)
	addBool(&drift, definition.ID, "settings.merge.allow_merge_commit", definition.Settings.Merge.AllowMergeCommit, observed.Merge.AllowMergeCommit)
	addBool(&drift, definition.ID, "settings.merge.allow_rebase", definition.Settings.Merge.AllowRebase, observed.Merge.AllowRebase)
	addBool(&drift, definition.ID, "settings.merge.delete_branch_on_merge", definition.Settings.Merge.DeleteBranchOnMerge, observed.Merge.DeleteBranchOnMerge)
	if observed.Archived {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "settings.archived", Desired: "false", Observed: "true", Status: driftDrift})
	}
	if observed.ActionsStatus != observationObserved {
		for _, field := range []string{"actions.enabled", "actions.allowed", "actions.selected", "actions.sha_pinning"} {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: field, Status: driftUnobservable, Reason: observed.ActionsReason})
		}
	} else {
		addString(&drift, definition.ID, "actions.enabled", formatBoolPtr(definition.Actions.Enabled), formatBoolPtr(observed.ActionsEnabled))
		addString(&drift, definition.ID, "actions.allowed", definition.Actions.Allowed, observed.ActionsAllowed)
		if !observed.SelectedKnown && (definition.Actions.Allowed == actionsAllowedSelected || observed.ActionsAllowed == actionsAllowedSelected) {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "actions.selected", Status: driftUnobservable, Reason: reasonActionsUnobservable})
		} else if definition.Actions.Allowed == actionsAllowedSelected || observed.ActionsAllowed == actionsAllowedSelected {
			desiredSelected := append([]string(nil), definition.Actions.Selected...)
			slices.Sort(desiredSelected)
			observedSelected := append([]string(nil), observed.ActionsSelected...)
			slices.Sort(observedSelected)
			addString(&drift, definition.ID, "actions.selected", strings.Join(desiredSelected, ","), strings.Join(observedSelected, ","))
		}
		if !observed.SHAPinningKnown {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "actions.sha_pinning", Status: driftUnobservable, Reason: reasonSHAPinningUnknown})
		} else if observed.SHAPinning {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "actions.sha_pinning", Desired: "false", Observed: "true", Status: driftUnobservable, Reason: reasonSHAPinning})
		}
		if observed.ActionsExtra {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "actions.additional_allowance", Status: driftUnobservable, Reason: reasonExtraActions})
		}
	}
	drift = append(drift, compareProtection(definition, observed)...)
	if definition.Bootstrap != nil {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "bootstrap.template", Desired: definition.Bootstrap.Template, Status: driftUnobservable, Reason: reasonBootstrapUnobservable})
	}
	if definition.Secrets != nil {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "secrets", Status: driftUnobservable, Reason: reasonSecretsUnobservable})
		if len(definition.Secrets.Environments) > 0 {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "secrets.environments", Status: driftUnobservable, Reason: reasonSecretsUnobservable})
		}
	}
	if len(definition.Identities) > 0 {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "identities", Status: driftUnsupported, Reason: reasonIdentitiesUnsupported})
	}
	drift = append(drift, repositoryDrift{ID: definition.ID, Field: "lifecycle.remove", Desired: definition.Lifecycle.Remove, Status: driftUnsupported, Reason: reasonRetainUnsupported})
	return drift
}

func compareProtection(definition repositoryDefinition, observed repositoryObserved) []repositoryDrift {
	drift := make([]repositoryDrift, 0)
	desiredRuleset := ""
	if definition.Protection != nil {
		desiredRuleset = definition.Protection.Ruleset.Name
	}
	if observed.RulesetStatus != observationObserved {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset", Desired: desiredRuleset, Status: driftUnobservable, Reason: observed.RulesetReason})
	} else if definition.Protection != nil {
		wanted := definition.Protection.Ruleset
		var match *observedRuleset
		extras := make([]string, 0)
		for i := range observed.Rulesets {
			item := &observed.Rulesets[i]
			if item.SourceType == "Repository" && item.Name == wanted.Name {
				if match == nil {
					match = item
					continue
				}
			}
			if item.SourceType == "Repository" && item.Name != wanted.Name {
				extras = append(extras, item.Name)
			}
		}
		if match == nil {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset", Desired: wanted.Name, Observed: "absent", Status: driftDrift})
		} else {
			if match.Enforcement != "active" {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.enforcement", Desired: "active", Observed: match.Enforcement, Status: driftDrift})
			}
			if observed.DefaultBranch != "" && !match.CoversDefaultBranch {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.target", Desired: "refs/heads/" + observed.DefaultBranch, Observed: strings.Join(match.Include, ","), Status: driftDrift})
			}
			if !match.HasPullRequest {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.required_approving_reviews", Desired: strconv.Itoa(intValue(wanted.RequiredApprovingReviews)), Observed: "absent", Status: driftDrift})
			} else if intValue(wanted.RequiredApprovingReviews) != intValue(match.RequiredApprovingReviews) {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.required_approving_reviews", Desired: strconv.Itoa(intValue(wanted.RequiredApprovingReviews)), Observed: strconv.Itoa(intValue(match.RequiredApprovingReviews)), Status: driftDrift})
			}
			if match.HasPullRequest && match.DismissStaleReviews == nil {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.dismiss_stale_reviews", Desired: formatBoolPtr(wanted.DismissStaleReviews), Status: driftUnobservable, Reason: reasonDismissUnknown})
			} else if match.HasPullRequest && boolValue(wanted.DismissStaleReviews) != boolValue(match.DismissStaleReviews) {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.dismiss_stale_reviews", Desired: formatBoolPtr(wanted.DismissStaleReviews), Observed: formatBoolPtr(match.DismissStaleReviews), Status: driftDrift})
			}
			if match.Unmodeled {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.additional_rules", Status: driftUnobservable, Reason: reasonUnmodeledRules})
			}
			if match.AdditionalReview {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.additional_review", Status: driftUnobservable, Reason: reasonExtraReview})
			}
			desiredChecks := append([]string(nil), wanted.RequiredChecks...)
			slices.Sort(desiredChecks)
			if !match.HasStatusChecks {
				if len(desiredChecks) > 0 || boolValue(wanted.StrictChecks) {
					drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.required_checks", Desired: strings.Join(desiredChecks, ","), Observed: "absent", Status: driftDrift})
				}
			} else if strings.Join(desiredChecks, ",") != strings.Join(match.RequiredChecks, ",") {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.required_checks", Desired: strings.Join(desiredChecks, ","), Observed: strings.Join(match.RequiredChecks, ","), Status: driftDrift})
			} else if boolValue(wanted.StrictChecks) != boolValue(match.StrictChecks) {
				drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.ruleset.strict_checks", Desired: formatBoolPtr(wanted.StrictChecks), Observed: formatBoolPtr(match.StrictChecks), Status: driftDrift})
			}
		}
		if len(extras) > 0 {
			slices.Sort(extras)
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.extra_rulesets", Observed: strings.Join(extras, ","), Status: driftUnsupported, Reason: reasonExtraRulesets})
		}
	} else {
		extras := make([]string, 0)
		for _, item := range observed.Rulesets {
			if item.SourceType == "Repository" {
				extras = append(extras, item.Name)
			}
		}
		if len(extras) > 0 {
			slices.Sort(extras)
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.extra_rulesets", Observed: strings.Join(extras, ","), Status: driftUnsupported, Reason: reasonExtraRulesets})
		}
	}
	if observed.BranchStatus != observationObserved {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch", Status: driftUnobservable, Reason: observed.BranchReason})
		return drift
	}
	for _, branch := range observed.BranchRules {
		if branch.Unmodeled {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.additional_rules", Status: driftUnobservable, Reason: reasonUnmodeledRules})
			break
		}
	}
	if definition.Protection == nil {
		if len(observed.BranchRules) > 0 {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch", Status: driftUnsupported, Reason: "branch rules are observed and were not changed"})
		}
		return drift
	}
	if len(observed.BranchRules) == 0 || !observed.BranchRules[0].HasPullRequest {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch", Desired: definition.Protection.Ruleset.Name, Observed: "not applied", Status: driftDrift})
		return drift
	}
	branch := observed.BranchRules[0]
	wanted := definition.Protection.Ruleset
	if intValue(wanted.RequiredApprovingReviews) != intValue(branch.RequiredApprovingReviews) {
		drift = append(drift, repositoryDrift{
			ID: definition.ID, Field: "protection.branch.required_approving_reviews",
			Desired:  strconv.Itoa(intValue(wanted.RequiredApprovingReviews)),
			Observed: strconv.Itoa(intValue(branch.RequiredApprovingReviews)),
			Status:   driftDrift,
		})
	}
	if branch.DismissStaleReviews == nil {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.dismiss_stale_reviews", Desired: formatBoolPtr(wanted.DismissStaleReviews), Status: driftUnobservable, Reason: reasonDismissUnknown})
	} else if boolValue(wanted.DismissStaleReviews) != boolValue(branch.DismissStaleReviews) {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.dismiss_stale_reviews", Desired: formatBoolPtr(wanted.DismissStaleReviews), Observed: formatBoolPtr(branch.DismissStaleReviews), Status: driftDrift})
	}
	desiredChecks := append([]string(nil), wanted.RequiredChecks...)
	slices.Sort(desiredChecks)
	if !branch.HasStatusChecks {
		if len(desiredChecks) > 0 || boolValue(wanted.StrictChecks) {
			drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.required_checks", Desired: strings.Join(desiredChecks, ","), Observed: "absent", Status: driftDrift})
		}
	} else if strings.Join(desiredChecks, ",") != strings.Join(branch.RequiredChecks, ",") {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.required_checks", Desired: strings.Join(desiredChecks, ","), Observed: strings.Join(branch.RequiredChecks, ","), Status: driftDrift})
	} else if boolValue(wanted.StrictChecks) != boolValue(branch.StrictChecks) {
		drift = append(drift, repositoryDrift{ID: definition.ID, Field: "protection.branch.strict_checks", Desired: formatBoolPtr(wanted.StrictChecks), Observed: formatBoolPtr(branch.StrictChecks), Status: driftDrift})
	}
	return drift
}

func branchNameObservable(branch string) bool {
	if branch == "" || len(branch) > 250 || strings.Contains(branch, "..") {
		return false
	}
	return !strings.ContainsAny(branch, "/\\ \t\r\n?#")
}

func unsupportedAdopted(definition repositoryDefinition, observed repositoryObserved) []repositoryUnsupported {
	unsupported := make([]repositoryUnsupported, 0)
	if definition.Bootstrap != nil {
		unsupported = append(unsupported, repositoryUnsupported{Kind: intentEnsureBootstrap, ID: definition.ID, Reason: reasonBootstrapUnobservable})
	}
	if definition.Secrets != nil {
		unsupported = append(unsupported, repositoryUnsupported{Kind: intentEnsureSecrets, ID: definition.ID, Reason: reasonSecretsUnobservable})
	}
	if len(definition.Identities) > 0 {
		unsupported = append(unsupported, repositoryUnsupported{Kind: intentEnsureIdentities, ID: definition.ID, Reason: reasonIdentitiesUnsupported})
	}
	unsupported = append(unsupported, repositoryUnsupported{Kind: intentRetainOnRemove, ID: definition.ID, Reason: reasonRetainUnsupported})
	if observed.ActionsStatus != observationObserved {
		unsupported = append(unsupported, repositoryUnsupported{Kind: intentEnsureActions, ID: definition.ID, Reason: observed.ActionsReason})
	}
	if definition.Protection != nil && observed.RulesetStatus != observationObserved {
		unsupported = append(unsupported, repositoryUnsupported{Kind: intentEnsureProtection, ID: definition.ID, Reason: observed.RulesetReason})
	}
	return unsupported
}

func addString(drift *[]repositoryDrift, id, field, desired, observed string) {
	if desired == observed {
		return
	}
	*drift = append(*drift, repositoryDrift{ID: id, Field: field, Desired: desired, Observed: observed, Status: driftDrift})
}

func addBool(drift *[]repositoryDrift, id, field string, desired, observed *bool) {
	addString(drift, id, field, formatBoolPtr(desired), formatBoolPtr(observed))
}

func formatBoolPtr(value *bool) string {
	if value == nil {
		return ""
	}
	if *value {
		return "true"
	}
	return "false"
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func observedByID(observed []repositoryObserved) map[string]repositoryObserved {
	out := make(map[string]repositoryObserved, len(observed))
	for _, item := range observed {
		out[item.ID] = item
	}
	return out
}

func bindingsStillMatch(bindings []repositoryBinding, repos []repositoryDefinition) error {
	byID := make(map[string]repositoryDefinition, len(repos))
	for _, definition := range repos {
		byID[definition.ID] = definition
	}
	for _, binding := range bindings {
		definition, ok := byID[binding.ID]
		if !ok {
			continue
		}
		if definition.Org != binding.Org || definition.Name != binding.Name {
			return errRepositoryRetarget
		}
	}
	return nil
}

func bindingFor(bindings []repositoryBinding, id string) *repositoryBinding {
	for i := range bindings {
		if bindings[i].ID == id {
			copied := bindings[i]
			return &copied
		}
	}
	return nil
}

func mergeRepositoryBinding(bindings []repositoryBinding, definition repositoryDefinition, observed repositoryObserved) ([]repositoryBinding, error) {
	for _, binding := range bindings {
		if binding.ID == definition.ID {
			continue
		}
		if binding.RepositoryID == observed.RepositoryID || binding.NodeID == observed.NodeID {
			return nil, errRepositoryPersisted
		}
	}
	for i := range bindings {
		if bindings[i].ID != definition.ID {
			continue
		}
		current := bindings[i]
		if current.Org != definition.Org || current.Name != definition.Name {
			return nil, errRepositoryRetarget
		}
		if current.RepositoryID != observed.RepositoryID || current.NodeID != observed.NodeID {
			return nil, errRepositoryPersisted
		}
		return bindings, nil
	}
	bindings = append(bindings, repositoryBinding{
		ID:           definition.ID,
		Org:          definition.Org,
		Name:         definition.Name,
		RepositoryID: observed.RepositoryID,
		NodeID:       observed.NodeID,
	})
	slices.SortFunc(bindings, func(left, right repositoryBinding) int {
		return strings.Compare(left.ID, right.ID)
	})
	return bindings, nil
}

func (j *repositoryJournal) normalize() error {
	if j.Version != observationVersion {
		return errRepositoryMalformed
	}
	if j.Bindings == nil {
		j.Bindings = []repositoryBinding{}
	}
	if j.Observed == nil {
		j.Observed = []repositoryObserved{}
	}
	slices.SortFunc(j.Bindings, func(left, right repositoryBinding) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(j.Observed, func(left, right repositoryObserved) int {
		return strings.Compare(left.ID, right.ID)
	})
	seen := map[string]struct{}{}
	ids := map[int64]struct{}{}
	nodes := map[string]struct{}{}
	for _, binding := range j.Bindings {
		if binding.ID == "" || binding.RepositoryID <= 0 || !validNodeID(binding.NodeID) {
			return errRepositoryMalformed
		}
		if _, dup := seen[binding.ID]; dup {
			return errRepositoryMalformed
		}
		if _, dup := ids[binding.RepositoryID]; dup {
			return errRepositoryMalformed
		}
		if _, dup := nodes[binding.NodeID]; dup {
			return errRepositoryMalformed
		}
		seen[binding.ID] = struct{}{}
		ids[binding.RepositoryID] = struct{}{}
		nodes[binding.NodeID] = struct{}{}
	}
	return nil
}

func validNodeID(value string) bool {
	if value == "" || len(value) > 200 || strings.ContainsAny(value, " \r\n\t") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '=' || r == '+' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func rejectObservedSecrets(observed repositoryObserved) error {
	payload, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	if privateKeyPattern.Match(payload) || githubTokenPattern.Match(payload) || jwtPattern.Match(payload) {
		return errRepositoryMalformed
	}
	return nil
}

func emptyRepositoryJournal() repositoryJournal {
	return repositoryJournal{Version: observationVersion, Bindings: []repositoryBinding{}, Observed: []repositoryObserved{}}
}

func readRepositoryJournal(dataDir string) (repositoryJournal, error) {
	if strings.TrimSpace(dataDir) == "" {
		return emptyRepositoryJournal(), nil
	}
	parent := filepath.Join(dataDir, observationDirName)
	info, err := os.Lstat(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyRepositoryJournal(), nil
		}
		return repositoryJournal{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return repositoryJournal{}, errors.New("observation journal directory is not a real directory")
	}
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return repositoryJournal{}, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, observationFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyRepositoryJournal(), nil
		}
		return repositoryJournal{}, err
	}
	file := os.NewFile(uintptr(fd), observationFileName)
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return repositoryJournal{}, err
	}
	if !stat.Mode().IsRegular() || stat.Mode()&os.ModeSymlink != 0 {
		return repositoryJournal{}, errors.New("observation journal must be a regular file")
	}
	if stat.Size() == 0 || stat.Size() > 1<<20 {
		return repositoryJournal{}, errRepositoryMalformed
	}
	payload, err := io.ReadAll(io.LimitReader(file, stat.Size()+1))
	if err != nil {
		return repositoryJournal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var journal repositoryJournal
	if err := decoder.Decode(&journal); err != nil {
		return repositoryJournal{}, errRepositoryMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return repositoryJournal{}, errRepositoryMalformed
	}
	if err := journal.normalize(); err != nil {
		return repositoryJournal{}, err
	}
	return journal, nil
}

func writeRepositoryJournal(dataDir string, journal repositoryJournal) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("observation journal directory is missing")
	}
	if err := journal.normalize(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > 1<<20 || privateKeyPattern.Match(payload) || githubTokenPattern.Match(payload) || jwtPattern.Match(payload) {
		return errRepositoryMalformed
	}
	parent := filepath.Join(dataDir, observationDirName)
	if err := os.MkdirAll(parent, observationDirMode); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("observation journal directory is not a real directory")
	}
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := unix.Fchmod(dir, uint32(observationDirMode)); err != nil {
		return err
	}
	tempName := ".repositories.json.tmp"
	fd, err := unix.Openat(dir, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, observationFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tempName)
	_, writeErr := file.Write(payload)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = unix.Unlinkat(dir, tempName, 0)
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	if err := unix.Renameat(dir, tempName, dir, observationFileName); err != nil {
		_ = unix.Unlinkat(dir, tempName, 0)
		return err
	}
	// The rename is already visible. A directory sync failure must not make
	// the caller keep the previous generation against this new journal.
	_ = unix.Fsync(dir)
	return nil
}

func applyRepositoryObservation(plan repositoryPlan, obs *repositoryObservationResult, wantDigest string) (repositoryPlan, *repositoryJournal, error) {
	if !plan.Requested {
		plan.Observation = observationNone
		return plan, nil, repositoryPlanIsReadOnly(plan)
	}
	if obs == nil || obs.Status == "" || obs.Status == observationUnavailable {
		plan.Observation = observationUnavailable
		return plan, nil, repositoryPlanIsReadOnly(plan)
	}
	if obs.Status != observationObserved {
		return repositoryPlan{}, nil, errors.New("repository observation status is invalid")
	}
	if obs.Digest != wantDigest || obs.Journal == nil || obs.Journal.Digest != wantDigest {
		return repositoryPlan{}, nil, errDesiredChanged
	}
	plan.Observation = observationObserved
	plan.Observed = obs.Observed
	plan.Drift = obs.Drift
	plan.Unsupported = obs.Unsupported
	plan.Applied = append([]string(nil), obs.Applied...)
	plan.RemoteMutation = obs.RemoteMutation
	if plan.RemoteMutation == "" {
		plan.RemoteMutation = remoteMutationNone
	}
	if plan.Observed == nil {
		plan.Observed = []repositoryObserved{}
	}
	if plan.Drift == nil {
		plan.Drift = []repositoryDrift{}
	}
	if plan.Applied == nil {
		plan.Applied = []string{}
	}
	if plan.Unsupported == nil {
		plan.Unsupported = []repositoryUnsupported{}
	}
	return plan, obs.Journal, repositoryPlanRecordsApply(plan)
}

func observationError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errRepositoryNotFound), errors.Is(err, errRepositoryIdentity), errors.Is(err, errRepositoryPersisted),
		errors.Is(err, errRepositoryRetarget), errors.Is(err, errRepositoryTruncated), errors.Is(err, errRepositoryAuthScope),
		errors.Is(err, errRepositoryRate), errors.Is(err, errRepositoryMalformed), errors.Is(err, errDesiredChanged),
		errors.Is(err, errRepositoryUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case githubKind(err, "missing"):
		return errRepositoryNotFound
	case githubKind(err, "auth"):
		return errRepositoryAuthScope
	case githubKind(err, "rate"):
		return errRepositoryRate
	case githubKind(err, "partial"):
		return errRepositoryMalformed
	case githubKind(err, "unavailable"):
		return errRepositoryUnavailable
	default:
		return err
	}
}

func jsonString(raw json.RawMessage, allowNull bool) (string, error) {
	if allowNull && string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func jsonBool(raw json.RawMessage) (bool, error) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func jsonInt(raw json.RawMessage) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, err
	}
	return value, nil
}
