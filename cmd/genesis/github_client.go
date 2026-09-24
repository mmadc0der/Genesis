package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	defaultGitHubAPI  = "https://api.github.com"
	githubAPIVersion  = "2022-11-28"
	githubJWTLifetime = 8 * time.Minute
	githubJWTSkew     = 60 * time.Second
	maxGitHubWait     = 2 * time.Second
	maxGitHubBody     = 1 << 20
	maxKeyPages       = 10
)

type githubCallError struct {
	kind string
}

func (e *githubCallError) Error() string {
	if e == nil || e.kind == "" {
		return "github request failed"
	}
	return "github " + e.kind
}

func githubErr(kind string) error {
	return &githubCallError{kind: kind}
}

func githubKind(err error, kind string) bool {
	var call *githubCallError
	return errors.As(err, &call) && call.kind == kind
}

type githubRegistrar struct {
	baseURL     string
	transport   http.RoundTripper
	now         func() time.Time
	sleep       func(time.Duration)
	resolve     func(context.Context, grantRegistration) (githubAppBinding, error)
	resolveRepo func(context.Context, repositoryDefinition) (githubAppBinding, error)
	logger      *slog.Logger
}

type githubAppBinding struct {
	Org            string
	Repo           string
	AppID          string
	InstallationID string
	Key            *rsa.PrivateKey
}

type gitHubPermissions struct {
	Administration string `json:"administration,omitempty"`
	Metadata       string `json:"metadata,omitempty"`
}

type gitHubTokenRequest struct {
	Repositories  []string          `json:"repositories,omitempty"`
	RepositoryIDs []int64           `json:"repository_ids,omitempty"`
	Permissions   gitHubPermissions `json:"permissions"`
}

type gitHubRepoRef struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

type gitHubTokenResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []gitHubRepoRef   `json:"repositories"`
}

type tokenResult struct {
	Token        string
	ExpiresAt    time.Time
	Permissions  map[string]string
	Selection    string
	Repositories []gitHubRepoRef
	repos        []string
	ids          []int64
	perms        gitHubPermissions
}

type gitHubRepo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type gitHubDeployKey struct {
	ID       int64  `json:"id"`
	Key      string `json:"key"`
	Title    string `json:"title"`
	ReadOnly *bool  `json:"read_only"`
}

type deployKeyView struct {
	ID          int64
	Title       string
	Fingerprint string
	ReadOnly    bool
	usable      bool
}

type githubClient struct {
	base   string
	origin *url.URL
	http   *http.Client
	now    func() time.Time
	sleep  func(time.Duration)
	app    githubAppBinding
	jwt    string
	jwtExp time.Time
	logger *slog.Logger
}

func newGitHubRegistrar(state *privilegedState) githubRegistrar {
	var logger *slog.Logger
	if state != nil {
		logger = state.logger
	}
	return githubRegistrar{
		baseURL:     defaultGitHubAPI,
		transport:   http.DefaultTransport,
		now:         time.Now,
		resolve:     state.resolveReconcilerApp,
		resolveRepo: state.resolveReconcilerRepository,
		logger:      logger,
	}
}

func (g githubRegistrar) Register(ctx context.Context, req grantRegistration) (grantRegistrationResult, error) {
	if g.transport == nil || g.resolve == nil {
		if err := ctx.Err(); err != nil {
			return grantRegistrationResult{}, err
		}
		return grantRegistrationResult{Status: remoteRegistrationUnsupported}, nil
	}
	return g.register(ctx, req)
}

