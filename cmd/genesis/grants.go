package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

var permissionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type grantUse struct {
	repo     string
	identity string
	author   bool
	review   bool
}

const (
	gitNone  = "none"
	gitRead  = "read"
	gitWrite = "write"

	intentEnsureRepositoryGrant    = "ensure_repository_grant"
	intentEnsureCredential         = "ensure_credential"
	intentEnsureRemoteRegistration = "ensure_remote_registration"

	grantLocalOnlyReason     = "repository grant is a local intent only; Genesis did not activate a credential or call GitHub"
	keyMaterialPendingReason = "key material is pending; Genesis did not mint an App JWT, installation access token, or SSH private key"
	keyMaterialNoneReason    = "no key material is required; Genesis did not activate a credential"
	remoteRegistrationReason = "remote registration is unsupported; Genesis did not call GitHub or register a key"
)

// agentGitHub is an optional capability on an agent. Identity is a provider
// identity name. The on-disk block never carries a secret value.
type agentGitHub struct {
	Repository  string            `json:"-" yaml:"repository"`
	Identity    string            `json:"-" yaml:"identity"`
	Git         string            `json:"-" yaml:"git"`
	Permissions map[string]string `json:"-" yaml:"permissions"`
}

// agentGitHubView is the only GitHub grant shape control APIs and the UI may
// show. It has no identity name and no secret reference.
type agentGitHubView struct {
	Repository  string            `json:"repository"`
	Git         string            `json:"git"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Credential  string            `json:"credential"`
}

type indexedIdentity struct {
	providerIdentity
	Org string
}

type grantPlan struct {
	Requested          bool               `json:"requested"`
	CredentialActive   bool               `json:"credential_active"`
	KeyMaterial        string             `json:"key_material"`
	RemoteRegistration string             `json:"remote_registration"`
	Intents            []grantIntent      `json:"intents"`
	Unsupported        []grantUnsupported `json:"unsupported"`
}

type grantIntent struct {
	Kind        string            `json:"kind"`
	Agent       string            `json:"agent"`
	Repository  string            `json:"repository"`
	Git         string            `json:"git"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Credential  string            `json:"credential"`
}

type grantUnsupported struct {
	Kind   string `json:"kind"`
	Agent  string `json:"agent,omitempty"`
	Reason string `json:"reason"`
}

type digestGitHub struct {
	Repository  string            `json:"repository"`
	Identity    string            `json:"identity"`
	Git         string            `json:"git"`
	Permissions map[string]string `json:"permissions"`
	Credential  string            `json:"credential"`
}

type digestProvider struct {
	ID         string                   `json:"id"`
	Provider   string                   `json:"provider"`
	Org        string                   `json:"org"`
	Identities []digestProviderIdentity `json:"identities"`
}

type digestProviderIdentity struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	Credential string `json:"credential"`
	SecretRef  string `json:"secret_ref,omitempty"`
}

// rolePermissionCeilings are the maximum GitHub App permission levels a
// runtime grant may request. administration, secrets, and environments are
// absent on purpose.
var rolePermissionCeilings = map[string]map[string]string{
	roleManager: {
		"pull_requests": "write",
		"issues":        "write",
		"contents":      "write",
		"metadata":      "read",
		"actions":       "read",
	},
	roleProgrammer: {
		"contents":      "write",
		"workflows":     "write",
		"pull_requests": "write",
		"issues":        "write",
		"actions":       "read",
		"checks":        "read",
		"metadata":      "read",
	},
	roleReviewer: {
		"pull_requests": "write",
		"contents":      "read",
		"actions":       "read",
		"checks":        "read",
		"metadata":      "read",
	},
	roleDevops: {
		"actions":       "write",
		"contents":      "read",
		"pull_requests": "read",
		"metadata":      "read",
	},
	roleReader: {
		"contents": "read",
		"metadata": "read",
	},
}

var forbiddenRuntimePermissions = map[string]struct{}{
	"administration": {},
	"secrets":        {},
	"environments":   {},
}

