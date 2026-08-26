package consolerequestlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultDirectory = "/app/data/console-request-logs"
	MaxStorageBytes  = int64(1 << 30)
	PruneCheckBytes  = int64(16 << 20)
)

var storage = struct {
	sync.Mutex
	active map[string]*RequestRecorder
}{active: make(map[string]*RequestRecorder)}

type RequestInfo struct {
	RequestID     string
	EventID       string
	ClientKeyID   uint64
	ClientKeyName string
	ClientIP      string
	RouteID       uint64
	Provider      string
	Operation     string
	Method        string
	Path          string
	UpstreamPath  string
	Headers       http.Header
	Protocol      string
	PublicModel   string
	UpstreamModel string
	StartedAt     time.Time
	ClientBody    []byte
	GuardConfig   any
	IdempotencyID string
}

type AttemptInfo struct {
	AccountID      uint64
	AccountName    string
	AccountEmail   string
	AccountUserID  string
	AccountTeamID  string
	AuthType       string
	EgressNodeID   uint64
	EgressExitIP   string
	EgressIdentity string
	QuotaProbe     bool
	QuotaProbeKind string
	Billing        any
	StartedAt      time.Time
}

type RateLimitInfo struct {
	Scope        string `json:"scope"`
	TeamID       string `json:"teamID"`
	Model        string `json:"model"`
	Actual       int    `json:"actual"`
	Limit        int    `json:"limit"`
	RetryAfterMS int64  `json:"retryAfterMS"`
}

type DiagnosticInfo struct {
	StatusCode    int
	Status        string
	Header        http.Header
	Body          []byte
	BodyTruncated bool
}

type ResponseInfo struct {
	StatusCode              int
	Status                  string
	UpstreamURL             string
	Header                  http.Header
	QuotaUnits              int
	ModelCatalogChanged     bool
	RateLimit               *RateLimitInfo
	Diagnostic              *DiagnosticInfo
	RecoveredPrimaryFailure *DiagnosticInfo
}

type ClearResult struct {
	Directory          string `json:"directory"`
	DeletedRequests    int    `json:"deletedRequests"`
	DeletedBytes       int64  `json:"deletedBytes"`
	ActiveRequests     int    `json:"activeRequests"`
	ActiveDeleteQueued int    `json:"activeDeleteQueued"`
}

type RequestRecorder struct {
	mu              sync.Mutex
	directory       string
	startedAt       time.Time
	events          *os.File
	nextAttempt     int
	openAttempts    int
	closed          bool
	finalMarked     bool
	bytesSincePrune int64
	deleteOnClose   atomic.Bool
	onError         func(error)
}

type AttemptRecorder struct {
	mu             sync.Mutex
	request        *RequestRecorder
	sequence       int
	startedAt      time.Time
	raw            *os.File
	timeline       *os.File
	offset         int64
	chunkSequence  int64
	eventSequence  int64
	pending        []byte
	pendingOffset  int64
	pendingFirstAt time.Time
	closed         bool
}

type recordingReadCloser struct {
	body    io.ReadCloser
	attempt *AttemptRecorder
}

