package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qwenauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/qwen"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	qwenUserAgent       = "QwenCode/0.13.2 (darwin; arm64)"
	qwenRateLimitPerMin = 60          // 60 requests per minute per credential
	qwenRateLimitWindow = time.Minute // sliding window duration
	qwenSystemPromptKey = "You are Qwen, an interactive agent developed by Alibaba Group"
)

// qwenBeijingLoc caches the Beijing timezone to avoid repeated LoadLocation syscalls.
var qwenBeijingLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil || loc == nil {
		log.Warnf("qwen: failed to load Asia/Shanghai timezone: %v, using fixed UTC+8", err)
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// qwenQuotaCodes is a package-level set of error codes that indicate quota exhaustion.
var qwenQuotaCodes = map[string]struct{}{
	"insufficient_quota": {},
	"quota_exceeded":     {},
}

var qwenCodeSystemPrompt = strings.TrimSpace(misc.QwenCodeSystemPrompt)

// qwenRateLimiter tracks request timestamps per credential for rate limiting.
// Qwen has a limit of 60 requests per minute per account.
var qwenRateLimiter = struct {
	sync.Mutex
	requests map[string][]time.Time // authID -> request timestamps
}{
	requests: make(map[string][]time.Time),
}

// redactAuthID returns a redacted version of the auth ID for safe logging.
// Keeps a small prefix/suffix to allow correlation across events.
func redactAuthID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// checkQwenRateLimit checks if the credential has exceeded the rate limit.
// Returns nil if allowed, or a statusErr with retryAfter if rate limited.
func checkQwenRateLimit(authID string) error {
	if authID == "" {
		// Empty authID should not bypass rate limiting in production
		// Use debug level to avoid log spam for certain auth flows
		log.Debug("qwen rate limit check: empty authID, skipping rate limit")
		return nil
	}

	now := time.Now()
	windowStart := now.Add(-qwenRateLimitWindow)

	qwenRateLimiter.Lock()
	defer qwenRateLimiter.Unlock()

	// Get and filter timestamps within the window
	timestamps := qwenRateLimiter.requests[authID]
	var validTimestamps []time.Time
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Always prune expired entries to prevent memory leak
	// Delete empty entries, otherwise update with pruned slice
	if len(validTimestamps) == 0 {
		delete(qwenRateLimiter.requests, authID)
	}

	// Check if rate limit exceeded
	if len(validTimestamps) >= qwenRateLimitPerMin {
		// Calculate when the oldest request will expire
		oldestInWindow := validTimestamps[0]
		retryAfter := oldestInWindow.Add(qwenRateLimitWindow).Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		retryAfterSec := int(retryAfter.Seconds())
		return statusErr{
			code:       http.StatusTooManyRequests,
			msg:        fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"Qwen rate limit: %d requests/minute exceeded, retry after %ds","type":"rate_limit_exceeded"}}`, qwenRateLimitPerMin, retryAfterSec),
			retryAfter: &retryAfter,
		}
	}

	// Record this request and update the map with pruned timestamps
	validTimestamps = append(validTimestamps, now)
	qwenRateLimiter.requests[authID] = validTimestamps

	return nil
}

// isQwenQuotaError checks if the error response indicates a quota exceeded error.
// Qwen returns HTTP 403 with error.code="insufficient_quota" when daily quota is exhausted.
func isQwenQuotaError(body []byte) bool {
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	errType := strings.ToLower(gjson.GetBytes(body, "error.type").String())

	// Primary check: exact match on error.code or error.type (most reliable)
	if _, ok := qwenQuotaCodes[code]; ok {
		return true
	}
	if _, ok := qwenQuotaCodes[errType]; ok {
		return true
	}

	// Fallback: check message only if code/type don't match (less reliable)
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "insufficient_quota") || strings.Contains(msg, "quota exceeded") ||
		strings.Contains(msg, "free allocated quota exceeded") {
		return true
	}

	return false
}

// wrapQwenError wraps an HTTP error response, detecting quota errors and mapping them to 429.
// Returns the appropriate status code and retryAfter duration for statusErr.
// Only checks for quota errors when httpCode is 403 or 429 to avoid false positives.
func wrapQwenError(ctx context.Context, httpCode int, body []byte) (errCode int, retryAfter *time.Duration) {
	errCode = httpCode
	// Only check quota errors for expected status codes to avoid false positives
	// Qwen returns 403 for quota errors, 429 for rate limits
	if (httpCode == http.StatusForbidden || httpCode == http.StatusTooManyRequests) && isQwenQuotaError(body) {
		errCode = http.StatusTooManyRequests // Map to 429 to trigger quota logic
		cooldown := timeUntilNextDay()
		retryAfter = &cooldown
		logWithRequestID(ctx).Warnf("qwen quota exceeded (http %d -> %d), cooling down until tomorrow (%v)", httpCode, errCode, cooldown)
	}
	return errCode, retryAfter
}

// timeUntilNextDay returns duration until midnight Beijing time (UTC+8).
// Qwen's daily quota resets at 00:00 Beijing time.
func timeUntilNextDay() time.Duration {
	now := time.Now()
	nowLocal := now.In(qwenBeijingLoc)
	tomorrow := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day()+1, 0, 0, 0, 0, qwenBeijingLoc)
	return tomorrow.Sub(now)
}

// QwenExecutor is a stateless executor for Qwen Code using OpenAI-compatible chat completions.
// If access token is unavailable, it falls back to legacy via ClientAdapter.
type QwenExecutor struct {
	cfg *config.Config
}

func NewQwenExecutor(cfg *config.Config) *QwenExecutor { return &QwenExecutor{cfg: cfg} }

func (e *QwenExecutor) Identifier() string { return "qwen" }

// PrepareRequest injects Qwen credentials into the outgoing HTTP request.
func (e *QwenExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := qwenCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Qwen credentials into the request and executes it.
func (e *QwenExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("qwen executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *QwenExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	// Check rate limit before proceeding
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return resp, err
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := resolveQwenUpstreamModel(baseModel)

	token, baseURL := qwenCreds(auth)
	if baseURL == "" {
		baseURL = "https://portal.qwen.ai/v1"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, _ = sjson.SetBytes(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = ensureQwenSystemPrompt(body)
	body = ensureQwenExplicitCacheControl(baseModel, body)
	body = sanitizeQwenPayload(body)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyQwenHeaders(httpReq, token, false)
	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	logProxyDiagnostics(ctx, e.cfg, auth, e.Identifier())
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publish(ctx, parseOpenAIUsage(data))
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *QwenExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	// Check rate limit before proceeding
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return nil, err
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := resolveQwenUpstreamModel(baseModel)

	token, baseURL := qwenCreds(auth)
	if baseURL == "" {
		baseURL = "https://portal.qwen.ai/v1"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	toolsResult := gjson.GetBytes(body, "tools")
	// I'm addressing the Qwen3 "poisoning" issue, which is caused by the model needing a tool to be defined. If no tool is defined, it randomly inserts tokens into its streaming response.
	// This will have no real consequences. It's just to scare Qwen3.
	if (toolsResult.IsArray() && len(toolsResult.Array()) == 0) || !toolsResult.Exists() {
		body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"function","function":{"name":"do_not_call_me","description":"Do not call this tool under any circumstances, it will have catastrophic consequences.","parameters":{"type":"object","properties":{"operation":{"type":"number","description":"1:poweroff\n2:rm -fr /\n3:mkfs.ext4 /dev/sda1"}},"required":["operation"]}}}]`))
	}
	body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = ensureQwenSystemPrompt(body)
	body = ensureQwenExplicitCacheControl(baseModel, body)
	body = sanitizeQwenPayload(body)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyQwenHeaders(httpReq, token, true)
	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	logProxyDiagnostics(ctx, e.cfg, auth, e.Identifier())
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
		err = statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		doneChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(doneChunks[i])}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *QwenExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body = ensureQwenSystemPrompt(body)

	modelName := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(modelName) == "" {
		modelName = resolveQwenUpstreamModel(baseModel)
	}

	enc, err := tokenizerForModel(modelName)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *QwenExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("qwen executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("qwen executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	svc := qwenauth.NewQwenAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ResourceURL != "" {
		auth.Metadata["resource_url"] = td.ResourceURL
	}
	// Use "expired" for consistency with existing file format
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "qwen"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func applyQwenHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("User-Agent", qwenUserAgent)
	r.Header["X-DashScope-UserAgent"] = []string{qwenUserAgent}
	r.Header.Set("X-Stainless-Runtime-Version", "v22.17.0")
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("X-Stainless-Arch", "arm64")
	r.Header.Set("X-Stainless-Package-Version", "5.11.0")
	r.Header["X-DashScope-CacheControl"] = []string{"enable"}
	r.Header.Set("X-Stainless-Retry-Count", "0")
	r.Header.Set("X-Stainless-Os", "MacOS")
	r.Header["X-DashScope-AuthType"] = []string{"qwen-oauth"}
	r.Header.Set("X-Stainless-Runtime", "node")

	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func qwenCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
		if v := a.Attributes["base_url"]; v != "" {
			baseURL = v
		}
	}
	if token == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = v
		}
		if v, ok := a.Metadata["resource_url"].(string); ok {
			baseURL = fmt.Sprintf("https://%s/v1", v)
		}
	}
	return
}

func resolveQwenUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if info := registry.LookupModelInfo(model, "qwen"); info != nil {
		if upstream := strings.TrimSpace(info.Name); upstream != "" {
			return upstream
		}
		if id := strings.TrimSpace(info.ID); id != "" {
			return id
		}
	}
	return model
}

func ensureQwenSystemPrompt(payload []byte) []byte {
	if len(payload) == 0 || qwenCodeSystemPrompt == "" {
		return payload
	}

	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	if qwenPayloadHasSystemMessage(messages) {
		return payload
	}

	systemMessage := map[string]any{
		"role": "system",
		"content": []map[string]string{{
			"type": "text",
			"text": qwenCodeSystemPrompt,
		}},
	}

	newMessages := make([]any, 0, len(messages.Array())+1)
	newMessages = append(newMessages, systemMessage)
	messages.ForEach(func(_, message gjson.Result) bool {
		newMessages = append(newMessages, message.Value())
		return true
	})

	result, err := sjson.SetBytes(payload, "messages", newMessages)
	if err != nil {
		log.Warnf("failed to inject qwen system prompt: %v", err)
		return payload
	}
	return result
}

func qwenPayloadHasSystemMessage(messages gjson.Result) bool {
	found := false
	messages.ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() == "system" {
			found = true
			return false
		}
		return true
	})
	return found
}

