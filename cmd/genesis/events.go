package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const eventsCommandUsage = "Usage: genesis events [options] [pattern]\n\n" +
	"Search, tail, and follow Genesis CloudEvents.\n\n" +
	"Options:\n" +
	"  -n, -tail <N>        Number of recent events to show (default 20, 0 for all)\n" +
	"  -f, -follow          Follow live events as they arrive (like tail -f)\n" +
	"  -e, -grep <pattern>  Filter events matching regex or text pattern\n" +
	"  -i, -ignore-case     Case-insensitive pattern match\n" +
	"  --type <type>        Filter by CloudEvent type (e.g. dev.genesis.agent.finished)\n" +
	"  --agent <agent>      Filter by agent ID (e.g. cpu-bench)\n" +
	"  --run <run_id>       Filter by run ID (e.g. gen_xxx)\n" +
	"  --since <time>       Show events since duration (e.g. 10m, 1h, 1d) or timestamp\n" +
	"  --format <format>    Output format: json (default), short, pretty\n" +
	"  --chunks             Include ephemeral token chunks (excluded by default)\n" +
	"  --url <url>          Listener events URL (default: $GENESIS_EVENTS_URL or http://127.0.0.1:8787/events)\n" +
	"  --data <dir>         Read directly from local data directory instead of HTTP"

func runEvents(logger *slog.Logger, args []string) {
	if err := dispatchEvents(args, os.Stdout, os.Stderr); err != nil {
		exitCLI(logger, "events", err)
	}
}

func dispatchEvents(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var (
		tail       int
		follow     bool
		grep       string
		ignoreCase bool
		eventType  string
		agent      string
		run        string
		since      string
		format     string
		eventsURL  string
		dataDir    string
		chunks     bool
	)

	flags.IntVar(&tail, "tail", 20, "Number of recent events to show")
	flags.IntVar(&tail, "n", 20, "Number of recent events to show")
	flags.BoolVar(&follow, "follow", false, "Follow live events")
	flags.BoolVar(&follow, "f", false, "Follow live events")
	flags.StringVar(&grep, "grep", "", "Filter events matching pattern")
	flags.StringVar(&grep, "e", "", "Filter events matching pattern")
	flags.BoolVar(&ignoreCase, "ignore-case", false, "Case-insensitive pattern match")
	flags.BoolVar(&ignoreCase, "i", false, "Case-insensitive pattern match")
	flags.StringVar(&eventType, "type", "", "Filter by event type")
	flags.StringVar(&agent, "agent", "", "Filter by agent ID")
	flags.StringVar(&run, "run", "", "Filter by run ID")
	flags.StringVar(&since, "since", "", "Filter events since duration or timestamp")
	flags.StringVar(&format, "format", "json", "Output format: json, short, pretty")
	flags.StringVar(&eventsURL, "url", "", "Listener events URL")
	flags.StringVar(&dataDir, "data", "", "Local data directory")
	flags.BoolVar(&chunks, "chunks", false, "Include ephemeral token chunks")

	if err := flags.Parse(args); err != nil {
		return usageError{msg: err.Error() + "\n" + eventsCommandUsage}
	}

	if grep == "" && flags.NArg() > 0 {
		grep = flags.Arg(0)
	}

	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "json", "short", "pretty":
	case "ndjson", "jsonl":
		format = "json"
	default:
		return usageError{msg: "invalid --format: must be json, short, or pretty\n" + eventsCommandUsage}
	}

	// 1. Direct offline mode if dataDir is provided
	if strings.TrimSpace(dataDir) != "" {
		if follow {
			return errors.New("--follow is not supported when reading directly from --data without a server")
		}
		filter, err := buildEventFilter(run, agent, eventType, grep, ignoreCase, since, tail, chunks, time.Now())
		if err != nil {
			return err
		}
		store := newRunStore(dataDir, nil, nil)
		events, err := store.findEvents(filter)
		if err != nil {
			return err
		}
		return printEvents(events, format, stdout)
	}

	// 2. HTTP mode querying the listener
	targetURL := strings.TrimSpace(eventsURL)
	if targetURL == "" {
		targetURL = listenerEventsURL()
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid events URL %q: %w", targetURL, err)
	}

	q := u.Query()
	q.Set("tail", strconv.Itoa(tail))
	q.Set("format", "ndjson")
	if follow {
		q.Set("follow", "true")
	}
	if grep != "" {
		q.Set("grep", grep)
	}
	if ignoreCase {
		q.Set("ignore_case", "true")
	}
	if eventType != "" {
		q.Set("type", eventType)
	}
	if agent != "" {
		q.Set("agent", agent)
	}
	if run != "" {
		q.Set("run", run)
	}
	if since != "" {
		q.Set("since", since)
	}
	if chunks {
		q.Set("chunks", "true")
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signalNotify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer signalStop(sigChan)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-ndjson")

	client := &http.Client{
		Timeout: 0,
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return fmt.Errorf("failed to reach listener at %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("listener returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if format == "json" {
			_, _ = fmt.Fprintln(stdout, string(line))
		} else {
			var ev lifecycleEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				_, _ = fmt.Fprintln(stdout, string(line))
				continue
			}
			if format == "short" {
				_, _ = fmt.Fprintln(stdout, formatEventShort(ev))
			} else if format == "pretty" {
				p, err := json.MarshalIndent(ev, "", "  ")
				if err != nil {
					_, _ = fmt.Fprintln(stdout, string(line))
				} else {
					_, _ = fmt.Fprintln(stdout, string(p))
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}

func printEvents(events []lifecycleEvent, format string, stdout io.Writer) error {
	for _, ev := range events {
		switch format {
		case "short":
			_, _ = fmt.Fprintln(stdout, formatEventShort(ev))
		case "pretty":
			p, err := json.MarshalIndent(ev, "", "  ")
			if err != nil {
				raw, _ := json.Marshal(ev)
				_, _ = fmt.Fprintln(stdout, string(raw))
			} else {
				_, _ = fmt.Fprintln(stdout, string(p))
			}
		default: // json / jsonl
			raw, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(stdout, string(raw))
		}
	}
	return nil
}

func formatEventShort(ev lifecycleEvent) string {
	var parts []string
	if ev.Time != "" {
		parts = append(parts, ev.Time)
	}
	if ev.Type != "" {
		parts = append(parts, fmt.Sprintf("%-28s", ev.Type))
	}
	if ev.AgentID != "" {
		parts = append(parts, "agent="+ev.AgentID)
	}
	if ev.RunID != "" {
		parts = append(parts, "run="+ev.RunID)
	}
	if ev.Subject != "" && ev.Subject != ev.RunID {
		parts = append(parts, "subject="+ev.Subject)
	}
	if ev.CauseType != "" {
		cause := ev.CauseType
		cause = strings.TrimPrefix(cause, "dev.genesis.")
		parts = append(parts, "cause="+cause)
	}
	if len(ev.Data) > 0 {
		var d struct {
			Message string `json:"message"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Error   string `json:"error_message"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			msg := d.Message
			if msg == "" {
				msg = d.Text
			}
			if msg == "" {
				msg = d.Command
			}
			if msg == "" {
				msg = d.Error
			}
			if msg != "" {
				clean := strings.ReplaceAll(strings.TrimSpace(msg), "\n", " ")
				if len(clean) > 80 {
					clean = clean[:77] + "..."
				}
				parts = append(parts, fmt.Sprintf("%q", clean))
			}
		}
	}
	return strings.Join(parts, "  ")
}
