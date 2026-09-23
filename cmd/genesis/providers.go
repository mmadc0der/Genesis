package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Provider definitions are root-owned desired identity state. They name a
// GitHub organization, identities, and secret references. They never contain
// secret values, and loading them does not call GitHub or mint credentials.

const (
	credentialApp                 = "app"
	credentialNone                = "none"
	credentialPending             = "pending"
	roleReader                    = "reader"
	roleReconciler                = "reconciler"
	designerAgentID               = "designer"
	designerWritableConfigRoot    = "/var/lib/genesis/config"
	defaultProvidersDir           = "providers.d"
	keyMaterialNone               = "none"
	keyMaterialPending            = "pending"
	remoteRegistrationNone        = "none"
	remoteRegistrationUnsupported = "unsupported"
)

var gitHubIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

type providerDefinition struct {
	ID         string             `json:"-" yaml:"-"`
	Provider   string             `json:"-" yaml:"provider"`
	Org        string             `json:"-" yaml:"org"`
	Identities []providerIdentity `json:"-" yaml:"identities"`
}

type providerIdentity struct {
	Name           string `json:"-" yaml:"name"`
	Role           string `json:"-" yaml:"role"`
	Credential     string `json:"-" yaml:"credential"`
	Secret         string `json:"-" yaml:"secret,omitempty"`
	AppID          string `json:"-" yaml:"app_id,omitempty"`
	InstallationID string `json:"-" yaml:"installation_id,omitempty"`
}

func loadProviders(directory, agentsDir, rulesDir, reposDir string) (map[string]providerDefinition, bool, error) {
	if strings.TrimSpace(directory) == "" {
		return map[string]providerDefinition{}, false, nil
	}
	if err := validateProvidersLocation(directory, agentsDir, rulesDir, reposDir); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]providerDefinition{}, false, nil
		}
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("providers directory must not be a symlink")
	}
	if !info.IsDir() {
		return nil, false, errors.New("providers path is not a directory")
	}
	// Parent symlinks are invisible to the lexical check above. Resolve them
	// and reject a real directory that lands inside the designer volume or
	// agents, rules, or repos.
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, false, err
	}
	resolvedAgents, err := resolveExistingPath(agentsDir)
	if err != nil {
		return nil, false, err
	}
	resolvedRules, err := resolveExistingPath(rulesDir)
	if err != nil {
		return nil, false, err
	}
	resolvedRepos, err := resolveExistingPath(reposDir)
	if err != nil {
		return nil, false, err
	}
	if err := validateProvidersLocation(resolved, resolvedAgents, resolvedRules, resolvedRepos); err != nil {
		return nil, false, err
	}
	directory = resolved
	dirFile, err := os.OpenFile(directory, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, false, errors.New("providers directory must not be a symlink")
		}
		return nil, false, err
	}
	defer dirFile.Close()
	opened, err := dirFile.Stat()
	if err != nil {
		return nil, false, err
	}
	if !opened.IsDir() {
		return nil, false, errors.New("providers path is not a directory")
	}

	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, false, err
	}
	providers := map[string]providerDefinition{}
	orgs := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		extension := strings.ToLower(filepath.Ext(name))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		if name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
			return nil, false, errors.New("invalid provider file name")
		}
		id, err := agentIDFromFilename(name)
		if err != nil || strings.Contains(id, "..") {
			return nil, false, errors.New("invalid provider id")
		}
		if _, exists := providers[id]; exists {
			return nil, false, errors.New("duplicate provider id")
		}
		contents, err := readProviderEntry(dirFile, name)
		if err != nil {
			return nil, false, err
		}
		var loaded providerDefinition
		if err := decodeYAMLBytes(name, contents, &loaded); err != nil {
			return nil, false, err
		}
		loaded.ID = id
		if err := loaded.validate(); err != nil {
			return nil, false, err
		}
		orgKey := strings.ToLower(loaded.Org)
		if other, exists := orgs[orgKey]; exists {
			return nil, false, fmt.Errorf("provider organization is already declared by %s", other)
		}
		orgs[orgKey] = id
		providers[id] = loaded
	}
	if err := validateProviderSet(providers); err != nil {
		return nil, false, err
	}
	return providers, true, nil
}

func readProviderEntry(dir *os.File, name string) ([]byte, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("provider file must not be a symlink")
		}
		if errors.Is(err, syscall.ENXIO) {
			return nil, errors.New("provider file must be a regular file")
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("provider file must be a regular file")
	}
	if info.Size() > maxRepositoryFileBytes {
		return nil, errors.New("provider file is too large")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxRepositoryFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maxRepositoryFileBytes {
		return nil, errors.New("provider file is too large")
	}
	return contents, nil
}

