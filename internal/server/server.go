package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"relay-monitor/internal/checker"
	"relay-monitor/internal/config"
	"relay-monitor/internal/notifier"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/proxy"
	"relay-monitor/internal/scheduler"
	"relay-monitor/internal/store"
	"relay-monitor/web"
)

func isValidProviderName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, r := range name {
		switch r {
		case '<', '>', '"', '\'', '\\', '`':
			return false
		}
	}
	return true
}

// isClaudeModel returns true if the model name is an Anthropic Claude model.
func isClaudeModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

func shouldProbeResponsesCapability(model, providerFormat string) bool {
	if providerFormat == "responses" {
		return true
	}
	low := strings.ToLower(model)
	return strings.Contains(low, "gpt-5") ||
		strings.Contains(low, "gpt-4.1") ||
		strings.Contains(low, "codex") ||
		strings.HasPrefix(low, "o1") ||
		strings.HasPrefix(low, "o3") ||
		strings.HasPrefix(low, "o4")
}

const capabilityRefreshInterval = 24 * time.Hour
const capabilityProbeLimit = 3

type capabilityProbeTarget struct {
	Result        checker.TestResult
	NeedChat      bool
	NeedResponses bool
}

type providerRunSummary struct {
	Models   int
	OK       int
	Correct  int
	Status   string
	Health   float64
	ErrorMsg string
}

func chatCapabilityNeedsRefresh(row store.CapabilityRow) bool {
	if row.Streaming == nil || row.ToolUse == nil {
		return true
	}
	return !row.ChatTestedAt.Valid || time.Since(row.ChatTestedAt.Time) >= capabilityRefreshInterval
}

func responsesCapabilityNeedsRefresh(row store.CapabilityRow) bool {
	if row.ResponsesBasic == nil || row.ResponsesStreaming == nil || row.ResponsesToolUse == nil {
		return true
	}
	return !row.ResponsesTestedAt.Valid || time.Since(row.ResponsesTestedAt.Time) >= capabilityRefreshInterval
}

func summarizeProviderResults(results []checker.TestResult, providerErr string) providerRunSummary {
	summary := providerRunSummary{
		Models: len(results),
		Status: "down",
	}
	for _, r := range results {
		if r.Status == "ok" {
			summary.OK++
		}
		if r.Correct {
			summary.Correct++
		}
	}
	if summary.Models > 0 {
		ratio := float64(summary.Correct) / float64(summary.Models)
		switch {
		case ratio > 0.8:
			summary.Status = "ok"
		case ratio > 0.5:
			summary.Status = "degraded"
		}
		summary.Health = ratio * 100
	}
	if providerErr != "" && summary.Models == 0 {
		summary.Status = "error"
		summary.ErrorMsg = providerErr
	}
	return summary
}

func capabilityModelPriority(model string) int {
	low := strings.ToLower(model)
	switch {
	case low == "gpt-5.4" || strings.HasPrefix(low, "gpt-5.4-"):
		return 0
	case strings.HasPrefix(low, "gpt-5") && !strings.Contains(low, "codex"):
		return 1
	case strings.HasPrefix(low, "gpt-4.1"):
		return 2
	case strings.HasPrefix(low, "o4"):
		return 3
	case strings.HasPrefix(low, "o3"):
		return 4
	case strings.HasPrefix(low, "o1"):
		return 5
	case strings.Contains(low, "codex"):
		return 6
	case strings.HasPrefix(low, "gpt"):
		return 7
	default:
		return 8
	}
}

func selectCapabilityProbeTargets(prov provider.Provider, results []checker.TestResult, existing []store.CapabilityRow, limit int) []capabilityProbeTarget {
	capByModel := make(map[string]store.CapabilityRow, len(existing))
	for _, c := range existing {
		capByModel[c.Model] = c
	}

	targets := make([]capabilityProbeTarget, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if !r.Correct || seen[r.Model] {
			continue
		}
		seen[r.Model] = true

		capRow, hasCap := capByModel[r.Model]
		needChat := prov.APIFormat != "responses" && (!hasCap || chatCapabilityNeedsRefresh(capRow))
		needResponses := shouldProbeResponsesCapability(r.Model, prov.APIFormat) &&
			(!hasCap || responsesCapabilityNeedsRefresh(capRow))
		if !needChat && !needResponses {
			continue
		}

		targets = append(targets, capabilityProbeTarget{
			Result:        r,
			NeedChat:      needChat,
			NeedResponses: needResponses,
		})
	}

	sort.SliceStable(targets, func(i, j int) bool {
		a := targets[i]
		b := targets[j]
		if a.NeedResponses != b.NeedResponses {
			return a.NeedResponses
		}
		if pa, pb := capabilityModelPriority(a.Result.Model), capabilityModelPriority(b.Result.Model); pa != pb {
			return pa < pb
		}
		if a.NeedChat != b.NeedChat {
			return a.NeedChat
		}
		if a.Result.LatencyMs != b.Result.LatencyMs {
			return a.Result.LatencyMs < b.Result.LatencyMs
		}
		return a.Result.Model < b.Result.Model
	})

	if limit > 0 && len(targets) > limit {
		targets = targets[:limit]
	}
	return targets
}

func (s *Server) refreshProviderMetadataAndCapabilities(ctx context.Context, prov provider.Provider, providerID int64, results []checker.TestResult) {
	if ctx.Err() != nil {
		return
	}

	dbProv, err := s.store.GetProviderByName(prov.Name)
	if err == nil && dbProv.Platform == "unknown" {
		if platform := checker.DetectPlatform(ctx, s.engine.Client, prov.BaseURL); platform != "unknown" {
			if err := s.store.UpdateProviderPlatform(providerID, platform); err != nil {
				log.Printf("[platform] %s: update error: %v", prov.Name, err)
			}
		}
	}

	if ctx.Err() != nil {
		return
	}

	if prov.AccessToken != "" {
		bi, berr := checker.QueryBalance(ctx, s.engine.Client, prov.BaseURL, prov.AccessToken)
		if berr != nil {
			log.Printf("[balance] %s: error: %v", prov.Name, berr)
		} else if bi != nil {
			log.Printf("[balance] %s: quota=%.0f", prov.Name, bi.Remaining)
			if err := s.store.UpdateProviderBalance(providerID, bi.Remaining); err != nil {
				log.Printf("[balance] %s: update error: %v", prov.Name, err)
			}
		} else {
			log.Printf("[balance] %s: no data (token may be invalid)", prov.Name)
		}
	}

	if ctx.Err() != nil {
		return
	}

	existingCaps, err := s.store.GetCapabilities(providerID)
	if err != nil {
		log.Printf("[capability] %s: load error: %v", prov.Name, err)
		return
	}
	targets := selectCapabilityProbeTargets(prov, results, existingCaps, capabilityProbeLimit)
	if len(targets) == 0 {
		return
	}

	var summaries []string
	for _, target := range targets {
		var formats []string
		if target.NeedResponses {
			formats = append(formats, "responses")
		}
		if target.NeedChat {
			formats = append(formats, "chat")
		}
		summaries = append(summaries, fmt.Sprintf("%s[%s]", target.Result.Model, strings.Join(formats, "+")))
	}
	log.Printf("[capability] %s: probing %s", prov.Name, strings.Join(summaries, ", "))

	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}

		if target.NeedResponses {
			caps := checker.ProbeResponsesCapabilities(ctx, s.engine.Client, prov.BaseURL, prov.APIKey, target.Result.Model)
			if caps.Basic != nil || caps.Streaming != nil || caps.ToolUse != nil {
				if err := s.store.UpsertCapability(providerID, target.Result.Model, "responses", caps.Basic, caps.Streaming, caps.ToolUse); err != nil {
					log.Printf("[capability] %s/%s responses update error: %v", prov.Name, target.Result.Model, err)
				}
			}
		}
		if target.NeedChat {
			caps := checker.ProbeCapabilities(ctx, s.engine.Client, prov.BaseURL, prov.APIKey, target.Result.Model, "chat")
			if caps.Streaming != nil || caps.ToolUse != nil {
				if err := s.store.UpsertCapability(providerID, target.Result.Model, "chat", nil, caps.Streaming, caps.ToolUse); err != nil {
					log.Printf("[capability] %s/%s chat update error: %v", prov.Name, target.Result.Model, err)
				}
			}
		}
	}
}