func (g githubRegistrar) register(ctx context.Context, req grantRegistration) (grantRegistrationResult, error) {
	if err := ctx.Err(); err != nil {
		return grantRegistrationResult{}, err
	}
	readOnly, err := deployReadOnly(req.Git)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	title, err := deployKeyTitle(req.GrantID)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	fingerprint, public, err := authorizedFingerprint(req.PublicKey)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	if fingerprint != req.Fingerprint {
		return grantRegistrationResult{}, errors.New("deploy key fingerprint does not match")
	}
	knownID, err := parseRemoteKeyID(req.RemoteKeyID)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	app, err := g.resolve(ctx, req)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	if app.Key == nil || app.Org == "" || app.Repo == "" {
		return grantRegistrationResult{}, errReconcilerUnavailable
	}
	client, err := g.client(app)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
	discovery, err := client.mint(ctx, []string{app.Repo}, nil, gitHubPermissions{Metadata: "read"})
	if err != nil {
		return grantRegistrationResult{}, err
	}
	repo, err := client.getRepo(ctx, &discovery)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	if len(discovery.Repositories) != 1 || repo.ID != discovery.Repositories[0].ID {
		return grantRegistrationResult{}, githubErr("partial")
	}
	keyToken, err := client.mint(ctx, nil, []int64{repo.ID}, gitHubPermissions{Administration: "write", Metadata: "read"})
	if err != nil {
		return grantRegistrationResult{}, err
	}
	keys, err := client.listKeys(ctx, &keyToken)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	adoptID, err := classifyDeployKeys(keys, title, fingerprint, readOnly, knownID)
	if err != nil {
		return grantRegistrationResult{}, err
	}
	if adoptID > 0 {
		return readyResult(adoptID)
	}
	created, err := client.createKey(ctx, &keyToken, title, line, readOnly)
	if githubKind(err, "collision") {
		keys, listErr := client.listKeys(ctx, &keyToken)
		if listErr != nil {
			return grantRegistrationResult{}, listErr
		}
		adoptID, classErr := classifyDeployKeys(keys, title, fingerprint, readOnly, knownID)
		if classErr != nil {
			return grantRegistrationResult{}, classErr
		}
		if adoptID == 0 {
			return grantRegistrationResult{}, githubErr("collision")
		}
		return readyResult(adoptID)
	}
	if err != nil {
		return grantRegistrationResult{}, err
	}
	if created.Fingerprint != fingerprint || created.Title != title || created.ReadOnly != readOnly {
		return grantRegistrationResult{}, githubErr("partial")
	}
	return readyResult(created.ID)
}

func (g githubRegistrar) client(app githubAppBinding) (*githubClient, error) {
	base, origin, err := validateGitHubBase(g.baseURL)
	if err != nil {
		return nil, err
	}
	now := g.now
	if now == nil {
		now = time.Now
	}
	return &githubClient{
		base:   base,
		origin: origin,
		http: &http.Client{
			Transport: g.transport,
			Timeout:   10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("github redirect refused")
			},
		},
		now:    now,
		sleep:  g.sleep,
		app:    app,
		logger: g.logger,
	}, nil
}

func validateGitHubBase(base string) (string, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("github api base is invalid")
	}
	host := parsed.Hostname()
	switch parsed.Scheme {
	case "https":
	case "http":
		if host != "127.0.0.1" && host != "localhost" {
			return "", nil, errors.New("github api base is invalid")
		}
	default:
		return "", nil, errors.New("github api base is invalid")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), parsed, nil
}

func (c *githubClient) mint(ctx context.Context, repos []string, ids []int64, perms gitHubPermissions) (tokenResult, error) {
	tok := tokenResult{repos: append([]string(nil), repos...), ids: append([]int64(nil), ids...), perms: perms}
	if err := c.remint(ctx, &tok); err != nil {
		return tokenResult{}, err
	}
	return tok, nil
}

func (c *githubClient) remint(ctx context.Context, tok *tokenResult) error {
	minted, err := withAuthRetry(c, func() (tokenResult, error) {
		return c.mintOnce(ctx, tok.repos, tok.ids, tok.perms)
	})
	if err != nil {
		return err
	}
	logGitHubTokenScope(c.logger, tok.perms, minted.Permissions)
	wantID := int64(0)
	if len(tok.ids) == 1 {
		wantID = tok.ids[0]
	}
	if err := c.requireScoped(minted, tok.perms, wantID); err != nil {
		return err
	}
	minted.repos = tok.repos
	minted.ids = tok.ids
	minted.perms = tok.perms
	*tok = minted
	return nil
}

