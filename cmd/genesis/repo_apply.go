package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed all:templates/lab-widget
var repositoryTemplates embed.FS

const (
	reasonRepositoryMatched = "repository settings already match; Genesis did not change visibility, description, features, or merge settings"
	reasonActionsMatched    = "actions permissions already match; Genesis did not change them"
	reasonProtectionMatched = "the declared ruleset already matches; Genesis did not change it"
	reasonBootstrapMatched  = "bootstrap template files are already present; Genesis did not overwrite them"
	reasonBypassActors      = "the ruleset has bypass actors this declaration cannot express; Genesis did not change it"
)

var (
	errRepositoryDefaultBranch   = errors.New("created repository default branch does not match the declaration")
	errRepositoryActionsUnsafe   = errors.New("actions policy cannot be expressed; Genesis did not overwrite it")
	errRepositoryTemplate        = errors.New("bootstrap template is not a Genesis-owned template")
	errRepositoryTemplateCheck   = errors.New("bootstrap template does not provide the declared required checks")
	errRepositoryArchived        = errors.New("repository is archived; Genesis did not unarchive, rename, transfer, or delete it")
	errRepositoryBootstrapBranch = errors.New("bootstrap was not committed because the default branch is not a single segment")
)

type templateFile struct {
	Path string
	Body []byte
}

type preparedRepo struct {
	definition       repositoryDefinition
	client           *githubClient
	token            tokenResult
	document         observedRepoDocument
	actions          actionsPolicy
	rulesets         []observedRuleset
	rulesObservable  bool
	created          bool
	settingsWrite    bool
	actionsWrite     bool
	protectionWrite  bool
	protectionReason string
}

