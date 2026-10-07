package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// A publication is one short story an agent in the reporter group files about
// the loop: a kind, a headline, a lede, and an optional body. The listener
// validates it, stamps it, and appends it to the register. The register is
// append-only. A correction is a new publication that supersedes an old one.
const (
	publicationSubmittedType = "dev.genesis.publication.submitted"
	reporterGroupName        = "reporter"

	publicationsDirName     = "publications"
	publicationRegisterName = "register.jsonl"

	maxPublicationHeadline = 120
	maxPublicationLede     = 400
	maxPublicationBody     = 16 << 10
	maxPublicationRefs     = 8
	// maxPublicationLine bounds one register line when it is read back.
	maxPublicationLine = 256 << 10
)

// publicationKinds is the closed set of kinds. Prompts quote this list.
var publicationKinds = []string{"report", "progress", "breakthrough", "impasse"}

var (
	publicationIDPattern = regexp.MustCompile(`^pub_[0-9a-f]{24}$`)
	publicationRunRegexp = regexp.MustCompile(`^gen_[0-9a-f]{32}$`)
)

type publication struct {
	Seq        uint64   `json:"seq"`
	ID         string   `json:"id"`
	Time       string   `json:"time"`
	Kind       string   `json:"kind"`
	Headline   string   `json:"headline"`
	Lede       string   `json:"lede"`
	Body       string   `json:"body,omitempty"`
	From       string   `json:"from"`
	To         string   `json:"to,omitempty"`
	Run        string   `json:"run,omitempty"`
	Refs       []string `json:"refs,omitempty"`
	Supersedes string   `json:"supersedes,omitempty"`
}

// publicationView is a publication as control serves it.
type publicationView struct {
	publication
	SupersededBy string `json:"superseded_by,omitempty"`
}

// publicationInput is the data an agent supplies. Unknown fields are refused
// so a typo comes back as an error instead of a silently dropped field.
type publicationInput struct {
	Kind       string   `json:"kind"`
	Headline   string   `json:"headline"`
	Lede       string   `json:"lede"`
	Body       string   `json:"body,omitempty"`
	To         string   `json:"to,omitempty"`
	Run        string   `json:"run,omitempty"`
	Refs       []string `json:"refs,omitempty"`
	Supersedes string   `json:"supersedes,omitempty"`
}

// validatePublicationInput checks everything that does not need the listener:
// the same rules run in `genesis publish` and again in the listener.
func validatePublicationInput(in *publicationInput) error {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Headline = strings.TrimSpace(in.Headline)
	in.Lede = strings.TrimSpace(in.Lede)
	in.To = strings.TrimSpace(in.To)
	in.Run = strings.TrimSpace(in.Run)
	in.Supersedes = strings.TrimSpace(in.Supersedes)
	if !slices.Contains(publicationKinds, in.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(publicationKinds, ", "))
	}
	if err := validatePublicationLine("headline", in.Headline, maxPublicationHeadline); err != nil {
		return err
	}
	if err := validatePublicationLine("lede", in.Lede, maxPublicationLede); err != nil {
		return err
	}
	if len(in.Body) > maxPublicationBody {
		return fmt.Errorf("body is longer than %d bytes", maxPublicationBody)
	}
	if !utf8.ValidString(in.Body) {
		return errors.New("body must be UTF-8")
	}
	for _, r := range in.Body {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return errors.New("body contains a control character")
		}
	}
	if in.To != "" {
		if err := validateCLIID(in.To); err != nil {
			return fmt.Errorf("to: %w", err)
		}
	}
	if in.Run != "" && !publicationRunRegexp.MatchString(in.Run) {
		return errors.New("run must be a gen_ run id")
	}
	if len(in.Refs) > maxPublicationRefs {
		return fmt.Errorf("refs lists more than %d ids", maxPublicationRefs)
	}
	for _, ref := range in.Refs {
		if !publicationRunRegexp.MatchString(ref) && !publicationIDPattern.MatchString(ref) {
			return fmt.Errorf("refs entry %q is not a gen_ run id or a pub_ publication id", ref)
		}
	}
	if in.Supersedes != "" && !publicationIDPattern.MatchString(in.Supersedes) {
		return errors.New("supersedes must be a pub_ publication id")
	}
	return nil
}

func validatePublicationLine(name, value string, max int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be UTF-8", name)
	}
	if utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%s is longer than %d characters", name, max)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must be one line of text", name)
		}
	}
	return nil
}