func (c *githubClient) mintOnce(ctx context.Context, repos []string, ids []int64, perms gitHubPermissions) (tokenResult, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return tokenResult{}, err
	}
	installationID, err := strconv.ParseInt(c.app.InstallationID, 10, 64)
	if err != nil || installationID <= 0 {
		return tokenResult{}, githubErr("auth")
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	body, _, err := c.do(ctx, http.MethodPost, path, jwt, gitHubTokenRequest{
		Repositories:  repos,
		RepositoryIDs: ids,
		Permissions:   perms,
	})
	if err != nil {
		return tokenResult{}, err
	}
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

func (c *githubClient) requireScoped(tok tokenResult, requested gitHubPermissions, wantID int64) error {
	if !tok.ExpiresAt.After(c.now().Add(30 * time.Second)) {
		return githubErr("auth")
	}
	want, err := expectedTokenPermissions(requested)
	if err != nil || !permissionMapsEqual(tok.Permissions, want) {
		return githubErr("auth")
	}
	if tok.Selection != "selected" || len(tok.Repositories) != 1 {
		return githubErr("partial")
	}
	repo := tok.Repositories[0]
	if repo.ID <= 0 || strings.TrimSpace(repo.FullName) == "" || !strings.EqualFold(repo.Name, c.app.Repo) {
		return githubErr("partial")
	}
	if !strings.EqualFold(repo.FullName, c.app.Org+"/"+c.app.Repo) {
		return githubErr("partial")
	}
	if wantID > 0 && repo.ID != wantID {
		return githubErr("partial")
	}
	return nil
}

func expectedTokenPermissions(requested gitHubPermissions) (map[string]string, error) {
	want := map[string]string{}
	if requested.Metadata != "" {
		if requested.Metadata != "read" && requested.Metadata != "write" {
			return nil, githubErr("auth")
		}
		want["metadata"] = requested.Metadata
	}
	if requested.Administration != "" {
		if requested.Administration != "read" && requested.Administration != "write" {
			return nil, githubErr("auth")
		}
		want["administration"] = requested.Administration
	}
	if len(want) == 0 {
		return nil, githubErr("auth")
	}
	return want, nil
}

func permissionMapsEqual(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func (c *githubClient) getRepo(ctx context.Context, tok *tokenResult) (gitHubRepo, error) {
	body, _, err := c.call(ctx, http.MethodGet, "/repos/"+url.PathEscape(c.app.Org)+"/"+url.PathEscape(c.app.Repo), tok, nil)
	if err != nil {
		return gitHubRepo{}, err
	}
	var repo gitHubRepo
	if err := json.Unmarshal(body, &repo); err != nil {
		return gitHubRepo{}, githubErr("partial")
	}
	if repo.ID <= 0 || strings.TrimSpace(repo.FullName) == "" || !strings.EqualFold(repo.Name, c.app.Repo) || !strings.EqualFold(repo.Owner.Login, c.app.Org) {
		return gitHubRepo{}, githubErr("partial")
	}
	if !strings.EqualFold(repo.FullName, c.app.Org+"/"+c.app.Repo) {
		return gitHubRepo{}, githubErr("partial")
	}
	if err := c.requireScoped(*tok, tok.perms, repo.ID); err != nil {
		return gitHubRepo{}, err
	}
	return repo, nil
}

func (c *githubClient) listKeys(ctx context.Context, tok *tokenResult) ([]deployKeyView, error) {
	path := keyPath(c.app.Org, c.app.Repo) + "?per_page=100"
	var all []deployKeyView
	for page := 0; page < maxKeyPages; page++ {
		body, header, err := c.call(ctx, http.MethodGet, path, tok, nil)
		if err != nil {
			return nil, err
		}
		var batch []gitHubDeployKey
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, githubErr("partial")
		}
		views, err := deployViews(batch)
		if err != nil {
			return nil, err
		}
		all = append(all, views...)
		next, err := nextKeyPage(c.origin, header, c.app.Org, c.app.Repo)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return all, nil
		}
		path = next
	}
	return nil, githubErr("partial")
}

