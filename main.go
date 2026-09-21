// Package main implements the workbuddy CLIProxyAPI dynamic plugin.
//
// workbuddy wraps Tencent CodeBuddy (copilot.tencent.com) as a cliproxy
// provider: it performs the CodeBuddy web login flow, accepts manual API-key
// credentials, refreshes OAuth access tokens, and forwards OpenAI-compatible
// chat completion requests to the upstream /v2/chat/completions endpoint.
//
// This file is a clean-room reimplementation reconstructed from the public
// workbuddy.so binary (symbol table, string constants and RPC shape) published
// by Sliverkiss. Original credit for the workbuddy plugin goes to Sliverkiss;
// see https://github.com/Sliverkiss/cpa-plugin. Built with -buildmode=c-shared
// and exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pluginVersion is injected at link time for release builds:
//
//	-ldflags "-X main.pluginVersion=0.5.1"
var pluginVersion = "0.5.1"

const (
	providerName   = "workbuddy"
	authFileName   = "workbuddy.json"
	authTypeOAuth  = "oauth"
	authTypeAPIKey = "api_key"

	// CN realm (default)
	upstreamBase  = "https://copilot.tencent.com"
	clientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	cnOrigin      = "https://www.codebuddy.cn"
	originReferer = cnOrigin // backward-compat alias

	// Global realm (www.workbuddy.ai accounts)
	globalBase   = "https://www.workbuddy.ai"
	globalOrigin = "https://www.workbuddy.ai"

	// OAuth endpoints are realm-dependent; use the auth*EndpointFor helpers.
	authStatePath    = "/v2/plugin/auth/state?platform=CLI"
	loginAccountPath = "/v2/plugin/login/account?state="
	authTokenPath    = "/v2/plugin/auth/token?state="
	tokenRefreshPath = "/v2/plugin/auth/token/refresh"

	// Chat endpoint (realm-dependent; use chatEndpointFor(sa) at call sites)
	endpointChat = upstreamBase + "/v2/chat/completions"

	// Model-list paths (appended to the per-realm base URL)
	cnModelsPath     = "/console/enterprises/personal/models"
	globalModelsPath = "/v2/enterprises/personal/models"

	loginTTL         = 5 * time.Minute
	modelCacheTTL    = 60 * time.Minute
	modelCacheErrTTL = 5 * time.Minute
)

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	client  *http.Client
	expires time.Time
	global  bool
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client

	// Per-realm dynamic model cache (keyed by "cn" or "global").
	modelCacheMu  sync.Mutex
	modelCacheMap = map[string]*modelCacheEntry{}

	accountCacheMu  sync.Mutex
	accountCacheMap = map[string]*accountCacheEntry{}

	// Kept as an indirection so management handlers are unit-testable without a
	// cgo host callback table. Production always uses hostCall.
	hostCallFn = hostCall
)

// modelCacheEntry holds a cached model list with a TTL.
type modelCacheEntry struct {
	models    []pluginapi.ModelInfo
	details   []dynModelEntry // display metadata: credits and capability flags
	fetchedAt time.Time
	isError   bool // true → cached failure; use shorter TTL
}

const (
	accountCacheTTL    = 3 * time.Minute
	accountCacheErrTTL = 30 * time.Second
)

// accountSummary is deliberately a display-only projection. It never contains
// API keys, OAuth tokens, raw upstream responses, or the credential storage JSON.
type accountSummary struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Nickname    string         `json:"nickname,omitempty"`
	UID         string         `json:"uid,omitempty"`
	Realm       string         `json:"realm"`
	AuthType    string         `json:"auth_type"`
	Disabled    bool           `json:"disabled"`
	Plan        string         `json:"plan,omitempty"`
	Balance     *creditSummary `json:"balance,omitempty"`
	Enterprise  *creditSummary `json:"enterprise,omitempty"`
	CycleStart  string         `json:"cycle_start,omitempty"`
	CycleEnd    string         `json:"cycle_end,omitempty"`
	Models      []modelSummary `json:"models,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Error       string         `json:"error,omitempty"`
	Unsupported bool           `json:"unsupported,omitempty"`
}

type creditSummary struct {
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Unit      string  `json:"unit"`
}

type modelSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Credits   string `json:"credits,omitempty"`
	Context   int64  `json:"context"`
	MaxOutput int64  `json:"max_output"`
	Images    bool   `json:"images"`
	Reasoning bool   `json:"reasoning"`
	ToolCall  bool   `json:"tool_call"`
	Disabled  bool   `json:"disabled"`
}

type accountCacheEntry struct {
	summary   accountSummary
	fetchedAt time.Time
	isError   bool
}

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		// Preserve HTTP status so CPA MarkResult can cool down / rotate auths on 401/402/429.
		writeResponse(response, errorEnvelopeFromErr(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// -----------------------------------------------------------------------------
// Host calls (async streaming)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// -----------------------------------------------------------------------------
// RPC dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic:
		// Models are bound to auth credentials (oauth scope). Static listing is empty
		// so a disabled/no-auth install does not keep advertising workbuddy models.
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: nil})
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(wbManagementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

// -----------------------------------------------------------------------------
// Registration & models
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// statusError implements CPA cliproxyexecutor.StatusError (StatusCode) and
// optional RetryAfter so the host can cool down / fail over credentials.
type statusError struct {
	Message    string
	Code       string
	HTTPStatus int
	retryAfter *time.Duration
	Retryable  bool
}

func (e *statusError) Error() string {
	if e == nil {
		return "workbuddy error"
	}
	return e.Message
}

func (e *statusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// RetryAfter is the method name CPA retryAfterFromError looks for.
func (e *statusError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	return e.retryAfter
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          pluginVersion,
			Author:           "aiden-git (clean-room rebuild; original workbuddy by Sliverkiss)",
			GitHubRepository: "https://github.com/aiden-git/wb2cpa",
		},
		Capabilities: registrationCapability{
			ModelProvider: true,
			AuthProvider:  true,
			Executor:      true,
			// OAuth/auth-bound only: models appear when a non-disabled auth is loaded.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ManagementAPI:         true,
		},
	}
}

func wbModels() []pluginapi.ModelInfo {
	const maxCompletionTokens int64 = 8192
	specs := []struct {
		id            string
		name          string
		contextLength int64
	}{
		{"glm-5.2", "GLM-5.2", 1000000},
		{"glm-5.1", "GLM-5.1", 131072},
		{"glm-5v-turbo", "GLM-5V Turbo", 131072},
		{"kimi-k2.7", "Kimi K2.7", 262144},
		{"minimax-m3-pay", "MiniMax M3", 204800},
		{"hy3", "Hy3", 262144},
		{"hy3-preview", "Hy3 Preview", 262144},
		{"hy3-preview-agent", "Hy3 Preview Agent", 262144},
		{"deepseek-v4-pro", "DeepSeek V4 Pro", 1000000},
		{"deepseek-v4-flash", "DeepSeek V4 Flash", 1000000},
	}
	models := make([]pluginapi.ModelInfo, 0, len(specs))
	for _, m := range specs {
		models = append(models, pluginapi.ModelInfo{
			ID:                         m.id,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                m.name,
			Name:                       m.id,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              m.contextLength,
			MaxCompletionTokens:        maxCompletionTokens,
			UserDefined:                true,
		})
	}
	return models
}

// -----------------------------------------------------------------------------
// Dynamic model discovery
// -----------------------------------------------------------------------------

// dynModelEntry matches the upstream model object returned by both CN and
// Global model-list endpoints. Unknown fields are ignored.
type dynModelEntry struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	MaxInputTokens    int64    `json:"maxInputTokens"`
	MaxOutputTokens   int64    `json:"maxOutputTokens"`
	Disabled          bool     `json:"disabled"`
	SupportsImages    bool     `json:"supportsImages"`
	SupportsReasoning bool     `json:"supportsReasoning"`
	SupportsToolCall  bool     `json:"supportsToolCall"`
	Tags              []string `json:"tags"`
	// Credits is a string (e.g. "1.0") in the upstream response.
	Credits   string `json:"credits"`
	Reasoning struct {
		DefaultEffort    string   `json:"defaultEffort"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// nonChatModel returns true for models that are not suitable for chat
// completions and should be excluded from the CPA model list.
func nonChatModel(e dynModelEntry) bool {
	// Prefixes that signal non-chat capabilities.
	for _, pfx := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(e.ID, pfx) {
			return true
		}
	}
	// Tiny maxOutputTokens (1–256) indicates an embedding/classifier slot.
	if e.MaxOutputTokens > 0 && e.MaxOutputTokens <= 256 {
		return true
	}
	// Explicit image-generation tag.
	for _, t := range e.Tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// fetchDynamicModels calls the upstream model-list endpoint, filters by the
// CLI agent allowlist and nonChatModel, and converts to pluginapi.ModelInfo.
// On any error the caller falls back to wbModels().
func fetchDynamicModels(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	cacheKey := "cn"
	if isGlobal(sa) {
		cacheKey = "global"
	}

	// Check cache first.
	modelCacheMu.Lock()
	entry, exists := modelCacheMap[cacheKey]
	if exists {
		ttl := modelCacheTTL
		if entry.isError {
			ttl = modelCacheErrTTL
		}
		if time.Since(entry.fetchedAt) < ttl {
			cached := entry.models
			modelCacheMu.Unlock()
			if entry.isError {
				return nil, fmt.Errorf("model fetch cached failure")
			}
			return cached, nil
		}
	}
	modelCacheMu.Unlock()

	// Fetch from upstream.
	url := modelsEndpointFor(sa)
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := httpClientForAuth(sa).Do(httpReq)
	if err != nil {
		storeModelCacheError(cacheKey)
		return nil, fmt.Errorf("models fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		storeModelCacheError(cacheKey)
		return nil, fmt.Errorf("models fetch: HTTP %d", resp.StatusCode)
	}

	// Parse {code, data:{models, agents:[{name:"cli", models:[...]}]}}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &envelope); err != nil {
		storeModelCacheError(cacheKey)
		return nil, fmt.Errorf("models fetch: parse error: %w", err)
	}

	// Build CLI agent allowlist.
	cliAllowlist := map[string]struct{}{}
	for _, ag := range envelope.Data.Agents {
		if ag.Name == "cli" {
			for _, mid := range ag.Models {
				cliAllowlist[mid] = struct{}{}
			}
			break
		}
	}
	hasCLIList := len(cliAllowlist) > 0

	// Convert to ModelInfo, filtering by allowlist + nonChatModel. Keep the
	// upstream metadata too: the management view uses it for credit multipliers
	// and capability badges without making a second models request.
	var result []pluginapi.ModelInfo
	var details []dynModelEntry
	for _, e := range envelope.Data.Models {
		if e.Disabled {
			continue
		}
		if hasCLIList {
			if _, inList := cliAllowlist[e.ID]; !inList {
				continue
			}
		}
		if nonChatModel(e) {
			continue
		}
		ctx := e.MaxInputTokens
		if ctx == 0 {
			ctx = 131072
		}
		maxOut := e.MaxOutputTokens
		if maxOut == 0 {
			maxOut = 8192
		}
		name := e.Name
		if name == "" {
			name = e.ID
		}
		result = append(result, pluginapi.ModelInfo{
			ID:                         e.ID,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                name,
			Name:                       e.ID,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              ctx,
			MaxCompletionTokens:        maxOut,
			UserDefined:                true,
		})
		e.MaxInputTokens = ctx
		e.MaxOutputTokens = maxOut
		e.Name = name
		details = append(details, e)
	}

	// Cache success.
	modelCacheMu.Lock()
	modelCacheMap[cacheKey] = &modelCacheEntry{
		models:    result,
		details:   details,
		fetchedAt: time.Now(),
		isError:   false,
	}
	modelCacheMu.Unlock()

	return result, nil
}

func storeModelCacheError(key string) {
	modelCacheMu.Lock()
	modelCacheMap[key] = &modelCacheEntry{fetchedAt: time.Now(), isError: true}
	modelCacheMu.Unlock()
}

