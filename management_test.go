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
}

func TestMaskAccountIdentifier(t *testing.T) {
	if got := maskAccountIdentifier("123456789"); got != "123…6789" {
		t.Fatalf("maskAccountIdentifier leaked or malformed: %q", got)
	}
	if got := maskAccountIdentifier("1234"); got != "1234" {
		t.Fatalf("short account mask = %q", got)
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
				ID: "glm-5.2", Name: "GLM-5.2", Credits: "1.0", MaxInputTokens: 131072, MaxOutputTokens: 8192, SupportsToolCall: true,
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
}

func TestBillingPackageUsesCycleCapacity(t *testing.T) {
	selected := billingPackage{
		CapacityRemain: 90, CapacityUsed: 10, CapacitySize: 100,
		CycleCapacityRemain: 7, CycleCapacityUsed: 3, CycleCapacitySize: 10,
	}
	remaining, used, total := selected.CapacityRemain, selected.CapacityUsed, selected.CapacitySize
	if selected.CycleCapacitySize > 0 {
		remaining, used, total = selected.CycleCapacityRemain, selected.CycleCapacityUsed, selected.CycleCapacitySize
	}
	if remaining != 7 || used != 3 || total != 10 {
		t.Fatalf("cycle capacity selection = %v/%v/%v", remaining, used, total)
	}
}