func parsePublicationInput(event cloudEvent) (publicationInput, error) {
	var in publicationInput
	raw, ok := event["data"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return in, errors.New("data is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return in, fmt.Errorf("data: %w", err)
	}
	if decoder.More() {
		return in, errors.New("data must be one JSON object")
	}
	if err := validatePublicationInput(&in); err != nil {
		return in, err
	}
	return in, nil
}

func newPublicationID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "pub_" + hex.EncodeToString(random[:]), nil
}

// publicationRegister is the listener's append-only register. Control reads
// the same file read-only through readPublications.
type publicationRegister struct {
	mu       sync.Mutex
	dir      string
	path     string
	next     uint64
	prepared bool
	redactor *redactor
	now      func() time.Time
}

func newPublicationRegister(dataDir string, redactor *redactor) *publicationRegister {
	dir := filepath.Join(dataDir, publicationsDirName)
	return &publicationRegister{
		dir:      dir,
		path:     filepath.Join(dir, publicationRegisterName),
		redactor: redactor,
		now:      time.Now,
	}
}

// prepare creates the directory, drops a torn tail left by a crash, and
// learns the next sequence number. The caller holds r.mu.
func (r *publicationRegister) prepare() error {
	if r.prepared {
		return nil
	}
	if info, err := os.Lstat(r.dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a real directory", publicationsDirName)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// The register is readable by the control process, which runs as another
	// uid, and by agents. Only the listener can write it.
	if err := os.MkdirAll(r.dir, transcriptsDirMode); err != nil {
		return err
	}
	if err := chmodOwned(r.dir, transcriptsDirMode); err != nil {
		return err
	}
	if err := dropTornTail(r.path); err != nil {
		return err
	}
	existing, err := readPublicationFile(r.path)
	if err != nil {
		return err
	}
	r.next = 1
	if len(existing) > 0 {
		r.next = existing[len(existing)-1].Seq + 1
	}
	r.prepared = true
	return nil
}

// Append stamps and writes one publication. supersedes is checked against the
// register so a typo or another agent's story cannot be replaced.
func (r *publicationRegister) Append(in publicationInput, from string) (publication, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.prepare(); err != nil {
		return publication{}, err
	}
	if in.Supersedes != "" {
		existing, err := readPublicationFile(r.path)
		if err != nil {
			return publication{}, err
		}
		found := false
		for _, item := range existing {
			if item.ID != in.Supersedes {
				continue
			}
			found = true
			if item.From != from {
				return publication{}, &publicationError{status: http.StatusForbidden, msg: "supersedes names a publication another agent filed"}
			}
		}
		if !found {
			return publication{}, &publicationError{status: http.StatusBadRequest, msg: "supersedes names no publication"}
		}
	}
	id, err := newPublicationID()
	if err != nil {
		return publication{}, err
	}
	item := publication{
		Seq:        r.next,
		ID:         id,
		Time:       r.now().UTC().Format(time.RFC3339Nano),
		Kind:       in.Kind,
		Headline:   in.Headline,
		Lede:       in.Lede,
		Body:       in.Body,
		From:       from,
		To:         in.To,
		Run:        in.Run,
		Refs:       in.Refs,
		Supersedes: in.Supersedes,
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return publication{}, err
	}
	payload = r.redactor.bytes(payload)
	var stored publication
	if err := json.Unmarshal(payload, &stored); err != nil {
		return publication{}, fmt.Errorf("redacted publication is not JSON: %w", err)
	}
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, transcriptFileMode)
	if err != nil {
		return publication{}, err
	}
	if err := file.Chmod(transcriptFileMode); err != nil {
		file.Close()
		return publication{}, err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		file.Close()
		return publication{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return publication{}, err
	}
	if err := file.Close(); err != nil {
		return publication{}, err
	}
	r.next++
	return stored, nil
}

type publicationError struct {
	status int
	msg    string
}

func (e *publicationError) Error() string { return e.msg }

// readPublicationFile returns the complete records in seq order. A missing
// file is empty. A torn or invalid line ends the read, as with the journals.
func readPublicationFile(path string) ([]publication, error) {
	payload, err := readNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []publication
	for len(payload) > 0 {
		line := payload
		rest := []byte(nil)
		if idx := bytes.IndexByte(payload, '\n'); idx >= 0 {
			line, rest = payload[:idx], payload[idx+1:]
		} else {
			// No newline: the last write did not finish.
			break
		}
		payload = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > maxPublicationLine {
			break
		}
		var item publication
		if err := json.Unmarshal(line, &item); err != nil {
			break
		}
		items = append(items, item)
	}
	return items, nil
}

// readPublications is the control-side read of <data>/publications.
func readPublications(dataDir string) ([]publication, error) {
	return readPublicationFile(filepath.Join(dataDir, publicationsDirName, publicationRegisterName))
}

type publicationQuery struct {
	Before            uint64
	After             uint64
	Limit             int
	IncludeSuperseded bool
}

type publicationPage struct {
	Publications []publicationView `json:"publications"`
	// NextBefore is the cursor for the next older page. It is omitted on the
	// last page and when After is used.
	NextBefore *uint64 `json:"next_before,omitempty"`
	// LastSeq is the newest sequence in the register, for polling with after.
	LastSeq uint64 `json:"last_seq"`
}

// supersededBy maps a publication id to the newest publication that names it.
func supersededBy(items []publication) map[string]string {
	out := map[string]string{}
	for _, item := range items {
		if item.Supersedes != "" {
			out[item.Supersedes] = item.ID
		}
	}
	return out
}

// pagePublications returns newest-first. Superseded stories are hidden unless
// asked for. Cursors count sequence numbers, so hidden ones never repeat.
func pagePublications(items []publication, query publicationQuery) publicationPage {
	replaced := supersededBy(items)
	page := publicationPage{Publications: []publicationView{}}
	if len(items) > 0 {
		page.LastSeq = items[len(items)-1].Seq
	}
	visible := make([]publicationView, 0, len(items))
	for _, item := range items {
		view := publicationView{publication: item, SupersededBy: replaced[item.ID]}
		if view.SupersededBy != "" && !query.IncludeSuperseded {
			continue
		}
		if query.After > 0 && item.Seq <= query.After {
			continue
		}
		if query.Before > 0 && item.Seq >= query.Before {
			continue
		}
		visible = append(visible, view)
	}
	sort.SliceStable(visible, func(i, j int) bool { return visible[i].Seq > visible[j].Seq })
	limit := query.Limit
	if limit < 1 {
		limit = 1
	}
	if len(visible) > limit {
		visible = visible[:limit]
		if query.After == 0 {
			next := visible[len(visible)-1].Seq
			page.NextBefore = &next
		}
	}
	page.Publications = append(page.Publications, visible...)
	return page
}

// handlePublication records one dev.genesis.publication.submitted event.
// The event does not start runs. The agent named in source must be a reporter
// in the active generation. source is not authenticated on the HTTP path, so
// this is a policy check, not a capability boundary.
func (s *eventServer) handlePublication(w http.ResponseWriter, event cloudEvent, current *generation) {
	source, _ := event.stringAttribute("source")
	from, ok := strings.CutPrefix(source, "urn:genesis:agent:")
	if !ok || from == "" || strings.Contains(from, "/") {
		http.Error(w, "source must be urn:genesis:agent:<agent id>", http.StatusForbidden)
		return
	}
	definition, known := current.agents[from]
	if !known {
		http.Error(w, fmt.Sprintf("agent %s is not in the active generation", from), http.StatusForbidden)
		return
	}
	if definition.Setup == nil || !slices.Contains(definition.Setup.Groups, reporterGroupName) {
		http.Error(w, fmt.Sprintf("agent %s is not a reporter: add %s to setup.groups", from, reporterGroupName), http.StatusForbidden)
		return
	}
	in, err := parsePublicationInput(event)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.To != "" {
		if _, exists := current.agents[in.To]; !exists {
			http.Error(w, fmt.Sprintf("to: agent %s is not in the active generation", in.To), http.StatusBadRequest)
			return
		}
	}
	register := s.publications()
	if register == nil {
		http.Error(w, "publication register is unavailable", http.StatusInternalServerError)
		return
	}
	stored, err := register.Append(in, from)
	if err != nil {
		var refused *publicationError
		if errors.As(err, &refused) {
			http.Error(w, refused.msg, refused.status)
			return
		}
		s.log().Error("append publication", "agent", from, "error", err)
		http.Error(w, "failed to record publication", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"publication": map[string]any{"id": stored.ID, "seq": stored.Seq},
	})
}

// publications opens the register on first use.
func (s *eventServer) publications() *publicationRegister {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.pubs == nil {
		if s.store == nil || s.store.dataDir == "" {
			return nil
		}
		values := make([]string, 0, len(s.secrets))
		for _, value := range s.secrets {
			values = append(values, value)
		}
		s.pubs = newPublicationRegister(s.store.dataDir, newRedactor(values))
	}
	return s.pubs
}