// -----------------------------------------------------------------------------
// Auth data shapes (matches persisted workbuddy.json)
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a workbuddy credential.
//
// Two auth modes are supported:
//  1. oauth (default / legacy): QR / web login → accessToken + refreshToken
//  2. api_key: manually pasted CodeBuddy API key
//
// Recognized shapes for manual import (CPA auth file / upload):
//
//	{"type":"workbuddy","auth_type":"api_key","api_key":"...","user_id":"anonymous","domain":"copilot.tencent.com"}
//	{"type":"workbuddy","apiKey":"..."}
//	{"auth":{"accessToken":"...","refreshToken":"..."},"account":{...}}  // legacy oauth
//
// Standard CPA credential fields (same root keys as host synthesizer / panel PATCH):
//
//	"prefix": "wb", "proxy_url": "http://127.0.0.1:7890", "priority": 100
//	"disabled": true
//	"excluded_models": ["hy3","minimax-m3-pay"]
//	"model_aliases": [{"name":"hy3-preview-agent","alias":"hy3","force-mapping":false}]
type storedAuth struct {
	Type           string   `json:"type,omitempty"`
	AuthType       string   `json:"auth_type,omitempty"`
	APIKey         string   `json:"api_key,omitempty"`
	APIKeyCamel    string   `json:"apiKey,omitempty"`
	UserID         string   `json:"user_id,omitempty"`
	Domain         string   `json:"domain,omitempty"`
	Endpoint       string   `json:"endpoint,omitempty"`
	EnterpriseID   string   `json:"enterprise_id,omitempty"`
	Prefix         string   `json:"prefix,omitempty"`
	ProxyURL       string   `json:"proxy_url,omitempty"`
	Priority       flexInt  `json:"priority,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
	ExcludedModels []string `json:"excluded_models,omitempty"`
	// ExcludedModelsAlt accepts host/panel hyphenated key on re-marshal via alias tag.
	ExcludedModelsHyphen []string      `json:"excluded-models,omitempty"`
	ModelAliases         []modelAlias  `json:"model_aliases,omitempty"`
	ModelAliasesHyphen   []modelAlias  `json:"model-aliases,omitempty"`
	Auth                 storedTokens  `json:"auth"`
	Account              storedAccount `json:"account"`
}

// modelAlias matches CPA OAuthModelAlias JSON (name=upstream, alias=client-facing).
type modelAlias struct {
	Name         string `json:"name"`
	Alias        string `json:"alias"`
	ForceMapping bool   `json:"force-mapping,omitempty"`
	Fork         bool   `json:"fork,omitempty"`
}

// flexInt accepts CPA priority as number or string (panel/synthesizer both appear).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("priority: %w", err)
		}
		*f = flexInt(n)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		i, errInt := n.Int64()
		if errInt != nil {
			f64, errF := n.Float64()
			if errF != nil {
				return errInt
			}
			*f = flexInt(int(f64))
			return nil
		}
		*f = flexInt(int(i))
		return nil
	}
	var i int
	if err := json.Unmarshal(b, &i); err != nil {
		return err
	}
	*f = flexInt(i)
	return nil
}

func (f flexInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(f))
}

func (f flexInt) Int() int { return int(f) }

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by every CodeBuddy API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	normalizeStored(&sa)

	// Reject credentials that clearly belong to another provider.
	if t := strings.ToLower(strings.TrimSpace(sa.Type)); t != "" && t != providerName && t != "codebuddy" {
		return nil, fmt.Errorf("parse_error: foreign type %q", sa.Type)
	}

	switch sa.authMode() {
	case authTypeAPIKey:
		if sa.resolvedAPIKey() == "" {
			return nil, fmt.Errorf("parse_error: missing api_key")
		}
	default:
		if sa.Auth.AccessToken == "" {
			return nil, fmt.Errorf("parse_error: missing accessToken")
		}
	}
	return &sa, nil
}

// normalizeStored fills defaults and collapses camelCase / snake_case aliases.
func normalizeStored(sa *storedAuth) {
	if sa == nil {
		return
	}
	if sa.APIKey == "" && sa.APIKeyCamel != "" {
		sa.APIKey = strings.TrimSpace(sa.APIKeyCamel)
	}
	sa.APIKey = strings.TrimSpace(sa.APIKey)
	sa.APIKeyCamel = ""
	sa.AuthType = strings.ToLower(strings.TrimSpace(sa.AuthType))
	sa.Type = strings.TrimSpace(sa.Type)
	sa.UserID = strings.TrimSpace(sa.UserID)
	sa.Domain = strings.TrimSpace(sa.Domain)
	sa.Endpoint = strings.TrimSpace(sa.Endpoint)
	sa.EnterpriseID = strings.TrimSpace(sa.EnterpriseID)
	sa.ProxyURL = strings.TrimSpace(sa.ProxyURL)
	// CPA prefix: single path segment, no slashes (matches host synthesizer).
	sa.Prefix = normalizeAuthPrefix(sa.Prefix)

	// Merge hyphenated host keys into canonical snake_case fields.
	if len(sa.ExcludedModels) == 0 && len(sa.ExcludedModelsHyphen) > 0 {
		sa.ExcludedModels = sa.ExcludedModelsHyphen
	}
	sa.ExcludedModelsHyphen = nil
	sa.ExcludedModels = cleanStringList(sa.ExcludedModels)
	if len(sa.ModelAliases) == 0 && len(sa.ModelAliasesHyphen) > 0 {
		sa.ModelAliases = sa.ModelAliasesHyphen
	}
	sa.ModelAliasesHyphen = nil
	sa.ModelAliases = cleanModelAliases(sa.ModelAliases)

	// Infer api_key mode when the key is present but auth_type was omitted.
	if sa.AuthType == "" && sa.APIKey != "" && sa.Auth.AccessToken == "" {
		sa.AuthType = authTypeAPIKey
	}
	if sa.AuthType == "" {
		sa.AuthType = authTypeOAuth
	}
	if sa.Type == "" {
		sa.Type = providerName
	}
	if sa.AuthType == authTypeAPIKey {
		if sa.UserID == "" {
			if sa.Account.UID != "" {
				sa.UserID = sa.Account.UID
			} else {
				sa.UserID = "anonymous"
			}
		}
		if sa.Domain == "" {
			if sa.Auth.Domain != "" {
				sa.Domain = sa.Auth.Domain
			} else {
				sa.Domain = "copilot.tencent.com"
			}
		}
		if sa.Account.UID == "" {
			sa.Account.UID = sa.UserID
		}
		if sa.Account.EnterpriseID == "" && sa.EnterpriseID != "" {
			sa.Account.EnterpriseID = sa.EnterpriseID
		}
		if sa.Auth.Domain == "" {
			sa.Auth.Domain = sa.Domain
		}
	}
}

func normalizeAuthPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.Trim(prefix, "/")
	if prefix == "" || strings.Contains(prefix, "/") {
		return ""
	}
	return prefix
}

func cleanStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanModelAliases(in []modelAlias) []modelAlias {
	if len(in) == 0 {
		return nil
	}
	out := make([]modelAlias, 0, len(in))
	seen := map[string]struct{}{}
	for _, a := range in {
		name := strings.TrimSpace(a.Name)
		alias := strings.TrimSpace(a.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(alias)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, modelAlias{
			Name:         name,
			Alias:        alias,
			ForceMapping: a.ForceMapping,
			Fork:         a.Fork,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyHostCredentialFields copies CPA host-managed credential fields from
// refresh/parse metadata when storage JSON omitted them (panel PATCH often
// updates metadata without rewriting plugin storage).
func applyHostCredentialFields(sa *storedAuth, metadata map[string]any, attributes map[string]string) {
	if sa == nil {
		return
	}
	if metadata != nil {
		if sa.Prefix == "" {
			if v, ok := metadata["prefix"].(string); ok {
				sa.Prefix = normalizeAuthPrefix(v)
			}
		}
		if sa.ProxyURL == "" {
			if v, ok := metadata["proxy_url"].(string); ok {
				sa.ProxyURL = strings.TrimSpace(v)
			}
		}
		if sa.Priority.Int() == 0 {
			if n, ok := anyToInt(metadata["priority"]); ok {
				sa.Priority = flexInt(n)
			}
		}
		if !sa.Disabled {
			if b, ok := metadata["disabled"].(bool); ok && b {
				sa.Disabled = true
			}
		}
		if len(sa.ExcludedModels) == 0 {
			if list := stringListFromAny(metadata["excluded_models"]); len(list) > 0 {
				sa.ExcludedModels = list
			} else if list := stringListFromAny(metadata["excluded-models"]); len(list) > 0 {
				sa.ExcludedModels = list
			}
		}
		if len(sa.ModelAliases) == 0 {
			if aliases := modelAliasesFromAny(metadata["model_aliases"]); len(aliases) > 0 {
				sa.ModelAliases = aliases
			} else if aliases := modelAliasesFromAny(metadata["model-aliases"]); len(aliases) > 0 {
				sa.ModelAliases = aliases
			}
		}
	}
	if attributes != nil {
		if sa.Priority.Int() == 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(attributes["priority"])); err == nil {
				sa.Priority = flexInt(n)
			}
		}
		if !sa.Disabled {
			if strings.EqualFold(strings.TrimSpace(attributes["disabled"]), "true") {
				sa.Disabled = true
			}
		}
		if len(sa.ExcludedModels) == 0 {
			if v := strings.TrimSpace(attributes["excluded_models"]); v != "" {
				sa.ExcludedModels = cleanStringList(strings.Split(v, ","))
			}
		}
		if len(sa.ModelAliases) == 0 {
			if v := strings.TrimSpace(attributes["model_aliases"]); v != "" {
				var aliases []modelAlias
				if json.Unmarshal([]byte(v), &aliases) == nil {
					sa.ModelAliases = cleanModelAliases(aliases)
				}
			}
		}
	}
	sa.ExcludedModels = cleanStringList(sa.ExcludedModels)
	sa.ModelAliases = cleanModelAliases(sa.ModelAliases)
}

func stringListFromAny(raw any) []string {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return cleanStringList(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return cleanStringList(out)
	case string:
		return cleanStringList(strings.Split(v, ","))
	default:
		return nil
	}
}

func modelAliasesFromAny(raw any) []modelAlias {
	if raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var aliases []modelAlias
	if json.Unmarshal(data, &aliases) != nil {
		return nil
	}
	return cleanModelAliases(aliases)
}

func anyToInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func (sa *storedAuth) authMode() string {
	if sa == nil {
		return authTypeOAuth
	}
	mode := strings.ToLower(strings.TrimSpace(sa.AuthType))
	if mode == authTypeAPIKey || mode == "apikey" || mode == "key" {
		return authTypeAPIKey
	}
	if sa.resolvedAPIKey() != "" && sa.Auth.AccessToken == "" {
		return authTypeAPIKey
	}
	return authTypeOAuth
}

func (sa *storedAuth) resolvedAPIKey() string {
	if sa == nil {
		return ""
	}
	if k := strings.TrimSpace(sa.APIKey); k != "" {
		return k
	}
	return strings.TrimSpace(sa.APIKeyCamel)
}

func (sa *storedAuth) isAPIKey() bool {
	return sa.authMode() == authTypeAPIKey
}

func (sa *storedAuth) label() string {
	if sa == nil {
		return "WorkBuddy"
	}
	if sa.isAPIKey() {
		key := sa.resolvedAPIKey()
		if len(key) > 12 {
			return "WorkBuddy API Key (" + key[:6] + "…" + key[len(key)-4:] + ")"
		}
		return "WorkBuddy API Key"
	}
	if sa.Account.Nickname != "" {
		return "WorkBuddy (" + sa.Account.Nickname + ")"
	}
	if sa.Account.UID != "" {
		return "WorkBuddy (" + sa.Account.UID + ")"
	}
	return "WorkBuddy"
}

func (sa *storedAuth) authID() string {
	if sa == nil {
		return providerName
	}
	if sa.isAPIKey() {
		// Stable-ish id from key fingerprint so multiple keys can coexist.
		sum := fmt.Sprintf("%x", shortHash(sa.resolvedAPIKey()))
		return providerName + "-key-" + sum
	}
	if sa.Account.UID != "" {
		return providerName + "-" + sa.Account.UID
	}
	return providerName
}

func shortHash(s string) []byte {
	// FNV-1a 32-bit, no extra import weight for crypto.
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return []byte{
		byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
	}
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
			Jar: jar,
		}
	})
	return sharedClient
}

// httpClientForAuth returns a client that honors the credential proxy_url when set.
// Login QR flow always uses the isolated cookie client (no per-auth proxy required).
func httpClientForAuth(sa *storedAuth) *http.Client {
	if sa == nil || strings.TrimSpace(sa.ProxyURL) == "" {
		return sharedHTTPClient()
	}
	proxyURL, err := url.Parse(strings.TrimSpace(sa.ProxyURL))
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return sharedHTTPClient()
	}
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			MaxIdleConns:        20,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 5,
		},
	}
}

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        20,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 5,
		},
		Jar: jar,
	}
}

func commonHeaders(req *http.Request) {
	commonHeadersFor(req, false)
}

// commonHeadersFor sets request headers. isGlobal selects the workbuddy.ai origin.
func commonHeadersFor(req *http.Request, isGlb bool) {
	origin := cnOrigin
	if isGlb {
		origin = globalOrigin
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// isGlobalDomain returns true for workbuddy.ai accounts (global realm).
// The domain field in storedAuth is the bare host (no scheme, no path).
func isGlobalDomain(domain string) bool {
	return domain == "www.workbuddy.ai" ||
		strings.HasSuffix(domain, ".workbuddy.ai")
}

// isGlobal is a convenience wrapper over isGlobalDomain for a storedAuth.
func isGlobal(sa *storedAuth) bool {
	d := sa.Auth.Domain
	if d == "" {
		d = sa.Domain
	}
	return isGlobalDomain(d)
}

// chatEndpointFor returns the chat completions URL for the credential's realm.
func chatEndpointFor(sa *storedAuth) string {
	if isGlobal(sa) {
		return globalBase + "/v2/chat/completions"
	}
	return endpointChat
}

// modelsEndpointFor returns the model-list URL for the credential's realm.
func modelsEndpointFor(sa *storedAuth) string {
	if isGlobal(sa) {
		return globalBase + globalModelsPath
	}
	return upstreamBase + cnModelsPath
}

func authBaseForGlobal(global bool) string {
	if global {
		return globalBase
	}
	return upstreamBase
}

func authStateEndpointFor(global bool) string {
	return authBaseForGlobal(global) + authStatePath
}

func loginAccountEndpointFor(global bool, state string) string {
	return authBaseForGlobal(global) + loginAccountPath + state
}

func authTokenEndpointFor(global bool, state string) string {
	return authBaseForGlobal(global) + authTokenPath + state
}

func tokenRefreshEndpointFor(sa *storedAuth) string {
	return authBaseForGlobal(isGlobal(sa)) + tokenRefreshPath
}

func domainForGlobal(global bool) string {
	if global {
		return "www.workbuddy.ai"
	}
	return "copilot.tencent.com"
}

// billingBaseFor deliberately differs from the CN chat/auth base. CodeBuddy
// serves billing from www.codebuddy.cn while Global uses workbuddy.ai for both.
func billingBaseFor(sa *storedAuth) string {
	if isGlobal(sa) {
		return globalBase
	}
	return cnOrigin
}

func billingHeaders(req *http.Request, sa *storedAuth) {
	commonHeadersFor(req, isGlobal(sa))
	req.Header.Set("Accept", "application/json")
	if sa == nil || sa.isAPIKey() || strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	if uid := firstNonEmpty(sa.Account.UID, sa.UserID); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if isGlobal(sa) {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
		return
	}
	enterpriseID := firstNonEmpty(sa.Account.EnterpriseID, sa.EnterpriseID)
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	req.Header.Set("X-Domain", firstNonEmpty(sa.Auth.Domain, sa.Domain, "copilot.tencent.com"))
}

type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(raw []byte) error {
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		*f = flexibleFloat(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return err
	}
	*f = flexibleFloat(n)
	return nil
}

type billingPackage struct {
	PackageName         string        `json:"PackageName"`
	CapacityRemain      flexibleFloat `json:"CapacityRemain"`
	CapacityUsed        flexibleFloat `json:"CapacityUsed"`
	CapacitySize        flexibleFloat `json:"CapacitySize"`
	CycleCapacityRemain flexibleFloat `json:"CycleCapacityRemain"`
	CycleCapacityUsed   flexibleFloat `json:"CycleCapacityUsed"`
	CycleCapacitySize   flexibleFloat `json:"CycleCapacitySize"`
	CycleStartTime      string        `json:"CycleStartTime"`
	CycleEndTime        string        `json:"CycleEndTime"`
}

type billingResourceData struct {
	Response struct {
		Data struct {
			Accounts []billingPackage `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

func fetchBillingData(sa *storedAuth, path string, request any, target any) error {
	if sa == nil || sa.isAPIKey() {
		return fmt.Errorf("billing is unavailable for API key credentials")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	data, _, err := doJSON(httpClientForAuth(sa), http.MethodPost, billingBaseFor(sa)+path, func(req *http.Request) {
		billingHeaders(req, sa)
	}, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("billing request failed")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("billing response parse failed")
	}
	return nil
}

func fetchPaymentType(sa *storedAuth) (string, error) {
	var response struct {
		PaymentType any `json:"paymentType"`
	}
	if err := fetchBillingData(sa, "/v2/billing/meter/get-payment-type", map[string]any{}, &response); err != nil {
		return "", err
	}
	switch value := response.PaymentType.(type) {
	case string:
		return strings.TrimSpace(value), nil
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	default:
		return "", nil
	}
}

func fetchUserResource(sa *storedAuth, now time.Time) (*creditSummary, string, string, error) {
	request := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format("2006-01-02 15:04:05"),
	}
	var response billingResourceData
	if err := fetchBillingData(sa, "/v2/billing/meter/get-user-resource", request, &response); err != nil {
		return nil, "", "", err
	}
	var selected *billingPackage
	for i := range response.Response.Data.Accounts {
		candidate := &response.Response.Data.Accounts[i]
		if candidate.CycleCapacitySize > 0 || candidate.CapacitySize > 0 {
			selected = candidate
			break
		}
	}
	if selected == nil {
		return nil, "", "", nil
	}
	remaining, used, total := selected.CapacityRemain, selected.CapacityUsed, selected.CapacitySize
	if selected.CycleCapacitySize > 0 {
		remaining, used, total = selected.CycleCapacityRemain, selected.CycleCapacityUsed, selected.CycleCapacitySize
	}
	return &creditSummary{Remaining: float64(remaining), Used: float64(used), Total: float64(total), Unit: "credits"}, selected.CycleStartTime, selected.CycleEndTime, nil
}

func fetchEnterpriseUsageCN(sa *storedAuth) (*creditSummary, string, string, error) {
	if sa == nil || isGlobal(sa) || firstNonEmpty(sa.Account.EnterpriseID, sa.EnterpriseID) == "" {
		return nil, "", "", nil
	}
	var response struct {
		Credit         flexibleFloat `json:"credit"`
		LimitNum       flexibleFloat `json:"limitNum"`
		CycleStartTime string        `json:"cycleStartTime"`
		CycleEndTime   string        `json:"cycleEndTime"`
	}
	if err := fetchBillingData(sa, "/billing/meter/get-enterprise-user-usage", map[string]any{}, &response); err != nil {
		return nil, "", "", err
	}
	return &creditSummary{Remaining: float64(response.Credit), Total: float64(response.LimitNum), Used: float64(response.LimitNum - response.Credit), Unit: "credits"}, response.CycleStartTime, response.CycleEndTime, nil
}

func maskAccountIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 4 {
		return value
	}
	if len(value) <= 8 {
		return value[:2] + "…" + value[len(value)-2:]
	}
	return value[:3] + "…" + value[len(value)-4:]
}