func (a agentDefinition) validateGitHubShape() error {
	if a.GitHub == nil {
		return nil
	}
	if a.id == designerAgentID || pathWithin(a.Cwd, designerWritableConfigRoot) {
		return errors.New("designer cannot declare a GitHub capability")
	}
	grant := a.GitHub
	if !agentIDPattern.MatchString(grant.Repository) || strings.Contains(grant.Repository, "..") {
		return errors.New("GitHub repository reference is invalid")
	}
	if err := validateClosedName("github.identity", grant.Identity); err != nil {
		return errors.New("GitHub identity reference is invalid")
	}
	switch grant.Git {
	case gitNone, gitRead, gitWrite:
	default:
		return errors.New("git access must be none, read, or write")
	}
	if len(grant.Permissions) == 0 {
		return errors.New("permissions are required")
	}
	if err := rejectSecretMaterial("github.repository", grant.Repository); err != nil {
		return err
	}
	if err := rejectSecretMaterial("github.identity", grant.Identity); err != nil {
		return err
	}
	for name, level := range grant.Permissions {
		if _, forbidden := forbiddenRuntimePermissions[name]; forbidden {
			return errors.New("runtime agents cannot receive administration, secrets, or environments")
		}
		if !permissionNamePattern.MatchString(name) {
			return errors.New("permission name is invalid")
		}
		switch level {
		case "read", "write":
		default:
			return errors.New("permission level must be read or write")
		}
		if err := rejectSecretMaterial("github.permissions", name); err != nil {
			return err
		}
		if err := rejectSecretMaterial("github.permissions", level); err != nil {
			return err
		}
	}
	return nil
}

func bindGrants(
	agents map[string]agentDefinition,
	repos map[string]repositoryDefinition,
	reposActive bool,
	providers map[string]providerDefinition,
	providersActive bool,
) (map[string]agentDefinition, error) {
	bound := make(map[string]agentDefinition, len(agents))
	for id, agent := range agents {
		bound[id] = agent
	}
	identities := indexIdentities(providers)
	ids := make([]string, 0, len(bound))
	for id := range bound {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	uses := make([]grantUse, 0)
	for _, id := range ids {
		agent := bound[id]
		if agent.GitHub == nil {
			continue
		}
		if err := agent.validateGitHubShape(); err != nil {
			return nil, fmt.Errorf("agent %s: %w", id, err)
		}
		if !providersActive {
			return nil, fmt.Errorf("agent %s: GitHub capability requires providers", id)
		}
		if !reposActive {
			return nil, fmt.Errorf("agent %s: GitHub capability requires repositories", id)
		}
		identity, ok := identities[agent.GitHub.Identity]
		if !ok {
			return nil, fmt.Errorf("agent %s: GitHub identity is not defined", id)
		}
		if identity.Role == roleReconciler {
			return nil, fmt.Errorf("agent %s: reconciler identity cannot be granted", id)
		}
		repository, ok := repos[agent.GitHub.Repository]
		if !ok {
			return nil, fmt.Errorf("agent %s: GitHub repository is not declared", id)
		}
		if !strings.EqualFold(repository.Org, identity.Org) {
			return nil, fmt.Errorf("agent %s: repository is outside the provider organization", id)
		}
		if !repositoryDeclaresRole(repository, identity.Role) {
			return nil, fmt.Errorf("agent %s: repository does not declare that runtime role", id)
		}
		if err := validateGrantPermissions(identity.Role, agent.GitHub.Git, agent.GitHub.Permissions); err != nil {
			return nil, fmt.Errorf("agent %s: %w", id, err)
		}
		mode, err := credentialModeFor(identity.providerIdentity, repository, *agent.GitHub)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", id, err)
		}
		author, review := grantShape(agent.GitHub.Git, agent.GitHub.Permissions)
		if author && review {
			return nil, fmt.Errorf("agent %s: author and reviewer access cannot be combined", id)
		}
		if review && identity.Role != roleReviewer {
			return nil, fmt.Errorf("agent %s: review access requires the reviewer identity", id)
		}
		if author && identity.Role != roleProgrammer && identity.Role != roleManager {
			return nil, fmt.Errorf("agent %s: author access requires a programmer or manager identity", id)
		}
		agent.credentialMode = mode
		bound[id] = agent
		uses = append(uses, grantUse{
			repo:     repository.ID,
			identity: identity.Name,
			author:   author,
			review:   review,
		})
	}
	if err := separateReviewerIdentity(uses); err != nil {
		return nil, err
	}
	return bound, nil
}

func indexIdentities(providers map[string]providerDefinition) map[string]indexedIdentity {
	indexed := map[string]indexedIdentity{}
	for _, provider := range providers {
		for _, identity := range provider.Identities {
			indexed[identity.Name] = indexedIdentity{providerIdentity: identity, Org: provider.Org}
		}
	}
	return indexed
}

func repositoryDeclaresRole(repository repositoryDefinition, role string) bool {
	if role == roleReader {
		return true
	}
	for _, identity := range repository.Identities {
		if identity.Role == role {
			return true
		}
	}
	return false
}