func (c *githubClient) createKey(ctx context.Context, tok *tokenResult, title, public string, readOnly bool) (deployKeyView, error) {
	body, _, err := c.call(ctx, http.MethodPost, keyPath(c.app.Org, c.app.Repo), tok, struct {
		Title    string `json:"title"`
		Key      string `json:"key"`
		ReadOnly bool   `json:"read_only"`
	}{Title: title, Key: public, ReadOnly: readOnly})
	if err != nil {
		return deployKeyView{}, err
	}
	var created gitHubDeployKey
	if err := json.Unmarshal(body, &created); err != nil {
		return deployKeyView{}, githubErr("partial")
	}
	views, err := deployViews([]gitHubDeployKey{created})
	if err != nil {
		return deployKeyView{}, err
	}
	if len(views) != 1 || !views[0].usable {
		return deployKeyView{}, githubErr("partial")
	}
	return views[0], nil
}

func (c *githubClient) call(ctx context.Context, method, path string, tok *tokenResult, payload any) ([]byte, http.Header, error) {
	if err := c.ensureFresh(ctx, tok); err != nil {
		return nil, nil, err
	}
	body, header, err := c.do(ctx, method, path, tok.Token, payload)
	if !githubKind(err, "auth") {
		return body, header, err
	}
	c.jwt = ""
	c.jwtExp = time.Time{}
	if err := c.remint(ctx, tok); err != nil {
		return nil, nil, err
	}
	return c.do(ctx, method, path, tok.Token, payload)
}

func (c *githubClient) ensureFresh(ctx context.Context, tok *tokenResult) error {
	if tok == nil || tok.Token == "" || !tok.ExpiresAt.After(c.now().Add(30*time.Second)) {
		c.jwt = ""
		c.jwtExp = time.Time{}
		return c.remint(ctx, tok)
	}
	return nil
}

func withAuthRetry[T any](c *githubClient, fn func() (T, error)) (T, error) {
	value, err := fn()
	if !githubKind(err, "auth") {
		return value, err
	}
	c.jwt = ""
	c.jwtExp = time.Time{}
	return fn()
}

func (c *githubClient) do(ctx context.Context, method, path, auth string, payload any) ([]byte, http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, githubErr("partial")
		}
	}
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		status, header, body, err := c.roundTrip(ctx, method, path, auth, encoded)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || githubKind(err, "partial") || githubKind(err, "auth") {
				return nil, nil, err
			}
			last = githubErr("unavailable")
			if attempt == 3 {
				return nil, nil, last
			}
			if err := c.wait(ctx, githubBackoff(attempt)); err != nil {
				return nil, nil, err
			}
			continue
		}
		switch {
		case status == http.StatusUnauthorized:
			return nil, header, githubErr("auth")
		case isGitHubRate(status, header):
			delay, ok := rateDelay(c.now(), header)
			if !ok || attempt == 3 {
				return nil, nil, githubErr("rate")
			}
			if err := c.wait(ctx, delay); err != nil {
				return nil, nil, err
			}
			continue
		case status == http.StatusNotFound:
			return nil, header, githubErr("missing")
		case status == http.StatusUnprocessableEntity:
			return nil, header, githubErr("collision")
		case status == http.StatusForbidden:
			return nil, nil, githubErr("auth")
		case status >= 500 || status == http.StatusRequestTimeout || status == http.StatusConflict:
			last = githubErr("unavailable")
			if attempt == 3 {
				return nil, nil, last
			}
			if err := c.wait(ctx, githubBackoff(attempt)); err != nil {
				return nil, nil, err
			}
			continue
		case status != http.StatusOK && status != http.StatusCreated:
			return nil, nil, githubErr("partial")
		default:
			return body, header, nil
		}
	}
	if last == nil {
		last = githubErr("unavailable")
	}
	return nil, nil, last
}