func modelSummariesFor(sa *storedAuth) []modelSummary {
	key := "cn"
	if isGlobal(sa) {
		key = "global"
	}
	modelCacheMu.Lock()
	entry := modelCacheMap[key]
	var details []dynModelEntry
	if entry != nil && !entry.isError && time.Since(entry.fetchedAt) < modelCacheTTL {
		details = append(details, entry.details...)
	}
	modelCacheMu.Unlock()
	if len(details) == 0 {
		_, _ = fetchDynamicModels(sa)
		modelCacheMu.Lock()
		if entry = modelCacheMap[key]; entry != nil && !entry.isError {
			details = append(details, entry.details...)
		}
		modelCacheMu.Unlock()
	}
	out := make([]modelSummary, 0, len(details))
	for _, model := range details {
		out = append(out, modelSummary{ID: model.ID, Name: firstNonEmpty(model.Name, model.ID), Credits: model.Credits, Context: model.MaxInputTokens, MaxOutput: model.MaxOutputTokens, Images: model.SupportsImages, Reasoning: model.SupportsReasoning, ToolCall: model.SupportsToolCall, Disabled: model.Disabled})
	}
	return out
}

// backendHeaders applies auth-derived headers to a chat completion request.
// Empty fields are signalled via the X-No-* convention used by CodeBuddy.
// API-key mode sends both Authorization Bearer and X-API-Key (CodeBuddy accepts either).
// X-Refresh-Token is intentionally NOT sent here; it belongs only in handleRefreshAuth.
func backendHeaders(req *http.Request, sa *storedAuth) {
	glb := isGlobal(sa)
	commonHeadersFor(req, glb)

	if sa.isAPIKey() {
		key := sa.resolvedAPIKey()
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-API-Key", key)
		} else {
			req.Header.Set("X-No-Authorization", "1")
		}
	} else if sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}

	userID := sa.Account.UID
	if userID == "" {
		userID = sa.UserID
	}
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}

	if glb {
		// Global accounts are personal — no enterprise concept.
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	} else {
		enterpriseID := sa.Account.EnterpriseID
		if enterpriseID == "" {
			enterpriseID = sa.EnterpriseID
		}
		if enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
			req.Header.Set("X-Tenant-Id", enterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}

		domain := sa.Auth.Domain
		if domain == "" {
			domain = sa.Domain
		}
		if domain != "" {
			req.Header.Set("X-Domain", domain)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}

	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", "2.63.2")
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a workbuddy credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// RawJSON is the full auth file; fields already on sa. Keep helpers for
	// partial shapes where only metadata carries CPA credential fields.
	applyHostCredentialFields(sa, nil, nil)
	// Also accept top-level fields that unmarshal into storedAuth via RawJSON.
	// If file had prefix/proxy_url/priority, parseStored already loaded them.
	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" {
		fileName = authFileName
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    toAuthData(sa, fileName),
	})
}

