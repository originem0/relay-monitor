package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relay-monitor/internal/checker"
	"relay-monitor/internal/config"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/store"
)

func boolPtr(v bool) *bool {
	return &v
}

func TestNewParsesClientModeTemplates(t *testing.T) {
	if _, err := New(config.DefaultConfig(), nil, nil, nil); err != nil {
		t.Fatalf("New failed to parse templates: %v", err)
	}
}

func TestHandleProviderUsesStoredSnapshotWithoutCallingUpstream(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"live-model"}]}`))
	}))
	defer upstream.Close()

	st, err := store.New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()
	providerID, err := st.UpsertProvider("slow-relay", upstream.URL+"/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	if err := st.ApplyProviderCheck("run-1", providerID, []store.CheckResultInput{{
		Model: "stored-model", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 100,
	}}, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("seed current result: %v", err)
	}

	srv, err := New(config.DefaultConfig(), st, &checker.Engine{Client: upstream.Client()}, []provider.Provider{{
		Name: "slow-relay", BaseURL: upstream.URL + "/v1", APIKey: "secret",
	}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/provider/slow-relay", nil)
	req.SetPathValue("name", "slow-relay")
	rec := httptest.NewRecorder()

	started := time.Now()
	srv.handleProvider(rec, req)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("provider page blocked for %v", elapsed)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 0 {
		t.Fatalf("provider page made %d upstream requests", got)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "stored-model") {
		t.Fatalf("unexpected response %d: %s", rec.Code, rec.Body.String())
	}
}

func TestModelsPageLoadsDetailsAndCredentialsOnDemand(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()
	providerID, err := st.UpsertProvider("relay-a", "https://relay.example/v1", "chat", "unknown")
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	if err := st.ApplyProviderCheck("run-1", providerID, []store.CheckResultInput{{
		Model: "gpt-test", Vendor: "GPT", Status: "ok", Correct: true, LatencyMs: 100,
	}}, true, true, "ok", 100, ""); err != nil {
		t.Fatalf("seed current result: %v", err)
	}

	const secret = "key-must-not-be-in-models-page"
	srv, err := New(config.DefaultConfig(), st, nil, []provider.Provider{{
		Name: "relay-a", BaseURL: "https://relay.example/v1", APIKey: secret,
	}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	pageRec := httptest.NewRecorder()
	srv.handleModels(pageRec, httptest.NewRequest(http.MethodGet, "/models", nil))
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), "gpt-test") {
		t.Fatalf("unexpected models page %d: %s", pageRec.Code, pageRec.Body.String())
	}
	if strings.Contains(pageRec.Body.String(), secret) || strings.Contains(pageRec.Body.String(), "data-apikey") {
		t.Fatal("models page embedded provider credentials")
	}
	if strings.Contains(pageRec.Body.String(), `data-provider="relay-a"`) {
		t.Fatal("models page eagerly rendered provider detail rows")
	}

	detailRec := httptest.NewRecorder()
	srv.handleModelProviders(detailRec, httptest.NewRequest(http.MethodGet, "/models/providers?model=gpt-test", nil))
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "relay-a") {
		t.Fatalf("unexpected detail response %d: %s", detailRec.Code, detailRec.Body.String())
	}
	if strings.Contains(detailRec.Body.String(), secret) {
		t.Fatal("detail fragment embedded provider credentials")
	}

	configReq := httptest.NewRequest(http.MethodGet, "/api/v1/providers/relay-a/config", nil)
	configReq.SetPathValue("name", "relay-a")
	configRec := httptest.NewRecorder()
	srv.handleGetProviderConfig(configRec, configReq)
	if configRec.Code != http.StatusOK || !strings.Contains(configRec.Body.String(), secret) {
		t.Fatalf("unexpected config response %d: %s", configRec.Code, configRec.Body.String())
	}
	if configRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential response is cacheable")
	}
}

func TestHeaderOverrideRejectsInjection(t *testing.T) {
	if !isValidHeaderOverride("codex_cli_rs/9.9.9") {
		t.Fatal("valid header override rejected")
	}
	if isValidHeaderOverride("ok\r\nX-Evil: injected") {
		t.Fatal("CRLF header injection accepted")
	}
}

func TestSelectCapabilityProbeTargetsPrioritizesResponsesHotModels(t *testing.T) {
	prov := provider.Provider{Name: "relay", APIFormat: "chat"}
	results := []checker.TestResult{
		{Model: "gpt-5.2-codex", Correct: true, LatencyMs: 1200},
		{Model: "gpt-5.3-codex", Correct: true, LatencyMs: 1100},
		{Model: "claude-3.7-sonnet", Correct: true, LatencyMs: 800},
		{Model: "gpt-5.4", Correct: true, LatencyMs: 1500},
	}

	targets := selectCapabilityProbeTargets(prov, results, nil, 3)
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	if targets[0].Result.Model != "gpt-5.4" {
		t.Fatalf("expected gpt-5.4 to be first, got %s", targets[0].Result.Model)
	}
	for _, target := range targets {
		if target.Result.Model == "claude-3.7-sonnet" {
			t.Fatalf("chat-only model should not outrank missing responses targets: %+v", targets)
		}
	}
}

func TestSelectCapabilityProbeTargetsSkipsFreshCapabilities(t *testing.T) {
	now := time.Now()
	prov := provider.Provider{Name: "relay", APIFormat: "chat"}
	results := []checker.TestResult{
		{Model: "claude-3.7-sonnet", Correct: true, LatencyMs: 700},
		{Model: "o4-mini", Correct: true, LatencyMs: 900},
		{Model: "gpt-5.4", Correct: true, LatencyMs: 1000},
	}
	existing := []store.CapabilityRow{
		{
			Model:              "gpt-5.4",
			Streaming:          boolPtr(true),
			ToolUse:            boolPtr(true),
			ChatTestedAt:       sql.NullTime{Time: now, Valid: true},
			ResponsesBasic:     boolPtr(true),
			ResponsesStreaming: boolPtr(true),
			ResponsesToolUse:   boolPtr(true),
			ResponsesTestedAt:  sql.NullTime{Time: now, Valid: true},
		},
		{
			Model:        "o4-mini",
			Streaming:    boolPtr(true),
			ToolUse:      boolPtr(true),
			ChatTestedAt: sql.NullTime{Time: now, Valid: true},
		},
	}

	targets := selectCapabilityProbeTargets(prov, results, existing, 1)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Result.Model != "o4-mini" {
		t.Fatalf("expected o4-mini to be selected, got %s", targets[0].Result.Model)
	}
	if !targets[0].NeedResponses {
		t.Fatalf("expected o4-mini to require responses probe")
	}
}

func TestSelectCapabilityProbeTargetsResolvesCLIAutoPerModel(t *testing.T) {
	prov := provider.Provider{Name: "cli", ClientMode: provider.ClientModeAuto}
	targets := selectCapabilityProbeTargets(prov, []checker.TestResult{
		{Model: "gpt-5.6-sol", Correct: true},
		{Model: "sonnet5", Correct: true},
	}, nil, 0)
	if len(targets) != 2 {
		t.Fatalf("targets = %#v", targets)
	}
	byModel := map[string]capabilityProbeTarget{}
	for _, target := range targets {
		byModel[target.Result.Model] = target
	}
	if !byModel["gpt-5.6-sol"].NeedResponses || byModel["gpt-5.6-sol"].NeedChat {
		t.Fatalf("unexpected Codex target: %#v", byModel["gpt-5.6-sol"])
	}
	if !byModel["sonnet5"].NeedChat || byModel["sonnet5"].NeedResponses {
		t.Fatalf("unexpected Claude target: %#v", byModel["sonnet5"])
	}
}

func TestSummarizeProviderResultsCountsOKSeparately(t *testing.T) {
	summary := summarizeProviderResults([]checker.TestResult{
		{Model: "m1", Status: "ok", Correct: true},
		{Model: "m2", Status: "ok", Correct: false},
		{Model: "m3", Status: "error", Correct: false},
	}, "")

	if summary.Models != 3 {
		t.Fatalf("models = %d, want 3", summary.Models)
	}
	if summary.OK != 2 {
		t.Fatalf("ok = %d, want 2", summary.OK)
	}
	if summary.Correct != 1 {
		t.Fatalf("correct = %d, want 1", summary.Correct)
	}
	if summary.Status != "down" {
		t.Fatalf("status = %s, want down", summary.Status)
	}
}

func TestAuthoritativeProviderStateRejectsPartialFullRun(t *testing.T) {
	replaceCurrent, updateProvider := authoritativeProviderState(checker.ModeFull, &checker.ProviderResult{
		Provider:    "relay",
		ModelsFound: 3,
		Results: []checker.TestResult{
			{Model: "m1", Status: "ok", Correct: true},
			{Model: "m2", Status: "ok", Correct: true},
		},
	})
	if replaceCurrent || updateProvider {
		t.Fatalf("partial full run should not mutate current state: replace=%v update=%v", replaceCurrent, updateProvider)
	}
}

func TestAuthoritativeProviderStateTreatsProviderLevelFailureAsNonDestructive(t *testing.T) {
	replaceCurrent, updateProvider := authoritativeProviderState(checker.ModeFull, &checker.ProviderResult{
		Provider: "relay",
		Error:    "fetch models failed",
	})
	// Provider-level failures (DNS, connection, auth) should update the provider's status
	// but NOT clear current_results — transient failures shouldn't evict verified models.
	if replaceCurrent {
		t.Fatalf("provider-level failure should NOT clear current snapshot: replace=%v", replaceCurrent)
	}
	if !updateProvider {
		t.Fatalf("provider-level failure should update provider status: update=%v", updateProvider)
	}
}

// Dashboard reads of the in-memory provider list must not race with handlers
// that mutate provider fields in place. Run with -race: the detector is the
// assertion here.
func TestProviderListAccessIsRaceFree(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	cfg := config.DefaultConfig()
	cfg.ProvidersFile = filepath.Join(t.TempDir(), "providers.json")
	srv, err := New(cfg, st, nil, []provider.Provider{
		{Name: "relay-a", BaseURL: "https://a.example/v1", APIKey: "k1", Note: "initial"},
		{Name: "relay-b", BaseURL: "https://b.example/v1", APIKey: "k2"},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.handleDashboard(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
		go func(i int) {
			defer wg.Done()
			form := url.Values{"note": {fmt.Sprintf("note-%d", i)}}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/relay-a/note", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("name", "relay-a")
			srv.handleUpdateNote(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()
}

// A manual single-provider check must not run concurrently with a full check:
// both write the same provider's current_results and rebuild the routing table.
func TestTriggerSingleCheckConflictsWithRunningCheck(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	srv, err := New(config.DefaultConfig(), st, &checker.Engine{Client: http.DefaultClient}, []provider.Provider{
		{Name: "relay-a", BaseURL: "https://a.example/v1", APIKey: "k1"},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.mu.Lock()
	srv.isChecking = true
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/check/trigger/relay-a", nil)
	req.SetPathValue("name", "relay-a")
	rec := httptest.NewRecorder()
	srv.handleTriggerSingleCheck(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a check is running", rec.Code)
	}
}

// Editing a provider with a partial form must not wipe fields the form
// didn't include — only submitted fields change.
func TestEditProviderPartialFormPreservesUnsubmittedFields(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "relay-monitor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer st.Close()

	cfg := config.DefaultConfig()
	cfg.ProvidersFile = filepath.Join(t.TempDir(), "providers.json")
	srv, err := New(cfg, st, nil, []provider.Provider{{
		Name: "relay-a", BaseURL: "https://a.example/v1", APIKey: "k1",
		AccessToken: "tok-keep", CodexUserAgent: "codex_cli_rs/1.0", CodexOriginator: "codex",
		Note: "keep-note", Priority: 1.5,
	}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	form := url.Values{"note": {"new-note"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/relay-a/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "relay-a")
	rec := httptest.NewRecorder()
	srv.handleEditProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got := srv.findProvider("relay-a")
	if got == nil {
		t.Fatal("provider disappeared")
	}
	if got.Note != "new-note" {
		t.Fatalf("Note = %q, want submitted value applied", got.Note)
	}
	if got.AccessToken != "tok-keep" {
		t.Fatalf("AccessToken = %q, want preserved", got.AccessToken)
	}
	if got.CodexUserAgent != "codex_cli_rs/1.0" {
		t.Fatalf("CodexUserAgent = %q, want preserved", got.CodexUserAgent)
	}
	if got.CodexOriginator != "codex" {
		t.Fatalf("CodexOriginator = %q, want preserved", got.CodexOriginator)
	}
	if got.Priority != 1.5 {
		t.Fatalf("Priority = %v, want preserved", got.Priority)
	}
}
