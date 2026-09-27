package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

type memoryHost struct {
	users    map[string]*unixAccount
	groups   map[string]uint32
	dirs     map[string]memoryDir
	commands [][]string
	nextUID  uint32
}

type memoryDir struct {
	uid  int
	gid  int
	mode os.FileMode
}

func newMemoryHost() *memoryHost {
	return &memoryHost{
		users:   map[string]*unixAccount{},
		groups:  map[string]uint32{"src": 42},
		dirs:    map[string]memoryDir{},
		nextUID: 2000,
	}
}

func (m *memoryHost) LookupUser(name string) (*unixAccount, error) {
	account, ok := m.users[name]
	if !ok {
		return nil, user.UnknownUserError(name)
	}
	copied := *account
	return &copied, nil
}

func (m *memoryHost) LookupGroup(name string) (uint32, error) {
	gid, ok := m.groups[name]
	if !ok {
		return 0, fmt.Errorf("unknown group %s", name)
	}
	return gid, nil
}

func (m *memoryHost) CreateUser(spec agentUserSpec) error {
	m.commands = append(m.commands, append([]string{"useradd"}, spec.Name))
	if _, exists := m.users[spec.Name]; exists {
		return fmt.Errorf("user %s exists", spec.Name)
	}
	uid := m.nextUID
	m.nextUID++
	m.users[spec.Name] = &unixAccount{Name: spec.Name, UID: uid, GID: uid, Home: spec.Home, Shell: agentShell}
	return nil
}

func (m *memoryHost) UpdateUser(spec agentUserSpec) error {
	m.commands = append(m.commands, append([]string{"usermod"}, spec.Name))
	account, ok := m.users[spec.Name]
	if !ok {
		return user.UnknownUserError(spec.Name)
	}
	account.Home = spec.Home
	account.Shell = agentShell
	return nil
}

func (m *memoryHost) EnsureDir(path string, uid, gid int, mode os.FileMode) error {
	m.dirs[filepath.Clean(path)] = memoryDir{uid: uid, gid: gid, mode: mode}
	return nil
}

func (m *memoryHost) Chown(path string, uid, gid int) error {
	dir := m.dirs[filepath.Clean(path)]
	dir.uid = uid
	dir.gid = gid
	m.dirs[filepath.Clean(path)] = dir
	return nil
}

func (m *memoryHost) Chmod(path string, mode os.FileMode) error {
	dir := m.dirs[filepath.Clean(path)]
	dir.mode = mode
	m.dirs[filepath.Clean(path)] = dir
	return nil
}

func (m *memoryHost) Stat(path string) (os.FileInfo, error) {
	cleaned := filepath.Clean(path)
	if _, ok := m.dirs[cleaned]; ok {
		info, err := os.Lstat(os.TempDir())
		return info, err
	}
	return os.Lstat(cleaned)
}

func TestBuildPlanEmitsDedicatedUserIntent(t *testing.T) {
	agents := map[string]agentDefinition{
		"janitor": {
			id:   "janitor",
			User: "workspace-janitor",
			Cwd:  "/home/workspace-janitor/workspace",
			Home: "/home/workspace-janitor",
			Setup: &agentSetup{
				Workspace: workspacePrivate,
			},
			Env: map[string]string{"PATH": "/usr/bin"},
		},
	}
	plan := buildPlan(agents, true)
	if len(plan.Intents) != 2 {
		t.Fatalf("intents = %#v", plan.Intents)
	}
	if plan.Intents[0].Kind != intentEnsureAgentUser || plan.Intents[0].User != "workspace-janitor" || plan.Intents[0].Shell != agentShell {
		t.Fatalf("user intent = %#v", plan.Intents[0])
	}
	if plan.Intents[1].Kind != intentProvisionDeclaredEnv {
		t.Fatalf("env intent = %#v", plan.Intents[1])
	}
	if !plan.Agents {
		t.Fatal("agent plan must mark the agents layer")
	}
	if len(buildPlan(agents, false).Intents) != 0 || buildPlan(agents, false).Agents {
		t.Fatal("rules-only plan must have no intents")
	}
}

