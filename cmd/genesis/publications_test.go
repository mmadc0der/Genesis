package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRunID = "gen_0123456789abcdef0123456789abcdef"

func publicationTestServer(t *testing.T) (*eventServer, *generation, string) {
	t.Helper()
	dir := t.TempDir()
	server := &eventServer{store: &runStore{dataDir: dir}}
	current := &generation{agents: map[string]agentDefinition{
		"oracle":   {Setup: &agentSetup{Groups: []string{sharedGroupName, reporterGroupName}}},
		"designer": {Setup: &agentSetup{Groups: []string{sharedGroupName}}},
		"worker":   {},
	}}
	return server, current, dir
}

func publicationEventFor(t *testing.T, source string, data any) cloudEvent {
	t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) json.RawMessage {
		raw, _ := json.Marshal(value)
		return raw
	}
	return cloudEvent{
		"specversion": quote("1.0"),
		"id":          quote("evt-1"),
		"type":        quote(publicationSubmittedType),
		"source":      quote(source),
		"data":        payload,
	}
}

func submit(t *testing.T, server *eventServer, current *generation, source string, data any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.handlePublication(recorder, publicationEventFor(t, source, data), current)
	return recorder
}

func validInput() map[string]any {
	return map[string]any{
		"kind":     "progress",
		"headline": "Baseline reproduced",
		"lede":     "The first loop matched the reference within noise.",
		"run":      testRunID,
	}
}

