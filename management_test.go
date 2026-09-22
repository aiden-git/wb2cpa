package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func managementEnvelope(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := okEnvelope(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBillingBaseForRealm(t *testing.T) {
	cn := &storedAuth{Domain: "copilot.tencent.com"}
	global := &storedAuth{Domain: "www.workbuddy.ai"}
	if got := billingBaseFor(cn); got != cnOrigin {
		t.Fatalf("CN billing base = %q, want %q", got, cnOrigin)
	}
	if got := billingBaseFor(global); got != globalBase {
		t.Fatalf("Global billing base = %q, want %q", got, globalBase)
	}
}

func TestOAuthEndpointsFollowRealm(t *testing.T) {
	if got := authStateEndpointFor(false); !strings.HasPrefix(got, upstreamBase) {
		t.Fatalf("CN state endpoint = %q", got)
	}
	if got := authStateEndpointFor(true); !strings.HasPrefix(got, globalBase) {
		t.Fatalf("Global state endpoint = %q", got)
	}
	if got := tokenRefreshEndpointFor(&storedAuth{Domain: "www.workbuddy.ai"}); !strings.HasPrefix(got, globalBase) {
		t.Fatalf("Global refresh endpoint = %q", got)
	}
}

func TestManagementRegistrationIncludesOverviewRoutes(t *testing.T) {
	reg := wbManagementRegistration()
	got := map[string]bool{}
	for _, route := range reg.Routes {
		got[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{"GET /workbuddy/accounts", "POST /workbuddy/accounts/refresh", "POST /workbuddy/oauth/start", "POST /workbuddy/oauth/poll"} {
		if !got[route] {
			t.Fatalf("management route %q missing from %#v", route, reg.Routes)
		}
	}
	resources := map[string]bool{}
	for _, resource := range reg.Resources {
		resources[resource.Path] = true
	}
	for _, path := range []string{"/api-key", "/global-oauth"} {
		if !resources[path] {
			t.Fatalf("management resource %q missing from %#v", path, reg.Resources)
		}
	}
}

func TestManagementResourcePageUsesPanelAuthorization(t *testing.T) {
	rawReq, err := json.Marshal(managementHandleRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/workbuddy/api-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleManagement(rawReq)
	if err != nil {
		t.Fatal(err)
	}
	var outer envelope
	if err := json.Unmarshal(raw, &outer); err != nil || !outer.OK {
		t.Fatalf("resource envelope = %s, err = %v", raw, err)
	}
	var response managementHandleResponse
	if err := json.Unmarshal(outer.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("resource status = %d, want 200", response.StatusCode)
	}
	if got := response.Headers["Content-Type"]; len(got) != 1 || got[0] != "text/html; charset=utf-8" {
		t.Fatalf("resource Content-Type = %#v", got)
	}

	page := string(response.Body)
	for _, forbidden := range []string{
		`id="token"`,
		`Management Token`,
		`q.get(`,
		`请填写 Management Token`,
		`split(/[`,
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("management page still contains forbidden %q", forbidden)
		}
	}
	for _, required := range []string{
		`cli-proxy-auth`,
		`enc::v1::`,
		`cli-proxy-api-webui::secure-storage`,
		`Authorization: 'Bearer ' + managementKey`,
		`function loadAccounts`,
		`onclick="startLogin('cn')"`,
		`id="globalLogin"`,
		`onclick="startLogin('global')"`,
		`globalLoginEntry`,
		`credit_packages`,
		`平台积分明细`,
		`function displayReasoningEfforts`,
		`reasoning_efforts`,
		`推理档位`,
		`split(',')`,
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("management page missing %q", required)
		}
	}
}

func TestGlobalOAuthResourcePage(t *testing.T) {
	rawReq, err := json.Marshal(managementHandleRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/workbuddy/global-oauth",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleManagement(rawReq)
	if err != nil {
		t.Fatal(err)
	}
	var outer envelope
	if err := json.Unmarshal(raw, &outer); err != nil || !outer.OK {
		t.Fatalf("global resource envelope = %s, err = %v", raw, err)
	}
	var response managementHandleResponse
	if err := json.Unmarshal(outer.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("global resource status = %d, want 200", response.StatusCode)
	}
	page := string(response.Body)
	for _, required := range []string{`globalLoginEntry`, `id="globalLogin"`, `startLogin('global')`} {
		if !strings.Contains(page, required) {
			t.Fatalf("global resource page missing %q", required)
		}
	}
}

func TestMaskAccountIdentifier(t *testing.T) {
	if got := maskAccountIdentifier("123456789"); got != "123…6789" {
		t.Fatalf("maskAccountIdentifier leaked or malformed: %q", got)
	}
	if got := maskAccountIdentifier("1234"); got != "1234" {
		t.Fatalf("short account mask = %q", got)
	}
}

func TestReasoningEffortsFor(t *testing.T) {
	model := dynModelEntry{}
	model.Reasoning.DefaultEffort = " medium "
	model.Reasoning.SupportedEfforts = []string{"low", "medium", "high", "medium", " "}

	efforts, defaultEffort := reasoningEffortsFor(model)
	if got := strings.Join(efforts, ","); got != "low,medium,high" || defaultEffort != "medium" {
		t.Fatalf("reasoning effort projection = %#v, default = %q", efforts, defaultEffort)
	}

	model.Reasoning.DefaultEffort = "unsupported"
	efforts, defaultEffort = reasoningEffortsFor(model)
	if got := strings.Join(efforts, ","); got != "low,medium,high" || defaultEffort != "" {
		t.Fatalf("unsupported default must be omitted: %#v, default = %q", efforts, defaultEffort)
	}

	model.Reasoning.SupportedEfforts = nil
	efforts, defaultEffort = reasoningEffortsFor(model)
	if len(efforts) != 0 || defaultEffort != "" {
		t.Fatalf("missing efforts = %#v, default = %q", efforts, defaultEffort)
	}
}

func TestAccountOverviewRedactsAPIKey(t *testing.T) {
	oldHost := hostCallFn
	oldModels := modelCacheMap
	oldAccounts := accountCacheMap
	defer func() {
		hostCallFn = oldHost
		modelCacheMap = oldModels
		accountCacheMap = oldAccounts
	}()

	secret := "wb_test_super_secret_key"
	modelCacheMap = map[string]*modelCacheEntry{
		"cn": {
			fetchedAt: time.Now(),
			details: []dynModelEntry{{
				ID: "glm-5.2", Name: "GLM-5.2", Credits: "1.0", MaxInputTokens: 131072, MaxOutputTokens: 8192, SupportsToolCall: true, SupportsReasoning: true,
				Reasoning: struct {
					DefaultEffort    string   `json:"defaultEffort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				}{DefaultEffort: "medium", SupportedEfforts: []string{"low", "medium", "high"}},
			}},
		},
	}
	accountCacheMap = map[string]*accountCacheEntry{}
	storage, err := json.Marshal(storedAuth{
		Type: providerName, AuthType: authTypeAPIKey, APIKey: secret, UserID: "123456789", Domain: "copilot.tencent.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	hostCallFn = func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return managementEnvelope(t, hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{ID: "credential-1", AuthIndex: "credential-1", Name: "workbuddy-key.json", Type: providerName, Label: "My key"}}}), nil
		case pluginabi.MethodHostAuthGet:
			return managementEnvelope(t, pluginapi.HostAuthGetResponse{AuthIndex: "credential-1", JSON: storage}), nil
		default:
			t.Fatalf("unexpected host method %q", method)
			return nil, nil
		}
	}

	rawReq, _ := json.Marshal(managementHandleRequest{Method: "GET", Path: "/v0/management/workbuddy/accounts"})
	raw, err := handleManagement(rawReq)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("management accounts response leaked API key")
	}
	var outer envelope
	if err := json.Unmarshal(raw, &outer); err != nil || !outer.OK {
		t.Fatalf("management envelope = %s, err = %v", raw, err)
	}
	var response managementHandleResponse
	if err := json.Unmarshal(outer.Result, &response); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Accounts []accountSummary `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts = %#v", body.Accounts)
	}
	account := body.Accounts[0]
	if account.UID != "123…6789" {
		t.Fatalf("UID = %q, want masked value", account.UID)
	}
	if !account.Unsupported || !strings.Contains(account.Error, "API Key") {
		t.Fatalf("API key overview = %#v, want explicit unsupported balance", account)
	}
	if len(account.Models) != 1 || account.Models[0].Credits != "1.0" || !account.Models[0].ToolCall {
		t.Fatalf("model summary = %#v", account.Models)
	}
	if got := strings.Join(account.Models[0].ReasoningEfforts, ","); got != "low,medium,high" || account.Models[0].DefaultReasoningEffort != "medium" {
		t.Fatalf("reasoning efforts = %#v", account.Models[0])
	}
}

func TestSummarizeCreditPackages(t *testing.T) {
	packages, balance, cycleStart, cycleEnd := summarizeCreditPackages([]billingPackage{
		{
			PackageName: "每日奖励", CapacityRemain: 90, CapacityUsed: 10, CapacitySize: 100,
			CycleCapacityRemain: 7, CycleCapacityUsed: 3, CycleCapacitySize: 10,
			CycleStartTime: "2026-09-01 00:00:00", CycleEndTime: "2026-10-01 00:00:00",
		},
		{
			PackageName: "限时奖励", CapacityRemain: 4, CapacityUsed: 5, CapacitySize: 9,
			CycleStartTime: "2026-09-02 00:00:00", CycleEndTime: "2026-10-02 00:00:00",
		},
		{
			CapacityRemain: 0, CapacityUsed: 100, CapacitySize: 100,
			CycleStartTime: "2026-09-03 00:00:00", CycleEndTime: "2026-10-03 00:00:00",
		},
		{PackageName: "忽略的空额度"},
	})
	if len(packages) != 3 {
		t.Fatalf("package count = %d, want 3", len(packages))
	}
	if packages[0].Total != 10 || packages[0].Remaining != 7 || packages[0].Used != 3 {
		t.Fatalf("cycle capacity package = %#v", packages[0])
	}
	if !packages[0].Available || !packages[1].Available || packages[2].Available {
		t.Fatalf("package availability = %#v", packages)
	}
	if packages[2].Name != "平台积分" {
		t.Fatalf("empty package name = %q", packages[2].Name)
	}
	if balance == nil || balance.Remaining != 11 || balance.Used != 108 || balance.Total != 119 || balance.Unit != "credits" {
		t.Fatalf("aggregate balance = %#v", balance)
	}
	if cycleStart != "2026-09-01 00:00:00" || cycleEnd != "2026-10-01 00:00:00" {
		t.Fatalf("summary cycle = %q to %q", cycleStart, cycleEnd)
	}
}