func (s *Server) rebuildProxyTable(memProviders []provider.Provider) {
	if s.proxy == nil {
		return
	}

	s.proxyRebuildMu.Lock()
	defer s.proxyRebuildMu.Unlock()

	results, _ := s.store.GetLatestResults()
	dbProviders, _ := s.store.GetProviders()
	caps, _ := s.store.GetAllCapabilities()
	s.proxy.RebuildTable(results, dbProviders, memProviders, s.buildFingerprintMap(), caps)
}

func checkRunModeLabel(mode checker.CheckMode) string {
	if mode == checker.ModeFull {
		return "full"
	}
	return "basic"
}

func authoritativeProviderState(mode checker.CheckMode, pr *checker.ProviderResult) (replaceCurrent bool, updateProvider bool) {
	if mode != checker.ModeFull || pr == nil {
		return false, false
	}
	if pr.Error != "" {
		// A full run that fails before any model results are written is still authoritative
		// about the provider being unavailable right now, so we clear the current snapshot.
		return len(pr.Results) == 0, len(pr.Results) == 0
	}
	if len(pr.Results) != pr.ModelsFound {
		return false, false
	}
	return true, true
}

func toStoreCheckResults(results []checker.TestResult) []store.CheckResultInput {
	out := make([]store.CheckResultInput, 0, len(results))
	for _, r := range results {
		out = append(out, store.CheckResultInput{
			Model:        r.Model,
			Vendor:       r.Vendor,
			Status:       r.Status,
			Correct:      r.Correct,
			Answer:       r.Answer,
			LatencyMs:    r.LatencyMs,
			ErrorMsg:     r.Error,
			HasReasoning: r.HasReasoning,
		})
	}
	return out
}

func csrfHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			check := r.Header.Get("Origin")
			if check == "" {
				check = r.Header.Get("Referer")
			}
			if check != "" {
				checkHost := check
				if i := strings.Index(check, "://"); i >= 0 {
					checkHost = check[i+3:]
				}
				if j := strings.Index(checkHost, "/"); j >= 0 {
					checkHost = checkHost[:j]
				}
				if checkHost != r.Host {
					http.Error(w, "CSRF check failed", http.StatusForbidden)
					return
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// Server is the HTTP dashboard server.
type Server struct {
	cfg       *config.AppConfig
	store     *store.Store
	engine    *checker.Engine
	providers []provider.Provider
	tmpls     map[string]*template.Template
	httpSrv   *http.Server
	sseHub    *SSEHub
	sched     *scheduler.Scheduler
	notifier  *notifier.Notifier
	proxy     *proxy.Proxy

	mu             sync.Mutex
	proxyRebuildMu sync.Mutex
	isChecking     bool
	lastCheck      time.Time
}

// ProviderCard is the view model for a dashboard card.
type ProviderCard struct {
	Name           string
	Status         string
	Platform       string
	URL            string
	ModelsOK       int
	ModelsTotal    int
	AvgLatencyMs   float64
	Health         int
	Balance        *float64
	HasAccessToken bool
	Pinned         bool
	Note           string
	ErrorMsg       string
}

// EventItem is the view model for an event in the list.
type EventItem struct {
	Type      string
	Provider  string
	Model     string
	Message   string
	CreatedAt string
	Read      bool
}

// ModelRow is the view model for a model in the provider detail table.
type ModelRow struct {
	Model     string
	Vendor    string
	Status    string
	Correct   bool
	LatencyMs int64
	Error     string
}

// ModelGroup is the view model for a model row in the models page.
type ModelGroup struct {
	Model          string
	Vendor         string
	AvailableCount int
	TotalCount     int
	BestProvider   string
	BestLatencyMs  int64
	BestFPScore    int
	CCCompat       bool
	Providers      []ModelProviderRow
}

// ModelProviderRow is one provider's data for a specific model.
type ModelProviderRow struct {
	ProviderName string
	BaseURL      string
	APIKey       string
	Model        string
	LatencyMs    int64
	Correct      bool
	Verdict      string
	VerdictClass string
	FPScore      int    // fingerprint total score (0 = no data)
	FPState      string // "exact", "sampled", "none"
	ToolUse      string // "true", "false", ""
	Streaming    string
	IsBest       bool
	CCCompat     bool // Claude Code compatible: claude model + tool_use + streaming
}

type pageData struct {
	Title         string
	UnreadEvents  int
	LastCheckTime string
	IsChecking    bool
	// Dashboard
	Providers []ProviderCard
	Events    []EventItem
	// Provider detail
	Provider *struct {
		Name        string
		Status      string
		Health      int
		URL         string
		Pinned      bool
		Note        string
		APIKey      string
		AccessToken string
		Priority    float64
	}
	Models []ModelRow
	// Models view
	ModelGroups []ModelGroup
	Vendors     []string
	// Fingerprint view
	Fingerprints []FingerprintViewRow
}

// FingerprintViewRow is the view model for the fingerprint results table.
type FingerprintViewRow struct {
	ProviderName   string
	Model          string
	L1, L2, L3, L4 string // "1/1", "2/3" format
	TotalScore     int
	ExpectedTier   string
	ExpectedMin    int
	Verdict        string
	VerdictClass   string
	SelfIDVerdict  string
	SelfIDDetail   string
}

func New(cfg *config.AppConfig, st *store.Store, eng *checker.Engine, providers []provider.Provider) (*Server, error) {
	funcMap := template.FuncMap{
		"deref": func(f *float64) float64 {
			if f == nil {
				return 0
			}
			return *f
		},
		"divf": func(a int64, b float64) float64 {
			return float64(a) / b
		},
		"quota2usd": func(q float64) float64 {
			return q / 500000.0
		},
	}

	tmplFS, err := fs.Sub(web.Assets, "templates")
	if err != nil {
		return nil, fmt.Errorf("template fs: %w", err)
	}

	// Parse each page template independently with layout to avoid define conflicts
	pages := []string{"dashboard.html", "provider.html", "models.html", "add_provider.html", "fingerprint.html", "proxy.html"}
	tmpls := make(map[string]*template.Template)
	for _, page := range pages {
		t, err := template.New("").Funcs(funcMap).ParseFS(tmplFS, "layout.html", page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		tmpls[page] = t
	}
	// Fragments parsed separately
	fragTmpl, err := template.New("").Funcs(funcMap).ParseFS(tmplFS, "fragments/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse fragments: %w", err)
	}
	tmpls["fragments"] = fragTmpl

	return &Server{
		cfg:       cfg,
		store:     st,
		engine:    eng,
		providers: providers,
		tmpls:     tmpls,
		sseHub:    NewSSEHub(),
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Static files
	staticFS, _ := fs.Sub(web.Assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Pages
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("GET /fingerprint", s.handleFingerprint)
	mux.HandleFunc("GET /provider/add", s.handleAddProviderPage)
	mux.HandleFunc("GET /provider/{name}", s.handleProvider)

	// API
	mux.HandleFunc("POST /api/v1/check/trigger", s.handleTriggerCheck)
	mux.HandleFunc("POST /api/v1/check/trigger/{name}", s.handleTriggerSingleCheck)
	mux.HandleFunc("POST /api/v1/fingerprint/trigger", s.handleTriggerFingerprint)
	mux.HandleFunc("GET /api/v1/providers", s.handleGetProviders)
	mux.HandleFunc("POST /api/v1/providers", s.handleAddProvider)
	mux.HandleFunc("DELETE /api/v1/providers/{name}", s.handleDeleteProvider)
	mux.HandleFunc("POST /api/v1/providers/{name}/pin", s.handleTogglePin)
	mux.HandleFunc("POST /api/v1/providers/{name}/note", s.handleUpdateNote)
	mux.HandleFunc("POST /api/v1/providers/{name}/edit", s.handleEditProvider)
	mux.HandleFunc("GET /api/v1/status", s.handleGetStatus)
	mux.HandleFunc("POST /api/v1/events/read", s.handleMarkEventsRead)
	mux.HandleFunc("GET /api/v1/sse", s.sseHub.HandleSSE)

	// Proxy routes (only when proxy is configured)
	if s.proxy != nil {
		mux.HandleFunc("/v1/chat/completions", s.proxy.HandleChatCompletions())
		mux.HandleFunc("/v1/responses", s.proxy.HandleResponses())
		mux.HandleFunc("/v1/models", s.proxy.HandleModels())
		mux.HandleFunc("GET /proxy", s.handleProxy)
		mux.HandleFunc("GET /api/v1/proxy/stats", s.handleProxyStats)
	}

	s.httpSrv = &http.Server{
		Addr:    s.cfg.Listen,
		Handler: csrfHandler(mux),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("Dashboard: http://%s", s.cfg.Listen)
	if err := s.httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// SetScheduler wires the scheduler into the server.
func (s *Server) SetScheduler(sched *scheduler.Scheduler) {
	s.sched = sched
}

// SetNotifier wires the notifier into the server.
func (s *Server) SetNotifier(n *notifier.Notifier) {
	s.notifier = n
}

// SetProxy wires the reverse proxy into the server.
func (s *Server) SetProxy(p *proxy.Proxy) {
	s.proxy = p
}

// RunCheckAndStore runs a check, persists results per-provider as they complete.
func (s *Server) RunCheckAndStore(ctx context.Context, provs []provider.Provider, triggerType string, mode checker.CheckMode) {
	s.mu.Lock()
	if s.isChecking {
		s.mu.Unlock()
		return
	}
	s.isChecking = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.isChecking = false
		s.lastCheck = time.Now()
		s.mu.Unlock()
	}()

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	if err := s.store.InsertCheckRun(runID, checkRunModeLabel(mode), triggerType); err != nil {
		log.Printf("insert check run %s: %v", runID, err)
		return
	}
	runFinished := false
	defer func() {
		if runFinished {
			return
		}
		summary := "check aborted"
		if ctx.Err() != nil {
			summary = ctx.Err().Error()
		}
		if err := s.store.AbortCheckRun(runID, summary); err != nil {
			log.Printf("abort check run %s: %v", runID, err)
		}
		s.sseHub.Publish(SSEEvent{Type: "check_completed", Data: summary})
	}()

	// Sync providers to DB
	provIDs := make(map[string]int64)
	for _, p := range provs {
		id, err := s.store.UpsertProvider(p.Name, p.BaseURL, p.APIFormat, p.Platform)
		if err != nil {
			log.Printf("upsert provider %s: %v", p.Name, err)
			continue
		}
		provIDs[p.Name] = id
		// Seed balance from providers.json if DB has none
		if p.LastKnownQuota > 0 {
			dp, _ := s.store.GetProviderByName(p.Name)
			if dp != nil && dp.LastBalance == nil {
				s.store.UpdateProviderBalance(id, p.LastKnownQuota)
			}
		}
	}

	// Snapshot previous results for change detection
	prevResults, _ := s.store.GetLatestResults()
	prevByProvider := make(map[int64]map[string]checker.PreviousModel)
	for _, cr := range prevResults {
		if prevByProvider[cr.ProviderID] == nil {
			prevByProvider[cr.ProviderID] = make(map[string]checker.PreviousModel)
		}
		prevByProvider[cr.ProviderID][cr.Model] = checker.PreviousModel{
			Status:  cr.Status,
			Correct: cr.Correct,
		}
	}

	var (
		totalMu                                            sync.Mutex
		totalProviders, totalModels, totalOK, totalCorrect int
	)

	logFn := func(msg string) {
		ts := time.Now().Format("15:04:05")
		log.Printf("[%s] %s", ts, msg)
		s.sseHub.Publish(SSEEvent{Type: "check_progress", Data: msg})
	}

	s.sseHub.Publish(SSEEvent{Type: "check_started", Data: fmt.Sprintf("%d 个站点", len(provs))})

	// Each provider's results are written immediately when it finishes
	s.engine.RunCheck(ctx, provs, mode, logFn, func(pr *checker.ProviderResult) {
		pid, ok := provIDs[pr.Provider]
		if !ok {
			return
		}

		storeResults := toStoreCheckResults(pr.Results)
		runSummary := summarizeProviderResults(pr.Results, pr.Error)
		replaceCurrent, updateProviderState := authoritativeProviderState(mode, pr)
		if err := s.store.ApplyProviderCheck(runID, pid, storeResults, replaceCurrent, updateProviderState, runSummary.Status, runSummary.Health, runSummary.ErrorMsg); err != nil {
			log.Printf("[store] %s: apply provider check failed: %v", pr.Provider, err)
			return
		}

		totalMu.Lock()
		totalProviders++
		totalModels += runSummary.Models
		totalOK += runSummary.OK
		totalCorrect += runSummary.Correct
		totalMu.Unlock()

		// Change detection
		if replaceCurrent {
			if prev := prevByProvider[pid]; prev != nil {
				for _, de := range checker.Diff(pr.Provider, pr.Results, prev) {
					s.store.InsertEvent(de.Type, de.Provider, de.Model, de.OldValue, de.NewValue, de.Message)
					s.sseHub.Publish(SSEEvent{Type: "event_created", Data: de.Message})
					if s.notifier != nil {
						s.notifier.HandleEvent(de.Type, de.Provider, de.Model, de.OldValue, de.NewValue, de.Message)
					}
				}
			}
		}

		if replaceCurrent || updateProviderState {
			s.rebuildProxyTable(s.providers)
		}

		// Push per-provider SSE update so dashboard refreshes incrementally
		s.sseHub.Publish(SSEEvent{Type: "provider_done", Data: pr.Provider})

		// Platform detection + balance + capability (lightweight, per provider)
		prov := s.findProvider(pr.Provider)
		if prov == nil {
			return
		}
		s.refreshProviderMetadataAndCapabilities(ctx, *prov, pid, pr.Results)
	})

	totalMu.Lock()
	summary := fmt.Sprintf("%d providers, %d models, %d ok, %d correct",
		totalProviders, totalModels, totalOK, totalCorrect)
	totalMu.Unlock()
	if err := s.store.FinishCheckRun(runID, totalProviders, totalModels, totalOK, totalCorrect, summary); err != nil {
		log.Printf("finish check run %s: %v", runID, err)
		return
	}
	runFinished = true
	s.sseHub.Publish(SSEEvent{Type: "check_completed", Data: summary})

	// Rebuild proxy routing table with fresh check data
	if s.proxy != nil {
		s.rebuildProxyTable(s.providers)
		log.Printf("[proxy] routing table rebuilt: %d models", len(s.proxy.Table().Models()))
	}
}

// buildFingerprintMap builds a lookup map from (providerName, model) → FingerprintScore.
func (s *Server) buildFingerprintMap() map[[2]string]proxy.FingerprintScore {
	fps, err := s.store.GetLatestFingerprints()
	if err != nil || len(fps) == 0 {
		return nil
	}
	m := make(map[[2]string]proxy.FingerprintScore, len(fps))
	for _, f := range fps {
		m[[2]string{f.ProviderName, f.Model}] = proxy.FingerprintScore{
			TotalScore:  f.TotalScore,
			ExpectedMin: f.ExpectedMin,
			Verdict:     f.Verdict,
		}
	}
	return m
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("总览")

	// Build provider cards from DB
	dbProviders, _ := s.store.GetProviders()
	latestResults, _ := s.store.GetLatestResults()

	// Group results by provider
	provResults := make(map[int64][]store.CheckResultRow)
	for _, cr := range latestResults {
		provResults[cr.ProviderID] = append(provResults[cr.ProviderID], cr)
	}

	// Build pinned/note lookup from in-memory providers
	provMeta := make(map[string]provider.Provider)
	for _, p := range s.providers {
		provMeta[p.Name] = p
	}

	for _, dp := range dbProviders {
		card := ProviderCard{
			Name:     dp.Name,
			Status:   dp.Status,
			Platform: dp.Platform,
			URL:      dp.BaseURL,
			Health:   int(dp.Health),
			Balance:  dp.LastBalance,
			ErrorMsg: dp.LastError,
		}
		if meta, ok := provMeta[dp.Name]; ok {
			card.Pinned = meta.Pinned
			card.Note = meta.Note
			card.HasAccessToken = meta.AccessToken != ""
		}
		if results, ok := provResults[dp.ID]; ok {
			card.ModelsTotal = len(results)
			var totalLatency int64
			latencyCount := 0
			for _, cr := range results {
				if cr.Correct {
					card.ModelsOK++
				}
				if cr.LatencyMs > 0 {
					totalLatency += cr.LatencyMs
					latencyCount++
				}
			}
			if latencyCount > 0 {
				card.AvgLatencyMs = float64(totalLatency) / float64(latencyCount) / 1000.0
			}
		}
		data.Providers = append(data.Providers, card)
	}

	// Sort: pinned first, then by health descending
	sort.Slice(data.Providers, func(i, j int) bool {
		if data.Providers[i].Pinned != data.Providers[j].Pinned {
			return data.Providers[i].Pinned
		}
		return data.Providers[i].Health > data.Providers[j].Health
	})

	// Events
	events, _ := s.store.GetRecentEvents(20)
	for _, e := range events {
		data.Events = append(data.Events, EventItem{
			Type:      e.Type,
			Provider:  e.Provider,
			Model:     e.Model,
			Message:   e.Message,
			CreatedAt: e.CreatedAt.Format("15:04"),
			Read:      e.Read,
		})
	}

	s.render(w, "dashboard.html", data)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("模型视图")

	latestResults, _ := s.store.GetLatestResults()
	dbProviders, _ := s.store.GetProviders()
	allCaps, _ := s.store.GetAllCapabilities()

	// Build fingerprint lookup: (providerName, model) → FingerprintRow
	fpRows, _ := s.store.GetLatestFingerprints()
	fpByKey := make(map[[2]string]store.FingerprintRow, len(fpRows))
	fpCountByProvider := make(map[string]int, len(fpRows))
	for _, f := range fpRows {
		fpByKey[[2]string{f.ProviderName, f.Model}] = f
		fpCountByProvider[f.ProviderName]++
	}

	// Build provider lookup: id -> (name, baseURL)
	provInfo := make(map[int64][2]string) // [name, baseURL]
	provIDByName := make(map[string]int64)
	for _, dp := range dbProviders {
		provInfo[dp.ID] = [2]string{dp.Name, dp.BaseURL}
		provIDByName[dp.Name] = dp.ID
	}
	provBaseURL := make(map[string]string)
	provAPIKey := make(map[string]string)
	for _, p := range s.providers {
		provBaseURL[p.Name] = p.BaseURL
		provAPIKey[p.Name] = p.APIKey
	}

	// Group results by model
	type modelKey struct{ model, vendor string }
	type provResult struct {
		providerID   int64
		providerName string
		baseURL      string
		apiKey       string
		latencyMs    int64
		correct      bool
		status       string
	}
	grouped := make(map[modelKey][]provResult)
	var order []modelKey

	for _, cr := range latestResults {
		k := modelKey{cr.Model, cr.Vendor}
		if _, exists := grouped[k]; !exists {
			order = append(order, k)
		}
		info := provInfo[cr.ProviderID]
		baseURL := info[1]
		if u, ok := provBaseURL[info[0]]; ok {
			baseURL = u
		}
		grouped[k] = append(grouped[k], provResult{
			providerID:   cr.ProviderID,
			providerName: info[0],
			baseURL:      baseURL,
			apiKey:       provAPIKey[info[0]],
			latencyMs:    cr.LatencyMs,
			correct:      cr.Correct,
			status:       cr.Status,
		})
	}

	boolStr := func(b *bool) string {
		if b == nil {
			return ""
		}
		if *b {
			return "true"
		}
		return "false"
	}

	vendorSet := make(map[string]bool)
	for _, k := range order {
		results := grouped[k]
		vendorSet[k.vendor] = true

		mg := ModelGroup{
			Model:      k.model,
			Vendor:     k.vendor,
			TotalCount: len(results),
		}

		var bestLatency int64
		anyCC := false
		for _, pr := range results {
			if pr.correct {
				mg.AvailableCount++
				if bestLatency == 0 || pr.latencyMs < bestLatency {
					bestLatency = pr.latencyMs
					mg.BestProvider = pr.providerName
					mg.BestLatencyMs = pr.latencyMs
				}
			}
		}

		for _, pr := range results {
			row := ModelProviderRow{
				ProviderName: pr.providerName,
				BaseURL:      pr.baseURL,
				APIKey:       pr.apiKey,
				Model:        k.model,
				LatencyMs:    pr.latencyMs,
				Correct:      pr.correct,
				IsBest:       pr.providerName == mg.BestProvider,
			}
			// Fill capabilities from DB
			if caps, ok := allCaps[pr.providerID]; ok {
				if cap, ok := caps[k.model]; ok {
					row.ToolUse = boolStr(cap.ToolUse)
					row.Streaming = boolStr(cap.Streaming)
					if cap.ToolUse != nil && *cap.ToolUse && cap.Streaming != nil && *cap.Streaming && isClaudeModel(k.model) {
						row.CCCompat = true
						anyCC = true
					}
				}
			}
			// Fill fingerprint verdict
			if fp, ok := fpByKey[[2]string{pr.providerName, k.model}]; ok {
				row.Verdict = fp.Verdict
				row.FPScore = fp.TotalScore
				row.FPState = "exact"
				switch {
				case fp.Verdict == "GENUINE":
					row.VerdictClass = "genuine"
				case fp.Verdict == "PLAUSIBLE":
					row.VerdictClass = "plausible"
				case strings.Contains(fp.Verdict, "SUSPECTED") || strings.Contains(fp.Verdict, "MISMATCH"):
					row.VerdictClass = "suspected"
				default:
					row.VerdictClass = "fake"
				}
			} else if fpCountByProvider[pr.providerName] > 0 {
				row.FPState = "sampled"
			} else {
				row.FPState = "none"
			}
			mg.Providers = append(mg.Providers, row)
		}
		mg.CCCompat = anyCC
		for _, pr := range mg.Providers {
			if pr.FPScore > mg.BestFPScore {
				mg.BestFPScore = pr.FPScore
			}
		}

		data.ModelGroups = append(data.ModelGroups, mg)
	}

	// Sort by available count descending
	sort.Slice(data.ModelGroups, func(i, j int) bool {
		if data.ModelGroups[i].AvailableCount != data.ModelGroups[j].AvailableCount {
			return data.ModelGroups[i].AvailableCount > data.ModelGroups[j].AvailableCount
		}
		return data.ModelGroups[i].Model < data.ModelGroups[j].Model
	})

	// Vendors list
	for v := range vendorSet {
		data.Vendors = append(data.Vendors, v)
	}
	sort.Strings(data.Vendors)

	s.render(w, "models.html", data)
}

func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	dp, err := s.store.GetProviderByName(name)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	data := s.basePageData(dp.Name)
	provMeta := s.findProvider(dp.Name)
	data.Provider = &struct {
		Name        string
		Status      string
		Health      int
		URL         string
		Pinned      bool
		Note        string
		APIKey      string
		AccessToken string
		Priority    float64
	}{
		Name:   dp.Name,
		Status: dp.Status,
		Health: int(dp.Health),
		URL:    dp.BaseURL,
	}
	if provMeta != nil {
		data.Provider.Pinned = provMeta.Pinned
		data.Provider.Note = provMeta.Note
		data.Provider.APIKey = provMeta.APIKey
		data.Provider.AccessToken = provMeta.AccessToken
		data.Provider.Priority = provMeta.Priority
	}

	// Get stored test results for this provider
	storedResults, _ := s.store.GetProviderResults(dp.ID)
	resultMap := make(map[string]store.CheckResultRow)
	for _, cr := range storedResults {
		resultMap[cr.Model] = cr
	}

	// Get capabilities
	caps, _ := s.store.GetCapabilities(dp.ID)
	capMap := make(map[string]store.CapabilityRow)
	for _, c := range caps {
		capMap[c.Model] = c
	}

	// Live-fetch full model list from the provider
	prov := s.findProvider(name)
	var allModels []string
	if prov != nil {
		fetched, fetchErr := s.engine.FetchModels(r.Context(), prov.BaseURL, prov.APIKey)
		if fetchErr == nil {
			for _, m := range fetched {
				if !provider.ShouldSkip(m) {
					allModels = append(allModels, m)
				}
			}
		}
	}

	// If live fetch failed or empty, fall back to stored results
	if len(allModels) == 0 {
		for _, cr := range storedResults {
			allModels = append(allModels, cr.Model)
		}
	}

	// Build model rows: merge live list with stored results
	seen := make(map[string]bool)
	for _, m := range allModels {
		if seen[m] {
			continue
		}
		seen[m] = true
		row := ModelRow{
			Model:  m,
			Vendor: provider.IdentifyVendor(m),
			Status: "untested",
		}
		if cr, ok := resultMap[m]; ok {
			row.Status = cr.Status
			row.Correct = cr.Correct
			row.LatencyMs = cr.LatencyMs
			row.Error = cr.ErrorMsg
		}
		data.Models = append(data.Models, row)
	}

	// Sort: correct first (by latency asc), then wrong, then untested, then error
	sort.Slice(data.Models, func(i, j int) bool {
		a, b := data.Models[i], data.Models[j]
		aRank := modelSortRank(a)
		bRank := modelSortRank(b)
		if aRank != bRank {
			return aRank < bRank
		}
		return a.LatencyMs < b.LatencyMs
	})

	s.render(w, "provider.html", data)
}

func (s *Server) handleTriggerCheck(w http.ResponseWriter, r *http.Request) {
	mode := checker.ModeFull
	if r.URL.Query().Get("mode") == "quick" {
		mode = checker.ModeQuick
	}

	s.mu.Lock()
	checking := s.isChecking
	s.mu.Unlock()
	if checking {
		http.Error(w, `{"error":"check already running"}`, http.StatusConflict)
		return
	}

	go s.RunCheckAndStore(context.Background(), s.providers, "manual", mode)

	w.WriteHeader(http.StatusAccepted)
	label := "全量"
	if mode == checker.ModeQuick {
		label = "快速"
	}
	w.Write([]byte(fmt.Sprintf(`{"status":"started","mode":"%s"}`, label)))
}

func (s *Server) handleTriggerSingleCheck(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var target *provider.Provider
	for i := range s.providers {
		if strings.EqualFold(s.providers[i].Name, name) {
			target = &s.providers[i]
			break
		}
	}
	if target == nil {
		http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
		return
	}

	// Run synchronously — single provider is fast enough
	pid, err := s.store.UpsertProvider(target.Name, target.BaseURL, target.APIFormat, target.Platform)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"upsert provider: %s"}`, err), http.StatusInternalServerError)
		return
	}
	pr := s.engine.TestProvider(r.Context(), *target, checker.ModeFull, func(msg string) {
		log.Printf("[manual] [%s] %s", time.Now().Format("15:04:05"), msg)
	})

	// Store results
	runID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
	if err := s.store.InsertCheckRun(runID, "full", "manual"); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"insert check run: %s"}`, err), http.StatusInternalServerError)
		return
	}

	runFinished := false
	defer func() {
		if runFinished {
			return
		}
		if err := s.store.AbortCheckRun(runID, "manual check aborted"); err != nil {
			log.Printf("abort manual check %s: %v", runID, err)
		}
	}()

	runSummary := summarizeProviderResults(pr.Results, pr.Error)
	replaceCurrent, updateProviderState := authoritativeProviderState(checker.ModeFull, pr)
	if err := s.store.ApplyProviderCheck(
		runID,
		pid,
		toStoreCheckResults(pr.Results),
		replaceCurrent,
		updateProviderState,
		runSummary.Status,
		runSummary.Health,
		runSummary.ErrorMsg,
	); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"apply provider check: %s"}`, err), http.StatusInternalServerError)
		return
	}
	if err := s.store.FinishCheckRun(runID, 1, runSummary.Models, runSummary.OK, runSummary.Correct,
		fmt.Sprintf("%s: %d models, %d correct", target.Name, runSummary.Models, runSummary.Correct)); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"finish check run: %s"}`, err), http.StatusInternalServerError)
		return
	}
	runFinished = true
	s.refreshProviderMetadataAndCapabilities(r.Context(), *target, pid, pr.Results)
	if s.proxy != nil {
		s.rebuildProxyTable(s.providers)
		log.Printf("[proxy] routing table rebuilt after manual check: %d models", len(s.proxy.Table().Models()))
	}

	// Return result directly as HTML feedback
	if runSummary.ErrorMsg != "" {
		fmt.Fprintf(w, `<span style="color:var(--down)">%s: %s</span>`, html.EscapeString(name), html.EscapeString(runSummary.ErrorMsg))
	} else if len(pr.Results) == 0 {
		fmt.Fprintf(w, `<span style="color:var(--down)">%s: 无可用模型</span>`, html.EscapeString(name))
	} else {
		fmt.Fprintf(w, `<span style="color:var(--ok)">%s: %d/%d 正确 — <a href="/provider/%s" style="color:var(--accent)">查看详情</a></span>`,
			html.EscapeString(name), runSummary.Correct, len(pr.Results), url.PathEscape(name))
	}
}