func toAuthData(sa *storedAuth, fileName string) pluginapi.AuthData {
	if fileName == "" {
		fileName = authFileName
	}
	// Persist a cleaned shape so re-parse is stable across versions.
	persist := *sa
	normalizeStored(&persist)
	if persist.isAPIKey() {
		// Keep only api_key fields on disk for key mode (drop empty oauth noise).
		persist.Auth = storedTokens{Domain: persist.Domain}
	}
	// Avoid writing hyphen aliases into storage (canonical snake_case only).
	persist.ExcludedModelsHyphen = nil
	persist.ModelAliasesHyphen = nil
	// Storage JSON includes CPA standard fields so panel PATCH + re-parse round-trip.
	storage, _ := json.Marshal(persist)
	meta := map[string]any{
		"type":      providerName,
		"auth_type": persist.authMode(),
	}
	if persist.Prefix != "" {
		meta["prefix"] = persist.Prefix
	}
	if persist.ProxyURL != "" {
		meta["proxy_url"] = persist.ProxyURL
	}
	if persist.Priority.Int() != 0 {
		meta["priority"] = persist.Priority.Int()
	}
	if persist.Disabled {
		meta["disabled"] = true
	}
	if len(persist.ExcludedModels) > 0 {
		meta["excluded_models"] = append([]string(nil), persist.ExcludedModels...)
		// Hyphen form for host synthesizers that only read excluded-models.
		meta["excluded-models"] = append([]string(nil), persist.ExcludedModels...)
	}
	if len(persist.ModelAliases) > 0 {
		meta["model_aliases"] = append([]modelAlias(nil), persist.ModelAliases...)
		meta["model-aliases"] = append([]modelAlias(nil), persist.ModelAliases...)
	}
	if persist.isAPIKey() {
		meta["api_key"] = true
		if persist.UserID != "" {
			meta["user_id"] = persist.UserID
		}
		if persist.Domain != "" {
			meta["domain"] = persist.Domain
		}
	} else {
		if persist.Account.UID != "" {
			meta["uid"] = persist.Account.UID
		}
		if persist.Account.Nickname != "" {
			meta["nickname"] = persist.Account.Nickname
		}
		if persist.Auth.Domain != "" {
			meta["domain"] = persist.Auth.Domain
		}
	}
	attrs := map[string]string{}
	if persist.Priority.Int() != 0 {
		attrs["priority"] = strconv.Itoa(persist.Priority.Int())
	}
	if persist.Disabled {
		attrs["disabled"] = "true"
	}
	// Host routing reads excluded_models / model_aliases from attributes.
	if len(persist.ExcludedModels) > 0 {
		attrs["excluded_models"] = strings.Join(persist.ExcludedModels, ",")
	}
	if len(persist.ModelAliases) > 0 {
		if raw, err := json.Marshal(persist.ModelAliases); err == nil {
			attrs["model_aliases"] = string(raw)
		}
	}
	// auth_kind helps CPA merge global oauth-excluded-models when appropriate.
	if persist.isAPIKey() {
		attrs["auth_kind"] = "apikey"
	} else {
		attrs["auth_kind"] = "oauth"
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          sa.authID(),
		FileName:    fileName,
		Label:       sa.label(),
		Prefix:      persist.Prefix,
		ProxyURL:    persist.ProxyURL,
		Disabled:    persist.Disabled,
		StorageJSON: storage,
		Metadata:    meta,
		Attributes:  attrs,
	}
}