func (c *githubClient) roundTrip(ctx context.Context, method, path, auth string, encoded []byte) (int, http.Header, []byte, error) {
	if strings.ContainsAny(auth, " \r\n") || auth == "" {
		return 0, nil, nil, githubErr("auth")
	}
	endpoint, err := c.endpoint(path)
	if err != nil {
		return 0, nil, nil, err
	}
	var body io.Reader
	if encoded != nil {
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, nil, githubErr("partial")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "genesis")
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubBody+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(payload) > maxGitHubBody {
		return 0, nil, nil, githubErr("partial")
	}
	logGitHubCall(c.logger, method, path, resp.StatusCode)
	return resp.StatusCode, resp.Header.Clone(), payload, nil
}

func logGitHubCall(logger *slog.Logger, method, path string, status int) {
	if logger == nil {
		return
	}
	logger.Info("github call", "method", method, "path", redactGitHubPath(path), "status", status)
}

func logGitHubTokenScope(logger *slog.Logger, requested gitHubPermissions, returned map[string]string) {
	if logger == nil {
		return
	}
	logger.Info("github token scope",
		"requested", formatRequestedPermissions(requested),
		"returned", formatPermissionMap(returned),
	)
}

func formatRequestedPermissions(perms gitHubPermissions) string {
	values := map[string]string{}
	if perms.Administration != "" {
		values["administration"] = perms.Administration
	}
	if perms.Metadata != "" {
		values["metadata"] = perms.Metadata
	}
	return formatPermissionMap(values)
}

func formatPermissionMap(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := values[key]
		if value != "read" && value != "write" {
			value = "redacted"
		}
		if strings.ContainsAny(key, " =,\r\n") || githubTokenPattern.MatchString(key) || jwtPattern.MatchString(key) {
			key = "redacted"
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}

func redactGitHubPath(path string) string {
	if cut, _, ok := strings.Cut(path, "?"); ok {
		path = cut
	}
	if path == "" || strings.Contains(path, "://") || githubTokenPattern.MatchString(path) || jwtPattern.MatchString(path) || strings.Contains(path, "PRIVATE KEY") {
		return "redacted"
	}
	return path
}

func (c *githubClient) endpoint(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "://") || strings.Contains(path, "..") {
		return "", githubErr("partial")
	}
	return c.base + path, nil
}

func (c *githubClient) appJWT() (string, error) {
	now := c.now()
	if c.jwt != "" && now.Add(30*time.Second).Before(c.jwtExp) {
		return c.jwt, nil
	}
	iat := now.Add(-githubJWTSkew)
	exp := now.Add(githubJWTLifetime)
	if !exp.After(iat) || exp.Sub(now) > 10*time.Minute {
		return "", githubErr("auth")
	}
	if _, err := strconv.ParseInt(c.app.AppID, 10, 64); err != nil {
		return "", githubErr("auth")
	}
	token, err := signAppJWT(c.app.Key, c.app.AppID, iat, exp)
	if err != nil {
		return "", githubErr("auth")
	}
	c.jwt = token
	c.jwtExp = exp
	return token, nil
}