func (s *Server) handleGetProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.GetProviders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providers)
}

func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status := map[string]any{
		"checking":   s.isChecking,
		"last_check": s.lastCheck.Format("15:04:05"),
		"providers":  len(s.providers),
	}
	s.mu.Unlock()
	if s.sched != nil {
		status["next_check"] = s.sched.NextRun().Format("15:04:05")
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleMarkEventsRead(w http.ResponseWriter, r *http.Request) {
	s.store.MarkEventsRead()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFingerprint(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("指纹鉴别")

	rows, _ := s.store.GetLatestFingerprints()
	for _, row := range rows {
		vr := FingerprintViewRow{
			ProviderName:  row.ProviderName,
			Model:         row.Model,
			TotalScore:    row.TotalScore,
			ExpectedTier:  row.ExpectedTier,
			ExpectedMin:   row.ExpectedMin,
			Verdict:       row.Verdict,
			SelfIDVerdict: row.SelfIDVerdict,
			SelfIDDetail:  row.SelfIDDetail,
		}
		// Format L1-L4 as "correct/total" using question counts per level
		levelTotals := map[int]int{}
		for _, q := range checker.FingerprintQuestions {
			levelTotals[q.Level]++
		}
		vr.L1 = fmt.Sprintf("%d/%d", row.L1, levelTotals[1])
		vr.L2 = fmt.Sprintf("%d/%d", row.L2, levelTotals[2])
		vr.L3 = fmt.Sprintf("%d/%d", row.L3, levelTotals[3])
		vr.L4 = fmt.Sprintf("%d/%d", row.L4, levelTotals[4])

		switch {
		case row.Verdict == "GENUINE":
			vr.VerdictClass = "verdict-good"
		case row.Verdict == "PLAUSIBLE":
			vr.VerdictClass = "verdict-info"
		case strings.Contains(row.Verdict, "SUSPECTED") || strings.Contains(row.Verdict, "MISMATCH"):
			vr.VerdictClass = "verdict-warn"
		default:
			vr.VerdictClass = "verdict-bad"
		}
		data.Fingerprints = append(data.Fingerprints, vr)
	}

	s.render(w, "fingerprint.html", data)
}

func (s *Server) handleTriggerFingerprint(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	checking := s.isChecking
	s.mu.Unlock()
	if checking {
		http.Error(w, `{"error":"check already running"}`, http.StatusConflict)
		return
	}

	go s.RunFingerprintAndStore(context.Background(), s.providers)

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"fingerprint started"}`))
}

// RunFingerprintAndStore runs fingerprint checks on all providers and stores results.
func (s *Server) RunFingerprintAndStore(ctx context.Context, provs []provider.Provider) {
	s.mu.Lock()
	if s.isChecking {
		s.mu.Unlock()
		return
	}
	s.isChecking = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.isChecking = false
		s.mu.Unlock()
		s.sseHub.Publish(SSEEvent{Type: "check_completed", Data: "fingerprint done"})
	}()

	// Sync providers to DB
	provIDs := make(map[string]int64)
	for _, p := range provs {
		id, err := s.store.UpsertProvider(p.Name, p.BaseURL, p.APIFormat, p.Platform)
		if err != nil {
			log.Printf("upsert provider %s: %v", p.Name, err)
			continue
		}
		provIDs[p.Name] = id
	}

	logFn := func(msg string) {
		ts := time.Now().Format("15:04:05")
		log.Printf("[%s] %s", ts, msg)
		s.sseHub.Publish(SSEEvent{Type: "check_progress", Data: msg})
	}

	s.sseHub.Publish(SSEEvent{Type: "check_started", Data: fmt.Sprintf("指纹检测 %d 个站点", len(provs))})

	for i, p := range provs {
		if ctx.Err() != nil {
			break
		}
		s.sseHub.Publish(SSEEvent{Type: "check_progress", Data: fmt.Sprintf("[%d/%d] %s 指纹检测中...", i+1, len(provs), p.Name)})
		results := s.engine.RunFingerprintAll(ctx, s.engine.Client, p, logFn)
		pid, ok := provIDs[p.Name]
		if !ok {
			continue
		}
		for _, fr := range results {
			answersJSON, _ := json.Marshal(fr.Answers)
			l1 := fr.Scores["L1"]
			l2 := fr.Scores["L2"]
			l3 := fr.Scores["L3"]
			l4 := fr.Scores["L4"]
			s.store.InsertFingerprintResult(pid, fr.Model, fr.Vendor,
				fr.TotalScore, l1[0], l2[0], l3[0], l4[0],
				fr.ExpectedTier, fr.ExpectedMin, fr.Verdict,
				fr.SelfID.Verdict, fr.SelfID.Detail, string(answersJSON))
		}
		s.sseHub.Publish(SSEEvent{Type: "provider_done", Data: fmt.Sprintf("%s 指纹完成 (%d 模型)", p.Name, len(results))})
	}

	// Rebuild proxy routing table with updated fingerprint scores
	if s.proxy != nil {
		s.rebuildProxyTable(s.providers)
		log.Printf("[proxy] routing table rebuilt with fingerprint data: %d models", len(s.proxy.Table().Models()))
	}
}

func (s *Server) basePageData(title string) pageData {
	s.mu.Lock()
	checking := s.isChecking
	lastCheck := s.lastCheck
	s.mu.Unlock()

	var lastCheckStr string
	if !lastCheck.IsZero() {
		lastCheckStr = lastCheck.Format("15:04:05")
	}

	unread, _ := s.store.GetUnreadEventCount()

	return pageData{
		Title:         title,
		UnreadEvents:  unread,
		LastCheckTime: lastCheckStr,
		IsChecking:    checking,
	}
}

// modelSortRank returns a sort priority: 0=correct, 1=wrong answer, 2=untested, 3=error
func modelSortRank(m ModelRow) int {
	if m.Status == "ok" && m.Correct {
		return 0
	}
	if m.Status == "ok" && !m.Correct {
		return 1
	}
	if m.Status == "untested" {
		return 2
	}
	return 3
}

func (s *Server) findProvider(name string) *provider.Provider {
	for i := range s.providers {
		if s.providers[i].Name == name {
			return &s.providers[i]
		}
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	t, ok := s.tmpls[name]
	if !ok {
		log.Printf("template %s not found", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

func (s *Server) handleAddProviderPage(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("添加站点")
	s.render(w, "add_provider.html", data)
}

func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := r.FormValue("name")
	baseURL := r.FormValue("base_url")
	apiKey := r.FormValue("api_key")
	accessToken := r.FormValue("access_token")
	apiFormat := r.FormValue("api_format")

	if name == "" || baseURL == "" || apiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<div class="result-error">站名、URL、API Key 不能为空</div>`)
		return
	}

	if !isValidProviderName(name) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<div class="result-error">站名包含非法字符</div>`)
		return
	}

	if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<div class="result-error">URL 格式无效，需以 http:// 或 https:// 开头</div>`)
		return
	}

	// Check for duplicate name or hostname
	for _, p := range s.providers {
		if p.Name == name {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `<div class="result-error">站名 "%s" 已存在</div>`, html.EscapeString(name))
			return
		}
		if config.SameHost(p.BaseURL, baseURL) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `<div class="result-error">与已有站点 "%s" 重复（相同域名）</div>`, html.EscapeString(p.Name))
			return
		}
	}

	// Validate connectivity
	_, err := s.engine.FetchModels(r.Context(), baseURL, apiKey)

	// Add to providers.json
	newProv := provider.Provider{
		Name: name, BaseURL: baseURL, APIKey: apiKey,
		AccessToken: accessToken, APIFormat: apiFormat,
	}

	s.mu.Lock()
	s.providers = append(s.providers, newProv)
	provs := make([]provider.Provider, len(s.providers))
	copy(provs, s.providers)
	s.mu.Unlock()

	if saveErr := config.SaveProviders(s.cfg.ProvidersFile, provs); saveErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<div class="result-error">保存失败: %s</div>`, html.EscapeString(saveErr.Error()))
		return
	}

	// Upsert to DB
	s.store.UpsertProvider(name, baseURL, apiFormat, "")

	msg := fmt.Sprintf("已添加 %s", name)
	if err != nil {
		msg += fmt.Sprintf(" (连接验证失败: %s, 但已保存)", err)
	}

	fmt.Fprintf(w, `<div class="result-success">%s <a href="/">返回总览</a></div>`, html.EscapeString(msg))

	// Trigger initial check in background
	go s.RunCheckAndStore(context.Background(), []provider.Provider{newProv}, "manual", checker.ModeFull)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Hold lock across both DB and in-memory deletion
	s.mu.Lock()
	if dp, _ := s.store.GetProviderByName(name); dp != nil {
		s.store.DeleteProvider(dp.ID)
	}
	var remaining []provider.Provider
	for _, p := range s.providers {
		if p.Name != name {
			remaining = append(remaining, p)
		}
	}
	s.providers = remaining
	s.mu.Unlock()

	config.SaveProviders(s.cfg.ProvidersFile, remaining)

	// Rebuild proxy routing table to remove deleted provider
	s.rebuildProxyTable(remaining)

	// Redirect to dashboard
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTogglePin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	for i, p := range s.providers {
		if p.Name == name {
			s.providers[i].Pinned = !s.providers[i].Pinned
			break
		}
	}
	provs := make([]provider.Provider, len(s.providers))
	copy(provs, s.providers)
	s.mu.Unlock()
	config.SaveProviders(s.cfg.ProvidersFile, provs)
	w.Header().Set("HX-Redirect", "/provider/"+url.PathEscape(name))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUpdateNote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	r.ParseForm()
	note := r.FormValue("note")
	s.mu.Lock()
	for i, p := range s.providers {
		if p.Name == name {
			s.providers[i].Note = note
			break
		}
	}
	provs := make([]provider.Provider, len(s.providers))
	copy(provs, s.providers)
	s.mu.Unlock()
	config.SaveProviders(s.cfg.ProvidersFile, provs)
	w.Header().Set("HX-Redirect", "/provider/"+url.PathEscape(name))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleEditProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	r.ParseForm()
	newName := r.FormValue("name")
	baseURL := r.FormValue("base_url")
	apiKey := r.FormValue("api_key")
	accessToken := r.FormValue("access_token")
	note := r.FormValue("note")
	priorityStr := r.FormValue("priority")

	var priority float64
	if priorityStr != "" {
		fmt.Sscanf(priorityStr, "%f", &priority)
	}

	if newName != "" && !isValidProviderName(newName) {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			http.Error(w, "invalid base_url", http.StatusBadRequest)
			return
		}
	}

	// Hold lock across both DB and in-memory updates to avoid TOCTOU
	s.mu.Lock()
	effectiveName := name
	if dp, err := s.store.GetProviderByName(name); err == nil && dp != nil {
		if newName != "" && newName != name {
			s.store.RenameProvider(dp.ID, newName)
			effectiveName = newName
		}
		if baseURL != "" {
			s.store.UpsertProvider(effectiveName, baseURL, dp.APIFormat, dp.Platform)
		}
	}
	for i, p := range s.providers {
		if p.Name == name {
			if newName != "" {
				s.providers[i].Name = newName
			}
			if baseURL != "" {
				s.providers[i].BaseURL = baseURL
			}
			if apiKey != "" {
				s.providers[i].APIKey = apiKey
			}
			s.providers[i].AccessToken = accessToken
			s.providers[i].Note = note
			s.providers[i].Priority = priority
			break
		}
	}
	provs := make([]provider.Provider, len(s.providers))
	copy(provs, s.providers)
	s.mu.Unlock()
	config.SaveProviders(s.cfg.ProvidersFile, provs)

	// Rebuild proxy routing table immediately so priority changes take effect
	s.rebuildProxyTable(provs)

	w.Header().Set("HX-Redirect", "/provider/"+url.PathEscape(effectiveName))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("Proxy")

	type ProxyPageData struct {
		pageData
		ProxyEnabled  bool
		ProxyEndpoint string
		ProxyAPIKey   string
		ProxyWarming  bool
		ProxyModels   []proxyModelStat
		ProxyCoverage []proxyProviderStat
		ProxyTraffic  []proxyProviderStat
		ModelCatalog  []proxy.ModelInfo
		TotalRequests int64
		TotalErrors   int64
		AvgLatencyMs  int64
		ModelCount    int
	}

	pd := ProxyPageData{pageData: data}

	if s.proxy != nil {
		cfg := s.proxy.Config()
		pd.ProxyEnabled = cfg.Enabled
		if s.cfg.ExternalURL != "" {
			pd.ProxyEndpoint = strings.TrimRight(s.cfg.ExternalURL, "/") + "/v1"
		} else {
			pd.ProxyEndpoint = fmt.Sprintf("http://%s/v1", s.cfg.Listen)
		}
		pd.ProxyAPIKey = cfg.APIKey
		pd.ProxyWarming = !s.proxy.Table().Ready()

		// Aggregate stats
		snap := s.proxy.Stats().Snapshot()
		modelStats := make(map[string]*proxyModelStat)
		providerStats := make(map[int64]*proxyProviderStat)
		var totalReqs, totalErrs, totalLatency int64

		// Get provider names
		dbProviders, _ := s.store.GetProviders()
		nameByID := make(map[int64]string)
		for _, p := range dbProviders {
			nameByID[p.ID] = p.Name
		}

		modelProviderCount := s.proxy.Table().ModelProviderCount()
		provModelCount := s.proxy.Table().ProviderModelCount()

		// Provider 路由表应该展示所有已入 routing table 的 provider，
		// 而不只是最近 10 分钟真正接到流量的那几个。
		for providerID, routedModels := range provModelCount {
			pname := nameByID[providerID]
			if pname == "" {
				pname = fmt.Sprintf("id:%d", providerID)
			}
			providerStats[providerID] = &proxyProviderStat{
				ProviderName:  pname,
				ProviderID:    providerID,
				RoutingModels: routedModels,
			}
		}

		for _, ss := range snap {
			totalReqs += ss.Requests
			totalErrs += ss.Errors
			totalLatency += ss.AvgLatencyMs * ss.Requests

			ms, ok := modelStats[ss.Model]
			if !ok {
				ms = &proxyModelStat{Model: ss.Model, Providers: modelProviderCount[ss.Model]}
				modelStats[ss.Model] = ms
			}
			ms.Requests += ss.Requests
			ms.Errors += ss.Errors

			pname := nameByID[ss.ProviderID]
			if pname == "" {
				pname = fmt.Sprintf("id:%d", ss.ProviderID)
			}
			ps, ok := providerStats[ss.ProviderID]
			if !ok {
				ps = &proxyProviderStat{ProviderName: pname, ProviderID: ss.ProviderID}
				providerStats[ss.ProviderID] = ps
			}
			ps.Requests += ss.Requests
			ps.Errors += ss.Errors
		}

		pd.TotalRequests = totalReqs
		pd.TotalErrors = totalErrs
		if totalReqs > 0 {
			pd.AvgLatencyMs = totalLatency / totalReqs
		}

		for _, ms := range modelStats {
			if ms.Requests > 0 {
				ms.ErrorRate = float64(ms.Errors) / float64(ms.Requests) * 100
			}
			pd.ProxyModels = append(pd.ProxyModels, *ms)
		}
		sort.Slice(pd.ProxyModels, func(i, j int) bool {
			return pd.ProxyModels[i].Requests > pd.ProxyModels[j].Requests
		})

		for _, ps := range providerStats {
			if ps.Requests > 0 {
				ps.ErrorRate = float64(ps.Errors) / float64(ps.Requests) * 100
				pd.ProxyTraffic = append(pd.ProxyTraffic, *ps)
			}
			pd.ProxyCoverage = append(pd.ProxyCoverage, *ps)
		}
		sort.Slice(pd.ProxyCoverage, func(i, j int) bool {
			if pd.ProxyCoverage[i].RoutingModels != pd.ProxyCoverage[j].RoutingModels {
				return pd.ProxyCoverage[i].RoutingModels > pd.ProxyCoverage[j].RoutingModels
			}
			if pd.ProxyCoverage[i].ProviderName != pd.ProxyCoverage[j].ProviderName {
				return pd.ProxyCoverage[i].ProviderName < pd.ProxyCoverage[j].ProviderName
			}
			return pd.ProxyCoverage[i].Requests > pd.ProxyCoverage[j].Requests
		})
		sort.Slice(pd.ProxyTraffic, func(i, j int) bool {
			if pd.ProxyTraffic[i].Requests != pd.ProxyTraffic[j].Requests {
				return pd.ProxyTraffic[i].Requests > pd.ProxyTraffic[j].Requests
			}
			if pd.ProxyTraffic[i].RoutingModels != pd.ProxyTraffic[j].RoutingModels {
				return pd.ProxyTraffic[i].RoutingModels > pd.ProxyTraffic[j].RoutingModels
			}
			return pd.ProxyTraffic[i].ProviderName < pd.ProxyTraffic[j].ProviderName
		})

		// Model catalog (always available, independent of request stats)
		pd.ModelCatalog = s.proxy.Table().ModelCatalog()
		pd.ModelCount = len(pd.ModelCatalog)
	}

	s.tmpls["proxy.html"].ExecuteTemplate(w, "layout.html", pd)
}

type proxyModelStat struct {
	Model     string
	Requests  int64
	Errors    int64
	ErrorRate float64
	Providers int
}

type proxyProviderStat struct {
	ProviderName  string
	ProviderID    int64
	Requests      int64
	Errors        int64
	ErrorRate     float64
	RoutingModels int
}

func parseOptionalBool(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func (s *Server) handleProxyStats(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}

	// ?debug=model_name returns detailed routing scores
	if debugModel := r.URL.Query().Get("debug"); debugModel != "" {
		scores := s.proxy.Table().DebugScores(debugModel)
		type entry struct {
			Provider           string  `json:"provider"`
			Score              float64 `json:"score"`
			LatencyMs          int64   `json:"latency_ms"`
			Format             string  `json:"format"`
			MatchRank          int     `json:"match_rank,omitempty"`
			BreakerState       string  `json:"breaker_state,omitempty"`
			Requests           int64   `json:"requests,omitempty"`
			Errors             int64   `json:"errors,omitempty"`
			ErrorRate          float64 `json:"error_rate,omitempty"`
			ChatStreaming      *bool   `json:"chat_streaming,omitempty"`
			ChatToolUse        *bool   `json:"chat_tool_use,omitempty"`
			ResponsesBasic     *bool   `json:"responses_basic,omitempty"`
			ResponsesStreaming *bool   `json:"responses_streaming,omitempty"`
			ResponsesToolUse   *bool   `json:"responses_tool_use,omitempty"`
		}
		staticOut := make([]entry, len(scores))
		for i, sp := range scores {
			staticOut[i] = entry{
				Provider:           sp.ProviderName,
				Score:              sp.Score,
				LatencyMs:          sp.LatencyMs,
				Format:             sp.APIFormat,
				ChatStreaming:      sp.ChatStreaming,
				ChatToolUse:        sp.ChatToolUse,
				ResponsesBasic:     sp.ResponsesBasic,
				ResponsesStreaming: sp.ResponsesStreaming,
				ResponsesToolUse:   sp.ResponsesToolUse,
			}
		}

		requestFormat := strings.TrimSpace(r.URL.Query().Get("format"))
		if requestFormat == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"model":         debugModel,
				"mode":          "static_only",
				"note":          "static_scores ignore request shape; pass format=chat|responses and optional stream/tools/tool_call to inspect actual routing",
				"static_scores": staticOut,
			})
			return
		}
		if requestFormat != "chat" && requestFormat != "responses" {
			http.Error(w, `{"error":"invalid format"}`, http.StatusBadRequest)
			return
		}
		streaming, err := parseOptionalBool(r.URL.Query().Get("stream"))
		if err != nil {
			http.Error(w, `{"error":"invalid stream flag"}`, http.StatusBadRequest)
			return
		}
		tools, err := parseOptionalBool(r.URL.Query().Get("tools"))
		if err != nil {
			http.Error(w, `{"error":"invalid tools flag"}`, http.StatusBadRequest)
			return
		}
		toolCall, err := parseOptionalBool(r.URL.Query().Get("tool_call"))
		if err != nil {
			http.Error(w, `{"error":"invalid tool_call flag"}`, http.StatusBadRequest)
			return
		}

		req := proxy.RequestRequirements{
			NeedsStreaming: streaming,
			NeedsTools:     tools,
			NeedsToolCall:  tools && toolCall,
		}
		requestCandidates := s.proxy.Table().DebugSelection(debugModel, requestFormat, req, s.proxy.Stats(), s.proxy.Breakers())
		requestOut := make([]entry, len(requestCandidates))
		for i, candidate := range requestCandidates {
			requestOut[i] = entry{
				Provider:           candidate.ProviderName,
				Score:              candidate.Score,
				LatencyMs:          candidate.LatencyMs,
				Format:             candidate.APIFormat,
				MatchRank:          candidate.MatchRank,
				BreakerState:       candidate.BreakerState.String(),
				Requests:           candidate.Requests,
				Errors:             candidate.Errors,
				ErrorRate:          candidate.ErrorRate,
				ChatStreaming:      candidate.ChatStreaming,
				ChatToolUse:        candidate.ChatToolUse,
				ResponsesBasic:     candidate.ResponsesBasic,
				ResponsesStreaming: candidate.ResponsesStreaming,
				ResponsesToolUse:   candidate.ResponsesToolUse,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":              debugModel,
			"mode":               "request_aware",
			"request_format":     requestFormat,
			"requirements":       req,
			"request_candidates": requestOut,
			"static_scores":      staticOut,
		})
		return
	}

	snap := s.proxy.Stats().Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"enabled": s.proxy.Config().Enabled,
		"ready":   s.proxy.Table().Ready(),
		"models":  s.proxy.Table().Models(),
		"stats":   snap,
	})
}