func handleModelsForAuth(raw []byte) ([]byte, error) {
	// Request carries StorageJSON for the selected auth (and sometimes Metadata).
	var req struct {
		StorageJSON []byte         `json:"StorageJSON"`
		Metadata    map[string]any `json:"Metadata"`
		// Loose fallbacks used by some host encodings.
		StorageJSONSnake []byte         `json:"storage_json"`
		MetadataSnake    map[string]any `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &req)
	storage := req.StorageJSON
	if len(storage) == 0 {
		storage = req.StorageJSONSnake
	}
	meta := req.Metadata
	if meta == nil {
		meta = req.MetadataSnake
	}
	// Also try nested Executor-style envelope: {"Auth":{...}} via raw map.
	if len(storage) == 0 {
		var loose map[string]any
		if json.Unmarshal(raw, &loose) == nil {
			if m, ok := loose["Metadata"].(map[string]any); ok && meta == nil {
				meta = m
			}
			for _, key := range []string{"StorageJSON", "storage_json", "storageJSON"} {
				switch v := loose[key].(type) {
				case string:
					if v != "" {
						storage = []byte(v)
					}
				case []byte:
					storage = v
				}
			}
		}
	}
	if len(storage) == 0 {
		// No auth material → no models (disabled / missing credential).
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: nil})
	}
	sa, err := parseStored(storage)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: nil})
	}
	applyHostCredentialFields(sa, meta, nil)
	if sa.Disabled {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: nil})
	}
	// Prefer dynamic model list from upstream; fall back to static list on error.
	models, err := fetchDynamicModels(sa)
	if err != nil || len(models) == 0 {
		models = wbModels()
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleStartLogin(raw []byte) ([]byte, error) {
	// The generic CPA OAuth card has no realm selector, so preserve its historical
	// CN default. The WorkBuddy management page starts Global login explicitly.
	var startReq pluginapi.AuthLoginStartRequest
	_ = json.Unmarshal(raw, &startReq)
	response, err := startLoginForRealm(startReq, false)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}

func startLoginForRealm(startReq pluginapi.AuthLoginStartRequest, global bool) (pluginapi.AuthLoginStartResponse, error) {
	client := newLoginClient()
	headers := func(r *http.Request) { commonHeadersFor(r, global) }
	data, _, err := doJSON(client, http.MethodPost, authStateEndpointFor(global), headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("auth state: missing state or authUrl")
	}
	expiresAt := time.Now().Add(loginTTL)
	loginStates.Store(st.State, &loginCtx{client: client, expires: expiresAt, global: global})
	meta := map[string]any{
		"realm":      map[bool]string{false: "cn", true: "global"}[global],
		"paste_hint": "Paste CodeBuddy API key in the callback/code box, or complete QR login.",
	}
	if dir := strings.TrimSpace(startReq.Host.AuthDir); dir != "" {
		meta["auth_dir"] = dir
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: expiresAt.UTC(),
		Metadata:  meta,
	}, nil
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}

	authDir := strings.TrimSpace(req.Host.AuthDir)
	if authDir == "" {
		authDir = hostAuthDirFromRaw(raw, req.Metadata)
	}

	// 1) CPA panel paste box: host writes .oauth-workbuddy-<state>.oauth with
	//    the pasted "callback URL / authorization code". We accept either a
	//    raw API key or a URL (extract key/code query params when present).
	if sa, handled, errPaste := tryConsumePastedCredential(authDir, state); handled {
		loginStates.Delete(state)
		if errPaste != nil {
			return okEnvelope(pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: errPaste.Error(),
			})
		}
		fileName := sa.authID() + ".json"
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "api_key credential saved",
			Auth:    toAuthData(sa, fileName),
		})
	}

	// 2) QR / web login: poll CodeBuddy with the cookie-affined client.
	v, ok := loginStates.Load(state)
	if !ok {
		// No in-memory login and no paste yet — keep waiting (host may paste later).
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for QR login or API key paste",
		})
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "login expired; restart login",
		})
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns code 11217 ("login ing") while pending, and code 0 with the
	// token bundle once complete. login/account sits behind the openresty gateway
	// and is rejected (401) until login finishes, so probe token first and only
	// fetch account once we hold a bearer. Every request stays on the realm that
	// issued this state; Global-issued JWTs are rejected by the CN gateway.
	realmHeaders := func(r *http.Request) { commonHeadersFor(r, lc.global) }
	tokRaw, _, errTok := doJSON(lc.client, http.MethodGet, authTokenEndpointFor(lc.global, state), realmHeaders, nil)
	if errTok != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	var acct accountData
	acctHeaders := func(r *http.Request) {
		commonHeadersFor(r, lc.global)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(lc.client, http.MethodGet, loginAccountEndpointFor(lc.global, state), acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	domain := strings.TrimSpace(tok.Domain)
	if domain == "" {
		domain = domainForGlobal(lc.global)
	}
	sa := &storedAuth{
		Type:     providerName,
		AuthType: authTypeOAuth,
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       domain,
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	loginStates.Delete(state)
	// Best-effort: drop any unused paste file for this state.
	_ = consumeOAuthCallbackFile(authDir, state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa, authFileName),
	})
}

// hostAuthDirFromRaw extracts AuthDir when typed HostConfigSummary is empty
// (host JSON may use AuthDir / auth_dir depending on encoding path).
func hostAuthDirFromRaw(raw []byte, metadata map[string]any) string {
	if v, ok := metadata["auth_dir"].(string); ok {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	var loose struct {
		Host map[string]any `json:"Host"`
		H2   map[string]any `json:"host"`
	}
	_ = json.Unmarshal(raw, &loose)
	for _, m := range []map[string]any{loose.Host, loose.H2} {
		if m == nil {
			continue
		}
		for _, k := range []string{"AuthDir", "auth_dir", "authDir"} {
			if v, ok := m[k].(string); ok {
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// oauthCallbackPayload matches CPA WriteOAuthCallbackFile JSON.
type oauthCallbackPayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

func oauthCallbackFilePath(authDir, state string) string {
	return filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", providerName, strings.TrimSpace(state)))
}

func readOAuthCallbackFile(authDir, state string) (oauthCallbackPayload, bool, error) {
	authDir = strings.TrimSpace(authDir)
	state = strings.TrimSpace(state)
	if authDir == "" || state == "" {
		return oauthCallbackPayload{}, false, nil
	}
	path := oauthCallbackFilePath(authDir, state)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return oauthCallbackPayload{}, false, nil
		}
		return oauthCallbackPayload{}, false, err
	}
	var payload oauthCallbackPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return oauthCallbackPayload{}, false, fmt.Errorf("invalid oauth callback file: %w", err)
	}
	return payload, true, nil
}

func consumeOAuthCallbackFile(authDir, state string) error {
	authDir = strings.TrimSpace(authDir)
	state = strings.TrimSpace(state)
	if authDir == "" || state == "" {
		return nil
	}
	path := oauthCallbackFilePath(authDir, state)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// tryConsumePastedCredential reads the host paste box payload.
// handled=false → no paste yet; handled=true + err → bad paste; handled=true + sa → success.
func tryConsumePastedCredential(authDir, state string) (*storedAuth, bool, error) {
	payload, ok, err := readOAuthCallbackFile(authDir, state)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil
	}
	if msg := strings.TrimSpace(payload.Error); msg != "" {
		_ = consumeOAuthCallbackFile(authDir, state)
		return nil, true, fmt.Errorf("%s", msg)
	}
	pasted := strings.TrimSpace(payload.Code)
	if pasted == "" {
		return nil, false, nil
	}

	kind, value, errClass := classifyPastedCredential(pasted)
	if errClass != nil {
		_ = consumeOAuthCallbackFile(authDir, state)
		return nil, true, errClass
	}
	switch kind {
	case "api_key":
		if err := consumeOAuthCallbackFile(authDir, state); err != nil {
			return nil, true, fmt.Errorf("consume paste file: %w", err)
		}
		sa := &storedAuth{
			Type:     providerName,
			AuthType: authTypeAPIKey,
			APIKey:   value,
			UserID:   "anonymous",
			Domain:   "copilot.tencent.com",
		}
		normalizeStored(sa)
		return sa, true, nil
	case "url":
		_ = consumeOAuthCallbackFile(authDir, state)
		return nil, true, fmt.Errorf("pasted URL is not a CodeBuddy API key; paste the API key string, or complete QR login in the browser")
	default:
		return nil, false, nil
	}
}

// classifyPastedCredential decides whether the CPA paste box holds an API key
// or a URL. Rules:
//   - http(s) URL → "url" (optionally extract ?api_key= / ?key= / ?code= as api_key when present)
//   - JSON object with api_key/apiKey → "api_key"
//   - anything else non-empty → "api_key" (raw key)
func classifyPastedCredential(pasted string) (kind, value string, err error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", "", fmt.Errorf("empty paste")
	}

	// Full JSON credential blob pasted by mistake / convenience.
	if strings.HasPrefix(pasted, "{") {
		var sa storedAuth
		if err := json.Unmarshal([]byte(pasted), &sa); err == nil {
			normalizeStored(&sa)
			if sa.resolvedAPIKey() != "" {
				return "api_key", sa.resolvedAPIKey(), nil
			}
		}
	}

	// CPA management UI may paste only the key into "redirect_url" without
	// building a real URL. Host still requires state+code fields separately.
	// When code arrives as a bare key (correct client), accept it here.

	lower := strings.ToLower(pasted)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		u, errParse := url.Parse(pasted)
		if errParse != nil {
			return "url", pasted, fmt.Errorf("invalid URL: %w", errParse)
		}
		q := u.Query()
		for _, key := range []string{"api_key", "apiKey", "key", "code"} {
			if v := strings.TrimSpace(q.Get(key)); v != "" && !looksLikeOAuthError(v) {
				// Prefer explicit api_key/key; "code" only if it looks like a key not a short oauth code.
				if key == "code" && !looksLikeAPIKey(v) {
					continue
				}
				if key == "code" || key == "api_key" || key == "apiKey" || key == "key" {
					if looksLikeAPIKey(v) || key != "code" {
						return "api_key", v, nil
					}
				}
			}
		}
		// Bare callback URL with no usable key — not supported for workbuddy QR.
		return "url", pasted, nil
	}

	if !looksLikeAPIKey(pasted) {
		return "", "", fmt.Errorf("paste does not look like a CodeBuddy API key (got %d chars)", len(pasted))
	}
	return "api_key", pasted, nil
}

func looksLikeOAuthError(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "access_denied" || strings.HasPrefix(s, "error")
}

func looksLikeAPIKey(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 8 || len(s) > 512 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	// Reject obvious non-keys.
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return false
	}
	return true
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Preserve host panel fields (prefix/proxy/priority/disabled/aliases/exclusions).
	applyHostCredentialFields(sa, req.Metadata, req.Attributes)
	if p := strings.TrimSpace(req.Attributes["proxy_url"]); p != "" && sa.ProxyURL == "" {
		sa.ProxyURL = p
	}
	// Disabled credentials still refresh storage so re-enable works, but stay Disabled.
	// API keys do not rotate; return the same credential unchanged.
	if sa.isAPIKey() {
		return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthData(sa, authFileName)})
	}
	if sa.Auth.RefreshToken == "" {
		return nil, fmt.Errorf("refresh: missing refreshToken")
	}
	headers := func(r *http.Request) {
		commonHeadersFor(r, isGlobal(sa))
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if !isGlobal(sa) && sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", providerName)
	}
	data, status, err := doJSON(httpClientForAuth(sa), http.MethodPost, tokenRefreshEndpointFor(sa), headers, nil)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthData(sa, authFileName)})
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	applyHostCredentialFields(sa, firstNonNilMap(req.AuthMetadata, req.Metadata), req.AuthAttributes)
	if sa.Disabled {
		return nil, fmt.Errorf("auth_disabled: workbuddy credential is disabled")
	}
	// CodeBuddy rejects non-stream requests (code 11101), so always stream
	// upstream and fold the chunks into a single chat.completion object.
	//
	// CPA resolves prefix/alias into req.Model, but when input format already
	// matches executor chat-completions it does not rewrite payload "model".
	// Without this step client aliases like "WorkBuddy/hy3" leak upstream (11102).
	body := forceStreamBody(req.Payload, req.OriginalRequest)
	body = rewriteModelForUpstream(body, req.Model, sa)
	body = rewriteSystemForUpstream(body)
	if err := ensureModelAllowed(body, sa); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, chatEndpointFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := httpClientForAuth(sa).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		return nil, upstreamHTTPError(resp.StatusCode, payload, resp.Header)
	}
	completion, err := aggregateCompletion(resp.Body, req.Model)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	applyHostCredentialFields(sa, firstNonNilMap(req.AuthMetadata, req.Metadata), req.AuthAttributes)
	if sa.Disabled {
		return nil, fmt.Errorf("auth_disabled: workbuddy credential is disabled")
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	// Same chat-completions path as execute: host may leave payload.model as the
	// client-facing id (prefix/alias). Rewrite to the upstream model before send.
	body = rewriteModelForUpstream(body, req.Model, sa)
	body = rewriteSystemForUpstream(body)
	if err := ensureModelAllowed(body, sa); err != nil {
		return nil, err
	}

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		chunks, errCollect := collectUpstreamStream(body, sa, sseFramed)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Open the upstream connection *before* returning so 401/402/429 reach the
	// host as execute_stream errors (with http_status). That lets CPA MarkResult
	// cool down / rotate credentials. Mid-stream failures still go via emit.
	httpReq, err := http.NewRequest(http.MethodPost, chatEndpointFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := httpClientForAuth(sa).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, upstreamHTTPError(resp.StatusCode, errPayload, resp.Header)
	}

	// Async pump of an already-accepted upstream stream body.
	go pumpUpstreamResponse(resp, req.StreamID, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamResponse reads an already-open upstream SSE body in the
// background and emits each cleaned chunk to the host stream. It closes the
// stream when done. An emit failure (client disconnected → host closed the
// stream) aborts the pump so we stop reading a dead upstream.
func pumpUpstreamResponse(resp *http.Response, streamID string, sseFramed bool) {
	if resp == nil {
		streamEmitError(streamID, "http_error: nil upstream response")
		streamClose(streamID)
		return
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			break
		}
	}
	streamClose(streamID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice.
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	httpReq, err := http.NewRequest(http.MethodPost, chatEndpointFor(sa), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := httpClientForAuth(sa).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		return nil, upstreamHTTPError(resp.StatusCode, errPayload, resp.Header)
	}
	return aggregateSSE(resp.Body, sseFramed), nil
}

// upstreamHTTPError builds a StatusError the CPA host can use for auth cooldown.
// 429 with CodeBuddy quota-exhausted (14018 / 额度已用尽) gets a long RetryAfter
// so the scheduler stops hammering the same credential.
func upstreamHTTPError(status int, body []byte, headers http.Header) error {
	msg := fmt.Sprintf("upstream %d: %s", status, truncate(string(body), 200))
	err := &statusError{
		Message:    msg,
		Code:       "upstream_error",
		HTTPStatus: status,
		Retryable:  status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500,
	}
	if status == http.StatusTooManyRequests {
		if ra := parseRetryAfterHeader(headers); ra != nil {
			err.retryAfter = ra
		} else if isQuotaExhaustedBody(body) {
			// Permanent-ish quota until the user recharges; cool for max CPA window.
			d := 30 * time.Minute
			err.retryAfter = &d
		}
	}
	return err
}

func parseRetryAfterHeader(headers http.Header) *time.Duration {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	// Seconds form.
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		return &d
	}
	// HTTP-date form.
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d > 0 {
			return &d
		}
	}
	return nil
}

func isQuotaExhaustedBody(body []byte) bool {
	s := string(body)
	if s == "" {
		return false
	}
	// CodeBuddy: {"error":{"data":{"code":14018,"msg":"额度已用尽…"}}}
	if strings.Contains(s, "14018") {
		return true
	}
	if strings.Contains(s, "额度已用尽") {
		return true
	}
	lower := strings.ToLower(s)
	return strings.Contains(lower, "quota") &&
		(strings.Contains(lower, "exhaust") || strings.Contains(lower, "exceed") || strings.Contains(lower, "insufficient"))
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSE reads an upstream SSE stream and emits one chunk per data event.
// Empty-valued delta fields are stripped and the trailing [DONE] is dropped
// (the host appends its own stream terminator). When sseFramed is true each
// payload is emitted as a "data: " line for cross-format translators; otherwise
// the payload is the raw JSON object and the host chat-completions writer adds
// the framing itself.
func aggregateSSE(r io.Reader, sseFramed bool) []pluginapi.ExecutorStreamChunk {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	return chunks
}

// cleanChunkJSON strips empty-valued fields (null/""/[]/{}) from choice deltas
// so strict clients don't trip on {"function_call":null,"tool_calls":[]}.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, _ := obj["messages"].([]any)
	changed := false

	// Normalize role "developer" → "system" (some clients send the Anthropic variant).
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "developer" {
			msg["role"] = "system"
			changed = true
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}

	// Tool-call pairing cleanup: repack scattered results then prune orphans.
	if msgs2, repacked := repackToolResultBlocks(messages); repacked {
		obj["messages"] = msgs2
		messages = msgs2
		changed = true
	}
	if msgs3, pruned := cleanupOrphanToolCalls(messages); pruned {
		obj["messages"] = msgs3
		changed = true
	}

	// Translate max_completion_tokens → max_tokens when max_tokens is absent.
	if mct, ok := obj["max_completion_tokens"]; ok {
		if _, hasMaxTokens := obj["max_tokens"]; !hasMaxTokens {
			obj["max_tokens"] = mct
		}
		delete(obj, "max_completion_tokens")
		changed = true
	}

	if forceMaxThinking(obj) {
		changed = true
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

// repackToolResultBlocks moves non-tool-result messages that appear inside a
// contiguous tool-result block to just after that block. Some clients emit:
//
//	[assistant(tool_calls), tool, tool, user("ok"), tool]
//
// CodeBuddy requires all tool results to directly follow their assistant call,
// so the stray "user" must be relocated. Returns the reordered slice and true
// if any change was made.
func repackToolResultBlocks(msgs []any) ([]any, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	changed := false
	result := make([]any, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			result = append(result, msgs[i])
			i++
			continue
		}
		role, _ := msg["role"].(string)
		// Start of a tool-result block: assistant with tool_calls.
		if role == "assistant" {
			if tc, hasTc := msg["tool_calls"]; hasTc && tc != nil {
				result = append(result, msgs[i])
				i++
				// Collect the contiguous tool-result messages; park non-tool ones.
				var toolResults []any
				var interleaved []any
				for i < len(msgs) {
					next, ok2 := msgs[i].(map[string]any)
					if !ok2 {
						interleaved = append(interleaved, msgs[i])
						i++
						continue
					}
					nr, _ := next["role"].(string)
					if nr == "tool" {
						toolResults = append(toolResults, msgs[i])
						i++
					} else {
						// Not a tool result → end of this block.
						break
					}
				}
				// Append: tool results first, then anything we parked.
				result = append(result, toolResults...)
				if len(interleaved) > 0 {
					result = append(result, interleaved...)
					changed = true
				}
				continue
			}
		}
		result = append(result, msgs[i])
		i++
	}
	return result, changed
}

// cleanupOrphanToolCalls removes tool_call/tool_result pairs that have no
// matching counterpart. An assistant message whose entire tool_calls array
// would become empty has tool_calls removed; a tool message with no matching
// call is dropped entirely.
func cleanupOrphanToolCalls(msgs []any) ([]any, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	// Collect call IDs emitted by assistant messages.
	callIDs := map[string]struct{}{}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, _ := msg["tool_calls"].([]any)
		for _, tc := range tcs {
			tcm, ok2 := tc.(map[string]any)
			if !ok2 {
				continue
			}
			if id, _ := tcm["id"].(string); id != "" {
				callIDs[id] = struct{}{}
			}
		}
	}
	// Collect result IDs from tool messages.
	resultIDs := map[string]struct{}{}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		if id, _ := msg["tool_call_id"].(string); id != "" {
			resultIDs[id] = struct{}{}
		}
	}
	changed := false
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "tool":
			// Drop if its call was never emitted.
			id, _ := msg["tool_call_id"].(string)
			if _, found := callIDs[id]; !found {
				changed = true
				continue
			}
			out = append(out, m)
		case "assistant":
			tcs, _ := msg["tool_calls"].([]any)
			if len(tcs) == 0 {
				out = append(out, m)
				continue
			}
			kept := make([]any, 0, len(tcs))
			for _, tc := range tcs {
				tcm, ok2 := tc.(map[string]any)
				if !ok2 {
					kept = append(kept, tc)
					continue
				}
				id, _ := tcm["id"].(string)
				if _, hasResult := resultIDs[id]; hasResult || id == "" {
					kept = append(kept, tc)
				} else {
					changed = true
				}
			}
			if len(kept) == 0 {
				delete(msg, "tool_calls")
			} else {
				msg["tool_calls"] = kept
			}
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	return out, changed
}

// sanitizeBlockedTemplates rewrites strings that would cause CodeBuddy to
// reject the request or exhibit undesired behaviour. It also neutralises
// the literal "11128" probe string that triggers an upstream blocklist.
func sanitizeBlockedTemplates(s string) string {
	// Fast path: skip expensive replacements when no trigger is present.
	const (
		triggerClaude   = "You are Claude"
		triggerMainBr   = "Main branch"
		triggerCodex    = "You are Codex"
		triggerFeedback = "give feedback to Anthropic"
		trigger11128    = "11128"
		triggerHeader   = "x-anthropic"
		triggerKV       = "cc_"
	)
	hasAny := strings.Contains(s, triggerClaude) ||
		strings.Contains(s, triggerMainBr) ||
		strings.Contains(s, triggerCodex) ||
		strings.Contains(s, triggerFeedback) ||
		strings.Contains(s, trigger11128) ||
		strings.Contains(s, triggerHeader) ||
		strings.Contains(s, triggerKV)
	if !hasAny {
		return s
	}

	// Rule 1: Claude Code identity → generic CLI tool label.
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	// Rule 1b: Codex CLI identity (same pattern, different product name).
	s = strings.ReplaceAll(s,
		"You are Codex, Anthropic's official CLI for Claude.",
		"You are Codex, Anthropic's official CLI tool for Claude.")

	// Rule 2: "Main branch" VCS hint → "Default branch".
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")

	// Rule 3: Anthropic feedback sentence — drop it.
	s = strings.ReplaceAll(s,
		"Use the feedback tool to give feedback to Anthropic.",
		"")

	// Rule 4: Anti-probe — "11128" → "11-128" everywhere.
	s = strings.ReplaceAll(s, "11128", "11-128")

	// Rule 5: Strip "x-anthropic-billing-header:value" segments
	// (colon-separated header:value pairs that begin with x-anthropic).
	// Also strip bare "x-anthropic-billing-header" keys.
	for strings.Contains(s, "x-anthropic") {
		start := strings.Index(s, "x-anthropic")
		end := start + len("x-anthropic")
		// Advance to end of the key name (up to : or whitespace or \n).
		for end < len(s) && s[end] != ':' && s[end] != ' ' && s[end] != '\n' && s[end] != '"' {
			end++
		}
		// If followed by ':value', consume the value too (up to whitespace or comma or newline).
		if end < len(s) && s[end] == ':' {
			end++ // skip ':'
			for end < len(s) && s[end] != ' ' && s[end] != '\n' && s[end] != ',' {
				end++
			}
		}
		s = s[:start] + s[end:]
	}

	// Rule 6: Strip "cc_xxx=...;" key-value pairs (CodeBuddy tracking).
	for strings.Contains(s, "cc_") {
		idx := strings.Index(s, "cc_")
		end := idx + 3
		for end < len(s) && s[end] != ';' && s[end] != ' ' && s[end] != '\n' {
			end++
		}
		if end < len(s) && s[end] == ';' {
			end++ // consume the semicolon
		}
		s = s[:idx] + s[end:]
	}

	return s
}

// forceMaxThinking pins reasoning_effort to "high" for hy3-family models so
// Tencent Hunyuan 3 always reasons at maximum depth. CodeBuddy only honors
// "high" for deep thinking (medium/low/max/xhigh/ultra all fall back to no
// reasoning), so we override whatever the client sent. Returns true if changed.
func forceMaxThinking(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	// After rewriteModelForUpstream the id should already be bare (hy3…); still
	// tolerate a leftover client prefix so thinking is forced when intended.
	bare := stripClientModelPrefix(model, nil)
	if !strings.HasPrefix(bare, "hy3") && !strings.HasPrefix(model, "hy3") {
		return false
	}
	if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
		return false
	}
	obj["reasoning_effort"] = "high"
	return true
}

// rewriteModelForUpstream writes the host-resolved / alias-mapped upstream model
// id into the request body. CPA puts the resolved id on ExecutorRequest.Model
// but leaves payload.model unchanged for same-format chat-completions.
func rewriteModelForUpstream(payload []byte, hostModel string, sa *storedAuth) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	payloadModel, _ := obj["model"].(string)
	resolved := resolveUpstreamModel(hostModel, payloadModel, sa)
	if resolved == "" {
		return payload
	}
	if cur, _ := obj["model"].(string); cur == resolved {
		return payload
	}
	obj["model"] = resolved
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// resolveUpstreamModel prefers the host-resolved model, then the payload model,
// strips client-facing prefixes, and maps credential model_aliases reverse.
func resolveUpstreamModel(hostModel, payloadModel string, sa *storedAuth) string {
	m := strings.TrimSpace(hostModel)
	if m == "" {
		m = strings.TrimSpace(payloadModel)
	}
	if m == "" {
		return ""
	}
	bare := stripClientModelPrefix(m, sa)
	if mapped := mapAliasToUpstream(bare, sa); mapped != "" {
		return mapped
	}
	return bare
}

// stripClientModelPrefix removes credential prefix / provider display prefixes
// that CPA or clients attach (prefix/model, WorkBuddy/model, workbuddy-model).
func stripClientModelPrefix(model string, sa *storedAuth) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	candidates := make([]string, 0, 4)
	if sa != nil {
		if p := strings.TrimSpace(sa.Prefix); p != "" {
			candidates = append(candidates, p)
		}
	}
	candidates = append(candidates, providerName, "WorkBuddy")
	for _, p := range candidates {
		if p == "" {
			continue
		}
		for _, sep := range []string{"/", "-"} {
			prefix := p + sep
			if len(model) > len(prefix) && strings.EqualFold(model[:len(prefix)], prefix) {
				return model[len(prefix):]
			}
		}
	}
	// Generic first-segment strip when it looks like provider/model and the
	// provider segment is not itself a known bare workbuddy model id.
	if i := strings.Index(model, "/"); i > 0 {
		first, rest := model[:i], model[i+1:]
		if rest != "" && !looksLikeWorkbuddyModelID(first) {
			if strings.EqualFold(first, providerName) ||
				strings.EqualFold(first, "WorkBuddy") ||
				(sa != nil && sa.Prefix != "" && strings.EqualFold(first, sa.Prefix)) {
				return rest
			}
		}
	}
	return model
}

func looksLikeWorkbuddyModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	for _, m := range wbModels() {
		if strings.EqualFold(m.ID, id) {
			return true
		}
	}
	// Common bare families even if list drifts.
	return strings.HasPrefix(id, "hy3") ||
		strings.HasPrefix(id, "glm-") ||
		strings.HasPrefix(id, "kimi-") ||
		strings.HasPrefix(id, "minimax-") ||
		strings.HasPrefix(id, "deepseek-")
}

// mapAliasToUpstream maps a client-facing alias to the upstream model name
// using credential model_aliases (name=upstream, alias=client).
func mapAliasToUpstream(model string, sa *storedAuth) string {
	model = strings.TrimSpace(model)
	if model == "" || sa == nil || len(sa.ModelAliases) == 0 {
		return ""
	}
	for _, a := range sa.ModelAliases {
		alias := strings.TrimSpace(a.Alias)
		name := strings.TrimSpace(a.Name)
		if alias == "" || name == "" {
			continue
		}
		if strings.EqualFold(model, alias) {
			return name
		}
	}
	return ""
}

// ensureModelAllowed rejects requests for models listed in the credential's
// excluded_models (defense in depth; CPA host also filters the model registry).
func ensureModelAllowed(payload []byte, sa *storedAuth) error {
	if sa == nil || len(sa.ExcludedModels) == 0 || len(payload) == 0 {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return nil
	}
	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	bare := stripClientModelPrefix(model, sa)
	if mapped := mapAliasToUpstream(bare, sa); mapped != "" {
		bare = mapped
	}
	for _, ex := range sa.ExcludedModels {
		if strings.EqualFold(model, ex) || strings.EqualFold(bare, ex) {
			return fmt.Errorf("model_excluded: %s is excluded on this workbuddy credential", model)
		}
	}
	return nil
}

// aggregateCompletion folds an SSE stream into a single non-streaming
// chat.completion object (used for non-stream client requests).
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	var toolCalls []map[string]any

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							toolCalls = append(toolCalls, call)
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNilMap(vals ...map[string]any) map[string]any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// -----------------------------------------------------------------------------
// Management API (add API key without OAuth paste box)
// -----------------------------------------------------------------------------
//
// CPA's management UI posts paste-box values as oauth-callback with only
// redirect_url set (no state/code). Host rejects that before the plugin runs.
// These routes give a reliable path: authenticated POST + host.auth.save.

type managementRegResponse struct {
	Routes    []managementRouteJSON    `json:"routes,omitempty"`
	Resources []managementResourceJSON `json:"resources,omitempty"`
}

type managementRouteJSON struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}

type managementResourceJSON struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementHandleRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementHandleResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

func hostCallbackResult(method string, request any, target any) error {
	rawRequest, err := json.Marshal(request)
	if err != nil {
		return err
	}
	raw, err := hostCallFn(method, rawRequest)
	if err != nil {
		return fmt.Errorf("host auth callback failed")
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("host auth callback response invalid")
	}
	if !env.OK {
		return fmt.Errorf("host auth callback rejected request")
	}
	if target != nil && json.Unmarshal(env.Result, target) != nil {
		return fmt.Errorf("host auth callback result invalid")
	}
	return nil
}

func cachedAccountSummary(cacheKey string, sa *storedAuth, item pluginapi.HostAuthFileEntry, force bool) accountSummary {
	accountCacheMu.Lock()
	cached := accountCacheMap[cacheKey]
	if !force && cached != nil {
		ttl := accountCacheTTL
		if cached.isError {
			ttl = accountCacheErrTTL
		}
		if time.Since(cached.fetchedAt) < ttl {
			result := cached.summary
			accountCacheMu.Unlock()
			return result
		}
	}
	accountCacheMu.Unlock()

	realm := "cn"
	if isGlobal(sa) {
		realm = "global"
	}
	summary := accountSummary{
		ID: firstNonEmpty(item.AuthIndex, item.ID, sa.authID()), Label: firstNonEmpty(item.Label, sa.label(), item.Name),
		Nickname: sa.Account.Nickname, UID: maskAccountIdentifier(firstNonEmpty(sa.Account.UID, sa.UserID)),
		Realm: realm, AuthType: sa.authMode(), Disabled: item.Disabled || sa.Disabled,
		Models: modelSummariesFor(sa), UpdatedAt: time.Now().UTC(),
	}
	if sa.isAPIKey() {
		summary.Unsupported = true
		summary.Error = "API Key 凭据暂不支持套餐与积分余额查询"
	} else {
		balance, cycleStart, cycleEnd, balanceErr := fetchUserResource(sa, time.Now())
		if balanceErr != nil {
			summary.Error = "套餐或积分余额暂时不可用"
		} else {
			summary.Balance, summary.CycleStart, summary.CycleEnd = balance, cycleStart, cycleEnd
		}
		if plan, planErr := fetchPaymentType(sa); planErr == nil {
			summary.Plan = plan
		}
		if enterprise, start, end, enterpriseErr := fetchEnterpriseUsageCN(sa); enterpriseErr == nil && enterprise != nil {
			summary.Enterprise = enterprise
			if summary.CycleStart == "" {
				summary.CycleStart, summary.CycleEnd = start, end
			}
		}
	}
	accountCacheMu.Lock()
	accountCacheMap[cacheKey] = &accountCacheEntry{summary: summary, fetchedAt: time.Now(), isError: summary.Error != "" && !summary.Unsupported}
	accountCacheMu.Unlock()
	return summary
}

func listAccountSummaries(force bool, onlyID string) ([]accountSummary, error) {
	var listed hostAuthListResponse
	if err := hostCallbackResult(pluginabi.MethodHostAuthList, map[string]any{}, &listed); err != nil {
		return nil, err
	}
	out := make([]accountSummary, 0)
	for _, item := range listed.Files {
		if item.Type != providerName && item.Provider != providerName {
			continue
		}
		if onlyID != "" && onlyID != item.AuthIndex && onlyID != item.ID {
			continue
		}
		var stored pluginapi.HostAuthGetResponse
		if err := hostCallbackResult(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: item.AuthIndex}, &stored); err != nil {
			out = append(out, accountSummary{ID: item.AuthIndex, Label: firstNonEmpty(item.Label, item.Name, "WorkBuddy"), Disabled: item.Disabled, UpdatedAt: time.Now().UTC(), Error: "无法读取该凭据"})
			continue
		}
		sa, err := parseStored(stored.JSON)
		if err != nil {
			out = append(out, accountSummary{ID: item.AuthIndex, Label: firstNonEmpty(item.Label, item.Name, "WorkBuddy"), Disabled: item.Disabled, UpdatedAt: time.Now().UTC(), Error: "该凭据格式无效"})
			continue
		}
		out = append(out, cachedAccountSummary(firstNonEmpty(item.AuthIndex, item.ID, sa.authID()), sa, item, force))
	}
	return out, nil
}

func managementJSON(status int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementHandleResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
}

func saveOAuthPollResult(body []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid login poll request"})
	}
	raw, err := handlePollLogin(body)
	if err != nil {
		return nil, err
	}
	var env envelope
	var poll pluginapi.AuthLoginPollResponse
	if json.Unmarshal(raw, &env) != nil || !env.OK || json.Unmarshal(env.Result, &poll) != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "登录状态读取失败"})
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		return managementJSON(http.StatusOK, map[string]any{"status": poll.Status, "message": poll.Message})
	}
	if len(poll.Auth.StorageJSON) == 0 {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "登录凭据无效"})
	}
	name := strings.TrimSpace(poll.Auth.FileName)
	if name == "" {
		name = authFileName
	}
	if err := hostCallbackResult(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: poll.Auth.StorageJSON}, nil); err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "无法保存登录凭据"})
	}
	return managementJSON(http.StatusOK, map[string]any{"status": "success", "message": "登录凭据已保存", "id": poll.Auth.ID, "label": poll.Auth.Label})
}

func wbManagementRegistration() managementRegResponse {
	return managementRegResponse{
		// Authenticated management API (needs management Bearer token).
		Routes: []managementRouteJSON{
			{Method: "POST", Path: "/workbuddy/api-key"},
			{Method: "GET", Path: "/workbuddy/api-key"},
			{Method: "GET", Path: "/workbuddy/accounts"},
			{Method: "POST", Path: "/workbuddy/accounts/refresh"},
			{Method: "POST", Path: "/workbuddy/oauth/start"},
			{Method: "POST", Path: "/workbuddy/oauth/poll"},
		},
		// Browser menu under /v0/resource/plugins/workbuddy/...
		Resources: []managementResourceJSON{
			{
				Path:        "/api-key",
				Menu:        "WorkBuddy 管理",
				Description: "账户概览、套餐与积分余额、国内版/国际版登录、API Key 管理。",
			},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementHandleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimSpace(req.Path)

	switch {
	case method == "GET" && strings.HasSuffix(path, "/api-key") && strings.Contains(path, "/resource/plugins/"+providerName):
		return okEnvelope(managementHTMLResponse())
	case method == "GET" && (path == "/v0/management/workbuddy/accounts" || strings.HasSuffix(path, "/workbuddy/accounts")):
		accounts, err := listAccountSummaries(false, "")
		if err != nil {
			return managementJSON(http.StatusBadGateway, map[string]string{"error": "无法读取 WorkBuddy 凭据"})
		}
		return managementJSON(http.StatusOK, map[string]any{"accounts": accounts, "updated_at": time.Now().UTC()})
	case method == "POST" && (path == "/v0/management/workbuddy/accounts/refresh" || strings.HasSuffix(path, "/workbuddy/accounts/refresh")):
		var payload struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Body, &payload)
		accounts, err := listAccountSummaries(true, strings.TrimSpace(payload.ID))
		if err != nil {
			return managementJSON(http.StatusBadGateway, map[string]string{"error": "账户刷新失败"})
		}
		return managementJSON(http.StatusOK, map[string]any{"accounts": accounts, "updated_at": time.Now().UTC()})
	case method == "POST" && (path == "/v0/management/workbuddy/oauth/start" || strings.HasSuffix(path, "/workbuddy/oauth/start")):
		var payload struct {
			Realm string `json:"realm"`
		}
		_ = json.Unmarshal(req.Body, &payload)
		global := strings.EqualFold(strings.TrimSpace(payload.Realm), "global")
		started, err := startLoginForRealm(pluginapi.AuthLoginStartRequest{}, global)
		if err != nil {
			return managementJSON(http.StatusBadGateway, map[string]string{"error": "无法发起登录"})
		}
		return managementJSON(http.StatusOK, map[string]any{"state": started.State, "url": started.URL, "realm": map[bool]string{false: "cn", true: "global"}[global], "expires_at": started.ExpiresAt})
	case method == "POST" && (path == "/v0/management/workbuddy/oauth/poll" || strings.HasSuffix(path, "/workbuddy/oauth/poll")):
		return saveOAuthPollResult(req.Body)
	case method == "GET" && (path == "/v0/management/workbuddy/api-key" || strings.HasSuffix(path, "/workbuddy/api-key")):
		return managementJSON(http.StatusOK, map[string]any{"provider": providerName, "usage": "POST /v0/management/workbuddy/api-key with JSON {\"api_key\":\"...\"}"})
	case method == "POST" && (path == "/v0/management/workbuddy/api-key" || strings.HasSuffix(path, "/workbuddy/api-key")):
		return handleSaveAPIKey(req.Body)
	default:
		return managementJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func managementHTMLResponse() managementHandleResponse {
	return managementHandleResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       []byte(apiKeyPageHTML),
	}
}

func handleSaveAPIKey(body []byte) ([]byte, error) {
	var payload struct {
		APIKey         string       `json:"api_key"`
		APIKeyCamel    string       `json:"apiKey"`
		Key            string       `json:"key"`
		UserID         string       `json:"user_id"`
		Domain         string       `json:"domain"`
		EnterpriseID   string       `json:"enterprise_id"`
		Prefix         string       `json:"prefix"`
		ProxyURL       string       `json:"proxy_url"`
		Priority       flexInt      `json:"priority"`
		Disabled       bool         `json:"disabled"`
		ExcludedModels []string     `json:"excluded_models"`
		ModelAliases   []modelAlias `json:"model_aliases"`
	}
	// Allow raw text body as the key itself.
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" && !strings.HasPrefix(trimmed, "{") {
		payload.APIKey = trimmed
	} else if err := json.Unmarshal(body, &payload); err != nil {
		return okEnvelope(managementHandleResponse{
			StatusCode: 400,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"error":"invalid json; expected {\"api_key\":\"...\"}"}`),
		})
	}
	key := strings.TrimSpace(firstNonEmpty(payload.APIKey, payload.APIKeyCamel, payload.Key))
	if !looksLikeAPIKey(key) {
		return okEnvelope(managementHandleResponse{
			StatusCode: 400,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"error":"api_key missing or invalid"}`),
		})
	}
	sa := &storedAuth{
		Type:           providerName,
		AuthType:       authTypeAPIKey,
		APIKey:         key,
		UserID:         firstNonEmpty(strings.TrimSpace(payload.UserID), "anonymous"),
		Domain:         firstNonEmpty(strings.TrimSpace(payload.Domain), "copilot.tencent.com"),
		EnterpriseID:   strings.TrimSpace(payload.EnterpriseID),
		Prefix:         payload.Prefix,
		ProxyURL:       payload.ProxyURL,
		Priority:       payload.Priority,
		Disabled:       payload.Disabled,
		ExcludedModels: payload.ExcludedModels,
		ModelAliases:   payload.ModelAliases,
	}
	normalizeStored(sa)
	fileName := sa.authID() + ".json"
	// Persist full CPA-compatible auth file (type + standard fields at root).
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	saveReq, _ := json.Marshal(map[string]any{
		"name": fileName,
		"json": json.RawMessage(storage),
	})
	if _, err := hostCallFn(pluginabi.MethodHostAuthSave, saveReq); err != nil {
		// Never return storage here: it contains the supplied API key.
		return managementJSON(http.StatusBadGateway, map[string]any{
			"error":    "无法保存 API Key 凭据",
			"fileName": fileName,
		})
	}
	out, _ := json.Marshal(map[string]any{
		"status":          "ok",
		"provider":        providerName,
		"fileName":        fileName,
		"id":              sa.authID(),
		"label":           sa.label(),
		"prefix":          sa.Prefix,
		"proxy_url":       sa.ProxyURL,
		"priority":        sa.Priority.Int(),
		"disabled":        sa.Disabled,
		"excluded_models": sa.ExcludedModels,
		"model_aliases":   sa.ModelAliases,
	})
	return okEnvelope(managementHandleResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       out,
	})
}

// apiKeyPageHTML is served at /v0/resource/plugins/workbuddy/api-key.
// It reuses the current CPA control-panel authorization in same-origin browser
// storage without rendering, persisting, or forwarding that authorization.
const apiKeyPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>WorkBuddy 管理</title>
<style>
:root{font-family:system-ui,sans-serif;color-scheme:light dark;--bg:#f4f4f5;--card:#fff;--border:#ddd;--text:#111;--muted:#555;--btn:#111;--btnfg:#fff}
@media(prefers-color-scheme:dark){:root{--bg:#09090b;--card:#18181b;--border:#333;--text:#fafafa;--muted:#aaa;--btn:#fafafa;--btnfg:#09090b}}
html[data-theme=dark]{--bg:#09090b;--card:#18181b;--border:#333;--text:#fafafa;--muted:#aaa;--btn:#fafafa;--btnfg:#09090b}
html[data-theme=light]{--bg:#f4f4f5;--card:#fff;--border:#ddd;--text:#111;--muted:#555;--btn:#111;--btnfg:#fff}
body{max-width:1000px;margin:28px auto;padding:0 16px;background:var(--bg);color:var(--text)}
.card{background:var(--card);border:1px solid var(--border);padding:16px;border-radius:10px;margin:12px 0}
.row,.tabs,.summary{display:flex;gap:8px;flex-wrap:wrap;align-items:center}.tabs{margin:16px 0}
button{padding:9px 12px;border:0;border-radius:6px;background:var(--btn);color:var(--btnfg);font-weight:600;cursor:pointer}
button:disabled{cursor:not-allowed;opacity:.55}.secondary{background:transparent;color:var(--text);border:1px solid var(--border)}.active{outline:2px solid var(--text)}
input,textarea,select{box-sizing:border-box;width:100%;padding:9px;border:1px solid var(--border);border-radius:6px;background:var(--card);color:var(--text)}
label{display:block;margin:10px 0 4px;color:var(--muted);font-size:13px}.metric{min-width:120px;border:1px solid var(--border);border-radius:7px;padding:8px}.metric small{color:var(--muted)}.metric strong{display:block;margin-top:4px}
.badge{font-size:12px;border:1px solid var(--border);border-radius:99px;padding:2px 6px;margin:2px}.models{width:100%;border-collapse:collapse;margin-top:12px;font-size:12px}.models th,.models td{border-top:1px solid var(--border);padding:7px;text-align:left}
.err{color:#c22}.ok{color:#187a35}.muted,p{color:var(--muted);font-size:13px}textarea{min-height:72px;font-family:ui-monospace,monospace}.panel[hidden]{display:none}.actions{margin-top:8px}
</style>
</head>
<body>
<div class="row">
  <div>
    <h1>WorkBuddy 管理</h1>
    <p>页面不会显示、保存或向上游发送 API Key、OAuth Token 或管理密钥；仅在浏览器内复用已登录 CPA 管理中心的授权访问同源管理 API。</p>
  </div>
  <button class="secondary" onclick="toggleTheme()">深色/浅色</button>
</div>
<div class="card">
  <div class="row">
    <div>
      <h2>账户概览</h2>
      <p>套餐和积分余额仅由插件后端用 OAuth 凭据查询。</p>
    </div>
  </div>
  <div class="row actions">
    <button class="secondary" onclick="loadAccounts(false)">加载账户</button>
    <button class="secondary" onclick="loadAccounts(true)">刷新全部</button>
    <span id="message" role="status" aria-live="polite"></span>
  </div>
</div>
<div class="tabs">
  <button class="tab active" data-id="overview" onclick="switchTab('overview')">账户概览</button>
  <button class="tab" data-id="login" onclick="switchTab('login')">OAuth 登录</button>
  <button class="tab" data-id="key" onclick="switchTab('key')">添加 API Key</button>
</div>
<section class="panel" id="overview">
  <div class="summary" id="summary"></div>
  <div id="accounts"><p>正在加载账户…</p></div>
</section>
<section class="panel" id="login" hidden>
  <div class="card">
    <h2>OAuth 登录</h2>
    <p>请选择账号所属区域，完成浏览器授权后本页会自动保存凭据。</p>
    <button onclick="startLogin('cn')">登录国内版（CodeBuddy）</button>
    <button class="secondary" onclick="startLogin('global')">登录国际版（WorkBuddy）</button>
    <p id="loginMsg" role="status" aria-live="polite"></p>
  </div>
</section>
<section class="panel" id="key" hidden>
  <div class="card">
    <h2>添加 API Key</h2>
    <p>API Key 可用于模型调用；上游未确认它支持套餐/积分余额接口，因此概览会明确标记为不支持。</p>
    <label>CodeBuddy API Key</label><textarea id="keyValue" placeholder="粘贴 API Key"></textarea>
    <label>区域</label><select id="domain"><option value="copilot.tencent.com">国内版</option><option value="www.workbuddy.ai">国际版</option></select>
    <label>User ID（可选，默认 anonymous）</label><input id="uid" value="anonymous">
    <label>prefix（可选）</label><input id="prefix">
    <label>proxy_url（可选）</label><input id="proxy" placeholder="http://127.0.0.1:7890">
    <label>priority（可选）</label><input id="priority" type="number" placeholder="0">
    <label>excluded_models（可选，逗号分隔）</label><input id="excluded">
    <label>model_aliases JSON（可选）</label><textarea id="aliases" placeholder='[{"name":"hy3-preview-agent","alias":"hy3"}]'></textarea>
    <label><input id="disabled" type="checkbox" style="width:auto"> 创建后立即禁用</label>
    <button id="saveKey" onclick="saveAPIKey()">保存 API Key</button>
    <span id="keyMsg" role="status" aria-live="polite"></span>
  </div>
</section>
<script>
const base = '/v0/management/workbuddy/';
const panelAuthStorageKey = 'cli-proxy-auth';
const panelAuthPrefix = 'enc::v1::';
const panelAuthSalt = 'cli-proxy-api-webui::secure-storage';
const summaryEl = document.getElementById('summary');
const accountsEl = document.getElementById('accounts');
const keyValueEl = document.getElementById('keyValue');
const domainEl = document.getElementById('domain');
const uidEl = document.getElementById('uid');
const prefixEl = document.getElementById('prefix');
const proxyEl = document.getElementById('proxy');
const priorityEl = document.getElementById('priority');
const excludedEl = document.getElementById('excluded');
const aliasesEl = document.getElementById('aliases');
const disabledEl = document.getElementById('disabled');
const saveKeyEl = document.getElementById('saveKey');
let managementKey = '';
let loginTimer;

function setMessage(id, text, bad) {
  const element = document.getElementById(id);
  element.textContent = text;
  element.className = bad ? 'err' : 'ok';
}

function restoreTheme() {
  try {
    const theme = localStorage.getItem('wb_theme');
    if (theme) document.documentElement.dataset.theme = theme;
  } catch (_) {}
}

function toggleTheme() {
  const theme = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
  document.documentElement.dataset.theme = theme;
  try { localStorage.setItem('wb_theme', theme); } catch (_) {}
}

function decodePanelAuth(value) {
  let decoded = value;
  if (decoded.startsWith(panelAuthPrefix)) {
    const encrypted = atob(decoded.slice(panelAuthPrefix.length));
    const key = new TextEncoder().encode(panelAuthSalt + '|' + location.host + '|' + navigator.userAgent);
    const bytes = new Uint8Array(encrypted.length);
    for (let index = 0; index < encrypted.length; index += 1) {
      bytes[index] = encrypted.charCodeAt(index) ^ key[index % key.length];
    }
    decoded = new TextDecoder().decode(bytes);
  }
  return JSON.parse(decoded);
}

function readPanelManagementKey() {
  try {
    const saved = localStorage.getItem(panelAuthStorageKey);
    if (saved) {
      const auth = decodePanelAuth(saved);
      const key = auth && auth.state ? auth.state.managementKey : auth && auth.managementKey;
      if (typeof key === 'string' && key.trim()) return key.trim();
    }
    for (const storageKey of ['managementKey', 'management_key', 'cpa_management_key']) {
      const key = localStorage.getItem(storageKey);
      if (key && key.trim()) return key.trim();
    }
  } catch (_) {}
  return '';
}

function switchTab(id, refreshOverview) {
  document.querySelectorAll('.panel').forEach((panel) => { panel.hidden = panel.id !== id; });
  document.querySelectorAll('.tab').forEach((tab) => { tab.classList.toggle('active', tab.dataset.id === id); });
  if (id === 'overview' && refreshOverview !== false) loadAccounts(false);
}

function managementError(status, payload) {
  if (status === 401 || status === 403) return 'CPA 管理授权已失效，请重新登录管理中心并启用“记住密码”。';
  if (status === 404) return 'CPA 管理 API 未启用。请配置管理密码后重新打开此页。';
  if (payload && typeof payload.error === 'string' && payload.error) return payload.error;
  return '管理请求失败（HTTP ' + status + '）。';
}

async function callManagement(path, method, body) {
  if (!managementKey) {
    throw new Error('未检测到 CPA 管理授权。请在管理中心登录时启用“记住密码”，然后刷新本页。');
  }
  const response = await fetch(base + path, {
    method: method || 'GET',
    headers: { Authorization: 'Bearer ' + managementKey, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await response.text();
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch (_) {}
  }
  if (!response.ok) throw new Error(managementError(response.status, payload));
  if (!payload) throw new Error('管理服务返回了无法识别的响应，请刷新页面后重试。');
  return payload;
}

function addMetric(parent, title, value) {
  const item = document.createElement('div');
  item.className = 'metric';
  const label = document.createElement('small');
  const strong = document.createElement('strong');
  label.textContent = title;
  strong.textContent = value;
  item.append(label, strong);
  parent.append(item);
}

function displayValue(value) {
  return value === undefined || value === null || value === '' ? '—' : String(value);
}

function displayCredits(value) {
  return value ? displayValue(value.remaining) + ' / ' + displayValue(value.total) + ' ' + displayValue(value.unit) : '暂未提供';
}

function renderAccounts(items) {
  summaryEl.replaceChildren();
  accountsEl.replaceChildren();
  let cn = 0;
  let global = 0;
  let enabled = 0;
  items.forEach((account) => {
    if (account.realm === 'global') global += 1; else cn += 1;
    if (!account.disabled) enabled += 1;
  });
  [['凭据', items.length], ['可用', enabled], ['国内版', cn], ['国际版', global]].forEach((item) => addMetric(summaryEl, item[0], item[1]));
  if (!items.length) {
    accountsEl.textContent = '尚未保存 WorkBuddy 凭据。';
    return;
  }
  items.forEach((account) => {
    const card = document.createElement('article');
    card.className = 'card';
    const heading = document.createElement('h2');
    heading.textContent = displayValue(account.label);
    card.append(heading);
    const badges = document.createElement('div');
    badges.className = 'row';
    [account.realm, account.auth_type, account.disabled ? '已禁用' : '已启用'].forEach((badge) => {
      const element = document.createElement('span');
      element.className = 'badge';
      element.textContent = displayValue(badge);
      badges.append(element);
    });
    card.append(badges);
    const metrics = document.createElement('div');
    metrics.className = 'summary';
    const identity = account.nickname || account.uid || '—';
    [['账号', identity], ['套餐', displayValue(account.plan)], ['积分余额', displayCredits(account.balance)], ['企业额度', displayCredits(account.enterprise)], ['周期', account.cycle_start || account.cycle_end ? displayValue(account.cycle_start) + ' ~ ' + displayValue(account.cycle_end) : '—']].forEach((item) => addMetric(metrics, item[0], item[1]));
    card.append(metrics);
    if (account.error) {
      const error = document.createElement('p');
      error.textContent = account.error;
      error.className = account.unsupported ? 'muted' : 'err';
      card.append(error);
    }
    if (account.models && account.models.length) {
      const table = document.createElement('table');
      table.className = 'models';
      table.innerHTML = '<thead><tr><th>模型</th><th>倍率</th><th>上下文</th><th>最大输出</th><th>能力</th></tr></thead>';
      const body = document.createElement('tbody');
      account.models.forEach((model) => {
        const row = document.createElement('tr');
        [model.name || model.id, model.credits || '—', model.context || '—', model.max_output || '—', [model.images ? '图片' : '', model.reasoning ? '推理' : '', model.tool_call ? '工具' : ''].filter(Boolean).join(' / ') || '—'].forEach((value) => {
          const cell = document.createElement('td');
          cell.textContent = String(value);
          row.append(cell);
        });
        body.append(row);
      });
      table.append(body);
      card.append(table);
    }
    const refresh = document.createElement('button');
    refresh.className = 'secondary';
    refresh.textContent = '刷新此账户';
    refresh.onclick = () => loadAccounts(true, account.id);
    card.append(refresh);
    accountsEl.append(card);
  });
}

async function loadAccounts(force, id) {
  try {
    setMessage('message', '加载中…');
    const response = await callManagement(force ? 'accounts/refresh' : 'accounts', force ? 'POST' : 'GET', force ? { id: id || '' } : null);
    renderAccounts(response.accounts || []);
    setMessage('message', '已更新');
  } catch (error) {
    setMessage('message', error.message, true);
  }
}

async function startLogin(realm) {
  try {
    setMessage('loginMsg', '正在创建登录会话…');
    const response = await callManagement('oauth/start', 'POST', { realm: realm });
    const popup = window.open(response.url, 'workbuddy-login');
    if (!popup) {
      setMessage('loginMsg', '请允许浏览器打开登录窗口。', true);
      return;
    }
    setMessage('loginMsg', '已打开登录窗口，正在等待授权…');
    clearInterval(loginTimer);
    loginTimer = setInterval(() => pollLogin(response.state), 2500);
  } catch (error) {
    setMessage('loginMsg', error.message, true);
  }
}

async function pollLogin(state) {
  try {
    const response = await callManagement('oauth/poll', 'POST', { state: state });
    if (response.status === 'success') {
      clearInterval(loginTimer);
      setMessage('loginMsg', '登录成功，凭据已保存。');
      switchTab('overview', false);
      loadAccounts(true);
    } else if (response.status === 'error') {
      clearInterval(loginTimer);
      setMessage('loginMsg', response.message || '登录失败', true);
    }
  } catch (error) {
    clearInterval(loginTimer);
    setMessage('loginMsg', error.message, true);
  }
}

async function saveAPIKey() {
  const key = keyValueEl.value.trim();
  if (!key) {
    setMessage('keyMsg', '请填写 API Key', true);
    return;
  }
  try {
    const aliases = aliasesEl.value.trim();
    const body = {
      api_key: key,
      user_id: uidEl.value.trim() || 'anonymous',
      domain: domainEl.value,
      prefix: prefixEl.value.trim(),
      proxy_url: proxyEl.value.trim(),
      disabled: disabledEl.checked,
    };
    if (priorityEl.value.trim()) body.priority = Number(priorityEl.value);
    if (excludedEl.value.trim()) body.excluded_models = excludedEl.value.split(',').map((value) => value.trim()).filter(Boolean);
    if (aliases) body.model_aliases = JSON.parse(aliases);
    saveKeyEl.disabled = true;
    const response = await callManagement('api-key', 'POST', body);
    keyValueEl.value = '';
    setMessage('keyMsg', '已保存：' + (response.fileName || response.id || ''));
    loadAccounts(true);
  } catch (error) {
    setMessage('keyMsg', error.message, true);
  } finally {
    saveKeyEl.disabled = false;
  }
}

restoreTheme();
managementKey = readPanelManagementKey();
if (managementKey) {
  loadAccounts(false);
} else {
  accountsEl.textContent = '未检测到 CPA 管理授权。请在管理中心登录时启用“记住密码”，然后刷新本页。';
  setMessage('message', '未检测到已保存的 CPA 管理授权。', true);
}
</script>
</body>
</html>`

// -----------------------------------------------------------------------------
// envelope helpers
// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	err := &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}
	if httpStatus == http.StatusTooManyRequests || httpStatus == http.StatusRequestTimeout || httpStatus >= 500 {
		err.Retryable = true
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: err})
	return raw
}

func errorEnvelopeFromErr(err error) []byte {
	if err == nil {
		return errorEnvelope("plugin_error", "plugin call failed", 0)
	}
	// Prefer typed statusError (and any StatusCode() implementer).
	type statusCoder interface {
		StatusCode() int
	}
	code := "plugin_error"
	status := 0
	retryable := false
	message := err.Error()
	if se, ok := err.(*statusError); ok && se != nil {
		if se.Code != "" {
			code = se.Code
		}
		status = se.HTTPStatus
		retryable = se.Retryable
		message = se.Message
	} else if sc, ok := err.(statusCoder); ok && sc != nil {
		status = sc.StatusCode()
	}
	envErr := &envelopeError{Code: code, Message: message, HTTPStatus: status, Retryable: retryable}
	if !envErr.Retryable && (status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500) {
		envErr.Retryable = true
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: envErr})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
