package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"relay-monitor/internal/checker"
	"relay-monitor/internal/config"
	"relay-monitor/internal/provider"
	"relay-monitor/internal/scheduler"
	"relay-monitor/internal/server"
	"relay-monitor/internal/store"
)

// Thread-safe timestamped printer
var printMu sync.Mutex

func tprint(format string, args ...any) {
	printMu.Lock()
	defer printMu.Unlock()
	ts := time.Now().Format("15:04:05")
	fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

func main() {
	args := os.Args[1:]

	// Parse flags
	var (
		runAll      bool
		listOnly    bool
		verifyMode  bool
		fingerprint bool
		addMode     bool
		removeMode  bool
		serveMode   bool
	)
	var positional []string

	for _, a := range args {
		switch a {
		case "--all":
			runAll = true
		case "--list":
			listOnly = true
		case "--verify":
			verifyMode = true
		case "--fingerprint":
			fingerprint = true
		case "--add":
			addMode = true
		case "--remove":
			removeMode = true
		case "--serve", "serve":
			serveMode = true
		case "--help", "-h":
			printUsage()
			return
		default:
			if !strings.HasPrefix(a, "--") {
				positional = append(positional, a)
			}
		}
	}

	// No args = serve mode (dashboard is the default)
	if len(os.Args) == 1 {
		serveMode = true
	}

	// Load config
	cfg, err := config.LoadConfig("config.toml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Load providers
	providers, err := config.LoadProviders(cfg.ProvidersFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "providers error: %v\n", err)
		os.Exit(1)
	}

	if addMode {
		runAddProvider(cfg.ProvidersFile, providers, cfg)
		return
	}
	if removeMode {
		runRemoveProvider(cfg.ProvidersFile, providers)
		return
	}

	if len(providers) == 0 {
		fmt.Printf("No providers configured. Use --add or edit %s\n", cfg.ProvidersFile)
		return
	}

	// Create engine
	engine := &checker.Engine{
		Client:          checker.NewClient(cfg.SSLVerify),
		MaxConcurrency:  cfg.MaxConcurrency,
		RequestInterval: cfg.RequestInterval.Duration,
	}

	// Context with Ctrl+C handling
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.Println("Interrupt received, shutting down...")
		cancel()
		<-sigCh
		os.Exit(1)
	}()

	// Serve mode: start dashboard
	if serveMode {
		runServe(ctx, cfg, engine, providers)
		return
	}

	// CLI modes below
	if !runAll && !listOnly && len(positional) == 0 {
		providers = selectProviders(providers)
	} else if len(positional) > 0 {
		providers = filterByName(providers, positional)
	}

	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name
	}
	fmt.Printf("Testing %d providers: %s\n", len(providers), strings.Join(names, ", "))

	if listOnly {
		runListModels(ctx, engine, providers)
		return
	}
	if verifyMode {
		fmt.Println("--verify mode not yet implemented")
		return
	}
	if fingerprint {
		fmt.Println("--fingerprint mode not yet implemented")
		return
	}

	// Basic test mode (CLI)
	fmt.Printf("Test: %s\n", checker.TestPrompt)
	fmt.Printf("Expected: %d\n", checker.CorrectNum)

	results := engine.RunBasicCheck(ctx, providers, func(msg string) { tprint("%s", msg) })

	order := make(map[string]int)
	for i, p := range providers {
		order[p.Name] = i
	}
	sort.Slice(results, func(i, j int) bool {
		return order[results[i].Provider] < order[results[j].Provider]
	})

	printReport(results)

	if ctx.Err() != nil {
		fmt.Println("\nTest interrupted, partial results shown above.")
	}
}