func (p providerDefinition) validate() error {
	if p.Provider != repositoryProviderGitHub {
		return fmt.Errorf("provider must be %q", repositoryProviderGitHub)
	}
	if err := validateGitHubOrg(p.Org); err != nil {
		return err
	}
	if err := rejectSecretMaterial("provider", p.Provider); err != nil {
		return err
	}
	if err := rejectSecretMaterial("org", p.Org); err != nil {
		return err
	}
	if len(p.Identities) == 0 {
		return errors.New("identities are required")
	}
	seenName := map[string]struct{}{}
	seenRole := map[string]struct{}{}
	for _, identity := range p.Identities {
		if err := identity.validate(); err != nil {
			return err
		}
		if _, exists := seenName[identity.Name]; exists {
			return errors.New("duplicate identity name")
		}
		seenName[identity.Name] = struct{}{}
		if _, exists := seenRole[identity.Role]; exists {
			return errors.New("duplicate identity role")
		}
		seenRole[identity.Role] = struct{}{}
	}
	return nil
}

func (identity providerIdentity) validate() error {
	if err := rejectSecretMaterial("identities.name", identity.Name); err != nil {
		return err
	}
	if !closedNamePattern.MatchString(identity.Name) || strings.Contains(identity.Name, "..") || strings.ContainsAny(identity.Name, `/\`) {
		return errors.New("identity name is invalid")
	}
	switch identity.Role {
	case roleReconciler, roleManager, roleProgrammer, roleReviewer, roleDevops, roleReader:
	default:
		return errors.New("identity role is not allowed")
	}
	if err := rejectSecretMaterial("identities.name", identity.Name); err != nil {
		return err
	}
	if err := rejectSecretMaterial("identities.role", identity.Role); err != nil {
		return err
	}
	if err := rejectSecretMaterial("identities.credential", identity.Credential); err != nil {
		return err
	}
	if identity.Secret != "" {
		if err := rejectSecretMaterial("identities.secret", identity.Secret); err != nil {
			return err
		}
	}
	if err := validateReconcilerAppIDs(identity); err != nil {
		return err
	}
	switch identity.Credential {
	case credentialApp:
		if identity.Role == roleReader {
			return errors.New("reader credential must be none")
		}
		if !secretNamePattern.MatchString(identity.Secret) {
			return errors.New("secret reference is invalid")
		}
	case credentialNone:
		if identity.Role != roleReader {
			return errors.New("only a reader identity can omit a credential")
		}
		if identity.Secret != "" {
			return errors.New("credential none cannot carry a secret reference")
		}
	default:
		return errors.New("credential must be app or none")
	}
	return nil
}

func validateReconcilerAppIDs(identity providerIdentity) error {
	if err := rejectSecretMaterial("identities.app_id", identity.AppID); err != nil {
		return err
	}
	if err := rejectSecretMaterial("identities.installation_id", identity.InstallationID); err != nil {
		return err
	}
	if identity.Role != roleReconciler {
		if identity.AppID != "" || identity.InstallationID != "" {
			return errors.New("app id is only valid on the reconciler identity")
		}
		return nil
	}
	if err := validateGitHubID("identities.app_id", identity.AppID); err != nil {
		return err
	}
	return validateGitHubID("identities.installation_id", identity.InstallationID)
}

func validateGitHubID(field, value string) error {
	if !gitHubIDPattern.MatchString(value) {
		return fmt.Errorf("%s is invalid", field)
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return fmt.Errorf("%s is invalid", field)
	}
	return rejectSecretMaterial(field, value)
}

func validateProviderSet(providers map[string]providerDefinition) error {
	seenName := map[string]struct{}{}
	seenSecret := map[string]struct{}{}
	for _, provider := range providers {
		for _, identity := range provider.Identities {
			if _, exists := seenName[identity.Name]; exists {
				return errors.New("duplicate identity name")
			}
			seenName[identity.Name] = struct{}{}
			if identity.Secret == "" {
				continue
			}
			if _, exists := seenSecret[identity.Secret]; exists {
				return errors.New("provider identities share a secret reference")
			}
			seenSecret[identity.Secret] = struct{}{}
		}
	}
	return nil
}

func validateProvidersLocation(providersDir, agentsDir, rulesDir, reposDir string) error {
	clean := filepath.Clean(providersDir)
	if clean == "" || clean == "." {
		return errors.New("providers directory is required")
	}
	if !filepath.IsAbs(clean) {
		return errors.New("providers directory must be absolute")
	}
	if pathWithin(clean, designerWritableConfigRoot) {
		return errors.New("providers directory must be outside the designer-writable config volume")
	}
	for _, dir := range []string{agentsDir, rulesDir, reposDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		parent := filepath.Clean(dir)
		if clean == parent || pathWithin(clean, parent) {
			return errors.New("providers directory must not be inside agents, rules, or repos")
		}
	}
	return nil
}

func resolveExistingPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	clean := filepath.Clean(path)
	if _, err := os.Lstat(clean); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return clean, nil
		}
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	if filepath.IsAbs(path) != filepath.IsAbs(root) {
		return false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != "." && !strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative)
}