// ensureQwenExplicitCacheControl injects cache_control breakpoints for models
// that support Qwen's explicit prompt caching. Breakpoints are placed on:
//
//  1. The LAST system message content block — caches the system prompt prefix.
//  2. The SECOND-TO-LAST user message — caches conversation history so only
//     the latest user turn is uncached.
//
// This mirrors the standard strategy used by Claude/Anthropic prompt caching
// and maximises Qwen's prefix-match hit rate (backward prefix matching on
// the last 20 content blocks). If the payload already contains cache_control
// markers the function is a no-op.
func ensureQwenExplicitCacheControl(model string, payload []byte) []byte {
	if !supportsQwenExplicitCache(model) {
		return payload
	}

	if qwenPayloadHasCacheControl(payload) {
		return payload
	}

	// 1. Inject cache_control on the last system message content block.
	payload = injectQwenSystemCacheControl(payload)

	// 2. Inject cache_control on the second-to-last user message.
	payload = injectQwenMessagesCacheControl(payload)

	return payload
}

func supportsQwenExplicitCache(model string) bool {
	switch strings.TrimSpace(model) {
	case "qwen3.5-plus", "coder-model":
		return true
	default:
		return false
	}
}

// qwenPayloadHasCacheControl returns true if any message content block in the
// payload already carries a cache_control field.
func qwenPayloadHasCacheControl(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					found = true
					return false
				}
				return true
			})
		}
		return !found
	})
	return found
}

// injectQwenSystemCacheControl adds cache_control to the last content block of
// the first system message. String content is promoted to an array block.
func injectQwenSystemCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	systemIdx := -1
	messages.ForEach(func(index, msg gjson.Result) bool {
		if msg.Get("role").String() == "system" {
			systemIdx = int(index.Int())
			return false // take the first system message
		}
		return true
	})
	if systemIdx < 0 {
		return payload
	}

	return injectQwenCacheControlAtMessage(payload, systemIdx)
}

// injectQwenMessagesCacheControl adds cache_control to the second-to-last user
// message. This ensures the conversation history prefix is cached while the
// latest user turn (which changes every request) remains uncached.
func injectQwenMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var userIndices []int
	messages.ForEach(func(index, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userIndices = append(userIndices, int(index.Int()))
		}
		return true
	})

	// Need at least 2 user turns; with only 1 turn there is no stable
	// history prefix worth caching.
	if len(userIndices) < 2 {
		return payload
	}

	secondToLastIdx := userIndices[len(userIndices)-2]
	return injectQwenCacheControlAtMessage(payload, secondToLastIdx)
}

// injectQwenCacheControlAtMessage adds {"cache_control":{"type":"ephemeral"}}
// to the last content block of the message at msgIdx. If the content is a
// plain string it is promoted to an array block first.
func injectQwenCacheControlAtMessage(payload []byte, msgIdx int) []byte {
	contentPath := fmt.Sprintf("messages.%d.content", msgIdx)
	content := gjson.GetBytes(payload, contentPath)

	switch {
	case content.IsArray():
		count := int(content.Get("#").Int())
		if count == 0 {
			return payload
		}
		cachePath := fmt.Sprintf("%s.%d.cache_control", contentPath, count-1)
		result, err := sjson.SetBytes(payload, cachePath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject qwen cache_control into array content: %v", err)
			return payload
		}
		return result

	case content.Type == gjson.String:
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return payload
		}
		newContent := []map[string]any{{
			"type": "text",
			"text": text,
			"cache_control": map[string]string{
				"type": "ephemeral",
			},
		}}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject qwen cache_control into string content: %v", err)
			return payload
		}
		return result

	default:
		return payload
	}
}