func New(info RequestInfo, onError func(error)) (*RequestRecorder, error) {
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	if strings.TrimSpace(info.RequestID) == "" {
		return nil, errors.New("request log request ID 为空")
	}
	if err := prune(); err != nil {
		return nil, err
	}

	storage.Lock()
	defer storage.Unlock()
	if err := os.MkdirAll(DefaultDirectory, 0o750); err != nil {
		return nil, err
	}
	name := info.StartedAt.UTC().Format("20060102T150405.000000000Z") + "_" + safeName(info.RequestID)
	directory := filepath.Join(DefaultDirectory, name)
	if err := os.Mkdir(directory, 0o750); err != nil {
		return nil, err
	}
	events, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	storedBody := redactBytes(info.ClientBody)
	recorder := &RequestRecorder{directory: directory, startedAt: info.StartedAt, events: events, onError: onError, bytesSincePrune: int64(len(storedBody))}
	storage.active[directory] = recorder

	bodyPath := filepath.Join(directory, "client-request-body.bin")
	if err := os.WriteFile(bodyPath, storedBody, 0o600); err != nil {
		delete(storage.active, directory)
		_ = events.Close()
		_ = os.RemoveAll(directory)
		return nil, err
	}
	metadata := map[string]any{
		"schemaVersion":                1,
		"requestID":                    info.RequestID,
		"eventID":                      info.EventID,
		"clientKeyID":                  info.ClientKeyID,
		"clientKeyName":                info.ClientKeyName,
		"clientIP":                     info.ClientIP,
		"routeID":                      info.RouteID,
		"provider":                     info.Provider,
		"operation":                    info.Operation,
		"method":                       info.Method,
		"path":                         redactURL(info.Path),
		"upstreamPath":                 info.UpstreamPath,
		"requestHeaders":               redactHeaders(info.Headers),
		"protocol":                     info.Protocol,
		"publicModel":                  info.PublicModel,
		"upstreamModel":                info.UpstreamModel,
		"startedAt":                    timestamp(info.StartedAt),
		"startedAtUnixNano":            info.StartedAt.UnixNano(),
		"clientRequestBodyFile":        "client-request-body.bin",
		"clientRequestBodyBytes":       len(info.ClientBody),
		"clientRequestBodySHA256":      digest(info.ClientBody),
		"clientRequestBodyStoredBytes": len(storedBody),
		"clientRequestBodyRedacted":    true,
		"guardConfig":                  info.GuardConfig,
		"idempotencyID":                info.IdempotencyID,
		"storageLimitBytes":            MaxStorageBytes,
	}
	if err := writeJSON(filepath.Join(directory, "request.json"), redactValue(metadata)); err != nil {
		delete(storage.active, directory)
		_ = events.Close()
		_ = os.RemoveAll(directory)
		return nil, err
	}
	recorder.recordLocked("request_started", map[string]any{
		"requestBodyBytes":  len(info.ClientBody),
		"requestBodySHA256": digest(info.ClientBody),
	})
	return recorder, nil
}

func (r *RequestRecorder) Directory() string { return r.directory }

func (r *RequestRecorder) StartAttempt(info AttemptInfo) *AttemptRecorder {
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.nextAttempt++
	sequence := r.nextAttempt
	base := fmt.Sprintf("attempt-%02d", sequence)
	raw, err := os.OpenFile(filepath.Join(r.directory, base+".sse"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.report(err)
		return nil
	}
	timeline, err := os.OpenFile(filepath.Join(r.directory, base+".timeline.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = raw.Close()
		r.report(err)
		return nil
	}
	attempt := &AttemptRecorder{request: r, sequence: sequence, startedAt: info.StartedAt, raw: raw, timeline: timeline}
	r.openAttempts++
	r.recordLocked("attempt_started", map[string]any{
		"networkAttempt":           sequence,
		"accountID":                info.AccountID,
		"accountName":              info.AccountName,
		"accountEmail":             info.AccountEmail,
		"accountUserID":            info.AccountUserID,
		"accountTeamID":            info.AccountTeamID,
		"authType":                 info.AuthType,
		"egressNodeID":             info.EgressNodeID,
		"egressExitIP":             info.EgressExitIP,
		"egressIdentity":           info.EgressIdentity,
		"quotaProbe":               info.QuotaProbe,
		"quotaProbeKind":           info.QuotaProbeKind,
		"billing":                  info.Billing,
		"attemptStartedAt":         timestamp(info.StartedAt),
		"attemptStartedAtUnixNano": info.StartedAt.UnixNano(),
	})
	return attempt
}

func (r *RequestRecorder) Record(kind string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.recordLocked(kind, fields)
}

func (r *RequestRecorder) MarkFinalAttempt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.finalMarked = true
	r.recordLocked("final_attempt_handed_off", nil)
	if r.openAttempts == 0 {
		r.closeLocked("completed_without_open_stream")
	}
}

func (r *RequestRecorder) Close(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked(reason)
}

func (r *RequestRecorder) recordLocked(kind string, fields map[string]any) {
	if r.events == nil {
		return
	}
	now := time.Now()
	entry := map[string]any{
		"kind":             kind,
		"at":               timestamp(now),
		"unixNano":         now.UnixNano(),
		"requestElapsedMS": elapsedMS(r.startedAt, now),
	}
	for key, value := range fields {
		entry[key] = redactValue(value)
	}
	if err := appendJSON(r.events, entry); err != nil {
		r.report(err)
	}
}

func (r *RequestRecorder) closeLocked(reason string) {
	if r.closed {
		return
	}
	r.recordLocked("request_log_closed", map[string]any{"reason": reason, "openAttempts": r.openAttempts})
	r.closed = true
	if r.events != nil {
		if err := r.events.Close(); err != nil {
			r.report(err)
		}
		r.events = nil
	}
	storage.Lock()
	delete(storage.active, r.directory)
	deleteOnClose := r.deleteOnClose.Load()
	storage.Unlock()
	if deleteOnClose {
		if err := os.RemoveAll(r.directory); err != nil {
			r.report(err)
		}
		return
	}
	if err := prune(); err != nil {
		r.report(err)
	}
}

func (r *RequestRecorder) attemptClosed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.openAttempts > 0 {
		r.openAttempts--
	}
	if r.finalMarked && r.openAttempts == 0 {
		r.closeLocked("final_stream_closed")
	}
}

