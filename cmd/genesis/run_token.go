package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	githubTokenEnv = "GITHUB_TOKEN"
	ghTokenEnv     = "GH_TOKEN"
)

var (
	errTokenPermissions = errors.New("installation token permissions did not match the grant")

	wholeInstallationToken = regexp.MustCompile(`^(?:gh[opusr]_|github_pat_)[A-Za-z0-9_]{16,}$`)
)

// grantNeedsAPIToken is true for a non-public git read or write grant.
// Public-read (credential none) and git:none stay without an API credential.
func grantNeedsAPIToken(gitAccess, credential string) bool {
	if credential != credentialPending {
		return false
	}
	return gitAccess == gitRead || gitAccess == gitWrite
}

type runTokenRequest struct {
	Agent       string
	Repository  string
	Identity    string
	Git         string
	Permissions map[string]string
}

type runTokenResult struct {
	Token       string
	Org         string
	Repo        string
	Permissions map[string]string
}

type runTokenMinter interface {
	MintRunToken(ctx context.Context, req runTokenRequest) (runTokenResult, error)
}

type grantTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions"`
}

func (s *privilegedState) runTokensEnabled() bool {
	return s != nil && (s.secrets != nil || s.tokens != nil)
}

func (s *privilegedState) tokenMinter() runTokenMinter {
	if s == nil {
		return nil
	}
	if s.tokens != nil {
		return s.tokens
	}
	if s.secrets == nil {
		return nil
	}
	if minter, ok := s.registrar.(runTokenMinter); ok {
		return minter
	}
	return newGitHubRegistrar(s)
}

func (s *privilegedState) activeGrant(agent string) (grantRecord, bool) {
	if s == nil || agent == "" {
		return grantRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.active[agent]
	if !ok {
		return grantRecord{}, false
	}
	record.Permissions = clonePermissions(record.Permissions)
	return record, true
}

func (s *privilegedState) clearActiveGrant(agent string) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, agent)
}

func (s *privilegedState) replaceActiveGrants(records map[string]grantRecord) {
	if s == nil {
		return
	}
	next := map[string]grantRecord{}
	for agent, record := range records {
		record.Permissions = clonePermissions(record.Permissions)
		next[agent] = record
	}
	s.mu.Lock()
	s.active = next
	s.mu.Unlock()
}

func (s *privilegedState) prepareRun(req spawnRequest, identity reconciledIdentity, sshAgent *runSSHAgent) (string, error) {
	if !s.runTokensEnabled() {
		if req.NeedsToken {
			return "", errors.New("installation token is not configured")
		}
		return "", nil
	}
	record, ok := s.activeGrant(req.Agent)
	needs := ok && grantNeedsAPIToken(record.Git, record.Credential)
	if req.NeedsToken && !needs {
		return "", errors.New("grant is not eligible for an installation token")
	}
	if needs && !req.NeedsToken {
		return "", errors.New("installation token was refused")
	}
	checkout := needs && sshAgent != nil
	mode := ""
	if checkout {
		var err error
		mode, err = classifyCheckout(identity.Cwd)
		if err != nil {
			return "", err
		}
	}
	token := ""
	org, repo := "", ""
	if needs {
		minted, err := s.mintRunToken(record)
		if err != nil {
			return "", err
		}
		if !permissionMapsEqual(minted.Permissions, record.Permissions) || minted.Token == "" {
			return "", errTokenPermissions
		}
		token = minted.Token
		org = minted.Org
		repo = minted.Repo
	}
	if checkout {
		if err := s.checkoutBoundRepo(identity, mode, org, repo, sshAgent.Socket(), token); err != nil {
			return "", err
		}
	}
	return token, nil
}

func (s *privilegedState) mintRunToken(record grantRecord) (runTokenResult, error) {
	minter := s.tokenMinter()
	if minter == nil {
		return runTokenResult{}, errors.New("installation token is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	minted, err := minter.MintRunToken(ctx, runTokenRequest{
		Agent:       record.Agent,
		Repository:  record.Repository,
		Identity:    record.Identity,
		Git:         record.Git,
		Permissions: clonePermissions(record.Permissions),
	})
	if err != nil {
		if errors.Is(err, errTokenPermissions) {
			return runTokenResult{}, errTokenPermissions
		}
		return runTokenResult{}, errors.New("installation token was refused")
	}
	if !wholeInstallationToken.MatchString(minted.Token) {
		return runTokenResult{}, errors.New("installation token was refused")
	}
	return minted, nil
}