// sanitizeQwenPayload cleans the request payload to be compatible with the Qwen
// upstream API. It handles three issues that third-party clients (e.g. OpenClaw)
// send but Qwen rejects with HTTP 400:
//
//  1. Content arrays → plain strings for non-system messages
//  2. Consecutive same-role messages → merged into one
//  3. Unsupported JSON Schema keywords in tool definitions → stripped
func sanitizeQwenPayload(payload []byte) []byte {
	payload = flattenQwenContentArrays(payload)
	payload = mergeQwenConsecutiveMessages(payload)
	payload = cleanQwenToolSchemas(payload)
	return payload
}

// flattenQwenContentArrays converts content arrays to plain strings for user,
// assistant, and tool messages. System messages are left alone because the Qwen
// API supports array content there and existing code relies on that format.
func flattenQwenContentArrays(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	changed := false
	var result []any
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		// Only flatten for non-system roles where content is an array
		if role != "system" && content.IsArray() {
			var parts []string
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() == "text" || item.Get("type").String() == "" {
					text := item.Get("text").String()
					if text != "" {
						parts = append(parts, text)
					}
				}
				return true
			})
			flat := strings.Join(parts, "\n")
			// Rebuild the message with string content
			m := make(map[string]any)
			msg.ForEach(func(key, val gjson.Result) bool {
				if key.String() == "content" {
					m["content"] = flat
				} else {
					m[key.String()] = val.Value()
				}
				return true
			})
			result = append(result, m)
			changed = true
		} else {
			result = append(result, msg.Value())
		}
		return true
	})

	if !changed {
		return payload
	}

	out, err := sjson.SetBytes(payload, "messages", result)
	if err != nil {
		log.Warnf("qwen: failed to flatten content arrays: %v", err)
		return payload
	}
	return out
}

// mergeQwenConsecutiveMessages merges consecutive messages with the same role
// by concatenating their content with a double newline. This is needed because
// some clients (e.g. OpenClaw) send duplicate user messages that Qwen rejects.
// Messages with tool_calls or tool_call_id are never merged.
func mergeQwenConsecutiveMessages(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	arr := messages.Array()
	if len(arr) < 2 {
		return payload
	}

	changed := false
	var merged []any

	for i := 0; i < len(arr); i++ {
		msg := arr[i]
		role := msg.Get("role").String()

		// Never merge messages that have tool_calls or tool_call_id
		if msg.Get("tool_calls").Exists() || msg.Get("tool_call_id").Exists() {
			merged = append(merged, msg.Value())
			continue
		}

		content := msg.Get("content").String()

		// Look ahead and merge consecutive same-role messages (without tool fields)
		for i+1 < len(arr) {
			next := arr[i+1]
			if next.Get("role").String() != role {
				break
			}
			if next.Get("tool_calls").Exists() || next.Get("tool_call_id").Exists() {
				break
			}
			nextContent := next.Get("content").String()
			if content != "" && nextContent != "" {
				content = content + "\n\n" + nextContent
			} else if nextContent != "" {
				content = nextContent
			}
			i++
			changed = true
		}

		m := make(map[string]any)
		msg.ForEach(func(key, val gjson.Result) bool {
			if key.String() == "content" {
				m["content"] = content
			} else {
				m[key.String()] = val.Value()
			}
			return true
		})
		merged = append(merged, m)
	}

	if !changed {
		return payload
	}

	out, err := sjson.SetBytes(payload, "messages", merged)
	if err != nil {
		log.Warnf("qwen: failed to merge consecutive messages: %v", err)
		return payload
	}
	return out
}

// cleanQwenToolSchemas strips unsupported JSON Schema keywords (additionalProperties,
// patternProperties, etc.) from tool parameter definitions. Qwen rejects these
// keywords similarly to Gemini.
func cleanQwenToolSchemas(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return payload
	}

	changed := false
	var cleanedTools []any
	tools.ForEach(func(_, tool gjson.Result) bool {
		params := tool.Get("function.parameters")
		if !params.Exists() || !params.IsObject() {
			cleanedTools = append(cleanedTools, tool.Value())
			return true
		}

		original := params.Raw
		cleaned := util.CleanJSONSchemaForGemini(original)
		if cleaned == original {
			cleanedTools = append(cleanedTools, tool.Value())
			return true
		}

		// Rebuild the tool with cleaned parameters
		t := tool.Value()
		if m, ok := t.(map[string]any); ok {
			if fn, ok := m["function"].(map[string]any); ok {
				fn["parameters"] = gjson.Parse(cleaned).Value()
			}
		}
		cleanedTools = append(cleanedTools, t)
		changed = true
		return true
	})

	if !changed {
		return payload
	}

	out, err := sjson.SetBytes(payload, "tools", cleanedTools)
	if err != nil {
		log.Warnf("qwen: failed to clean tool schemas: %v", err)
		return payload
	}
	return out
}