func validateGrantPermissions(role, gitAccess string, permissions map[string]string) error {
	ceiling, ok := rolePermissionCeilings[role]
	if !ok {
		return errors.New("identity cannot be granted to an agent")
	}
	for name, level := range permissions {
		if _, forbidden := forbiddenRuntimePermissions[name]; forbidden {
			return errors.New("runtime agents cannot receive administration, secrets, or environments")
		}
		allowed, ok := ceiling[name]
		if !ok || (level == "write" && allowed != "write") {
			return errors.New("permission is not allowlisted for this identity")
		}
	}
	contents := permissions["contents"]
	workflows := permissions["workflows"]
	switch gitAccess {
	case gitWrite:
		if role == roleReviewer || role == roleReader || role == roleDevops {
			return errors.New("git write is not allowlisted for this identity")
		}
		if contents != "write" {
			return errors.New("git write requires contents write")
		}
	case gitRead:
		if contents == "write" {
			return errors.New("contents write cannot be combined with git read")
		}
	case gitNone:
		if contents == "write" && role != roleManager {
			return errors.New("contents write requires git write")
		}
	default:
		return errors.New("git access must be none, read, or write")
	}
	if workflows == "write" && (contents != "write" || gitAccess != gitWrite) {
		return errors.New("workflows write requires contents write and git write")
	}
	return nil
}

func credentialModeFor(identity providerIdentity, repository repositoryDefinition, grant agentGitHub) (string, error) {
	if identity.Credential != credentialNone {
		return credentialPending, nil
	}
	if repository.Settings.Visibility != visibilityPublic {
		return "", errors.New("public read on a private repository cannot omit a credential")
	}
	if grant.Git == gitWrite {
		return "", errors.New("git write cannot omit a credential")
	}
	for _, level := range grant.Permissions {
		if level == "write" {
			return "", errors.New("write permissions cannot omit a credential")
		}
	}
	return credentialNone, nil
}

func grantShape(gitAccess string, permissions map[string]string) (author bool, review bool) {
	author = gitAccess == gitWrite || permissions["contents"] == "write" || permissions["workflows"] == "write"
	review = permissions["pull_requests"] == "write" && !author
	return author, review
}

func separateReviewerIdentity(uses []grantUse) error {
	authors := map[string]map[string]struct{}{}
	reviewers := map[string]map[string]struct{}{}
	for _, use := range uses {
		if use.author {
			if authors[use.repo] == nil {
				authors[use.repo] = map[string]struct{}{}
			}
			authors[use.repo][use.identity] = struct{}{}
		}
		if use.review {
			if reviewers[use.repo] == nil {
				reviewers[use.repo] = map[string]struct{}{}
			}
			reviewers[use.repo][use.identity] = struct{}{}
		}
	}
	for repo, reviewIDs := range reviewers {
		for identity := range reviewIDs {
			if _, overlap := authors[repo][identity]; overlap {
				return fmt.Errorf("repository %s: reviewer identity must stay separate from the author identity", repo)
			}
		}
	}
	return nil
}

func appendGrantIntents(intents []privilegedIntent, id string, agent agentDefinition) []privilegedIntent {
	if agent.GitHub == nil {
		return intents
	}
	mode := agent.credentialMode
	if mode != credentialNone {
		mode = credentialPending
	}
	permissions := maps.Clone(agent.GitHub.Permissions)
	base := privilegedIntent{
		Agent:       id,
		Repository:  agent.GitHub.Repository,
		Identity:    agent.GitHub.Identity,
		Git:         agent.GitHub.Git,
		Permissions: permissions,
		Credential:  mode,
	}
	grant := base
	grant.Kind = intentEnsureRepositoryGrant
	credential := base
	credential.Kind = intentEnsureCredential
	remote := base
	remote.Kind = intentEnsureRemoteRegistration
	return append(intents, grant, credential, remote)
}

func classifyGrantIntent(intent privilegedIntent) (unsupportedChange, bool) {
	switch intent.Kind {
	case intentEnsureRepositoryGrant:
		return unsupportedChange{Kind: intent.Kind, Agent: intent.Agent, Reason: grantLocalOnlyReason}, true
	case intentEnsureCredential:
		reason := keyMaterialPendingReason
		if intent.Credential == credentialNone {
			reason = keyMaterialNoneReason
		}
		return unsupportedChange{Kind: intent.Kind, Agent: intent.Agent, Reason: reason}, true
	case intentEnsureRemoteRegistration:
		return unsupportedChange{Kind: intent.Kind, Agent: intent.Agent, Reason: remoteRegistrationReason}, true
	default:
		return unsupportedChange{}, false
	}
}

