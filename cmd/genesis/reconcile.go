package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	minRegularUID = 1000
	maxOSNameLen  = 32
)

var (
	osNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

	reservedOSUsernames = map[string]struct{}{
		"root": {}, "bin": {}, "daemon": {}, "sys": {}, "sync": {},
		"games": {}, "man": {}, "lp": {}, "mail": {}, "news": {},
		"uucp": {}, "proxy": {}, "www-data": {}, "backup": {}, "list": {},
		"irc": {}, "gnats": {}, "nobody": {}, "sshd": {}, "nologin": {},
		"genesis": {}, "ubuntu": {}, "admin": {}, "messagebus": {},
	}
	reservedOSGroups = map[string]struct{}{
		"root": {}, "bin": {}, "daemon": {}, "sys": {}, "adm": {},
		"tty": {}, "disk": {}, "sudo": {}, "wheel": {}, "shadow": {},
		"staff": {}, "genesis": {}, "docker": {}, "lxd": {}, "kvm": {},
	}
	forbiddenPathPrefixes = []string{
		"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/proc",
		"/sys", "/dev", "/root", "/boot", "/run", "/app",
		"/var/lib/genesis/config", "/var/lib/genesis/data",
	}
)

type unixAccount struct {
	Name  string
	UID   uint32
	GID   uint32
	Home  string
	Shell string
}

type reconciledIdentity struct {
	Agent    string
	Username string
	Home     string
	Cwd      string
	UID      uint32
	GID      uint32
	Groups   []uint32
}

type agentUserSpec struct {
	Agent     string
	Name      string
	Home      string
	Cwd       string
	Groups    []string
	Workspace string
}

type hostAPI interface {
	LookupUser(name string) (*unixAccount, error)
	LookupGroup(name string) (uint32, error)
	CreateUser(spec agentUserSpec) error
	UpdateUser(spec agentUserSpec) error
	EnsureDir(path string, uid, gid int, mode os.FileMode) error
	Chown(path string, uid, gid int) error
	Chmod(path string, mode os.FileMode) error
	Stat(path string) (os.FileInfo, error)
}

type unixHost struct{}

func validateOSUsername(name string) error {
	if err := validateOSName(name); err != nil {
		return err
	}
	if _, reserved := reservedOSUsernames[name]; reserved || strings.HasPrefix(name, "systemd-") {
		return fmt.Errorf("OS user %q is reserved", name)
	}
	return nil
}

func validateOSGroupName(name string) error {
	if err := validateOSName(name); err != nil {
		return err
	}
	if _, reserved := reservedOSGroups[name]; reserved {
		return fmt.Errorf("OS group %q is reserved", name)
	}
	return nil
}

func validateOSName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > maxOSNameLen || !osNamePattern.MatchString(name) {
		return fmt.Errorf("invalid OS name %q", name)
	}
	return nil
}

func (s *privilegedState) canMutate() bool {
	return s != nil && s.mutate && s.host != nil
}

func (s *privilegedState) apply(plan privilegedPlan) (coordinateResult, error) {
	result := coordinateResult{
		HostMutation: hostMutationNone,
		Applied:      []string{},
		Unsupported:  []unsupportedChange{},
	}
	seenUsers := map[string]struct{}{}
	for _, intent := range plan.Intents {
		switch intent.Kind {
		case intentEnsureAgentUser:
			if s.listenerUser != "" && intent.User == s.listenerUser {
				return coordinateResult{}, fmt.Errorf("agent %s: OS user %q is the listener user", intent.Agent, intent.User)
			}
			if err := applyAgentUserIntent(s.host, intent); err != nil {
				return coordinateResult{}, err
			}
			identity, err := lookupReconciledIdentity(s.host, intent)
			if err != nil {
				return coordinateResult{}, err
			}
			s.remember(identity)
			result.Applied = append(result.Applied, intent.Kind+":"+intent.Agent)
			result.HostMutation = hostMutationApplied
			seenUsers[identity.Username] = struct{}{}
		case intentEnsureAgentPaths:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "agent has no OS user; Genesis does not create, chown, or mkdir cwd/home as root",
			})
		case intentProvisionDeclaredEnv:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "env is a process map, not a package graph; Genesis does not install runtimes or packages",
			})
		default:
			return coordinateResult{}, fmt.Errorf("unknown privileged intent kind %q", intent.Kind)
		}
	}
	result.Retained = s.retained(seenUsers)
	return result, nil
}

func (s *privilegedState) remember(identity reconciledIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users == nil {
		s.users = map[string]reconciledIdentity{}
	}
	s.users[identity.Username] = identity
}