func runServe(ctx context.Context, cfg *config.AppConfig, engine *checker.Engine, providers []provider.Provider) {
	// Open SQLite
	dbPath := filepath.Join(cfg.DataDir, "relay-monitor.db")
	st, err := store.New(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	if err := st.Cleanup(cfg.RetentionDays); err != nil {
		log.Printf("cleanup: %v", err)
	}

	// Create server
	srv, err := server.New(cfg, st, engine, providers)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}
	// Toast notifications disabled — user prefers checking dashboard manually
	// srv.SetNotifier(notifier.New())

	// Create scheduler: 8h interval, full test (not quick)
	sched := scheduler.New(cfg.CheckInterval.Duration, func(sctx context.Context) {
		srv.RunCheckAndStore(sctx, providers, "scheduled", checker.ModeFull)
	})
	srv.SetScheduler(sched)

	// Start scheduler in background
	go sched.Start(ctx)

	// Start HTTP server (blocks until ctx cancelled)
	if err := srv.Start(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func filterByName(providers []provider.Provider, names []string) []provider.Provider {
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[strings.ToLower(n)] = true
	}
	var filtered []provider.Provider
	for _, p := range providers {
		if nameSet[strings.ToLower(p.Name)] {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		fmt.Println("No matching providers. Available:")
		for _, p := range providers {
			fmt.Printf("  %s\n", p.Name)
		}
		os.Exit(1)
	}
	return filtered
}

func printUsage() {
	fmt.Println(`relay-monitor — relay station monitoring tool

Usage:
  relay-monitor                  Start dashboard (default)
  relay-monitor serve            Start dashboard
  relay-monitor --all            CLI: test all providers
  relay-monitor --list           CLI: list models only
  relay-monitor --verify         CLI: verify mode
  relay-monitor --fingerprint    CLI: fingerprint mode
  relay-monitor --add            Add a new provider
  relay-monitor --remove         Remove a provider
  relay-monitor <name> ...       CLI: test specific providers`)
}

func selectProviders(providers []provider.Provider) []provider.Provider {
	fmt.Println("\nAvailable providers:")
	fmt.Println("   0. Test all")
	for i, p := range providers {
		tag := ""
		if p.APIFormat == "responses" {
			tag = " [responses]"
		}
		fmt.Printf("  %2d. %s%s\n", i+1, p.Name, tag)
	}

	fmt.Print("\nEnter numbers (comma/space separated, 0=all, enter=all): ")
	var input string
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" || input == "0" {
		return providers
	}

	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' '
	})

	var selected []provider.Provider
	seen := make(map[int]bool)
	for _, s := range parts {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			continue
		}
		if n == 0 {
			return providers
		}
		if n >= 1 && n <= len(providers) && !seen[n] {
			selected = append(selected, providers[n-1])
			seen[n] = true
		}
	}
	if len(selected) == 0 {
		return providers
	}
	return selected
}

func runListModels(ctx context.Context, engine *checker.Engine, providers []provider.Provider) {
	type listResult struct {
		name   string
		models []string
		err    string
	}

	results := make([]listResult, len(providers))
	var wg sync.WaitGroup

	for i, p := range providers {
		wg.Add(1)
		go func(i int, p provider.Provider) {
			defer wg.Done()
			models, err := engine.FetchModels(ctx, p.BaseURL, p.APIKey)
			r := listResult{name: p.Name}
			if err != nil {
				r.err = err.Error()
			} else {
				r.models = models
			}
			results[i] = r
		}(i, p)
	}
	wg.Wait()

	for _, r := range results {
		fmt.Printf("\n--- %s ---\n", r.name)
		if r.err != "" {
			fmt.Printf("  (error: %s)\n", r.err)
			continue
		}
		for _, m := range r.models {
			skip := ""
			if provider.ShouldSkip(m) {
				skip = "  [SKIP]"
			}
			fmt.Printf("  %s  [%s]%s\n", m, provider.IdentifyVendor(m), skip)
		}
	}
}