func signAppJWT(key *rsa.PrivateKey, appID string, iat, exp time.Time) (string, error) {
	if key == nil || appID == "" {
		return "", githubErr("auth")
	}
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", githubErr("auth")
	}
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{Iat: iat.Unix(), Exp: exp.Unix(), Iss: appID})
	if err != nil {
		return "", githubErr("auth")
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", githubErr("auth")
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *githubClient) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	if c.sleep != nil {
		c.sleep(delay)
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func githubBackoff(attempt int) time.Duration {
	delay := 200 * time.Millisecond
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxGitHubWait {
			return maxGitHubWait
		}
	}
	return delay
}

func isGitHubRate(status int, header http.Header) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden || header == nil {
		return false
	}
	return header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != ""
}

func rateDelay(now time.Time, header http.Header) (time.Duration, bool) {
	if header == nil {
		return time.Second, true
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil {
			if secs < 0 {
				return 0, false
			}
			delay := time.Duration(secs) * time.Second
			if delay > maxGitHubWait {
				return 0, false
			}
			return delay, true
		}
		when, err := http.ParseTime(raw)
		if err != nil {
			return 0, false
		}
		delay := when.Sub(now)
		if delay < 0 {
			delay = 0
		}
		if delay > maxGitHubWait {
			return 0, false
		}
		return delay, true
	}
	if raw := strings.TrimSpace(header.Get("X-RateLimit-Reset")); raw != "" {
		unixSeconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false
		}
		delay := time.Unix(unixSeconds, 0).Sub(now)
		if delay < 0 {
			delay = 0
		}
		if delay > maxGitHubWait {
			return 0, false
		}
		return delay, true
	}
	return time.Second, true
}

func deployReadOnly(gitAccess string) (bool, error) {
	switch gitAccess {
	case gitRead:
		return true, nil
	case gitWrite:
		return false, nil
	default:
		return false, errors.New("git access cannot be registered")
	}
}

func deployKeyTitle(grantID string) (string, error) {
	if !grantIDPattern.MatchString(grantID) {
		return "", errors.New("grant id is invalid")
	}
	return "genesis-" + grantID, nil
}

func parseRemoteKeyID(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, githubErr("partial")
	}
	return id, nil
}

func readyResult(id int64) (grantRegistrationResult, error) {
	if id <= 0 {
		return grantRegistrationResult{}, githubErr("partial")
	}
	return grantRegistrationResult{Status: remoteStatusReady, RemoteKeyID: strconv.FormatInt(id, 10)}, nil
}

func authorizedFingerprint(public string) (string, ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(public)))
	if err != nil || key == nil || key.Type() != ssh.KeyAlgoED25519 {
		return "", nil, errors.New("deploy key is not a usable ed25519 public key")
	}
	return ssh.FingerprintSHA256(key), key, nil
}

func deployViews(keys []gitHubDeployKey) ([]deployKeyView, error) {
	views := make([]deployKeyView, 0, len(keys))
	seen := map[int64]struct{}{}
	for _, key := range keys {
		if key.ID <= 0 || key.ReadOnly == nil {
			return nil, githubErr("partial")
		}
		if _, dup := seen[key.ID]; dup {
			return nil, githubErr("partial")
		}
		seen[key.ID] = struct{}{}
		view := deployKeyView{ID: key.ID, Title: key.Title, ReadOnly: *key.ReadOnly}
		fingerprint, _, err := authorizedFingerprint(key.Key)
		if err == nil {
			view.Fingerprint = fingerprint
			view.usable = true
		}
		views = append(views, view)
	}
	return views, nil
}

