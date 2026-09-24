package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	webhookPath            = "/webhooks/github"
	githubSignatureHeader  = "X-Hub-Signature-256"
	githubDeliveryHeader   = "X-GitHub-Delivery"
	githubEventHeader      = "X-GitHub-Event"
	githubWebhookSource    = "urn:genesis:github"
	githubEventIssues      = "issues"
	githubEventPullRequest = "pull_request"
	githubEventPush        = "push"
	githubEventCheckRun    = "check_run"
	githubTypeIssues       = "dev.genesis.github.issues"
	githubTypePullRequest  = "dev.genesis.github.pull_request"
	githubTypePush         = "dev.genesis.github.push"
	githubTypeCheckRun     = "dev.genesis.github.check_run"
	webhookSecretName      = "GITHUB_APP_WEBHOOK_SECRET"
	deliveryDirName        = "deliveries"
	// Stay inside the privileged IPC limit after base64 JSON wrapping.
	maxWebhookBody = 512 << 10
)

var (
	errWebhookSignature         = errors.New("webhook signature rejected")
	errWebhookUnavailable       = errors.New("webhook verification is unavailable")
	errWebhookSecretUnavailable = errors.New("webhook secret is unavailable")
	errWebhookBindings          = errors.New("repository bindings are unavailable")
	errWebhookMalformed         = errors.New("webhook payload was rejected")
	errWebhookTooLarge          = errors.New("payload is too large")
	errDeliveryUnrecorded       = errors.New("webhook delivery was not recorded")

	deliveryIDPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	githubActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type webhookVerifier func(context.Context, []byte, string) error

type repositoryBinder func(context.Context, int64) (bool, error)

type webhookVerifyRequest struct {
	Signature string `json:"signature"`
	Body      []byte `json:"body"`
}

type webhookVerifyReply struct {
	OK bool `json:"ok"`
}

type webhookRepositoryRequest struct {
	RepositoryID int64 `json:"repository_id"`
}

type webhookRepositoryReply struct {
	Bound bool `json:"bound"`
}

func (s *eventServer) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}

	signature := r.Header.Get(githubSignatureHeader)
	if !hubSignatureWellFormed(signature) {
		s.log().Error("webhook signature rejected")
		http.Error(w, "signature rejected", http.StatusUnauthorized)
		return
	}
	body, err := readWebhookBody(r.Body)
	if errors.Is(err, errWebhookTooLarge) {
		http.Error(w, "payload is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		s.log().Error("webhook verification failed")
		http.Error(w, "webhook verification failed", http.StatusInternalServerError)
		return
	}
	if s.verifyWebhook == nil {
		s.log().Error("webhook verification is unavailable")
		http.Error(w, "webhook verification is unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.verifyWebhook(r.Context(), body, signature); err != nil {
		if errors.Is(err, errWebhookSignature) {
			s.log().Error("webhook signature rejected")
			http.Error(w, "signature rejected", http.StatusUnauthorized)
			return
		}
		s.log().Error("webhook verification failed")
		http.Error(w, "webhook verification failed", http.StatusInternalServerError)
		return
	}

	deliveryID := r.Header.Get(githubDeliveryHeader)
	if strings.TrimSpace(deliveryID) == "" {
		http.Error(w, "delivery id is required", http.StatusBadRequest)
		return
	}
	if !deliveryIDPattern.MatchString(deliveryID) {
		http.Error(w, "delivery id is invalid", http.StatusBadRequest)
		return
	}
	eventName, ok := canonicalGitHubEvent(r.Header.Get(githubEventHeader))
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	action, repositoryID, actionInvalid, err := parseGitHubDelivery(body)
	if err != nil {
		http.Error(w, "webhook payload was rejected", http.StatusBadRequest)
		return
	}
	if repositoryID <= 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	bound, err := s.deliveryRepositoryBound(r.Context(), repositoryID)
	if err != nil {
		s.log().Error("webhook repository bindings are unavailable")
		http.Error(w, "repository bindings are unavailable", http.StatusInternalServerError)
		return
	}
	if !bound {
		w.WriteHeader(http.StatusOK)
		return
	}
	if actionInvalid || (action != "" && !githubActionPattern.MatchString(action)) {
		http.Error(w, "webhook payload was rejected", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	syncing := s.syncing
	current := s.generation
	s.mu.RUnlock()
	if current == nil {
		http.Error(w, "generation is not loaded", http.StatusInternalServerError)
		return
	}
	if syncing {
		w.Header().Set("Retry-After", syncRetryAfter)
		http.Error(w, "sync in progress", http.StatusServiceUnavailable)
		return
	}
	if s.store == nil || strings.TrimSpace(s.store.dataDir) == "" {
		http.Error(w, "delivery storage is unavailable", http.StatusInternalServerError)
		return
	}

	event, err := githubCloudEvent(deliveryID, eventName, action, repositoryID)
	if err != nil {
		s.log().Error("webhook event was not built")
		http.Error(w, "webhook payload was rejected", http.StatusInternalServerError)
		return
	}
	replay, err := claimDelivery(s.store.dataDir, deliveryID, eventName)
	if err != nil {
		s.log().Error("webhook delivery was not recorded")
		http.Error(w, "delivery was not recorded", http.StatusInternalServerError)
		return
	}
	if replay {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.dispatchCloudEvent(w, event)
}

func (s *eventServer) deliveryRepositoryBound(ctx context.Context, repositoryID int64) (bool, error) {
	if s.repositoryBound != nil {
		return s.repositoryBound(ctx, repositoryID)
	}
	if s.store == nil {
		return false, errWebhookBindings
	}
	return repositoryIDBound(s.store.dataDir, repositoryID)
}

func readWebhookBody(body io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, maxWebhookBody+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxWebhookBody {
		return nil, errWebhookTooLarge
	}
	return payload, nil
}

func hubSignatureWellFormed(header string) bool {
	const prefix = "sha256="
	if len(header) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(header, prefix) {
		return false
	}
	for i := len(prefix); i < len(header); i++ {
		c := header[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func verifyHubSignature256(secret, body []byte, header string) bool {
	if len(secret) == 0 || !hubSignatureWellFormed(header) {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	decoded := make([]byte, sha256.Size)
	if _, err := hex.Decode(decoded, []byte(header[len("sha256="):])); err != nil {
		return false
	}
	return hmac.Equal(decoded, expected)
}

func canonicalGitHubEvent(header string) (string, bool) {
	switch header {
	case githubEventIssues, githubEventPullRequest, githubEventPush, githubEventCheckRun:
		return header, true
	default:
		return "", false
	}
}

func parseGitHubDelivery(body []byte) (action string, repositoryID int64, actionInvalid bool, err error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var payload struct {
		Action     json.RawMessage `json:"action"`
		Repository *struct {
			ID json.RawMessage `json:"id"`
		} `json:"repository"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return "", 0, false, errWebhookMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", 0, false, errWebhookMalformed
	}
	if len(payload.Action) != 0 && string(payload.Action) != "null" {
		if err := json.Unmarshal(payload.Action, &action); err != nil {
			actionInvalid = true
		}
	}
	if payload.Repository == nil || len(payload.Repository.ID) == 0 || string(payload.Repository.ID) == "null" {
		return action, 0, actionInvalid, nil
	}
	if err := json.Unmarshal(payload.Repository.ID, &repositoryID); err != nil {
		return "", 0, false, errWebhookMalformed
	}
	return action, repositoryID, actionInvalid, nil
}

func githubCloudEvent(deliveryID, eventName, action string, repositoryID int64) (cloudEvent, error) {
	cloudType, subject, ok := githubCloudNames(eventName)
	if !ok || deliveryID == "" || repositoryID <= 0 {
		return nil, errWebhookMalformed
	}
	fields := map[string]any{
		"specversion":     cloudEventSpecVersion,
		"id":              deliveryID,
		"source":          githubWebhookSource,
		"type":            cloudType,
		"subject":         subject,
		"event":           eventName,
		"repositoryid":    strconv.FormatInt(repositoryID, 10),
		"datacontenttype": "application/json",
	}
	data := map[string]any{
		"event":         eventName,
		"repository_id": repositoryID,
	}
	if action != "" {
		fields["action"] = action
		data["action"] = action
	}
	fields["data"] = data
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var event cloudEvent
	if err := json.Unmarshal(encoded, &event); err != nil {
		return nil, err
	}
	return event, nil
}

func githubCloudNames(eventName string) (cloudType, subject string, ok bool) {
	switch eventName {
	case githubEventIssues:
		return githubTypeIssues, githubEventIssues, true
	case githubEventPullRequest:
		return githubTypePullRequest, githubEventPullRequest, true
	case githubEventPush:
		return githubTypePush, githubEventPush, true
	case githubEventCheckRun:
		return githubTypeCheckRun, githubEventCheckRun, true
	default:
		return "", "", false
	}
}

func repositoryIDBound(dataDir string, repositoryID int64) (bool, error) {
	if repositoryID <= 0 {
		return false, nil
	}
	journal, err := readRepositoryJournal(dataDir)
	if err != nil {
		return false, err
	}
	for _, binding := range journal.Bindings {
		if binding.RepositoryID == repositoryID {
			return true, nil
		}
	}
	return false, nil
}

func claimDelivery(dataDir, id, eventName string) (replay bool, err error) {
	if strings.TrimSpace(dataDir) == "" || !deliveryIDPattern.MatchString(id) {
		return false, errDeliveryUnrecorded
	}
	switch eventName {
	case githubEventIssues, githubEventPullRequest, githubEventPush, githubEventCheckRun:
	default:
		return false, errDeliveryUnrecorded
	}
	parent := filepath.Join(dataDir, deliveryDirName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errDeliveryUnrecorded
	}
	dirfd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(dirfd)
	if err := unix.Fchmod(dirfd, 0o700); err != nil {
		return false, err
	}
	var dirStat unix.Stat_t
	if err := unix.Fstat(dirfd, &dirStat); err != nil {
		return false, err
	}
	if dirStat.Mode&unix.S_IFMT != unix.S_IFDIR || os.FileMode(dirStat.Mode).Perm()&0o077 != 0 {
		return false, errDeliveryUnrecorded
	}
	fd, err := unix.Openat(dirfd, id, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, os.ErrExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), "delivery")
	if err := unix.Fchmod(int(file.Fd()), 0o600); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(dirfd, id, 0)
		return false, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil || os.FileMode(st.Mode).Perm()&0o077 != 0 {
		_ = file.Close()
		_ = unix.Unlinkat(dirfd, id, 0)
		if err == nil {
			err = errDeliveryUnrecorded
		}
		return false, err
	}
	payload := []byte(eventName + "\n")
	_, writeErr := file.Write(payload)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = unix.Unlinkat(dirfd, id, 0)
		if writeErr != nil {
			return false, writeErr
		}
		if syncErr != nil {
			return false, syncErr
		}
		return false, closeErr
	}
	if err := unix.Fsync(dirfd); err != nil {
		_ = unix.Unlinkat(dirfd, id, 0)
		return false, err
	}
	return false, nil
}

func (s *privilegedState) webhookSignatureOK(body []byte, signature string) bool {
	if s == nil || s.secrets == nil {
		if s != nil && s.logger != nil {
			s.logger.Error("webhook secret is unavailable")
		}
		return false
	}
	secret, err := s.secrets.readWebhookSecret()
	if err != nil {
		if s.logger != nil {
			s.logger.Error("webhook secret is unavailable")
		}
		return false
	}
	defer zeroBytes(secret)
	return verifyHubSignature256(secret, body, signature)
}

func (c *ipcCoordinator) VerifyWebhook(ctx context.Context, body []byte, signature string) error {
	if c == nil || len(body) > maxWebhookBody {
		return errWebhookUnavailable
	}
	payload, err := json.Marshal(webhookVerifyRequest{Signature: signature, Body: body})
	if err != nil || len(payload) > maxIPCBytes-256 {
		return errWebhookUnavailable
	}
	reply, err := c.roundTrip(ctx, ipcOpVerifyWebhook, payload)
	if err != nil {
		return errWebhookUnavailable
	}
	if len(reply.Payload) > 64 {
		return errWebhookUnavailable
	}
	var result webhookVerifyReply
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return errWebhookUnavailable
	}
	if !result.OK {
		return errWebhookSignature
	}
	return nil
}

func (c *ipcCoordinator) RepositoryBound(ctx context.Context, repositoryID int64) (bool, error) {
	if c == nil {
		return false, errWebhookBindings
	}
	if repositoryID <= 0 {
		return false, nil
	}
	payload, err := json.Marshal(webhookRepositoryRequest{RepositoryID: repositoryID})
	if err != nil {
		return false, errWebhookBindings
	}
	reply, err := c.roundTrip(ctx, ipcOpWebhookRepository, payload)
	if err != nil {
		return false, errWebhookBindings
	}
	if len(reply.Payload) > 64 {
		return false, errWebhookBindings
	}
	var result webhookRepositoryReply
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return false, errWebhookBindings
	}
	return result.Bound, nil
}

func handleVerifyWebhook(state *privilegedState, payload json.RawMessage) (json.RawMessage, error) {
	var request webhookVerifyRequest
	if err := decodeExactJSON(payload, &request); err != nil {
		return nil, errWebhookUnavailable
	}
	defer zeroBytes(request.Body)
	if len(request.Body) > maxWebhookBody {
		return nil, errWebhookUnavailable
	}
	ok := state != nil && state.webhookSignatureOK(request.Body, request.Signature)
	return json.Marshal(webhookVerifyReply{OK: ok})
}

func handleWebhookRepository(state *privilegedState, payload json.RawMessage) (json.RawMessage, error) {
	var request webhookRepositoryRequest
	if err := decodeExactJSON(payload, &request); err != nil {
		return nil, errWebhookBindings
	}
	if state == nil || strings.TrimSpace(state.dataDir) == "" {
		return nil, errWebhookBindings
	}
	bound, err := repositoryIDBound(state.dataDir, request.RepositoryID)
	if err != nil {
		return nil, errWebhookBindings
	}
	return json.Marshal(webhookRepositoryReply{Bound: bound})
}

func zeroBytes(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}