func (r *RequestRecorder) report(err error) {
	if err != nil && r.onError != nil {
		r.onError(err)
	}
}

func (r *RequestRecorder) noteBytes(value int64) {
	if value <= 0 {
		return
	}
	r.mu.Lock()
	r.bytesSincePrune += value
	shouldPrune := r.bytesSincePrune >= PruneCheckBytes
	if shouldPrune {
		r.bytesSincePrune = 0
	}
	r.mu.Unlock()
	if shouldPrune {
		if err := prune(); err != nil {
			r.report(err)
		}
	}
}

func (a *AttemptRecorder) RecordResponse(info ResponseInfo) {
	a.request.Record("upstream_response_headers", map[string]any{
		"networkAttempt":          a.sequence,
		"statusCode":              info.StatusCode,
		"status":                  info.Status,
		"upstreamURL":             redactURL(info.UpstreamURL),
		"headers":                 redactHeaders(info.Header),
		"quotaUnits":              info.QuotaUnits,
		"modelCatalogChanged":     info.ModelCatalogChanged,
		"rateLimit":               info.RateLimit,
		"diagnostic":              diagnosticValue(info.Diagnostic),
		"recoveredPrimaryFailure": diagnosticValue(info.RecoveredPrimaryFailure),
	})
}

func (a *AttemptRecorder) RecordError(err error) {
	if err == nil {
		return
	}
	a.request.Record("upstream_transport_error", map[string]any{
		"networkAttempt": a.sequence,
		"error":          redactText(err.Error()),
	})
}

func (a *AttemptRecorder) RecordDecision(fields map[string]any) {
	fields["networkAttempt"] = a.sequence
	a.request.Record("console_guard_decision", fields)
}

func (a *AttemptRecorder) Wrap(body io.ReadCloser) io.ReadCloser {
	return &recordingReadCloser{body: body, attempt: a}
}

func (a *AttemptRecorder) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	now := time.Now()
	if len(a.pending) > 0 {
		a.eventSequence++
		a.writeTimeline(map[string]any{
			"kind":                "partial_event",
			"eventSequence":       a.eventSequence,
			"firstByteAt":         timestamp(a.pendingFirstAt),
			"completedAt":         timestamp(now),
			"completedAtUnixNano": now.UnixNano(),
			"requestElapsedMS":    elapsedMS(a.request.startedAt, now),
			"attemptElapsedMS":    elapsedMS(a.startedAt, now),
			"offset":              a.pendingOffset,
			"bytes":               len(a.pending),
			"sha256":              digest(a.pending),
		})
	}
	a.request.Record("attempt_stream_closed", map[string]any{
		"networkAttempt":   a.sequence,
		"rawBytes":         a.offset,
		"chunks":           a.chunkSequence,
		"events":           a.eventSequence,
		"attemptElapsedMS": elapsedMS(a.startedAt, now),
	})
	var result error
	if err := a.raw.Close(); err != nil {
		result = err
	}
	if err := a.timeline.Close(); err != nil && result == nil {
		result = err
	}
	a.mu.Unlock()
	if result != nil {
		a.request.report(result)
	}
	a.request.attemptClosed()
	return result
}

func (a *AttemptRecorder) appendChunk(data []byte, now time.Time) {
	if len(data) == 0 {
		return
	}
	stored := redactBytes(data)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	startOffset := a.offset
	if _, err := a.raw.Write(stored); err != nil {
		a.request.report(err)
		return
	}
	a.offset += int64(len(stored))
	a.request.noteBytes(int64(len(stored)))
	a.chunkSequence++
	a.writeTimeline(map[string]any{
		"kind":               "chunk",
		"chunkSequence":      a.chunkSequence,
		"receivedAt":         timestamp(now),
		"receivedAtUnixNano": now.UnixNano(),
		"requestElapsedMS":   elapsedMS(a.request.startedAt, now),
		"attemptElapsedMS":   elapsedMS(a.startedAt, now),
		"offset":             startOffset,
		"receivedBytes":      len(data),
		"bytes":              len(stored),
		"rawSHA256":          digest(data),
		"sha256":             digest(stored),
		"redacted":           !bytes.Equal(data, stored),
	})
	if len(a.pending) == 0 {
		a.pendingOffset = startOffset
		a.pendingFirstAt = now
	}
	a.pending = append(a.pending, stored...)
	a.flushEvents(now)
}

