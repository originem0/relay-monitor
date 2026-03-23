package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sort"
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

	mu         sync.Mutex
	isChecking bool
	lastCheck  time.Time
}

// ProviderCard is the view model for a dashboard card.
type ProviderCard struct {
	Name         string
	Status       string
	Platform     string
	ModelsOK     int
	ModelsTotal  int
	AvgLatencyMs float64
	Health       int
	Balance      *float64
	Pinned       bool
	Note         string
	ErrorMsg     string
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
	CCCompat       bool
	Providers      []ModelProviderRow
}

// ModelProviderRow is one provider's data for a specific model.
type ModelProviderRow struct {
	ProviderName string
	BaseURL      string
	Model        string
	LatencyMs    int64
	Correct      bool
	Verdict      string
	VerdictClass string
	ToolUse      string // "true", "false", ""
	Streaming    string
	IsBest       bool
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
		Name     string
		Status   string
		Health   int
		URL      string
		Pinned   bool
		Note     string
		APIKey   string
		AccessToken string
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
	ProviderName  string
	Model         string
	L1, L2, L3, L4 string // "1/1", "2/3" format
	TotalScore    int
	ExpectedTier  string
	ExpectedMin   int
	Verdict       string
	VerdictClass  string
	SelfIDVerdict string
	SelfIDDetail  string
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
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("Dashboard: http://localhost%s", s.cfg.Listen)
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
	s.store.InsertCheckRun(runID, "basic", triggerType)

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
		totalMu                                sync.Mutex
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

		// Write results to DB immediately
		provModels, provOK, provCorrect := 0, 0, 0
		for _, r := range pr.Results {
			s.store.InsertCheckResult(runID, pid, r.Model, r.Vendor, r.Status,
				r.Correct, r.Answer, r.LatencyMs, r.Error, r.HasReasoning)
			provModels++
			if r.Status == "ok" {
				provOK++
			}
			if r.Correct {
				provCorrect++
			}
		}

		totalMu.Lock()
		totalProviders++
		totalModels += provModels
		totalOK += provOK
		totalCorrect += provCorrect
		totalMu.Unlock()

		// Change detection
		if prev := prevByProvider[pid]; prev != nil {
			for _, de := range checker.Diff(pr.Provider, pr.Results, prev) {
				s.store.InsertEvent(de.Type, de.Provider, de.Model, de.OldValue, de.NewValue, de.Message)
				s.sseHub.Publish(SSEEvent{Type: "event_created", Data: de.Message})
				if s.notifier != nil {
					s.notifier.HandleEvent(de.Type, de.Provider, de.Model, de.OldValue, de.NewValue, de.Message)
				}
			}
		}

		// Update provider status
		status, health := "down", 0.0
		if len(pr.Results) > 0 {
			ratio := float64(provCorrect) / float64(len(pr.Results))
			if ratio > 0.8 {
				status = "ok"
			} else if ratio > 0.5 {
				status = "degraded"
			}
			health = ratio * 100
		}
		errMsg := ""
		if pr.Error != "" && len(pr.Results) == 0 {
			status = "error"
			errMsg = pr.Error
		}
		s.store.UpdateProviderStatus(pid, status, health, errMsg)

		// Push per-provider SSE update so dashboard refreshes incrementally
		s.sseHub.Publish(SSEEvent{Type: "provider_done", Data: pr.Provider})

		// Platform detection + balance + capability (lightweight, per provider)
		prov := s.findProvider(pr.Provider)
		if prov == nil {
			return
		}
		dbProv, _ := s.store.GetProviderByName(pr.Provider)
		if dbProv != nil && dbProv.Platform == "unknown" {
			if platform := checker.DetectPlatform(ctx, s.engine.Client, prov.BaseURL); platform != "unknown" {
				s.store.UpdateProviderPlatform(pid, platform)
			}
		}
		if prov.AccessToken != "" {
			bi, berr := checker.QueryBalance(ctx, s.engine.Client, prov.BaseURL, prov.AccessToken)
			if berr != nil {
				log.Printf("[balance] %s: error: %v", pr.Provider, berr)
			} else if bi != nil {
				log.Printf("[balance] %s: quota=%.0f", pr.Provider, bi.Remaining)
				s.store.UpdateProviderBalance(pid, bi.Remaining)
			} else {
				log.Printf("[balance] %s: no data (token may be invalid)", pr.Provider)
			}
		}
		// Capability probing: max 3 new models per provider
		existingCaps, _ := s.store.GetCapabilities(pid)
		capSet := make(map[string]bool)
		for _, c := range existingCaps {
			capSet[c.Model] = true
		}
		probed := 0
		for _, r := range pr.Results {
			if probed >= 3 || !r.Correct || capSet[r.Model] || ctx.Err() != nil {
				continue
			}
			tu, st := checker.ProbeCapabilities(ctx, s.engine.Client, prov.BaseURL, prov.APIKey, r.Model, prov.APIFormat)
			s.store.UpsertCapability(pid, r.Model, &st, &tu)
			probed++
		}
	})

	totalMu.Lock()
	summary := fmt.Sprintf("%d providers, %d models, %d ok, %d correct",
		totalProviders, totalModels, totalOK, totalCorrect)
	totalMu.Unlock()
	s.store.FinishCheckRun(runID, totalProviders, totalModels, totalOK, totalCorrect, summary)
	s.sseHub.Publish(SSEEvent{Type: "check_completed", Data: summary})

	// Rebuild proxy routing table with fresh check data
	if s.proxy != nil {
		results, _ := s.store.GetLatestResults()
		dbProviders, _ := s.store.GetProviders()
		s.proxy.RebuildTable(results, dbProviders, s.providers)
		log.Printf("[proxy] routing table rebuilt: %d models", len(s.proxy.Table().Models()))
	}
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
			Health:   int(dp.Health),
			Balance:  dp.LastBalance,
			ErrorMsg: dp.LastError,
		}
		if meta, ok := provMeta[dp.Name]; ok {
			card.Pinned = meta.Pinned
			card.Note = meta.Note
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

	// Build provider lookup: id -> (name, baseURL)
	provInfo := make(map[int64][2]string) // [name, baseURL]
	provIDByName := make(map[string]int64)
	for _, dp := range dbProviders {
		provInfo[dp.ID] = [2]string{dp.Name, dp.BaseURL}
		provIDByName[dp.Name] = dp.ID
	}
	provBaseURL := make(map[string]string)
	for _, p := range s.providers {
		provBaseURL[p.Name] = p.BaseURL
	}

	// Group results by model
	type modelKey struct{ model, vendor string }
	type provResult struct {
		providerID   int64
		providerName string
		baseURL      string
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
					if cap.ToolUse != nil && *cap.ToolUse && cap.Streaming != nil && *cap.Streaming {
						anyCC = true
					}
				}
			}
			mg.Providers = append(mg.Providers, row)
		}
		mg.CCCompat = anyCC

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
		Name     string
		Status   string
		Health   int
		URL      string
		Pinned   bool
		Note     string
		APIKey   string
		AccessToken string
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
	for _, p := range s.providers {
		if strings.EqualFold(p.Name, name) {
			target = &p
			break
		}
	}
	if target == nil {
		http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
		return
	}

	// Run synchronously — single provider is fast enough
	pid, _ := s.store.UpsertProvider(target.Name, target.BaseURL, target.APIFormat, target.Platform)
	pr := s.engine.TestProvider(r.Context(), *target, checker.ModeFull, func(msg string) {
		log.Printf("[manual] [%s] %s", time.Now().Format("15:04:05"), msg)
	})

	// Store results
	runID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
	s.store.InsertCheckRun(runID, "basic", "manual")
	for _, tr := range pr.Results {
		s.store.InsertCheckResult(runID, pid, tr.Model, tr.Vendor, tr.Status,
			tr.Correct, tr.Answer, tr.LatencyMs, tr.Error, tr.HasReasoning)
	}

	correct := 0
	for _, tr := range pr.Results {
		if tr.Correct {
			correct++
		}
	}
	status, health := "down", 0.0
	if len(pr.Results) > 0 {
		ratio := float64(correct) / float64(len(pr.Results))
		if ratio > 0.8 {
			status = "ok"
		} else if ratio > 0.5 {
			status = "degraded"
		}
		health = ratio * 100
	}
	errMsg := ""
	if pr.Error != "" && len(pr.Results) == 0 {
		status = "error"
		errMsg = pr.Error
	}
	s.store.UpdateProviderStatus(pid, status, health, errMsg)
	s.store.FinishCheckRun(runID, 1, len(pr.Results), 0, correct,
		fmt.Sprintf("%s: %d models, %d correct", target.Name, len(pr.Results), correct))

	// Return result directly as HTML feedback
	if errMsg != "" {
		fmt.Fprintf(w, `<span style="color:var(--down)">%s: %s</span>`, name, errMsg)
	} else if len(pr.Results) == 0 {
		fmt.Fprintf(w, `<span style="color:var(--down)">%s: 无可用模型</span>`, name)
	} else {
		fmt.Fprintf(w, `<span style="color:var(--ok)">%s: %d/%d 正确 — <a href="/provider/%s" style="color:var(--accent)">查看详情</a></span>`,
			name, correct, len(pr.Results), name)
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
	}

	for _, p := range provs {
		if ctx.Err() != nil {
			break
		}
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
	for _, p := range s.providers {
		if p.Name == name {
			return &p
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

	// Check for duplicate hostname
	for _, p := range s.providers {
		if config.SameHost(p.BaseURL, baseURL) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `<div class="result-error">与已有站点 "%s" 重复（相同域名）</div>`, p.Name)
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
		fmt.Fprintf(w, `<div class="result-error">保存失败: %s</div>`, saveErr)
		return
	}

	// Upsert to DB
	s.store.UpsertProvider(name, baseURL, apiFormat, "")

	msg := fmt.Sprintf("已添加 %s", name)
	if err != nil {
		msg += fmt.Sprintf(" (连接验证失败: %s, 但已保存)", err)
	}

	fmt.Fprintf(w, `<div class="result-success">%s <a href="/">返回总览</a></div>`, msg)

	// Trigger initial check in background
	go s.RunCheckAndStore(context.Background(), []provider.Provider{newProv}, "manual", checker.ModeFull)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	s.mu.Lock()
	var remaining []provider.Provider
	for _, p := range s.providers {
		if p.Name != name {
			remaining = append(remaining, p)
		}
	}
	s.providers = remaining
	s.mu.Unlock()

	config.SaveProviders(s.cfg.ProvidersFile, remaining)

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
	w.Header().Set("HX-Redirect", "/provider/"+name)
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
	w.Header().Set("HX-Redirect", "/provider/"+name)
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

	s.mu.Lock()
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
			break
		}
	}
	provs := make([]provider.Provider, len(s.providers))
	copy(provs, s.providers)
	s.mu.Unlock()
	config.SaveProviders(s.cfg.ProvidersFile, provs)

	// Update DB provider name so store stays in sync
	if newName != "" && newName != name {
		dp, err := s.store.GetProviderByName(name)
		if err == nil && dp != nil {
			s.store.RenameProvider(dp.ID, newName)
		}
	}

	redirect := "/provider/" + name
	if newName != "" && newName != name {
		redirect = "/provider/" + newName
	}
	w.Header().Set("HX-Redirect", redirect)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("Proxy")

	type ProxyPageData struct {
		pageData
		ProxyEnabled   bool
		ProxyEndpoint  string
		ProxyAPIKey    string
		ProxyWarming   bool
		ProxyModels    []proxyModelStat
		ProxyProviders []proxyProviderStat
		ModelCatalog   []proxy.ModelInfo
		TotalRequests  int64
		TotalErrors    int64
		AvgLatencyMs   int64
		ModelCount     int
	}

	pd := ProxyPageData{pageData: data}

	if s.proxy != nil {
		cfg := s.proxy.Config()
		pd.ProxyEnabled = cfg.Enabled
		pd.ProxyEndpoint = fmt.Sprintf("http://localhost%s/v1", s.cfg.Listen)
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

		provModelCount := s.proxy.Table().ProviderModelCount()
		for _, ps := range providerStats {
			ps.RoutingModels = provModelCount[ps.ProviderID]
			if ps.Requests > 0 {
				ps.ErrorRate = float64(ps.Errors) / float64(ps.Requests) * 100
			}
			pd.ProxyProviders = append(pd.ProxyProviders, *ps)
		}
		sort.Slice(pd.ProxyProviders, func(i, j int) bool {
			return pd.ProxyProviders[i].Requests > pd.ProxyProviders[j].Requests
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

func (s *Server) handleProxyStats(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}

	snap := s.proxy.Stats().Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"enabled":  s.proxy.Config().Enabled,
		"ready":    s.proxy.Table().Ready(),
		"models":   s.proxy.Table().Models(),
		"stats":    snap,
	})
}