func (g githubRegistrar) MintRunToken(ctx context.Context, req runTokenRequest) (runTokenResult, error) {
	if err := ctx.Err(); err != nil {
		return runTokenResult{}, errors.New("installation token was refused")
	}
	if g.transport == nil || g.resolve == nil {
		return runTokenResult{}, errors.New("installation token is not configured")
	}
	if !grantNeedsAPIToken(req.Git, credentialPending) || len(req.Permissions) == 0 {
		return runTokenResult{}, errTokenPermissions
	}
	app, err := g.resolve(ctx, grantRegistration{
		Agent:       req.Agent,
		Repository:  req.Repository,
		Identity:    req.Identity,
		Git:         req.Git,
		Permissions: clonePermissions(req.Permissions),
	})
	if err != nil || app.Key == nil || app.Org == "" || app.Repo == "" {
		return runTokenResult{}, errors.New("installation token was refused")
	}
	client, err := g.client(app)
	if err != nil {
		return runTokenResult{}, errors.New("installation token was refused")
	}
	minted, err := client.mintGrant(ctx, clonePermissions(req.Permissions))
	if err != nil {
		if errors.Is(err, errTokenPermissions) {
			return runTokenResult{}, errTokenPermissions
		}
		return runTokenResult{}, errors.New("installation token was refused")
	}
	token := minted.Token
	minted.Token = ""
	if !wholeInstallationToken.MatchString(token) {
		return runTokenResult{}, errors.New("installation token was refused")
	}
	return runTokenResult{
		Token:       token,
		Org:         app.Org,
		Repo:        app.Repo,
		Permissions: clonePermissions(minted.Permissions),
	}, nil
}

func (c *githubClient) mintGrant(ctx context.Context, perms map[string]string) (tokenResult, error) {
	want := clonePermissions(perms)
	if len(want) == 0 || strings.TrimSpace(c.app.Repo) == "" {
		return tokenResult{}, githubErr("auth")
	}
	for name, level := range want {
		if !permissionNamePattern.MatchString(name) || (level != "read" && level != "write") {
			return tokenResult{}, githubErr("auth")
		}
		if _, forbidden := forbiddenRuntimePermissions[name]; forbidden {
			return tokenResult{}, githubErr("auth")
		}
	}
	minted, err := withAuthRetry(c, func() (tokenResult, error) {
		return c.mintGrantOnce(ctx, want)
	})
	if err != nil {
		return tokenResult{}, err
	}
	logPermissionMaps(c.logger, want, minted.Permissions)
	if err := c.acceptRunToken(minted, want); err != nil {
		minted.Token = ""
		return tokenResult{}, err
	}
	return minted, nil
}

func (c *githubClient) mintGrantOnce(ctx context.Context, perms map[string]string) (tokenResult, error) {
	minted, err := c.postGrantToken(ctx, perms)
	if err == nil || !githubKind(err, "missing") {
		return minted, err
	}
	if err := c.refreshCompanyInstallation(ctx); err != nil {
		return tokenResult{}, err
	}
	return c.postGrantToken(ctx, perms)
}

func (c *githubClient) postGrantToken(ctx context.Context, perms map[string]string) (tokenResult, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return tokenResult{}, err
	}
	installationID, err := parsePositiveID(c.app.InstallationID)
	if err != nil {
		return tokenResult{}, githubErr("auth")
	}
	path := "/app/installations/" + installationID + "/access_tokens"
	body, _, err := c.do(ctx, "POST", path, jwt, grantTokenRequest{
		Repositories: []string{c.app.Repo},
		Permissions:  perms,
	})
	if err != nil {
		return tokenResult{}, err
	}
	return parseTokenResponse(body)
}

