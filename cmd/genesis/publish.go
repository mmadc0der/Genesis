package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const publishCommandUsage = "genesis publish --kind <kind> --headline <text> --lede <text> " +
	"[--to <agent>] [--run <gen_ run id>] [--ref <id>]... [--supersedes <pub_ id>] [--body-file <path>]"

// runPublish files one publication through the listener. The agent must be in
// the reporter group. It prints the publication id.
func runPublish(logger *slog.Logger, args []string) {
	if err := dispatchPublish(args, os.Stdout); err != nil {
		exitCLI(logger, "publish", err)
	}
}

type refFlag []string

func (r *refFlag) String() string { return strings.Join(*r, ",") }

func (r *refFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func dispatchPublish(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	kind := flags.String("kind", "", "one of "+strings.Join(publicationKinds, ", "))
	headline := flags.String("headline", "", "one line, at most 120 characters")
	lede := flags.String("lede", "", "one or two sentences, at most 400 characters")
	to := flags.String("to", "", "agent the work is handed to")
	run := flags.String("run", "", "gen_ run id the story is about")
	supersedes := flags.String("supersedes", "", "pub_ id of your own publication this replaces")
	bodyFile := flags.String("body-file", "", "markdown file with the full account")
	var refs refFlag
	flags.Var(&refs, "ref", "gen_ or pub_ id the story cites; repeatable")
	if err := flags.Parse(args); err != nil {
		return usageError{msg: err.Error() + "\n" + publishCommandUsage}
	}
	if flags.NArg() != 0 {
		return usageError{msg: publishCommandUsage}
	}
	source := defaultAgentSource()
	if source == "" {
		return errors.New("GENESIS_AGENT is not set: genesis publish runs inside an agent")
	}
	in := publicationInput{
		Kind:       *kind,
		Headline:   *headline,
		Lede:       *lede,
		To:         *to,
		Run:        *run,
		Refs:       refs,
		Supersedes: *supersedes,
	}
	if strings.TrimSpace(*bodyFile) != "" {
		body, err := readPublicationBody(*bodyFile)
		if err != nil {
			return err
		}
		in.Body = body
	}
	if err := validatePublicationInput(&in); err != nil {
		return usageError{msg: err.Error() + "\n" + publishCommandUsage}
	}
	event, err := publicationEvent(in, source)
	if err != nil {
		return err
	}
	status, body, err := postPublication(event)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("listener returned %d: %s", status, strings.TrimSpace(string(body)))
	}
	var accepted struct {
		Publication struct {
			ID string `json:"id"`
		} `json:"publication"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.Publication.ID == "" {
		return errors.New("listener accepted the publication without an id")
	}
	fmt.Fprintln(out, accepted.Publication.ID)
	return nil
}

func readPublicationBody(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("--body-file must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxPublicationBody+1))
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	if len(payload) > maxPublicationBody {
		return "", fmt.Errorf("--body-file is longer than %d bytes", maxPublicationBody)
	}
	if !utf8.Valid(payload) {
		return "", errors.New("--body-file must be UTF-8")
	}
	return string(payload), nil
}

func publicationEvent(in publicationInput, source string) (cloudEvent, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	id, err := newLifecycleEventID()
	if err != nil {
		return nil, err
	}
	return composeCloudEvent(nil, map[string]any{
		"specversion": cloudEventSpecVersion,
		"type":        publicationSubmittedType,
		"subject":     strings.TrimPrefix(source, "urn:genesis:agent:"),
	}, map[string]any{
		"id":              id,
		"source":          source,
		"time":            time.Now().UTC().Format(time.RFC3339Nano),
		"datacontenttype": "application/json",
	}, data)
}

// postPublication is postCloudEvent that keeps the answer: the listener's
// reason for a refusal is what the agent needs in order to fix the call.
func postPublication(event cloudEvent) (int, []byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return 0, nil, err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(eventRetryDelay)
		}
		req, err := http.NewRequest(http.MethodPost, listenerEventsURL(), bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", cloudEventsJSON)
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			last = errors.New("sync in progress")
			continue
		}
		return resp.StatusCode, body, nil
	}
	if last == nil {
		last = errors.New("listener did not answer")
	}
	return 0, nil, last
}