func buildGrantPlan(plan privilegedPlan, requested bool) (grantPlan, error) {
	result := grantPlan{
		Requested:          requested,
		CredentialActive:   false,
		KeyMaterial:        keyMaterialNone,
		RemoteRegistration: remoteRegistrationNone,
		Intents:            []grantIntent{},
		Unsupported:        []grantUnsupported{},
	}
	if !requested {
		return result, grantPlanIsInactive(result)
	}
	pending := false
	for _, intent := range plan.Intents {
		change, ok := classifyGrantIntent(intent)
		if !ok {
			continue
		}
		if intent.Credential == credentialPending {
			pending = true
		}
		result.Intents = append(result.Intents, grantIntent{
			Kind:        intent.Kind,
			Agent:       intent.Agent,
			Repository:  intent.Repository,
			Git:         intent.Git,
			Permissions: maps.Clone(intent.Permissions),
			Credential:  intent.Credential,
		})
		result.Unsupported = append(result.Unsupported, grantUnsupported{
			Kind:   change.Kind,
			Agent:  change.Agent,
			Reason: change.Reason,
		})
	}
	if pending {
		result.KeyMaterial = keyMaterialPending
	}
	if len(result.Intents) > 0 {
		result.RemoteRegistration = remoteRegistrationUnsupported
	}
	return result, grantPlanIsInactive(result)
}

func grantPlanIsInactive(plan grantPlan) error {
	if plan.CredentialActive {
		return errors.New("grant plan activated a credential")
	}
	if plan.KeyMaterial != keyMaterialNone && plan.KeyMaterial != keyMaterialPending {
		return fmt.Errorf("grant plan key_material = %q", plan.KeyMaterial)
	}
	if plan.RemoteRegistration != remoteRegistrationNone && plan.RemoteRegistration != remoteRegistrationUnsupported {
		return fmt.Errorf("grant plan remote_registration = %q", plan.RemoteRegistration)
	}
	if !plan.Requested {
		if len(plan.Intents) != 0 || len(plan.Unsupported) != 0 || plan.KeyMaterial != keyMaterialNone || plan.RemoteRegistration != remoteRegistrationNone {
			return errors.New("unrequested grant plan is not empty")
		}
		return nil
	}
	if len(plan.Intents) != len(plan.Unsupported) {
		return errors.New("grant plan left an intent without an unsupported result")
	}
	for index, intent := range plan.Intents {
		unsupported := plan.Unsupported[index]
		if unsupported.Kind != intent.Kind || unsupported.Agent != intent.Agent || unsupported.Reason == "" {
			return errors.New("grant intent was not marked unsupported")
		}
		if intent.Credential != credentialNone && intent.Credential != credentialPending {
			return errors.New("grant intent credential is not pending or none")
		}
	}
	return nil
}

func gitHubViewFrom(agent agentDefinition) *agentGitHubView {
	if agent.GitHub == nil {
		return nil
	}
	mode := agent.credentialMode
	if mode != credentialNone {
		mode = credentialPending
	}
	return &agentGitHubView{
		Repository:  agent.GitHub.Repository,
		Git:         agent.GitHub.Git,
		Permissions: maps.Clone(agent.GitHub.Permissions),
		Credential:  mode,
	}
}

func sameGitHub(left, right *agentGitHubView) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Repository == right.Repository &&
		left.Git == right.Git &&
		left.Credential == right.Credential &&
		maps.Equal(left.Permissions, right.Permissions)
}

func gitHubDigest(agent agentDefinition) *digestGitHub {
	if agent.GitHub == nil {
		return nil
	}
	mode := agent.credentialMode
	if mode != credentialNone {
		mode = credentialPending
	}
	return &digestGitHub{
		Repository:  agent.GitHub.Repository,
		Identity:    agent.GitHub.Identity,
		Git:         agent.GitHub.Git,
		Permissions: maps.Clone(agent.GitHub.Permissions),
		Credential:  mode,
	}
}

func canonicalProviderDigests(providers map[string]providerDefinition) []digestProvider {
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]digestProvider, 0, len(ids))
	for _, id := range ids {
		provider := providers[id]
		identities := append([]providerIdentity(nil), provider.Identities...)
		slices.SortFunc(identities, func(left, right providerIdentity) int {
			return strings.Compare(left.Name, right.Name)
		})
		digests := make([]digestProviderIdentity, 0, len(identities))
		for _, identity := range identities {
			digests = append(digests, digestProviderIdentity{
				Name:       identity.Name,
				Role:       identity.Role,
				Credential: identity.Credential,
				SecretRef:  identity.Secret,
			})
		}
		out = append(out, digestProvider{
			ID:         provider.ID,
			Provider:   provider.Provider,
			Org:        provider.Org,
			Identities: digests,
		})
	}
	return out
}

func providerUTF8(providers map[string]providerDefinition) error {
	for _, provider := range providers {
		if err := requireUTF8(provider.ID, provider.Provider, provider.Org); err != nil {
			return err
		}
		for _, identity := range provider.Identities {
			if err := requireUTF8(identity.Name, identity.Role, identity.Credential, identity.Secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func grantPlanExposes(plan grantPlan, forbidden ...string) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	text := string(payload)
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(text, secret) {
			return errors.New("grant plan exposed provider material")
		}
	}
	return nil
}