func (s *privilegedState) lookupUser(name string) (reconciledIdentity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.users[name]
	return identity, ok
}

func (s *privilegedState) retained(active map[string]struct{}) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0)
	for name := range s.users {
		if _, ok := active[name]; ok {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func applyAgentUserIntent(host hostAPI, intent privilegedIntent) error {
	if host == nil {
		return errors.New("privileged host is not configured")
	}
	if err := validateOSUsername(intent.User); err != nil {
		return fmt.Errorf("agent %s: %w", intent.Agent, err)
	}
	if intent.Shell != "" && intent.Shell != agentShell {
		return fmt.Errorf("agent %s: shell must be %s", intent.Agent, agentShell)
	}
	if err := validateAbsolutePath("home", intent.Home); err != nil {
		return fmt.Errorf("agent %s: %w", intent.Agent, err)
	}
	if err := validateAbsolutePath("cwd", intent.Cwd); err != nil {
		return fmt.Errorf("agent %s: %w", intent.Agent, err)
	}
	homeKind, err := classifyHostPath(intent.Home)
	if err != nil {
		return fmt.Errorf("agent %s home: %w", intent.Agent, err)
	}
	if homeKind != pathManaged {
		return fmt.Errorf("agent %s home %q cannot be an OS user home", intent.Agent, intent.Home)
	}
	cwdKind, err := classifyHostPath(intent.Cwd)
	if err != nil {
		return fmt.Errorf("agent %s cwd: %w", intent.Agent, err)
	}
	if cwdKind == pathForbidden {
		return fmt.Errorf("agent %s cwd %q is not a permitted workspace", intent.Agent, intent.Cwd)
	}

	spec := agentUserSpec{
		Agent:     intent.Agent,
		Name:      intent.User,
		Home:      filepath.Clean(intent.Home),
		Cwd:       filepath.Clean(intent.Cwd),
		Groups:    append([]string(nil), intent.Groups...),
		Workspace: intent.Workspace,
	}
	if spec.Workspace == "" {
		spec.Workspace = workspacePrivate
	}

	existing, err := host.LookupUser(spec.Name)
	if err != nil {
		if !isUnknownUser(err) {
			return fmt.Errorf("agent %s: lookup user %q: %w", intent.Agent, spec.Name, err)
		}
		if err := host.CreateUser(spec); err != nil {
			return fmt.Errorf("agent %s: create user %q: %w", intent.Agent, spec.Name, err)
		}
	} else {
		if existing.UID < minRegularUID || existing.UID == 0 {
			return fmt.Errorf("agent %s: refusing to hijack system user %q (uid %d)", intent.Agent, spec.Name, existing.UID)
		}
		if err := host.UpdateUser(spec); err != nil {
			return fmt.Errorf("agent %s: update user %q: %w", intent.Agent, spec.Name, err)
		}
	}

	account, err := host.LookupUser(spec.Name)
	if err != nil {
		return fmt.Errorf("agent %s: user %q missing after reconcile: %w", intent.Agent, spec.Name, err)
	}
	uid := int(account.UID)
	gid := int(account.GID)
	if err := host.EnsureDir(spec.Home, uid, gid, 0o700); err != nil {
		return fmt.Errorf("agent %s: home: %w", intent.Agent, err)
	}

	workspaceUID, workspaceGID, workspaceMode, err := workspaceOwnership(host, spec, account)
	if err != nil {
		return fmt.Errorf("agent %s: %w", intent.Agent, err)
	}
	switch cwdKind {
	case pathStickyShared:
		if err := ensureStickyWritable(host, spec.Cwd); err != nil {
			return fmt.Errorf("agent %s cwd: %w", intent.Agent, err)
		}
	case pathManaged:
		if err := host.EnsureDir(spec.Cwd, workspaceUID, workspaceGID, workspaceMode); err != nil {
			return fmt.Errorf("agent %s cwd: %w", intent.Agent, err)
		}
	}
	return nil
}

func lookupReconciledIdentity(host hostAPI, intent privilegedIntent) (reconciledIdentity, error) {
	account, err := host.LookupUser(intent.User)
	if err != nil {
		return reconciledIdentity{}, err
	}
	groups := []uint32{account.GID}
	seen := map[uint32]struct{}{account.GID: {}}
	for _, name := range intent.Groups {
		gid, err := host.LookupGroup(name)
		if err != nil {
			return reconciledIdentity{}, fmt.Errorf("agent %s group %q: %w", intent.Agent, name, err)
		}
		if _, exists := seen[gid]; exists {
			continue
		}
		seen[gid] = struct{}{}
		groups = append(groups, gid)
	}
	return reconciledIdentity{
		Agent:    intent.Agent,
		Username: account.Name,
		Home:     filepath.Clean(intent.Home),
		Cwd:      filepath.Clean(intent.Cwd),
		UID:      account.UID,
		GID:      account.GID,
		Groups:   groups,
	}, nil
}

func workspaceOwnership(host hostAPI, spec agentUserSpec, account *unixAccount) (uid, gid int, mode os.FileMode, err error) {
	uid = int(account.UID)
	gid = int(account.GID)
	switch spec.Workspace {
	case workspacePrivate, "":
		return uid, gid, 0o700, nil
	case workspaceSharedRead, workspaceSharedWrite:
		mode = 0o750
		if spec.Workspace == workspaceSharedWrite {
			mode = 0o770
		}
		if len(spec.Groups) > 0 {
			groupID, err := host.LookupGroup(spec.Groups[0])
			if err != nil {
				return 0, 0, 0, fmt.Errorf("workspace group %q: %w", spec.Groups[0], err)
			}
			gid = int(groupID)
		}
		return uid, gid, mode, nil
	default:
		return 0, 0, 0, fmt.Errorf("unknown workspace mode %q", spec.Workspace)
	}
}

type pathKind int

const (
	pathManaged pathKind = iota
	pathStickyShared
	pathForbidden
)

func classifyHostPath(value string) (pathKind, error) {
	path := filepath.Clean(value)
	if !filepath.IsAbs(path) {
		return pathForbidden, errors.New("path must be absolute")
	}
	if path == "/" || path == "/home" || path == "/var" || path == "/var/lib" || path == "/var/lib/genesis" {
		return pathForbidden, fmt.Errorf("%q is not a permitted agent path", path)
	}
	for _, prefix := range forbiddenPathPrefixes {
		if pathHasPrefix(path, prefix) {
			return pathForbidden, fmt.Errorf("%q is not a permitted agent path", path)
		}
	}
	if path == "/tmp" || path == "/var/tmp" {
		return pathStickyShared, nil
	}
	return pathManaged, nil
}

func pathHasPrefix(path, prefix string) bool {
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+string(os.PathSeparator))
}

func ensureStickyWritable(host hostAPI, path string) error {
	info, err := host.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	mode := info.Mode()
	if mode&os.ModeSticky == 0 || mode&0o3 != 0o3 {
		return fmt.Errorf("%q is not a sticky world-writable directory", path)
	}
	return nil
}

func isUnknownUser(err error) bool {
	var unknown user.UnknownUserError
	return errors.As(err, &unknown) || strings.Contains(strings.ToLower(err.Error()), "unknown user")
}

func (unixHost) LookupUser(name string) (*unixAccount, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("uid: %w", err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("gid: %w", err)
	}
	return &unixAccount{
		Name:  account.Username,
		UID:   uint32(uid),
		GID:   uint32(gid),
		Home:  account.HomeDir,
		Shell: "",
	}, nil
}

func (unixHost) LookupGroup(name string) (uint32, error) {
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("gid: %w", err)
	}
	return uint32(gid), nil
}