func TestEvaluatePlanRejectsDedicatedUserWithoutRoot(t *testing.T) {
	plan := privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/home/workspace-janitor/workspace", Shell: agentShell,
	}}}
	_, err := evaluatePlan(plan)
	if err == nil || !strings.Contains(err.Error(), "requires root genesis launch") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyPlanCreatesUserIdempotentlyAndRetainsRemovedUsers(t *testing.T) {
	host := newMemoryHost()
	state := &privilegedState{host: host, mutate: true, users: map[string]reconciledIdentity{}}
	intent := privilegedIntent{
		Kind:      intentEnsureAgentUser,
		Agent:     "janitor",
		User:      "workspace-janitor",
		Home:      "/home/workspace-janitor",
		Cwd:       "/home/workspace-janitor/workspace",
		Shell:     agentShell,
		Workspace: workspacePrivate,
	}
	first, err := state.apply(privilegedPlan{Intents: []privilegedIntent{intent}})
	if err != nil {
		t.Fatal(err)
	}
	if first.HostMutation != hostMutationApplied || len(first.Applied) != 1 {
		t.Fatalf("first = %#v", first)
	}
	second, err := state.apply(privilegedPlan{Intents: []privilegedIntent{intent}})
	if err != nil {
		t.Fatal(err)
	}
	if second.HostMutation != hostMutationApplied {
		t.Fatalf("second = %#v", second)
	}
	home := host.dirs["/home/workspace-janitor"]
	if home.mode != 0o755 || home.uid != 2000 || home.mode&0o022 != 0 {
		t.Fatalf("home = %#v", home)
	}
	workspace := host.dirs["/home/workspace-janitor/workspace"]
	if workspace.mode != 0o755 || workspace.uid != 2000 || workspace.gid != 2000 || workspace.mode&0o022 != 0 {
		t.Fatalf("private workspace = %#v", workspace)
	}
	removed, err := state.apply(privilegedPlan{Agents: true, Intents: []privilegedIntent{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := host.users["workspace-janitor"]; !ok {
		t.Fatal("removed agent definition deleted the OS user")
	}
	if len(removed.Retained) != 1 || removed.Retained[0] != "workspace-janitor" {
		t.Fatalf("retained = %#v", removed.Retained)
	}
	for _, command := range host.commands {
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "userdel") || strings.Contains(joined, "sh -c") {
			t.Fatalf("unsafe host command %#v", command)
		}
	}
}

func TestApplyPlanRejectsSystemUserAndForbiddenPaths(t *testing.T) {
	host := newMemoryHost()
	host.users["janitor"] = &unixAccount{Name: "janitor", UID: 1, GID: 1, Home: "/home/janitor"}
	state := &privilegedState{host: host, mutate: true, listenerUser: "genesis"}
	_, err := state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "janitor",
		Home: "/home/janitor", Cwd: "/home/janitor/workspace", Shell: agentShell,
	}}})
	if err == nil || !strings.Contains(err.Error(), "system user") {
		t.Fatalf("system user error = %v", err)
	}

	host = newMemoryHost()
	state = &privilegedState{host: host, mutate: true}
	_, err = state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/etc/passwd", Cwd: "/tmp/work", Shell: agentShell,
	}}})
	if err == nil || (!strings.Contains(err.Error(), "home must be") && !strings.Contains(err.Error(), "not a permitted")) {
		t.Fatalf("forbidden home error = %v", err)
	}

	host = newMemoryHost()
	state = &privilegedState{host: host, mutate: true}
	_, err = state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/tmp/work", Shell: agentShell,
	}}})
	if err == nil || !strings.Contains(err.Error(), "not a permitted") {
		t.Fatalf("tmp subdirectory cwd error = %v", err)
	}

	state = &privilegedState{host: newMemoryHost(), mutate: true, listenerUser: "workspace-janitor"}
	_, err = state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/home/workspace-janitor/workspace", Shell: agentShell,
	}}})
	if err == nil || !strings.Contains(err.Error(), "listener user") {
		t.Fatalf("listener collision error = %v", err)
	}
}

func TestApplyPlanAllowsStickyTmpWorkspace(t *testing.T) {
	host := newMemoryHost()
	state := &privilegedState{host: host, mutate: true}
	_, err := state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/tmp", Shell: agentShell, Workspace: workspacePrivate,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := host.dirs["/tmp"]; ok {
		t.Fatal("reconcile chowned /tmp")
	}
}

func TestRunHostCommandRefusesShell(t *testing.T) {
	if err := runHostCommand("/bin/bash", "-c", "id"); err == nil {
		t.Fatal("expected shell refusal")
	}
	if err := runHostCommand("sh", "-c", "id"); err == nil {
		t.Fatal("expected sh refusal")
	}
}

func TestClassifyHostPath(t *testing.T) {
	kind, err := classifyHostPath("/home/workspace-janitor")
	if err != nil || kind != pathManaged {
		t.Fatalf("home kind = %v err = %v", kind, err)
	}
	kind, err = classifyHostPath("/tmp")
	if err != nil || kind != pathStickyShared {
		t.Fatalf("tmp kind = %v err = %v", kind, err)
	}
	if _, err := classifyHostPath("/etc/shadow"); err == nil {
		t.Fatal("expected forbidden /etc")
	}
	if _, err := classifyHostPath("/var/lib/genesis/data/runs"); err == nil {
		t.Fatal("expected forbidden data")
	}
	if _, err := classifyHostPath("/tmp/work"); err == nil {
		t.Fatal("expected forbidden /tmp subdirectory")
	}
	if _, err := classifyHostPath("/opt/agents"); err == nil {
		t.Fatal("expected forbidden /opt")
	}
}