func classifyDeployKeys(keys []deployKeyView, title, fingerprint string, readOnly bool, knownID int64) (int64, error) {
	var byID *deployKeyView
	var byFingerprint []deployKeyView
	var byTitle []deployKeyView
	for i := range keys {
		key := keys[i]
		if knownID > 0 && key.ID == knownID {
			matched := key
			byID = &matched
		}
		if key.usable && key.Fingerprint == fingerprint {
			byFingerprint = append(byFingerprint, key)
		}
		if key.Title == title {
			byTitle = append(byTitle, key)
		}
	}
	if len(byFingerprint) > 1 || len(byTitle) > 1 {
		return 0, githubErr("collision")
	}
	if byID != nil {
		if !byID.usable || byID.Fingerprint != fingerprint || byID.Title != title || byID.ReadOnly != readOnly {
			return 0, githubErr("collision")
		}
		return byID.ID, nil
	}
	if len(byFingerprint) == 1 && len(byTitle) == 1 && byFingerprint[0].ID == byTitle[0].ID && byFingerprint[0].ReadOnly == readOnly && byFingerprint[0].Title == title && byFingerprint[0].Fingerprint == fingerprint {
		return byFingerprint[0].ID, nil
	}
	if len(byFingerprint) == 0 && len(byTitle) == 0 {
		return 0, nil
	}
	return 0, githubErr("collision")
}

func keyPath(org, repo string) string {
	return "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/keys"
}

func nextKeyPage(origin *url.URL, header http.Header, org, repo string) (string, error) {
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
	if parsed.EscapedPath() != keyPath(org, repo) {
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

func (s *privilegedState) resolveReconcilerApp(ctx context.Context, req grantRegistration) (githubAppBinding, error) {
	if err := ctx.Err(); err != nil {
		return githubAppBinding{}, err
	}
	if s == nil || s.secrets == nil || strings.TrimSpace(s.agentsDir) == "" {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	repos, reposActive, err := loadRepositories(s.reposDir)
	if err != nil || !reposActive {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	repo, ok := repos[req.Repository]
	if !ok || repo.Provider != repositoryProviderGitHub {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	if err := validateGitHubOrg(repo.Org); err != nil || validateGitHubRepoName(repo.Name) != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	providers, providersActive, err := loadProviders(s.providersDir, s.agentsDir, s.rulesDir, s.reposDir)
	if err != nil || !providersActive {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	agents, err := loadAgents(s.agentsDir)
	if err != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	bound, err := bindGrants(agents, repos, true, providers, true)
	if err != nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	agent, ok := bound[req.Agent]
	if !ok || agent.GitHub == nil {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	grant := agent.GitHub
	if grant.Repository != req.Repository || grant.Identity != req.Identity || grant.Git != req.Git || !permissionMapsEqual(grant.Permissions, req.Permissions) {
		return githubAppBinding{}, errReconcilerUnavailable
	}
	chosen, err := selectReconciler(repo, providers, grant.Identity)
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
		Org:            repo.Org,
		Repo:           repo.Name,
		AppID:          chosen.AppID,
		InstallationID: chosen.InstallationID,
		Key:            key,
	}, nil
}

func selectReconciler(repo repositoryDefinition, providers map[string]providerDefinition, identityName string) (providerIdentity, error) {
	var reconciler providerIdentity
	var matched providerIdentity
	haveReconciler := false
	haveIdentity := false
	for _, provider := range providers {
		if provider.Provider != repositoryProviderGitHub {
			continue
		}
		sameOrg := strings.EqualFold(provider.Org, repo.Org)
		for _, identity := range provider.Identities {
			if identity.Role == roleReconciler && sameOrg {
				if haveReconciler || identity.Credential != credentialApp || identity.Secret == "" {
					return providerIdentity{}, errReconcilerUnavailable
				}
				reconciler = identity
				haveReconciler = true
			}
			if identity.Name != identityName {
				continue
			}
			if haveIdentity || !sameOrg || identity.Role == roleReconciler || identity.Credential != credentialApp || identity.Secret == "" {
				return providerIdentity{}, errReconcilerUnavailable
			}
			if !repositoryDeclaresRole(repo, identity.Role) {
				return providerIdentity{}, errReconcilerUnavailable
			}
			matched = identity
			haveIdentity = true
		}
	}
	if !haveIdentity || !haveReconciler || matched.Secret == reconciler.Secret {
		return providerIdentity{}, errReconcilerUnavailable
	}
	return reconciler, nil
}