func (a *AttemptRecorder) flushEvents(now time.Time) {
	for {
		index, delimiterLength := eventDelimiter(a.pending)
		if index < 0 {
			return
		}
		length := index + delimiterLength
		rawEvent := a.pending[:length]
		eventField, dataType := eventIdentity(rawEvent)
		a.eventSequence++
		a.writeTimeline(map[string]any{
			"kind":                "event",
			"eventSequence":       a.eventSequence,
			"firstByteAt":         timestamp(a.pendingFirstAt),
			"completedAt":         timestamp(now),
			"completedAtUnixNano": now.UnixNano(),
			"requestElapsedMS":    elapsedMS(a.request.startedAt, now),
			"attemptElapsedMS":    elapsedMS(a.startedAt, now),
			"offset":              a.pendingOffset,
			"bytes":               length,
			"sha256":              digest(rawEvent),
			"eventField":          eventField,
			"dataType":            dataType,
		})
		a.pending = append(a.pending[:0], a.pending[length:]...)
		a.pendingOffset += int64(length)
		if len(a.pending) > 0 {
			a.pendingFirstAt = now
		}
	}
}

func (a *AttemptRecorder) writeTimeline(value map[string]any) {
	if err := appendJSON(a.timeline, value); err != nil {
		a.request.report(err)
	}
}

func diagnosticValue(value *DiagnosticInfo) any {
	if value == nil {
		return nil
	}
	redacted := redactBytes(value.Body)
	result := map[string]any{
		"statusCode":      value.StatusCode,
		"status":          value.Status,
		"headers":         redactHeaders(value.Header),
		"bodyBytes":       len(value.Body),
		"storedBodyBytes": len(redacted),
		"bodySHA256":      digest(value.Body),
		"bodyBase64":      base64.StdEncoding.EncodeToString(redacted),
		"bodyTruncated":   value.BodyTruncated,
		"bodyWasRedacted": !bytes.Equal(value.Body, redacted),
	}
	if utf8.Valid(redacted) {
		result["bodyText"] = string(redacted)
	}
	return result
}

func (r *recordingReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.body.Read(buffer)
	now := time.Now()
	if n > 0 {
		r.attempt.appendChunk(buffer[:n], now)
	}
	if err != nil {
		fields := map[string]any{
			"networkAttempt":   r.attempt.sequence,
			"at":               timestamp(now),
			"attemptElapsedMS": elapsedMS(r.attempt.startedAt, now),
			"error":            redactText(err.Error()),
		}
		if errors.Is(err, io.EOF) {
			fields["eof"] = true
		}
		r.attempt.request.Record("upstream_body_read_result", fields)
	}
	return n, err
}

func (r *recordingReadCloser) Close() error {
	bodyErr := r.body.Close()
	logErr := r.attempt.Close()
	if bodyErr != nil {
		return bodyErr
	}
	return logErr
}

func Clear() (ClearResult, error) {
	result := ClearResult{Directory: DefaultDirectory}
	storage.Lock()
	defer storage.Unlock()
	if err := os.MkdirAll(DefaultDirectory, 0o750); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(DefaultDirectory)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		path := filepath.Join(DefaultDirectory, entry.Name())
		if active := storage.active[path]; active != nil {
			active.deleteOnClose.Store(true)
			result.ActiveRequests++
			result.ActiveDeleteQueued++
			continue
		}
		size, sizeErr := pathSize(path)
		if sizeErr != nil {
			return result, sizeErr
		}
		if err := os.RemoveAll(path); err != nil {
			return result, err
		}
		result.DeletedRequests++
		result.DeletedBytes += size
	}
	return result, nil
}