func TestValidateOSUsername(t *testing.T) {
	if err := validateOSUsername("workspace-janitor"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root", "genesis", "Alice", "systemd-network", "", "bad_name"} {
		if err := validateOSUsername(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestApplySharedWorkspaceUsesGroup(t *testing.T) {
	host := newMemoryHost()
	state := &privilegedState{host: host, mutate: true}
	_, err := state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind:      intentEnsureAgentUser,
		Agent:     "janitor",
		User:      "workspace-janitor",
		Home:      "/home/workspace-janitor",
		Cwd:       "/home/workspace-janitor/workspace",
		Shell:     agentShell,
		Groups:    []string{"src"},
		Workspace: workspaceSharedWrite,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cwd := host.dirs["/home/workspace-janitor/workspace"]
	if cwd.mode != 0o775 || cwd.gid != 42 || cwd.mode&0o002 != 0 {
		t.Fatalf("cwd = %#v", cwd)
	}
	home := host.dirs["/home/workspace-janitor"]
	if home.mode != 0o755 || home.mode&0o022 != 0 {
		t.Fatalf("home = %#v", home)
	}
}

func TestApplySharedReadWorkspaceIsWorldReadable(t *testing.T) {
	host := newMemoryHost()
	state := &privilegedState{host: host, mutate: true}
	_, err := state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind:      intentEnsureAgentUser,
		Agent:     "janitor",
		User:      "workspace-janitor",
		Home:      "/home/workspace-janitor",
		Cwd:       "/home/workspace-janitor/workspace",
		Shell:     agentShell,
		Groups:    []string{"src"},
		Workspace: workspaceSharedRead,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cwd := host.dirs["/home/workspace-janitor/workspace"]
	if cwd.mode != 0o755 || cwd.gid != 42 || cwd.mode&0o022 != 0 {
		t.Fatalf("shared-read workspace = %#v", cwd)
	}
}

func TestApplyPlanRefusesForeignHomeAndLowUIDCreate(t *testing.T) {
	host := newMemoryHost()
	host.users["workspace-janitor"] = &unixAccount{Name: "workspace-janitor", UID: 2000, GID: 2000, Home: "/var/lib/workspace-janitor"}
	state := &privilegedState{host: host, mutate: true}
	_, err := state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/home/workspace-janitor/workspace", Shell: agentShell,
	}}})
	if err == nil || !strings.Contains(err.Error(), "hijack") {
		t.Fatalf("foreign home error = %v", err)
	}

	host = newMemoryHost()
	host.nextUID = 50
	state = &privilegedState{host: host, mutate: true}
	_, err = state.apply(privilegedPlan{Intents: []privilegedIntent{{
		Kind: intentEnsureAgentUser, Agent: "janitor", User: "workspace-janitor",
		Home: "/home/workspace-janitor", Cwd: "/home/workspace-janitor/workspace", Shell: agentShell,
	}}})
	if err == nil || !strings.Contains(err.Error(), "system user") {
		t.Fatalf("low uid error = %v", err)
	}
}

func TestApplyPlanRulesOnlyDoesNotMarkUsersRetained(t *testing.T) {
	host := newMemoryHost()
	state := &privilegedState{host: host, mutate: true, users: map[string]reconciledIdentity{}}
	intent := privilegedIntent{
		Kind:      intentEnsureAgentUser,
		Agent:     "janitor",
		User:      "workspace-janitor",
		Home:      "/home/workspace-janitor",
		Cwd:       "/home/workspace-janitor/workspace",
		Shell:     agentShell,
		Workspace: workspacePrivate,
	}
	if _, err := state.apply(privilegedPlan{Intents: []privilegedIntent{intent}}); err != nil {
		t.Fatal(err)
	}
	rulesOnly, err := state.apply(privilegedPlan{Intents: []privilegedIntent{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rulesOnly.Retained) != 0 {
		t.Fatalf("rules-only retained = %#v", rulesOnly.Retained)
	}
	if _, ok := state.lookupUser("workspace-janitor"); !ok {
		t.Fatal("rules-only apply forgot the reconciled user")
	}
}

func TestUnixHostEnsureDirRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	host := unixHost{}
	if err := host.EnsureDir(link, os.Getuid(), os.Getgid(), 0o700); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink ensure error = %v", err)
	}
	if err := host.Chown(link, os.Getuid(), os.Getgid()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink chown error = %v", err)
	}
}

func TestUnknownUserDetection(t *testing.T) {
	if !isUnknownUser(user.UnknownUserError("missing")) {
		t.Fatal("UnknownUserError not detected")
	}
	if isUnknownUser(errors.New("permission denied")) {
		t.Fatal("unrelated error treated as unknown user")
	}
}