func (s *privilegedState) reconcileDesiredRepositories(ctx context.Context, wantDigest string) (repositoryObservationResult, error) {
	if err := ctx.Err(); err != nil {
		return repositoryObservationResult{}, err
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
	ordered := canonicalRepositories(repos)
	sawAdopt := false
	for _, definition := range ordered {
		if definition.Lifecycle.Existing == lifecycleExistingAdopt {
			sawAdopt = true
			break
		}
	}
	ready, err := s.observationReady(ctx, ordered)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if !ready {
		return s.observeDesiredRepositories(ctx, wantDigest)
	}
	if !sawAdopt {
		observed, err := s.observeDesiredRepositories(ctx, wantDigest)
		if err != nil {
			return repositoryObservationResult{}, err
		}
		skip := map[string]string{}
		for _, definition := range ordered {
			for _, intent := range repositoryIntents(definition) {
				skip[intent.Kind+":"+definition.ID] = reasonRefuseUnsupported
			}
		}
		return finishApplyPlan(observed, ordered, map[string]struct{}{}, skip), nil
	}
	templates := map[string][]templateFile{}
	for _, definition := range ordered {
		if definition.Lifecycle.Existing != lifecycleExistingAdopt || definition.Bootstrap == nil {
			continue
		}
		files, loadErr := loadRepositoryTemplate(definition.Bootstrap.Template)
		if loadErr != nil {
			return repositoryObservationResult{}, loadErr
		}
		if err := templateCoversChecks(files, definition.Protection); err != nil {
			return repositoryObservationResult{}, err
		}
		templates[definition.ID] = files
	}
	prior, err := readRepositoryJournal(s.dataDir)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	if err := bindingsStillMatch(prior.Bindings, ordered); err != nil {
		return repositoryObservationResult{}, err
	}
	registrar := s.repoObserver()
	wrote := map[string]struct{}{}
	skip := map[string]string{}
	for _, definition := range ordered {
		if definition.Lifecycle.Existing != lifecycleExistingAdopt {
			for _, intent := range repositoryIntents(definition) {
				skip[intent.Kind+":"+definition.ID] = reasonRefuseUnsupported
			}
			continue
		}
		prepared, prepErr := registrar.prepareRepository(ctx, definition, bindingFor(prior.Bindings, definition.ID))
		if prepErr != nil {
			return repositoryObservationResult{}, observationError(prepErr)
		}
		key := func(kind string) string { return kind + ":" + definition.ID }
		if prepared.created || prepared.settingsWrite {
			if err := prepared.client.patchRepositorySettings(ctx, &prepared.token, definition); err != nil {
				return repositoryObservationResult{}, observationError(err)
			}
			confirmed, readErr := prepared.client.getManagedRepo(ctx, &prepared.token)
			if readErr != nil {
				return repositoryObservationResult{}, observationError(readErr)
			}
			if err := confirmed.matchesDeclaration(definition); err != nil {
				return repositoryObservationResult{}, err
			}
			if settingsDiffer(definition, confirmed) {
				return repositoryObservationResult{}, errRepositoryMalformed
			}
			prepared.document = confirmed
			wrote[key(intentEnsureRepository)] = struct{}{}
		}
		if prepared.actionsWrite {
			if err := prepared.client.putActionsPolicy(ctx, &prepared.token, definition); err != nil {
				return repositoryObservationResult{}, observationError(err)
			}
			reread, readErr := prepared.client.getActionsPolicy(ctx, &prepared.token)
			if readErr != nil {
				return repositoryObservationResult{}, observationError(readErr)
			}
			need, decideErr := actionsNeedWrite(definition, reread)
			if decideErr != nil || need {
				return repositoryObservationResult{}, errRepositoryMalformed
			}
			wrote[key(intentEnsureActions)] = struct{}{}
		}
		if prepared.protectionWrite {
			if err := prepared.client.putDeclaredRuleset(ctx, &prepared.token, definition, prepared.document.DefaultBranch, prepared.rulesets); err != nil {
				return repositoryObservationResult{}, observationError(err)
			}
			confirmedRules, rulesErr := prepared.client.getRulesets(ctx, &prepared.token, prepared.document.DefaultBranch)
			if rulesErr != nil {
				return repositoryObservationResult{}, observationError(rulesErr)
			}
			stillWrite, _ := protectionDecision(definition, prepared.document.DefaultBranch, true, confirmedRules)
			if stillWrite {
				return repositoryObservationResult{}, errRepositoryMalformed
			}
			wrote[key(intentEnsureProtection)] = struct{}{}
		} else if definition.Protection != nil {
			reason := prepared.protectionReason
			if reason == "" {
				reason = reasonProtectionMatched
			}
			skip[key(intentEnsureProtection)] = reason
		}
		if definition.Bootstrap != nil {
			changed, bootErr := prepared.client.commitMissingTemplate(ctx, definition, prepared.document, templates[definition.ID])
			if bootErr != nil {
				return repositoryObservationResult{}, observationError(bootErr)
			}
			if changed {
				wrote[key(intentEnsureBootstrap)] = struct{}{}
			}
		}
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
	observed, err := s.observeDesiredRepositories(ctx, wantDigest)
	if err != nil {
		return repositoryObservationResult{}, err
	}
	return finishApplyPlan(observed, ordered, wrote, skip), nil
}

func finishApplyPlan(observed repositoryObservationResult, repos []repositoryDefinition, wrote map[string]struct{}, skip map[string]string) repositoryObservationResult {
	applied := make([]string, 0)
	unsupported := make([]repositoryUnsupported, 0)
	for _, definition := range repos {
		for _, intent := range repositoryIntents(definition) {
			key := intent.Kind + ":" + definition.ID
			if _, ok := wrote[key]; ok {
				applied = append(applied, key)
				continue
			}
			reason := skip[key]
			if reason == "" {
				reason = defaultUnappliedReason(intent.Kind)
			}
			unsupported = append(unsupported, repositoryUnsupported{Kind: intent.Kind, ID: definition.ID, Reason: reason})
		}
	}
	if len(applied) == 0 {
		observed.RemoteMutation = remoteMutationNone
		observed.Applied = []string{}
	} else {
		observed.RemoteMutation = remoteMutationApplied
		observed.Applied = applied
	}
	observed.Unsupported = unsupported
	return observed
}

func defaultUnappliedReason(kind string) string {
	switch kind {
	case intentEnsureRepository:
		return reasonRepositoryMatched
	case intentEnsureActions:
		return reasonActionsMatched
	case intentEnsureBootstrap:
		return reasonBootstrapMatched
	case intentEnsureSecrets:
		return reasonSecretsUnobservable
	case intentEnsureProtection:
		return reasonProtectionMatched
	case intentEnsureIdentities:
		return reasonIdentitiesUnsupported
	case intentRetainOnRemove:
		return reasonRetainUnsupported
	default:
		return reasonRetainUnsupported
	}
}

func (g githubRegistrar) prepareRepository(ctx context.Context, definition repositoryDefinition, prior *repositoryBinding) (preparedRepo, error) {
	if g.resolveRepo == nil || g.transport == nil {
		return preparedRepo{}, errReconcilerUnavailable
	}
	app, err := g.resolveRepo(ctx, definition)
	if err != nil {
		return preparedRepo{}, err
	}
	if app.Key == nil || app.Org != definition.Org || app.Repo != definition.Name {
		return preparedRepo{}, errReconcilerUnavailable
	}
	client, err := g.client(app)
	if err != nil {
		return preparedRepo{}, err
	}
	writePerms := gitHubPermissions{Administration: "write", Metadata: "read"}
	discovery, err := client.mint(ctx, []string{definition.Name}, nil, writePerms)
	if err != nil {
		return preparedRepo{}, err
	}
	document, err := client.getManagedRepo(ctx, &discovery)
	created := false
	if githubKind(err, "missing") {
		if prior != nil {
			return preparedRepo{}, errRepositoryNotFound
		}
		if err := client.createOrgRepository(ctx, &discovery, definition); err != nil {
			return preparedRepo{}, err
		}
		created = true
		discovery, err = client.mint(ctx, []string{definition.Name}, nil, writePerms)
		if err != nil {
			return preparedRepo{}, err
		}
		document, err = client.getManagedRepo(ctx, &discovery)
	}
	if err != nil {
		return preparedRepo{}, err
	}
	if err := document.matchesDeclaration(definition); err != nil {
		return preparedRepo{}, err
	}
	if prior != nil && (document.ID != prior.RepositoryID || document.NodeID != prior.NodeID) {
		return preparedRepo{}, errRepositoryPersisted
	}
	pinned, err := client.mint(ctx, nil, []int64{document.ID}, writePerms)
	if err != nil {
		return preparedRepo{}, err
	}
	confirmed, err := client.getManagedRepo(ctx, &pinned)
	if err != nil {
		return preparedRepo{}, err
	}
	if err := confirmed.matchesDeclaration(definition); err != nil {
		return preparedRepo{}, err
	}
	if confirmed.ID != document.ID || confirmed.NodeID != document.NodeID || confirmed.FullName != document.FullName || confirmed.Owner != document.Owner || confirmed.Name != document.Name {
		return preparedRepo{}, errRepositoryIdentity
	}
	if created && confirmed.DefaultBranch != definition.Settings.DefaultBranch {
		return preparedRepo{}, errRepositoryDefaultBranch
	}
	if confirmed.Archived {
		return preparedRepo{}, errRepositoryArchived
	}
	policy, err := client.getActionsPolicy(ctx, &pinned)
	if err != nil {
		if errors.Is(err, errFeatureUnavailable) {
			return preparedRepo{}, errRepositoryActionsUnsafe
		}
		return preparedRepo{}, err
	}
	actionsWrite, err := actionsNeedWrite(definition, policy)
	if err != nil {
		return preparedRepo{}, err
	}
	prepared := preparedRepo{
		definition: definition, client: client, token: pinned, document: confirmed,
		actions: policy, created: created, settingsWrite: created || settingsDiffer(definition, confirmed),
		actionsWrite: actionsWrite,
	}
	rulesets, rulesErr := client.getRulesets(ctx, &pinned, confirmed.DefaultBranch)
	switch {
	case rulesErr == nil:
		prepared.rulesObservable = true
		prepared.rulesets = rulesets
	case errors.Is(rulesErr, errFeatureUnavailable):
		prepared.rulesObservable = false
	default:
		return preparedRepo{}, rulesErr
	}
	if definition.Bootstrap != nil && !branchNameObservable(confirmed.DefaultBranch) {
		return preparedRepo{}, errRepositoryBootstrapBranch
	}
	writeProtection, reason := protectionDecision(definition, confirmed.DefaultBranch, prepared.rulesObservable, prepared.rulesets)
	prepared.protectionWrite = writeProtection
	prepared.protectionReason = reason
	return prepared, nil
}

func settingsDiffer(definition repositoryDefinition, document observedRepoDocument) bool {
	settings := definition.Settings
	if settings.Visibility != document.Visibility || settings.Description != document.Description {
		return true
	}
	if boolValue(settings.Features.Issues) != document.Issues || boolValue(settings.Features.Wiki) != document.Wiki || boolValue(settings.Features.Projects) != document.Projects {
		return true
	}
	if boolValue(settings.Merge.AllowSquash) != document.AllowSquash || boolValue(settings.Merge.AllowMergeCommit) != document.AllowMerge || boolValue(settings.Merge.AllowRebase) != document.AllowRebase {
		return true
	}
	return boolValue(settings.Merge.DeleteBranchOnMerge) != document.DeleteBranch
}

func actionsNeedWrite(definition repositoryDefinition, policy actionsPolicy) (bool, error) {
	if !policy.PinningKnown || policy.Pinning || policy.Extra {
		return false, errRepositoryActionsUnsafe
	}
	selectedRelevant := definition.Actions.Allowed == actionsAllowedSelected || policy.Allowed == actionsAllowedSelected
	if selectedRelevant && !policy.SelectedKnown {
		return false, errRepositoryActionsUnsafe
	}
	if boolValue(definition.Actions.Enabled) != policy.Enabled || definition.Actions.Allowed != policy.Allowed {
		return true, nil
	}
	if definition.Actions.Allowed != actionsAllowedSelected && policy.Allowed != actionsAllowedSelected {
		return false, nil
	}
	return !sameStrings(definition.Actions.Selected, policy.Selected), nil
}

func sameStrings(left, right []string) bool {
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	slices.Sort(a)
	slices.Sort(b)
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

func protectionDecision(definition repositoryDefinition, branch string, observable bool, rulesets []observedRuleset) (bool, string) {
	if definition.Protection == nil {
		return false, ""
	}
	if !observable {
		return false, reasonRulesetsUnobservable
	}
	if branch == "" {
		return false, reasonNoDefaultBranch
	}
	if !branchNameObservable(branch) {
		return false, reasonBranchName
	}
	match := matchingRuleset(definition.Protection.Ruleset.Name, rulesets)
	if match == nil {
		return true, ""
	}
	if match.Unmodeled {
		return false, reasonUnmodeledRules
	}
	if match.AdditionalReview {
		return false, reasonExtraReview
	}
	if match.BypassActors {
		return false, reasonBypassActors
	}
	if match.HasPullRequest && match.DismissStaleReviews == nil {
		return false, reasonDismissUnknown
	}
	if rulesetMatches(definition.Protection.Ruleset, branch, *match) {
		return false, reasonProtectionMatched
	}
	return true, ""
}

func matchingRuleset(name string, rulesets []observedRuleset) *observedRuleset {
	for i := range rulesets {
		if rulesets[i].SourceType == "Repository" && rulesets[i].Name == name {
			return &rulesets[i]
		}
	}
	return nil
}

func rulesetMatches(wanted repositoryRuleset, branch string, match observedRuleset) bool {
	if match.Enforcement != "active" || !match.CoversDefaultBranch || !match.HasPullRequest {
		return false
	}
	if intValue(wanted.RequiredApprovingReviews) != intValue(match.RequiredApprovingReviews) {
		return false
	}
	if boolValue(wanted.DismissStaleReviews) != boolValue(match.DismissStaleReviews) {
		return false
	}
	desiredChecks := append([]string(nil), wanted.RequiredChecks...)
	slices.Sort(desiredChecks)
	if len(desiredChecks) == 0 {
		return !match.HasStatusChecks
	}
	return match.HasStatusChecks && strings.Join(desiredChecks, ",") == strings.Join(match.RequiredChecks, ",") && boolValue(wanted.StrictChecks) == boolValue(match.StrictChecks)
}

func (c *githubClient) getManagedRepo(ctx context.Context, tok *tokenResult) (observedRepoDocument, error) {
	if tok == nil || tok.perms.Administration != "write" || tok.perms.Metadata != "read" || tok.perms.Contents != "" {
		return observedRepoDocument{}, githubErr("auth")
	}
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
	return document, nil
}

func (c *githubClient) createOrgRepository(ctx context.Context, tok *tokenResult, definition repositoryDefinition) error {
	if tok == nil || tok.perms.Administration != "write" || tok.perms.Metadata != "read" || tok.perms.Contents != "" {
		return githubErr("auth")
	}
	_, _, err := c.call(ctx, "POST", "/orgs/"+url.PathEscape(definition.Org)+"/repos", tok, map[string]any{
		"name":        definition.Name,
		"description": definition.Settings.Description,
		"visibility":  definition.Settings.Visibility,
		"private":     definition.Settings.Visibility != visibilityPublic,
		"auto_init":   true,
	})
	return err
}

func (c *githubClient) patchRepositorySettings(ctx context.Context, tok *tokenResult, definition repositoryDefinition) error {
	if tok == nil || tok.perms.Administration != "write" || tok.perms.Metadata != "read" || tok.perms.Contents != "" {
		return githubErr("auth")
	}
	settings := definition.Settings
	_, _, err := c.call(ctx, "PATCH", "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo), tok, map[string]any{
		"description":            settings.Description,
		"visibility":             settings.Visibility,
		"has_issues":             boolValue(settings.Features.Issues),
		"has_wiki":               boolValue(settings.Features.Wiki),
		"has_projects":           boolValue(settings.Features.Projects),
		"allow_squash_merge":     boolValue(settings.Merge.AllowSquash),
		"allow_merge_commit":     boolValue(settings.Merge.AllowMergeCommit),
		"allow_rebase_merge":     boolValue(settings.Merge.AllowRebase),
		"delete_branch_on_merge": boolValue(settings.Merge.DeleteBranchOnMerge),
	})
	return err
}

func (c *githubClient) putActionsPolicy(ctx context.Context, tok *tokenResult, definition repositoryDefinition) error {
	if tok == nil || tok.perms.Administration != "write" || tok.perms.Metadata != "read" || tok.perms.Contents != "" {
		return githubErr("auth")
	}
	base := "/repos/" + url.PathEscape(c.app.Org) + "/" + url.PathEscape(c.app.Repo) + "/actions/permissions"
	if _, _, err := c.call(ctx, "PUT", base, tok, map[string]any{
		"enabled":         boolValue(definition.Actions.Enabled),
		"allowed_actions": definition.Actions.Allowed,
	}); err != nil {
		return err
	}
	if definition.Actions.Allowed != actionsAllowedSelected {
		return nil
	}
	patterns := append([]string(nil), definition.Actions.Selected...)
	slices.Sort(patterns)
	_, _, err := c.call(ctx, "PUT", base+"/selected-actions", tok, map[string]any{
		"github_owned_allowed": false,
		"verified_allowed":     false,
		"patterns_allowed":     patterns,
	})
	return err
}

func (c *githubClient) putDeclaredRuleset(ctx context.Context, tok *tokenResult, definition repositoryDefinition, branch string, rulesets []observedRuleset) error {
	if tok == nil || tok.perms.Administration != "write" || tok.perms.Metadata != "read" || tok.perms.Contents != "" {
		return githubErr("auth")
	}
	if definition.Protection == nil || !branchNameObservable(branch) {
		return githubErr("partial")
	}
	body := declaredRulesetBody(definition.Protection.Ruleset, branch)
	match := matchingRuleset(definition.Protection.Ruleset.Name, rulesets)
	path := "/repos/" + url.PathEscape(c.app.Org) + "/" + url.PathEscape(c.app.Repo) + "/rulesets"
	if match == nil {
		_, _, err := c.call(ctx, "POST", path, tok, body)
		return err
	}
	_, _, err := c.call(ctx, "PUT", path+"/"+strconv.FormatInt(match.ID, 10), tok, body)
	return err
}

func declaredRulesetBody(wanted repositoryRuleset, branch string) map[string]any {
	rules := []any{
		map[string]any{
			"type": "pull_request",
			"parameters": map[string]any{
				"required_approving_review_count": intValue(wanted.RequiredApprovingReviews),
				"dismiss_stale_reviews_on_push":   boolValue(wanted.DismissStaleReviews),
			},
		},
	}
	checks := append([]string(nil), wanted.RequiredChecks...)
	slices.Sort(checks)
	if len(checks) > 0 {
		required := make([]map[string]string, 0, len(checks))
		for _, check := range checks {
			required = append(required, map[string]string{"context": check})
		}
		rules = append(rules, map[string]any{
			"type": "required_status_checks",
			"parameters": map[string]any{
				"strict_required_status_checks_policy": boolValue(wanted.StrictChecks),
				"required_status_checks":               required,
			},
		})
	}
	return map[string]any{
		"name":        wanted.Name,
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"refs/heads/" + branch},
				"exclude": []string{},
			},
		},
		"rules": rules,
	}
}

