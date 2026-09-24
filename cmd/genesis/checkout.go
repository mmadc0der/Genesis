package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	checkoutClone = "clone"
	checkoutFetch = "fetch"
)

func (s *privilegedState) checkoutBoundRepo(identity reconciledIdentity, mode, org, repo, socket, token string) error {
	remote, err := s.checkoutRemote(org, repo)
	if err != nil || !allowCheckoutRemote(remote) || (token != "" && strings.Contains(remote, token)) {
		return errors.New("repository checkout failed")
	}
	if githubTokenPattern.MatchString(remote) || strings.Contains(remote, "PRIVATE KEY") {
		return errors.New("repository checkout failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch mode {
	case checkoutClone:
		return s.runGit(ctx, identity, identity.Cwd, socket, "clone", "--quiet", "--", remote, ".")
	case checkoutFetch:
		origin, err := s.gitOutput(ctx, identity, identity.Cwd, socket, "remote", "get-url", "origin")
		if err != nil || !sameCheckoutRemote(origin, remote) {
			return errors.New("workspace is not an empty directory or the bound repository")
		}
		if strings.TrimSpace(origin) != remote {
			if err := s.runGit(ctx, identity, identity.Cwd, socket, "remote", "set-url", "origin", remote); err != nil {
				return err
			}
		}
		return s.runGit(ctx, identity, identity.Cwd, socket, "fetch", "--quiet", "origin")
	default:
		return errors.New("repository checkout failed")
	}
}

func (s *privilegedState) checkoutRemote(org, repo string) (string, error) {
	if s != nil && s.gitRemote != nil {
		return s.gitRemote(org, repo)
	}
	if err := validateGitHubOrg(org); err != nil || validateGitHubRepoName(repo) != nil {
		return "", errors.New("repository checkout failed")
	}
	return "git@github.com:" + org + "/" + repo + ".git", nil
}

func allowCheckoutRemote(remote string) bool {
	if remote == "" || strings.ContainsAny(remote, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(remote, "git@github.com:") || strings.HasPrefix(remote, "ssh://git@github.com/") {
		return !strings.Contains(remote, "://") || strings.HasPrefix(remote, "ssh://git@github.com/")
	}
	return filepath.IsAbs(remote) && !strings.Contains(remote, "://")
}

func classifyCheckout(cwd string) (string, error) {
	info, err := os.Lstat(cwd)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace is not an empty directory or the bound repository")
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return "", errors.New("repository checkout failed")
	}
	if len(entries) == 0 {
		return checkoutClone, nil
	}
	gitInfo, err := os.Lstat(filepath.Join(cwd, ".git"))
	if err != nil || gitInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace is not an empty directory or the bound repository")
	}
	return checkoutFetch, nil
}

func sameCheckoutRemote(got, want string) bool {
	return canonicalRemote(got) != "" && canonicalRemote(got) == canonicalRemote(want)
}

func canonicalRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, "/")
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") || githubTokenPattern.MatchString(raw) {
		return ""
	}
	lower := strings.ToLower(raw)
	lower = strings.TrimSuffix(lower, ".git")
	switch {
	case strings.HasPrefix(lower, "git@github.com:"):
		return "github:" + strings.TrimPrefix(lower, "git@github.com:")
	case strings.HasPrefix(lower, "ssh://git@github.com/"):
		return "github:" + strings.TrimPrefix(lower, "ssh://git@github.com/")
	case strings.HasPrefix(lower, "https://github.com/"):
		path := strings.TrimPrefix(lower, "https://github.com/")
		if strings.Contains(path, "@") {
			return ""
		}
		return "github:" + path
	default:
		if filepath.IsAbs(raw) {
			cleaned := filepath.Clean(raw)
			cleaned = strings.TrimSuffix(cleaned, string(filepath.Separator)+".git")
			return "path:" + cleaned
		}
		return ""
	}
}

func (s *privilegedState) gitExecutable() (string, error) {
	if s != nil && s.gitBin != "" {
		return s.gitBin, nil
	}
	path, err := exec.LookPath("git")
	if err != nil || path == "" {
		return "", errors.New("repository checkout failed")
	}
	return path, nil
}

func gitProcessEnv(identity reconciledIdentity, socket string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + identity.Home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GCM_INTERACTIVE=Never",
	}
	if socket != "" {
		env = append(env,
			sshAuthSockEnv+"="+socket,
			"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
		)
	}
	return env
}

func (s *privilegedState) runGit(ctx context.Context, identity reconciledIdentity, dir, socket string, args ...string) error {
	if err := rejectGitArgs(args); err != nil {
		return err
	}
	bin, err := s.gitExecutable()
	if err != nil {
		return err
	}
	env := gitProcessEnv(identity, socket)
	if s != nil && s.observeGit != nil {
		s.observeGit(append([]string{}, env...), append([]string{bin}, args...))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	if cred := identityCredential(identity); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		s.logGitFailure(stderr.Bytes())
		return errors.New("repository checkout failed")
	}
	return nil
}

func (s *privilegedState) gitOutput(ctx context.Context, identity reconciledIdentity, dir, socket string, args ...string) (string, error) {
	if err := rejectGitArgs(args); err != nil {
		return "", err
	}
	bin, err := s.gitExecutable()
	if err != nil {
		return "", err
	}
	env := gitProcessEnv(identity, socket)
	if s != nil && s.observeGit != nil {
		s.observeGit(append([]string{}, env...), append([]string{bin}, args...))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	if cred := identityCredential(identity); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		s.logGitFailure(stderr.Bytes())
		return "", errors.New("repository checkout failed")
	}
	text := strings.TrimSpace(stdout.String())
	if githubTokenPattern.MatchString(text) || strings.ContainsAny(text, "\r\n") {
		return "", errors.New("repository checkout failed")
	}
	return text, nil
}

func (s *privilegedState) logGitFailure(stderr []byte) {
	if s == nil || s.logger == nil || len(stderr) == 0 {
		return
	}
	text := string(redactPrivateKeys(stderr))
	if len(text) > 400 {
		text = text[:400]
	}
	s.logger.Error("repository checkout failed", "error", text)
}

func rejectGitArgs(args []string) error {
	for _, arg := range args {
		if arg == "" || strings.ContainsAny(arg, "\r\n") || githubTokenPattern.MatchString(arg) || strings.Contains(arg, "PRIVATE KEY") {
			return errors.New("repository checkout failed")
		}
	}
	return nil
}

func identityCredential(identity reconciledIdentity) *syscall.Credential {
	if os.Geteuid() != 0 && int(identity.UID) == os.Geteuid() && int(identity.GID) == os.Getegid() {
		return nil
	}
	groups := append([]uint32{}, identity.Groups...)
	if len(groups) == 0 {
		groups = []uint32{identity.GID}
	}
	return &syscall.Credential{Uid: identity.UID, Gid: identity.GID, Groups: groups}
}