func TestPublicationAppendsInOrderAndSurvivesRestart(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	first := submit(t, server, current, "urn:genesis:agent:oracle", validInput())
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	second := submit(t, server, current, "urn:genesis:agent:oracle", validInput())
	if second.Code != http.StatusAccepted {
		t.Fatalf("second status = %d: %s", second.Code, second.Body.String())
	}
	restarted := &eventServer{store: &runStore{dataDir: dir}}
	third := submit(t, restarted, current, "urn:genesis:agent:oracle", validInput())
	if third.Code != http.StatusAccepted {
		t.Fatalf("third status = %d: %s", third.Code, third.Body.String())
	}
	items, err := readPublications(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("register has %d records, want 3", len(items))
	}
	for index, item := range items {
		if item.Seq != uint64(index+1) {
			t.Fatalf("record %d has seq %d", index, item.Seq)
		}
		if item.From != "oracle" || !publicationIDPattern.MatchString(item.ID) || item.Time == "" {
			t.Fatalf("record %d was not stamped: %+v", index, item)
		}
	}
	info, err := os.Stat(filepath.Join(dir, publicationsDirName, publicationRegisterName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != transcriptFileMode {
		t.Fatalf("register mode = %v", info.Mode().Perm())
	}
}

func TestPublicationRefusesNonReporters(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	for _, source := range []string{
		"urn:genesis:agent:designer",
		"urn:genesis:agent:worker",
		"urn:genesis:agent:ghost",
		"urn:genesis:operator",
		"https://example.test/anyone",
		"urn:genesis:agent:",
	} {
		recorder := submit(t, server, current, source, validInput())
		if recorder.Code != http.StatusForbidden {
			t.Errorf("source %q status = %d, want 403", source, recorder.Code)
		}
	}
	items, _ := readPublications(dir)
	if len(items) != 0 {
		t.Fatalf("refused submissions were recorded: %+v", items)
	}
}

func TestPublicationValidation(t *testing.T) {
	server, current, _ := publicationTestServer(t)
	long := func(n int) string { return strings.Repeat("x", n) }
	cases := map[string]func(map[string]any){
		"unknown kind":        func(d map[string]any) { d["kind"] = "gossip" },
		"empty headline":      func(d map[string]any) { d["headline"] = "  " },
		"multi-line headline": func(d map[string]any) { d["headline"] = "a\nb" },
		"long headline":       func(d map[string]any) { d["headline"] = long(maxPublicationHeadline + 1) },
		"long lede":           func(d map[string]any) { d["lede"] = long(maxPublicationLede + 1) },
		"long body":           func(d map[string]any) { d["body"] = long(maxPublicationBody + 1) },
		"control in body":     func(d map[string]any) { d["body"] = "a\x00b" },
		"bad run":             func(d map[string]any) { d["run"] = "../etc" },
		"bad ref":             func(d map[string]any) { d["refs"] = []string{"nope"} },
		"too many refs": func(d map[string]any) {
			refs := make([]string, maxPublicationRefs+1)
			for i := range refs {
				refs[i] = testRunID
			}
			d["refs"] = refs
		},
		"unknown field":      func(d map[string]any) { d["colour"] = "red" },
		"unknown agent":      func(d map[string]any) { d["to"] = "ghost" },
		"bad supersedes":     func(d map[string]any) { d["supersedes"] = "pub_x" },
		"missing supersedes": func(d map[string]any) { d["supersedes"] = "pub_" + strings.Repeat("0", 24) },
	}
	for name, mutate := range cases {
		data := validInput()
		mutate(data)
		recorder := submit(t, server, current, "urn:genesis:agent:oracle", data)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		}
	}
	empty := httptest.NewRecorder()
	server.handlePublication(empty, cloudEvent{"source": json.RawMessage(`"urn:genesis:agent:oracle"`)}, current)
	if empty.Code != http.StatusBadRequest {
		t.Errorf("missing data: status = %d, want 400", empty.Code)
	}
}

func TestPublicationSupersedeRules(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	current.agents["scout"] = agentDefinition{Setup: &agentSetup{Groups: []string{reporterGroupName}}}
	var accepted struct {
		Publication struct {
			ID string `json:"id"`
		} `json:"publication"`
	}
	first := submit(t, server, current, "urn:genesis:agent:oracle", validInput())
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	id := accepted.Publication.ID

	foreign := validInput()
	foreign["supersedes"] = id
	if code := submit(t, server, current, "urn:genesis:agent:scout", foreign).Code; code != http.StatusForbidden {
		t.Fatalf("foreign supersede status = %d, want 403", code)
	}
	own := validInput()
	own["headline"] = "Baseline reproduced, corrected"
	own["supersedes"] = id
	if code := submit(t, server, current, "urn:genesis:agent:oracle", own).Code; code != http.StatusAccepted {
		t.Fatalf("own supersede status = %d", code)
	}

	items, _ := readPublications(dir)
	page := pagePublications(items, publicationQuery{Limit: 10})
	if len(page.Publications) != 1 || page.Publications[0].Headline != "Baseline reproduced, corrected" {
		t.Fatalf("default page = %+v", page.Publications)
	}
	all := pagePublications(items, publicationQuery{Limit: 10, IncludeSuperseded: true})
	if len(all.Publications) != 2 || all.Publications[1].SupersededBy == "" {
		t.Fatalf("full page = %+v", all.Publications)
	}
}

func TestPublicationPagingAndTornTail(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	for i := 0; i < 5; i++ {
		data := validInput()
		data["headline"] = "Story " + string(rune('A'+i))
		if code := submit(t, server, current, "urn:genesis:agent:oracle", data).Code; code != http.StatusAccepted {
			t.Fatalf("submit %d status = %d", i, code)
		}
	}
	items, _ := readPublications(dir)
	first := pagePublications(items, publicationQuery{Limit: 2})
	if len(first.Publications) != 2 || first.Publications[0].Headline != "Story E" || first.NextBefore == nil || first.LastSeq != 5 {
		t.Fatalf("first page = %+v", first)
	}
	second := pagePublications(items, publicationQuery{Limit: 2, Before: *first.NextBefore})
	if second.Publications[0].Headline != "Story C" || second.NextBefore == nil {
		t.Fatalf("second page = %+v", second)
	}
	last := pagePublications(items, publicationQuery{Limit: 2, Before: *second.NextBefore})
	if len(last.Publications) != 1 || last.NextBefore != nil {
		t.Fatalf("last page = %+v", last)
	}
	fresh := pagePublications(items, publicationQuery{Limit: 10, After: 3})
	if len(fresh.Publications) != 2 || fresh.Publications[0].Seq != 5 {
		t.Fatalf("after page = %+v", fresh)
	}

	path := filepath.Join(dir, publicationsDirName, publicationRegisterName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"seq":6,"id":"pub_tor`)
	file.Close()
	torn, _ := readPublications(dir)
	if len(torn) != 5 {
		t.Fatalf("torn tail was read as a record: %d", len(torn))
	}
	restarted := &eventServer{store: &runStore{dataDir: dir}}
	if code := submit(t, restarted, current, "urn:genesis:agent:oracle", validInput()).Code; code != http.StatusAccepted {
		t.Fatalf("append after torn tail status = %d", code)
	}
	healed, _ := readPublications(dir)
	if len(healed) != 6 || healed[5].Seq != 6 {
		t.Fatalf("register after torn tail = %d records", len(healed))
	}
}

func TestPublicationRedactsSecrets(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	current.agents["worker"] = agentDefinition{Secrets: []string{"TOKEN"}}
	server.secrets = map[string]string{
		"TOKEN":                 "s3cr3t-value-123456",
		syncTokenEnv:            "sync-token-value-999",
		"GENESIS_LISTENER_USER": "genesis",
		"UNNAMED_VALUE":         "unnamed-value-777",
	}
	data := validInput()
	data["body"] = "the token is s3cr3t-value-123456, sync sync-token-value-999, user genesis, other unnamed-value-777"
	if code := submit(t, server, current, "urn:genesis:agent:oracle", data).Code; code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	payload, err := os.ReadFile(filepath.Join(dir, publicationsDirName, publicationRegisterName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cr3t-value-123456", "sync-token-value-999"} {
		if bytes.Contains(payload, []byte(secret)) {
			t.Fatalf("secret %q reached the register: %s", secret, payload)
		}
	}
	// Values nobody named as secrets are ordinary text. The listener user
	// name is in the process environment and must stay readable.
	for _, plain := range []string{"user genesis", "unnamed-value-777"} {
		if !bytes.Contains(payload, []byte(plain)) {
			t.Fatalf("%q was redacted: %s", plain, payload)
		}
	}
}

func TestControlPublicationsAPI(t *testing.T) {
	server, current, dir := publicationTestServer(t)
	for i := 0; i < 3; i++ {
		if code := submit(t, server, current, "urn:genesis:agent:oracle", validInput()).Code; code != http.StatusAccepted {
			t.Fatalf("submit %d status = %d", i, code)
		}
	}
	control := &controlServer{dataDir: dir}
	get := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		control.handlePublications(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		return recorder
	}
	var page publicationPage
	recorder := get("/api/publications?limit=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Publications) != 2 || page.Publications[0].Seq != 3 || page.NextBefore == nil || *page.NextBefore != 2 {
		t.Fatalf("page = %+v", page)
	}
	if get("/api/publications?before=x").Code != http.StatusBadRequest {
		t.Fatal("bad cursor was accepted")
	}
	one := httptest.NewRecorder()
	control.handlePublication(one, httptest.NewRequest(http.MethodGet, "/api/publications/"+page.Publications[0].ID, nil))
	if one.Code != http.StatusOK {
		t.Fatalf("single status = %d", one.Code)
	}
	missing := httptest.NewRecorder()
	control.handlePublication(missing, httptest.NewRequest(http.MethodGet, "/api/publications/pub_"+strings.Repeat("f", 24), nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.Code)
	}
	invalid := httptest.NewRecorder()
	control.handlePublication(invalid, httptest.NewRequest(http.MethodGet, "/api/publications/not-an-id", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d", invalid.Code)
	}
	empty := &controlServer{dataDir: t.TempDir()}
	none := httptest.NewRecorder()
	empty.handlePublications(none, httptest.NewRequest(http.MethodGet, "/api/publications", nil))
	if none.Code != http.StatusOK || !strings.Contains(none.Body.String(), `"publications":[]`) {
		t.Fatalf("empty register = %d %s", none.Code, none.Body.String())
	}
}

func TestReservedPublicationType(t *testing.T) {
	if !reservedCLIEventType(publicationSubmittedType) {
		t.Fatal("jobs and schedules must not be able to emit publications")
	}
}

func TestPublishCLIRequiresAgentAndValidatesLocally(t *testing.T) {
	t.Setenv("GENESIS_AGENT", "")
	err := dispatchPublish([]string{"--kind=report", "--headline=h", "--lede=l"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GENESIS_AGENT") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("GENESIS_AGENT", "oracle")
	err = dispatchPublish([]string{"--kind=gossip", "--headline=h", "--lede=l"}, io.Discard)
	if _, ok := err.(usageError); !ok {
		t.Fatalf("err = %#v, want usage error", err)
	}
}

func TestPublishCLIPostsEvent(t *testing.T) {
	var got cloudEvent
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"publication":{"id":"pub_0123456789abcdef01234567","seq":1}}`))
	}))
	defer listener.Close()
	t.Setenv("GENESIS_AGENT", "oracle")
	t.Setenv("GENESIS_EVENTS_URL", listener.URL+"/events")
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("# Details\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := dispatchPublish([]string{
		"--kind=breakthrough", "--headline=Found it", "--lede=It works.",
		"--run=" + testRunID, "--ref=" + testRunID, "--body-file=" + body,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "pub_0123456789abcdef01234567" {
		t.Fatalf("output = %q", out.String())
	}
	if typ, _ := got.stringAttribute("type"); typ != publicationSubmittedType {
		t.Fatalf("type = %q", typ)
	}
	if source, _ := got.stringAttribute("source"); source != "urn:genesis:agent:oracle" {
		t.Fatalf("source = %q", source)
	}
	if !strings.Contains(string(got["data"]), `"body":"# Details\n"`) {
		t.Fatalf("data = %s", got["data"])
	}
}