func (c *githubClient) commitMissingTemplate(ctx context.Context, definition repositoryDefinition, document observedRepoDocument, files []templateFile) (bool, error) {
	if len(files) == 0 || !branchNameObservable(document.DefaultBranch) {
		return false, errRepositoryTemplate
	}
	contentsPerms := gitHubPermissions{Contents: "write", Metadata: "read"}
	tok, err := c.mint(ctx, nil, []int64{document.ID}, contentsPerms)
	if err != nil {
		return false, err
	}
	if tok.perms.Contents != "write" || tok.perms.Metadata != "read" || tok.perms.Administration != "" {
		return false, githubErr("auth")
	}
	changed := false
	for _, file := range files {
		present, presentErr := c.templateFilePresent(ctx, &tok, file.Path, document.DefaultBranch)
		if presentErr != nil {
			return false, presentErr
		}
		if present {
			continue
		}
		if err := c.createTemplateFile(ctx, &tok, file, document.DefaultBranch, definition.Bootstrap.Template); err != nil {
			return false, err
		}
		present, presentErr = c.templateFilePresent(ctx, &tok, file.Path, document.DefaultBranch)
		if presentErr != nil || !present {
			return false, errRepositoryMalformed
		}
		changed = true
	}
	return changed, nil
}

func (c *githubClient) templateFilePresent(ctx context.Context, tok *tokenResult, path, branch string) (bool, error) {
	endpoint, err := contentsPath(c.app.Org, c.app.Repo, path, branch)
	if err != nil {
		return false, err
	}
	body, _, err := c.getOnly(ctx, endpoint, tok)
	if githubKind(err, "missing") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var parsed struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Type != "file" || parsed.Path != path {
		return false, githubErr("partial")
	}
	return true, nil
}