func prune() error {
	storage.Lock()
	defer storage.Unlock()
	entries, err := os.ReadDir(DefaultDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type storedRequest struct {
		path string
		name string
		size int64
	}
	requests := make([]storedRequest, 0, len(entries))
	var total int64
	for _, entry := range entries {
		path := filepath.Join(DefaultDirectory, entry.Name())
		size, sizeErr := pathSize(path)
		if sizeErr != nil {
			return sizeErr
		}
		total += size
		if storage.active[path] == nil {
			requests = append(requests, storedRequest{path: path, name: entry.Name(), size: size})
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].name < requests[j].name })
	for _, request := range requests {
		if total <= MaxStorageBytes {
			break
		}
		if err := os.RemoveAll(request.path); err != nil {
			return err
		}
		total -= request.size
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

var (
	bearerPattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	basicPattern            = regexp.MustCompile(`(?i)\bBasic\s+[A-Za-z0-9+/=]+`)
	urlCredentialPattern    = regexp.MustCompile(`(?i)(://)([^/\s:@]+):([^@\s/]+)@`)
	urlQuerySecretPattern   = regexp.MustCompile(`(?i)([?&](?:key|api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|password|secret)=)([^&#\s]+)`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|cookie|set-cookie|api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|password|credential|secret|auth[-_]?token|token[-_]?value)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}&]+)`)
)

func redactBytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(bytes.TrimSpace(data), &value); err == nil {
		scrubJSON(value)
		if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
			return encoded
		}
	}
	return []byte(redactText(string(data)))
}

func redactValue(value any) any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return redactText(fmt.Sprint(value))
	}
	redacted := redactBytes(data)
	var result any
	if err := json.Unmarshal(redacted, &result); err == nil {
		return result
	}
	return string(redacted)
}

func scrubJSON(value any) any {
	switch typed := value.(type) {
	case string:
		return redactText(typed)
	case map[string]any:
		for key, nested := range typed {
			if sensitiveField(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			typed[key] = scrubJSON(nested)
		}
	case []any:
		for index, nested := range typed {
			typed[index] = scrubJSON(nested)
		}
	}
	return value
}

func redactText(value string) string {
	value = urlCredentialPattern.ReplaceAllString(value, `${1}[REDACTED]@`)
	value = urlQuerySecretPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = bearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	value = basicPattern.ReplaceAllString(value, "Basic [REDACTED]")
	return secretAssignmentPattern.ReplaceAllString(value, `${1}[REDACTED]`)
}

func sensitiveField(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.NewReplacer("_", "-", " ", "").Replace(normalized)
	switch normalized {
	case "authorization", "proxy-authorization", "proxyauthorization", "cookie", "set-cookie", "setcookie", "api-key", "apikey", "access-token", "accesstoken", "refresh-token", "refreshtoken", "id-token", "idtoken", "client-secret", "clientsecret", "password", "credential", "secret", "auth-token", "authtoken", "token-value", "tokenvalue":
		return true
	}
	return strings.Contains(normalized, "authorization") || strings.Contains(normalized, "api-key") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "cookie") || strings.Contains(normalized, "password") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "secret") || strings.HasSuffix(normalized, "-token")
}

func appendJSON(file *os.File, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = file.Write(data)
	return err
}

func pathSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func elapsedMS(start, end time.Time) float64 {
	return float64(end.Sub(start).Microseconds()) / 1000
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func safeName(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, current := range value {
		if unicode.IsLetter(current) || unicode.IsDigit(current) || current == '-' || current == '_' {
			builder.WriteRune(current)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 80 {
			break
		}
	}
	if builder.Len() == 0 {
		return "request"
	}
	return builder.String()
}

func eventDelimiter(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		return crlf, 4
	case crlf < 0:
		return lf, 2
	case lf < crlf:
		return lf, 2
	default:
		return crlf, 4
	}
}

func eventIdentity(raw []byte) (string, string) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var eventField string
	dataLines := make([]string, 0, 1)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "event:") {
			eventField = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	var payload struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &payload)
	return eventField, payload.Type
}

func redactHeaders(header http.Header) map[string][]string {
	result := make(map[string][]string, len(header))
	for name, values := range header {
		if sensitiveHeader(name) {
			result[name] = []string{"[REDACTED]"}
			continue
		}
		result[name] = make([]string, len(values))
		for index, value := range values {
			result[name][index] = redactText(value)
		}
	}
	return result
}

func sensitiveHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"authorization", "cookie", "token", "secret", "api-key", "apikey", "x-key"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return redactText(raw)
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveHeader(key) || strings.EqualFold(strings.TrimSpace(key), "key") {
			query.Set(key, "[REDACTED]")
		}
	}
	parsed.RawQuery = query.Encode()
	parsed.User = nil
	return redactText(parsed.String())
}