// refreshCompanyInstallation drops the cached installation id for the org,
// looks it up once, and updates the binding. The caller retries the mint
// once. A second 404 is not refreshed again.
func (c *githubClient) refreshCompanyInstallation(ctx context.Context) error {
	if c == nil || c.state == nil {
		return githubErr("missing")
	}
	org := c.app.Org
	c.state.dropInstallation(org)
	jwt, err := c.appJWT()
	if err != nil {
		return err
	}
	id, err := c.getOrgInstallation(ctx, org, jwt)
	if err != nil {
		return err
	}
	c.app.InstallationID = id
	c.state.rememberInstallation(org, id)
	return nil
}

func parsePositiveID(value string) (string, error) {
	if !gitHubIDPattern.MatchString(value) {
		return "", githubErr("auth")
	}
	return value, nil
}

func parseTokenResponse(body []byte) (tokenResult, error) {
	var parsed gitHubTokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return tokenResult{}, githubErr("partial")
	}
	if strings.ContainsAny(parsed.Token, " \r\n") || parsed.Token == "" {
		return tokenResult{}, githubErr("auth")
	}
	return tokenResult{
		Token:        parsed.Token,
		ExpiresAt:    parsed.ExpiresAt,
		Permissions:  parsed.Permissions,
		Selection:    parsed.RepositorySelection,
		Repositories: parsed.Repositories,
	}, nil
}

func (c *githubClient) acceptRunToken(tok tokenResult, want map[string]string) error {
	if !wholeInstallationToken.MatchString(tok.Token) {
		return githubErr("auth")
	}
	if !tok.ExpiresAt.After(c.now().Add(30 * time.Second)) {
		return githubErr("auth")
	}
	if !permissionMapsEqual(tok.Permissions, want) {
		return errTokenPermissions
	}
	if tok.Selection != "selected" || len(tok.Repositories) != 1 {
		return githubErr("partial")
	}
	repo := tok.Repositories[0]
	if repo.ID <= 0 || !strings.EqualFold(repo.Name, c.app.Repo) {
		return githubErr("partial")
	}
	if repo.FullName != c.app.Org+"/"+c.app.Repo {
		return githubErr("partial")
	}
	return nil
}

func logPermissionMaps(logger *slog.Logger, requested, returned map[string]string) {
	if logger == nil {
		return
	}
	logger.Info("github token scope",
		"requested", formatPermissionMap(requested),
		"returned", formatPermissionMap(returned),
	)
}

func (s *privilegedState) credentialActive(pid int) bool {
	if s == nil || pid <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.spawned[pid]
	return child != nil && child.credentialActive && child.token != ""
}

func (s *privilegedState) finishChild(child *spawnedChild) {
	if s == nil || child == nil {
		return
	}
	if child.ssh != nil {
		child.ssh.stop()
		child.ssh = nil
	}
	s.mu.Lock()
	token := child.token
	home := child.dshHome
	wasActive := child.credentialActive
	agent := child.agent
	child.token = ""
	child.credentialActive = false
	s.mu.Unlock()
	if token != "" {
		scrubTree(home, token)
	}
	if wasActive {
		s.logCredential(agent, false)
	}
}

func (s *privilegedState) logCredential(agent string, active bool) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Info("run credential", "agent", agent, "credential_active", active)
}

func scrubTree(root, secret string) {
	if strings.TrimSpace(root) == "" || len(secret) < 8 {
		return
	}
	secretBytes := []byte(secret)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() <= 0 || info.Size() > 32<<20 {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil || !bytesContains(payload, secretBytes) {
			return nil
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o600
		}
		_ = os.WriteFile(path, replaceBytes(payload, secretBytes), mode)
		return nil
	})
}

func bytesContains(payload, secret []byte) bool {
	return bytes.Contains(payload, secret)
}

func replaceBytes(payload, secret []byte) []byte {
	return bytes.ReplaceAll(payload, secret, []byte(redactedSecret))
}

func redactTokenError(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, redactedSecret))
}

func appendOneEnv(env []string, key, value string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		if name == key {
			continue
		}
		filtered = append(filtered, item)
	}
	if value != "" {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}