func (c *githubClient) createTemplateFile(ctx context.Context, tok *tokenResult, file templateFile, branch, templateID string) error {
	endpoint, err := contentsPath(c.app.Org, c.app.Repo, file.Path, "")
	if err != nil {
		return err
	}
	message := "Add " + file.Path + " from template " + templateID
	if err := rejectSecretMaterial("bootstrap", message); err != nil {
		return errRepositoryMalformed
	}
	_, _, err = c.call(ctx, "PUT", endpoint, tok, map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(file.Body),
		"branch":  branch,
	})
	return err
}

func contentsPath(org, repo, file, branch string) (string, error) {
	if strings.Contains(file, "..") || strings.Contains(file, `\`) || strings.HasPrefix(file, "/") {
		return "", githubErr("partial")
	}
	parts := strings.Split(file, "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", githubErr("partial")
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	path := "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/contents/" + strings.Join(escaped, "/")
	if branch == "" {
		return path, nil
	}
	if !branchNameObservable(branch) {
		return "", githubErr("partial")
	}
	return path + "?ref=" + url.QueryEscape(branch), nil
}

func loadRepositoryTemplate(id string) ([]templateFile, error) {
	if err := validateClosedName("bootstrap.template", id); err != nil {
		return nil, errRepositoryTemplate
	}
	root := "templates/" + id
	files := make([]templateFile, 0)
	err := fs.WalkDir(repositoryTemplates, root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, root+"/")
		if rel == path || strings.Contains(rel, "..") {
			return errRepositoryTemplate
		}
		body, readErr := fs.ReadFile(repositoryTemplates, path)
		if readErr != nil {
			return readErr
		}
		files = append(files, templateFile{Path: rel, Body: body})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errRepositoryTemplate
		}
		return nil, err
	}
	if len(files) == 0 {
		return nil, errRepositoryTemplate
	}
	slices.SortFunc(files, func(left, right templateFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	return files, nil
}

func templateCoversChecks(files []templateFile, protection *repositoryProtection) error {
	if protection == nil || len(protection.Ruleset.RequiredChecks) == 0 {
		return nil
	}
	names, err := templateCheckNames(files)
	if err != nil {
		return err
	}
	have := map[string]struct{}{}
	for _, name := range names {
		have[name] = struct{}{}
	}
	for _, check := range protection.Ruleset.RequiredChecks {
		if _, ok := have[check]; !ok {
			return errRepositoryTemplateCheck
		}
	}
	return nil
}

func templateCheckNames(files []templateFile) ([]string, error) {
	names := make([]string, 0)
	seen := map[string]struct{}{}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, ".github/workflows/") {
			continue
		}
		if !strings.HasSuffix(file.Path, ".yml") && !strings.HasSuffix(file.Path, ".yaml") {
			continue
		}
		var document struct {
			Jobs map[string]struct {
				Name string `yaml:"name"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(file.Body, &document); err != nil {
			return nil, errRepositoryTemplate
		}
		for id, job := range document.Jobs {
			name := job.Name
			if name == "" {
				name = id
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}