func printReport(results []*checker.ProviderResult) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  Report  %s\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Println(strings.Repeat("=", 70))

	type modelEntry struct {
		provider string
		model    string
		status   string
		correct  bool
		latency  int64
		answer   string
		err      string
	}

	vendorMap := make(map[string][]modelEntry)
	for _, pr := range results {
		for _, r := range pr.Results {
			ans := r.Answer
			if len(ans) > 40 {
				ans = ans[:40]
			}
			ans = strings.ReplaceAll(ans, "\n", " ")
			errMsg := r.Error
			if len(errMsg) > 60 {
				errMsg = errMsg[:60]
			}
			vendorMap[r.Vendor] = append(vendorMap[r.Vendor], modelEntry{
				provider: pr.Provider,
				model:    r.Model,
				status:   r.Status,
				correct:  r.Correct,
				latency:  r.LatencyMs,
				answer:   ans,
				err:      errMsg,
			})
		}
	}

	vendors := make([]string, 0, len(vendorMap))
	for v := range vendorMap {
		vendors = append(vendors, v)
	}
	sort.Strings(vendors)

	for _, vendor := range vendors {
		items := vendorMap[vendor]
		correctCount := 0
		for _, x := range items {
			if x.correct {
				correctCount++
			}
		}
		fmt.Printf("\n--- %s (%d/%d correct) ---\n", vendor, correctCount, len(items))

		sort.Slice(items, func(i, j int) bool {
			if items[i].correct != items[j].correct {
				return items[i].correct
			}
			if (items[i].status == "ok") != (items[j].status == "ok") {
				return items[i].status == "ok"
			}
			return items[i].latency < items[j].latency
		})

		for _, x := range items {
			if x.status == "ok" {
				tag := "WRONG"
				if x.correct {
					tag = " OK "
				}
				fmt.Printf("  [%s] %6.2fs  %-12s  %s\n", tag, float64(x.latency)/1000, x.provider, x.model)
				if !x.correct {
					fmt.Printf("         answer: %s\n", x.answer)
				}
			} else {
				fmt.Printf("  [FAIL]         %-12s  %s  %s\n", x.provider, x.model, x.err)
			}
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Println("  Best 2 per vendor")
	fmt.Println(strings.Repeat("=", 70))
	for _, vendor := range vendors {
		items := vendorMap[vendor]
		var correctItems []modelEntry
		for _, x := range items {
			if x.correct {
				correctItems = append(correctItems, x)
			}
		}
		sort.Slice(correctItems, func(i, j int) bool {
			return correctItems[i].latency < correctItems[j].latency
		})
		if len(correctItems) == 0 {
			fmt.Printf("  %s: no available models\n", vendor)
			continue
		}
		top := correctItems
		if len(top) > 2 {
			top = top[:2]
		}
		for i, x := range top {
			fmt.Printf("  %s #%d: %s/%s (%.2fs)\n", vendor, i+1, x.provider, x.model, float64(x.latency)/1000)
		}
	}
}

func runAddProvider(path string, providers []provider.Provider, cfg *config.AppConfig) {
	fmt.Println("\n=== Add Provider ===")
	name := prompt("Name: ")
	if name == "" {
		fmt.Println("Cancelled"); return
	}
	baseURL := prompt("Base URL (e.g. https://example.com/v1): ")
	if baseURL == "" {
		fmt.Println("Cancelled"); return
	}
	apiKey := prompt("API Key (sk-xxx): ")
	if apiKey == "" {
		fmt.Println("Cancelled"); return
	}
	fmtStr := prompt("API format (enter=chat, r=responses): ")

	entry := provider.Provider{Name: name, BaseURL: baseURL, APIKey: apiKey}
	if strings.HasPrefix(strings.ToLower(fmtStr), "r") {
		entry.APIFormat = "responses"
	}

	fmt.Printf("Validating %s...\n", name)
	engine := &checker.Engine{Client: checker.NewClient(cfg.SSLVerify)}
	models, err := engine.FetchModels(context.Background(), baseURL, apiKey)
	if err != nil {
		fmt.Printf("  %s\n", err)
		confirm := prompt("Add anyway? (y/N): ")
		if strings.ToLower(confirm) != "y" {
			fmt.Println("Cancelled"); return
		}
	} else {
		fmt.Printf("  OK, %d models found\n", len(models))
	}

	providers = append(providers, entry)
	if err := config.SaveProviders(path, providers); err != nil {
		fmt.Fprintf(os.Stderr, "save error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Added %s, total %d providers. Config: %s\n", name, len(providers), path)
}

func runRemoveProvider(path string, providers []provider.Provider) {
	if len(providers) == 0 {
		fmt.Println("No providers to remove."); return
	}
	fmt.Println("\n=== Remove Provider ===")
	for i, p := range providers {
		fmt.Printf("  %2d. %s  (%s)\n", i+1, p.Name, p.BaseURL)
	}

	raw := prompt("\nNumbers to remove (comma/space separated): ")
	if raw == "" {
		fmt.Println("Cancelled"); return
	}

	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	toRemove := make(map[int]bool)
	for _, s := range parts {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 1 && n <= len(providers) {
			toRemove[n-1] = true
		}
	}
	if len(toRemove) == 0 {
		fmt.Println("No valid selection"); return
	}

	var names []string
	for i := range toRemove {
		names = append(names, providers[i].Name)
	}
	confirm := prompt(fmt.Sprintf("Delete %s? (y/N): ", strings.Join(names, ", ")))
	if strings.ToLower(confirm) != "y" {
		fmt.Println("Cancelled"); return
	}

	var remaining []provider.Provider
	for i, p := range providers {
		if !toRemove[i] {
			remaining = append(remaining, p)
		}
	}
	if err := config.SaveProviders(path, remaining); err != nil {
		fmt.Fprintf(os.Stderr, "save error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %d, %d remaining.\n", len(toRemove), len(remaining))
}

func prompt(label string) string {
	fmt.Print(label)
	var s string
	fmt.Scanln(&s)
	return strings.TrimSpace(s)
}