func (unixHost) CreateUser(spec agentUserSpec) error {
	args := []string{
		"--create-home",
		"--home-dir", spec.Home,
		"--shell", agentShell,
		"--user-group",
	}
	if len(spec.Groups) > 0 {
		args = append(args, "--groups", strings.Join(spec.Groups, ","))
	}
	args = append(args, spec.Name)
	return runHostCommand(hostBin("useradd"), args...)
}

func (unixHost) UpdateUser(spec agentUserSpec) error {
	args := []string{"--shell", agentShell, "--home", spec.Home}
	if account, err := user.Lookup(spec.Name); err == nil && account.HomeDir != spec.Home {
		args = append(args, "--move-home")
	}
	if len(spec.Groups) > 0 {
		args = append(args, "--groups", strings.Join(spec.Groups, ","))
	}
	args = append(args, spec.Name)
	return runHostCommand(hostBin("usermod"), args...)
}

func (unixHost) EnsureDir(path string, uid, gid int, mode os.FileMode) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (unixHost) Chown(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
}

func (unixHost) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

func (unixHost) Stat(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func hostBin(name string) string {
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name
}

func runHostCommand(name string, args ...string) error {
	if name == "" || name == "sh" || name == "bash" || filepath.Base(name) == "sh" || filepath.Base(name) == "bash" {
		return fmt.Errorf("refusing to run %q as a host mutation command", name)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-c") && (name == "/bin/sh" || strings.HasSuffix(name, "/sh")) {
			return errors.New("refusing arbitrary root shell")
		}
	}
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{}
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("%s: %w", filepath.Base(name), err)
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, message)
	}
	return nil
}
