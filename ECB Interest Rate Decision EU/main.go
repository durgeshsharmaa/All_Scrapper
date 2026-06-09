package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	country   = "EU"
	eventName = "ECB Interest Rate Decision"

	indexURL = "https://www.ecb.europa.eu/press/govcdec/mopo/html/index.en.html"

	// Event configuration for final production deployment.
	// Calendar release time: 11 June 2026 17:45:00 IST.
	// IST is UTC+05:30, so the UTC time below is 2026-06-11 12:15:00.
	defaultEventTimeIST = "2026-06-11 17:45:00"
	defaultEventTimeUTC = "2026-06-11 12:15:00"
	defaultExpectedDate = "2026-06-11"

	testConnectionLead = 1 * time.Minute
	sniperLead         = 2 * time.Second
	pollWindow         = 3 * time.Minute
	pollEvery          = 500 * time.Millisecond
	requestTimeout     = 12 * time.Second
	bodyLimit          = 8 << 20
	maxDiscoverySnips  = 3
	discoveryAttempts  = 3
	releaseAttempts    = 2

	userAgent = "ECBInterestRateDecisionEU/1.0 (official ECB sniper scraper; contact=research@example.com)"
)

var (
	snippetListRE = regexp.MustCompile(`(?is)\bdata-snippets\s*=\s*['"]([^'"]+)['"]`)
	snippetItemRE = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']([^"']*ecb\.mp(\d{6})~[A-Za-z0-9]+\.en\.html)["'][^>]*>\s*Monetary policy decisions\s*</a>`)
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	breakTagRE    = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockCloseRE  = regexp.MustCompile(`(?is)</(tr|div|p|h[1-6]|li|table|thead|tbody|tfoot|caption|dd|dt)>`)
	tagRE         = regexp.MustCompile(`(?is)<[^>]+>`)
	yearRE        = regexp.MustCompile(`^(19|20)\d{2}$`)                       // package-level: don't compile in loop
	effDateRE     = regexp.MustCompile(`(?i)\b(\d{1,2})\s+([A-Za-z]{3,9})\.?`) // package-level: don't compile in loop
)

type FetchMeta struct {
	URL               string
	Status            string
	StatusCode        int
	ServerDate        string
	LastModified      string
	ETag              string
	RequestStarted    time.Time
	FirstByteReceived time.Time
	ResponseReceived  time.Time
	Latency           time.Duration
	TimeToFirstByte   time.Duration
	ResponseBytes     int
}

type DecisionItem struct {
	Title       string
	DateISO     string
	DateDisplay string
	URL         string
	LinkSlug    string
	SnippetURL  string
}

func main() {
	eventTimeRaw := flag.String("event-time-utc", defaultEventTimeUTC, "ECB decision release time in UTC, format YYYY-MM-DD HH:MM:SS")
	expectedDate := flag.String("expected-date", defaultExpectedDate, "expected ECB decision date in YYYY-MM-DD; blank disables date validation")
	once := flag.Bool("once", false, "fetch latest item and parse its press release immediately without waiting for a new link")
	timeout := flag.Duration("request-timeout", requestTimeout, "timeout per ECB request")
	flag.Parse()

	eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(*eventTimeRaw), time.UTC)
	if err != nil {
		fmt.Printf("Configuration error: invalid -event-time-utc: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*expectedDate) != "" {
		if _, err := time.Parse("2006-01-02", strings.TrimSpace(*expectedDate)); err != nil {
			fmt.Printf("Configuration error: invalid -expected-date: %v\n", err)
			os.Exit(1)
		}
	}

	client := newHTTPClient()

	expectedDateValue := strings.TrimSpace(*expectedDate)
	printHeader(eventTime, expectedDateValue)
	printSources()

	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout(*timeout))
	baseline, _, err := discoverLatestDecision(ctx, client, *timeout)
	cancel()
	if err != nil {
		fmt.Printf("NOT_CONFIRMED\nInitial discovery failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching current published decision from official sources...")
	currentResult, currentErr := fetchAndParseDecision(context.Background(), client, baseline, eventTime, "", *timeout)
	keyRatesResult, keyRatesErr := fetchAndParseKeyInterestRates(context.Background(), client, *timeout)
	printCurrentSnapshot(baseline, currentResult, currentErr, keyRatesResult, keyRatesErr)

	if *once {
		if currentErr != nil {
			fmt.Printf("NOT_CONFIRMED\n%s\n", currentErr)
			os.Exit(1)
		}
		printDecision(currentResult)
		printKeyRatesConfirmation(currentResult, keyRatesResult, keyRatesErr)
		return
	}

	// Only fast-path if the event time has already passed — prevents printing stale pre-event data.
	if sameExpectedDate(baseline, expectedDateValue) && !time.Now().UTC().Before(eventTime) {
		result, err := fetchAndParseDecision(context.Background(), client, baseline, eventTime, expectedDateValue, *timeout)
		if err != nil {
			fmt.Printf("NOT_CONFIRMED\n%s\n", err)
			os.Exit(1)
		}
		printDecision(result)
		keyRatesResult, keyRatesErr := fetchAndParseKeyInterestRates(context.Background(), client, *timeout)
		printKeyRatesConfirmation(result, keyRatesResult, keyRatesErr)
		return
	}

	fmt.Println("Current data captured. Waiting for the configured announcement date.")

	testTime := eventTime.Add(-testConnectionLead)
	sniperStart := eventTime.Add(-sniperLead)
	endTime := eventTime.Add(pollWindow)
	now := time.Now().UTC()

	if now.Before(testTime) {
		fmt.Printf("Countdown to connection test: %s\n", testTime.Sub(now).Round(time.Second))
		countdownUntilWithStatus("Time remaining to connection test", testTime, time.Second)
		fmt.Println() // newline after \r countdown
	}

	fmt.Println("Testing discovery path before sniper mode...")
	ctx, cancel = context.WithTimeout(context.Background(), discoveryTimeout(*timeout))
	check, _, err := discoverLatestDecision(ctx, client, *timeout)
	cancel()
	if err != nil {
		fmt.Printf("Connection test failed: %v\n", err)
	} else {
		fmt.Printf("Connection test latest item: %s | %s\n", check.DateISO, check.LinkSlug)
	}

	now = time.Now().UTC()
	if now.Before(sniperStart) {
		fmt.Printf("Final countdown to sniper mode: %s\n", sniperStart.Sub(now).Round(time.Millisecond))
		countdownUntilWithStatus("Time remaining to sniper mode", sniperStart, 100*time.Millisecond)
		fmt.Println() // newline after \r countdown
	}
	if time.Now().UTC().After(endTime) {
		fmt.Println("Event polling window is already over. Use -once to parse the current latest release or update -event-time-utc.")
		return
	}

	fmt.Println("SNIPER MODE ACTIVE: polling ECB release index/include for a new Monetary policy decisions link.")
	ctx, cancel = context.WithDeadline(context.Background(), endTime)
	defer cancel()

	pollCount := 0
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("NOT_CONFIRMED")
			fmt.Println("No new ECB Monetary policy decisions link appeared before the polling window ended.")
			return
		default:
		}

		pollCount++
		reqCtx, reqCancel := context.WithTimeout(ctx, discoveryTimeout(*timeout))
		latest, _, err := discoverLatestDecision(reqCtx, client, *timeout)
		reqCancel()
		if err == nil && isNewDecision(latest, baseline, expectedDateValue) {
			fmt.Printf("[%s] NEW LINK DETECTED poll=%d date=%s link=%s\n",
				time.Now().UTC().Format("15:04:05.000"), pollCount, latest.DateISO, latest.URL)
			result, parseErr := fetchAndParseDecision(ctx, client, latest, eventTime, expectedDateValue, *timeout)
			if parseErr != nil {
				// Use return instead of os.Exit so deferred ticker.Stop() and cancel() run cleanly.
				fmt.Printf("NOT_CONFIRMED\n%s\n", parseErr)
				return
			}
			printDecision(result)
			keyRatesResult, keyRatesErr := fetchAndParseKeyInterestRates(ctx, client, *timeout)
			printKeyRatesConfirmation(result, keyRatesResult, keyRatesErr)
			return
		}
		if err != nil && pollCount%10 == 0 {
			fmt.Printf("[%s] poll=%d discovery error: %v\n", time.Now().UTC().Format("15:04:05.000"), pollCount, err)
		}

		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
	}
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 12 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func printHeader(eventTime time.Time, expectedDate string) {
	fmt.Println("ECB INTEREST RATE DECISION EU - SNIPER MODE")
	fmt.Printf("Configured Event Time (IST): %s\n", defaultEventTimeIST)
	fmt.Printf("Event Time (IST): %s\n", formatISTTime(eventTime))
	fmt.Printf("Event Time (Europe/Berlin): %s\n", formatEventTime(eventTime))
	fmt.Printf("Event Time (UTC): %s\n", eventTime.UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected Release Date: %s\n", blankNA(expectedDate))
	fmt.Println("FIELD=main refinancing operations rate")
	fmt.Println("IGNORED=deposit facility, marginal lending facility")
}

func printSources() {
	fmt.Println()
	fmt.Println("Sources:")
	fmt.Println("1. ECB monetary policy decisions index | Official HTML | Latest ecb.mpYYMMDD~HASH link discovery")
	fmt.Println("2. ECB monetary policy press-release page | Official HTML | Key ECB rates sentence parse")
	fmt.Println("3. ECB key interest rates page | Official HTML | Effective-date rate table confirmation")
	fmt.Println()
}

func printCurrentSnapshot(item DecisionItem, result RateDecision, err error, keyRates KeyRatesPageResult, keyRatesErr error) {
	if err != nil {
		fmt.Printf("  OK   [Source 1 current] Date: %s Link: %s    Title: %s\n",
			item.DateISO,
			item.LinkSlug,
			item.Title,
		)
		fmt.Printf("  FAIL [Source 2 current] %s\n", err)
		printSource3Current(keyRates, keyRatesErr, nil)
		return
	}
	fmt.Printf("  OK   [Source 1 current] Date: %s Actual: %s%%    Link: %s    Title: %s\n",
		item.DateISO,
		result.Actual,
		item.LinkSlug,
		item.Title,
	)
	fmt.Printf("  OK   [Source 2 current] Date: %s Actual: %s%%    MRO: %s%% Deposit: %s%% Marginal: %s%%\n",
		result.DecisionDate,
		result.Actual,
		result.MainRefinancingOperationsRate,
		result.DepositFacilityRate,
		result.MarginalLendingFacilityRate,
	)
	printSource3Current(keyRates, keyRatesErr, &result)
}

func formatEventTime(eventTime time.Time) string {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return eventTime.UTC().Format("2006-01-02 15:04:05 MST")
	}
	return eventTime.In(loc).Format("2006-01-02 15:04:05 MST")
}

func formatISTTime(eventTime time.Time) string {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return eventTime.UTC().Add(5*time.Hour+30*time.Minute).Format("2006-01-02 15:04:05") + " IST"
	}
	return eventTime.In(loc).Format("2006-01-02 15:04:05 MST")
}

// Source 1: poll the ECB monetary policy decisions index and resolve the latest
// ecb.mpYYMMDD~HASH.en.html press-release link.
func discoverLatestDecision(ctx context.Context, client *http.Client, timeout time.Duration) (DecisionItem, FetchMeta, error) {
	indexBody, indexMeta, err := fetchWithRetry(ctx, client, indexURL, timeout, discoveryAttempts)
	if err != nil {
		return DecisionItem{}, indexMeta, fmt.Errorf("fetch ECB index: %w", err)
	}

	snippets, err := parseSnippetURLs(indexURL, indexBody)
	if err != nil {
		return DecisionItem{}, indexMeta, err
	}

	var latest *DecisionItem
	var errs []string
	for i, snippetURL := range snippets {
		if i >= maxDiscoverySnips {
			break
		}
		body, _, err := fetchWithRetry(ctx, client, snippetURL, timeout, discoveryAttempts)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", snippetURL, err))
			continue
		}
		items, err := parseDecisionItems(snippetURL, body)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", snippetURL, err))
			continue
		}
		for i := range items {
			item := items[i]
			if latest == nil || item.DateISO > latest.DateISO {
				latest = &item
			}
		}
		if latest != nil {
			break
		}
	}

	if latest == nil {
		if len(errs) > 0 {
			return DecisionItem{}, indexMeta, fmt.Errorf("no monetary policy decision item found; snippet errors: %s", strings.Join(errs, "; "))
		}
		return DecisionItem{}, indexMeta, errors.New("no monetary policy decision item found in ECB snippets")
	}
	return *latest, indexMeta, nil
}

func parseSnippetURLs(base string, body []byte) ([]string, error) {
	match := snippetListRE.FindSubmatch(body)
	if match == nil {
		return nil, errors.New("ECB index data-snippets attribute not found")
	}

	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, err
	}

	raw := html.UnescapeString(string(match[1]))
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ref, err := url.Parse(part)
		if err != nil {
			return nil, fmt.Errorf("parse snippet URL %q: %w", part, err)
		}
		absolute := baseURL.ResolveReference(ref).String()
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		out = append(out, absolute)
	}
	if len(out) == 0 {
		return nil, errors.New("ECB index data-snippets attribute was empty")
	}
	return out, nil
}

func parseDecisionItems(snippetURL string, body []byte) ([]DecisionItem, error) {
	baseURL, err := url.Parse(snippetURL)
	if err != nil {
		return nil, err
	}

	matches := snippetItemRE.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, errors.New("no Monetary policy decisions links matching ecb.mpYYMMDD~HASH.en.html found")
	}

	items := make([]DecisionItem, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		dateISO, err := slugDateToISO(strings.TrimSpace(string(match[2])))
		if err != nil {
			return nil, err
		}
		href := strings.TrimSpace(html.UnescapeString(string(match[1])))
		ref, err := url.Parse(href)
		if err != nil {
			return nil, fmt.Errorf("parse decision href %q: %w", href, err)
		}
		absolute := baseURL.ResolveReference(ref).String()
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		items = append(items, DecisionItem{
			Title:       "Monetary policy decisions",
			DateISO:     dateISO,
			DateDisplay: displayDate(dateISO),
			URL:         absolute,
			LinkSlug:    lastPathSegment(absolute),
			SnippetURL:  snippetURL,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].DateISO == items[j].DateISO {
			return items[i].URL < items[j].URL
		}
		return items[i].DateISO > items[j].DateISO
	})
	return items, nil
}

func fetch(ctx context.Context, client *http.Client, rawURL string) ([]byte, FetchMeta, error) {
	meta := FetchMeta{URL: rawURL, RequestStarted: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, meta, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			meta.FirstByteReceived = time.Now().UTC()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := client.Do(req)
	if err != nil {
		meta.ResponseReceived = time.Now().UTC()
		meta.Latency = meta.ResponseReceived.Sub(meta.RequestStarted)
		return nil, meta, err
	}
	defer resp.Body.Close()

	meta.ResponseReceived = time.Now().UTC()
	meta.Latency = meta.ResponseReceived.Sub(meta.RequestStarted)
	if !meta.FirstByteReceived.IsZero() {
		meta.TimeToFirstByte = meta.FirstByteReceived.Sub(meta.RequestStarted)
	}
	meta.StatusCode = resp.StatusCode
	meta.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	meta.ServerDate = resp.Header.Get("Date")
	meta.LastModified = resp.Header.Get("Last-Modified")
	meta.ETag = resp.Header.Get("ETag")

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	meta.ResponseBytes = len(body)
	if readErr != nil {
		return body, meta, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return body, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(body))
	}
	return body, meta, nil
}

func fetchWithRetry(ctx context.Context, client *http.Client, rawURL string, timeout time.Duration, attempts int) ([]byte, FetchMeta, error) {
	var lastBody []byte
	var lastMeta FetchMeta
	var lastErr error
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		body, meta, err := fetch(reqCtx, client, rawURL)
		cancel()
		lastBody, lastMeta, lastErr = body, meta, err
		if err == nil {
			return body, meta, nil
		}
		if ctx.Err() != nil {
			return lastBody, lastMeta, lastErr
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return lastBody, lastMeta, lastErr
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
	}
	return lastBody, lastMeta, lastErr
}

func discoveryTimeout(timeout time.Duration) time.Duration {
	return time.Duration(discoveryAttempts)*timeout + 3*time.Second
}

func isNewDecision(latest, baseline DecisionItem, expectedDate string) bool {
	if expectedDate != "" {
		return latest.DateISO == expectedDate && latest.URL != baseline.URL
	}
	if latest.DateISO > baseline.DateISO {
		return true
	}
	return latest.DateISO == baseline.DateISO && latest.URL != baseline.URL
}

func sameExpectedDate(item DecisionItem, expectedDate string) bool {
	expectedDate = strings.TrimSpace(expectedDate)
	return expectedDate != "" && item.DateISO == expectedDate
}

func stripHTML(raw string) string {
	s := scriptStyleRE.ReplaceAllString(raw, " ")
	s = breakTagRE.ReplaceAllString(s, "\n")
	s = blockCloseRE.ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return s
}

func compactSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func displayDate(dateISO string) string {
	t, err := time.Parse("2006-01-02", dateISO)
	if err != nil {
		return dateISO
	}
	return t.Format("2 January 2006")
}

func lastPathSegment(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return rawURL
	}
	return parts[len(parts)-1]
}

func slugDateToISO(yymmdd string) (string, error) {
	if len(yymmdd) != 6 {
		return "", fmt.Errorf("invalid ECB mp slug date %q", yymmdd)
	}
	yy, err := strconv.Atoi(yymmdd[:2])
	if err != nil {
		return "", fmt.Errorf("invalid ECB mp slug year %q: %w", yymmdd, err)
	}
	month, err := strconv.Atoi(yymmdd[2:4])
	if err != nil {
		return "", fmt.Errorf("invalid ECB mp slug month %q: %w", yymmdd, err)
	}
	day, err := strconv.Atoi(yymmdd[4:6])
	if err != nil {
		return "", fmt.Errorf("invalid ECB mp slug day %q: %w", yymmdd, err)
	}
	year := 2000 + yy
	if yy >= 90 {
		year = 1900 + yy
	}
	date := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", fmt.Errorf("invalid ECB mp slug date %q: %w", yymmdd, err)
	}
	return date, nil
}

func formatWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return "[]"
	}
	return "[" + strings.Join(warnings, "; ") + "]"
}

func blankNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/A"
	}
	return strings.TrimSpace(s)
}

func firstLine(body []byte) string {
	text := compactSpaces(stripHTML(string(body)))
	if text == "" {
		return "empty response body"
	}
	if len(text) > 180 {
		return text[:180]
	}
	return text
}

func countdownUntilWithStatus(label string, target time.Time, step time.Duration) {
	lastPrint := time.Time{}
	for {
		remaining := time.Until(target)
		if remaining <= 0 {
			// Print a final cleared line so no garbage chars remain
			fmt.Printf("\r%-80s\n", label+": done")
			return
		}
		now := time.Now()
		if lastPrint.IsZero() || now.Sub(lastPrint) >= 1*time.Second {
			// Pad to 80 chars so shorter strings fully overwrite longer previous lines (Windows \r fix)
			line := fmt.Sprintf("%s: %s", label, formatCountdownDuration(remaining))
			fmt.Printf("\r%-80s", line)
			lastPrint = now
		}
		sleepFor := step
		if remaining < sleepFor {
			sleepFor = remaining
		}
		time.Sleep(sleepFor)
	}
}

func formatCountdownDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	millis := int(d.Milliseconds()) % 1000
	if millis < 0 {
		millis = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}

const actualRateField = "main_refinancing_operations"

var (
	titleRE           = regexp.MustCompile(`(?is)<h1[^>]*>\s*(.*?)\s*</h1>`)
	isoDateRE         = regexp.MustCompile(`(?is)<meta\s+property=["']article:published_time["']\s+content=["']([^"']+)["']`)
	percentRE         = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?\s*%`)
	releaseDateTextRE = regexp.MustCompile(`\b(\d{1,2})\s+(January|February|March|April|May|June|July|August|September|October|November|December)\s+(20\d{2})\b`)
)

// Source 2: fetch the latest ECB press-release page and extract the rates.
// Pattern: /press/pr/date/YYYY/html/ecb.mpYYMMDD~HASH.en.html
type RateDecision struct {
	Country                         string    `json:"country"`
	EventName                       string    `json:"event_name"`
	Actual                          string    `json:"actual"`
	ActualRateField                 string    `json:"actual_rate_field"`
	Title                           string    `json:"title"`
	DecisionDate                    string    `json:"decision_date"`
	ReleaseURL                      string    `json:"release_url"`
	ReleaseLinkSlug                 string    `json:"release_link_slug"`
	DepositFacilityRate             string    `json:"deposit_facility_rate"`
	MainRefinancingOperationsRate   string    `json:"main_refinancing_operations_rate"`
	MarginalLendingFacilityRate     string    `json:"marginal_lending_facility_rate"`
	Unit                            string    `json:"unit"`
	RateSentence                    string    `json:"rate_sentence"`
	DiscoverySourceURL              string    `json:"discovery_source_url"`
	DiscoverySnippetURL             string    `json:"discovery_snippet_url"`
	DetectedAtUTC                   time.Time `json:"detected_at_utc"`
	DetectionLatencyFromEventMillis int64     `json:"detection_latency_from_event_ms"`
	FetchLatencyMillis              int64     `json:"fetch_latency_ms"`
	TimeToFirstByteMillis           int64     `json:"time_to_first_byte_ms"`
	ServerDateHeader                string    `json:"server_date_header"`
	ETag                            string    `json:"etag"`
	LastModified                    string    `json:"last_modified"`
	Confidence                      string    `json:"confidence"`
	Warnings                        []string  `json:"warnings"`
}

type ParsedRates struct {
	DepositFacility          float64
	MainRefinancingOperation float64
	MarginalLendingFacility  float64
	Sentence                 string
}

func fetchAndParseDecision(ctx context.Context, client *http.Client, item DecisionItem, eventTime time.Time, expectedDate string, timeout time.Duration) (RateDecision, error) {
	if expectedDate != "" && item.DateISO != expectedDate {
		return RateDecision{}, fmt.Errorf("latest ECB decision date is %s, expected %s", item.DateISO, expectedDate)
	}

	body, meta, err := fetchWithRetry(ctx, client, item.URL, timeout, releaseAttempts)
	if err != nil {
		return RateDecision{}, fmt.Errorf("fetch latest ECB press release: %w", err)
	}

	title := parseTitle(body)
	if !strings.EqualFold(title, "Monetary policy decisions") {
		return RateDecision{}, fmt.Errorf("latest ECB release title=%q, expected Monetary policy decisions", title)
	}

	releaseDate := parseReleaseDate(body)
	if releaseDate == "" {
		releaseDate = item.DateISO
	}
	if releaseDate != item.DateISO {
		return RateDecision{}, fmt.Errorf("release date %s differs from discovered item date %s", releaseDate, item.DateISO)
	}
	if expectedDate != "" && releaseDate != expectedDate {
		return RateDecision{}, fmt.Errorf("release date is %s, expected %s", releaseDate, expectedDate)
	}

	rates, err := parseKeyRates(body)
	if err != nil {
		return RateDecision{}, err
	}

	detectedAt := time.Now().UTC()
	confidence := "HIGH"
	var warnings []string
	if expectedDate == "" {
		confidence = "MEDIUM"
		warnings = append(warnings, "expected date validation disabled")
	}

	return RateDecision{
		Country:                         country,
		EventName:                       eventName,
		Actual:                          actualFromRates(rates),
		ActualRateField:                 actualRateField,
		Title:                           title,
		DecisionDate:                    releaseDate,
		ReleaseURL:                      item.URL,
		ReleaseLinkSlug:                 item.LinkSlug,
		DepositFacilityRate:             formatPercent(rates.DepositFacility),
		MainRefinancingOperationsRate:   formatPercent(rates.MainRefinancingOperation),
		MarginalLendingFacilityRate:     formatPercent(rates.MarginalLendingFacility),
		Unit:                            "percent",
		RateSentence:                    rates.Sentence,
		DiscoverySourceURL:              indexURL,
		DiscoverySnippetURL:             item.SnippetURL,
		DetectedAtUTC:                   detectedAt,
		DetectionLatencyFromEventMillis: detectedAt.Sub(eventTime).Milliseconds(),
		FetchLatencyMillis:              meta.Latency.Milliseconds(),
		TimeToFirstByteMillis:           meta.TimeToFirstByte.Milliseconds(),
		ServerDateHeader:                meta.ServerDate,
		ETag:                            meta.ETag,
		LastModified:                    meta.LastModified,
		Confidence:                      confidence,
		Warnings:                        warnings,
	}, nil
}

func parseTitle(body []byte) string {
	matches := titleRE.FindAllSubmatch(body, -1)
	var fallback string
	for _, match := range matches {
		title := compactSpaces(stripHTML(string(match[1])))
		if strings.EqualFold(title, "Monetary policy decisions") {
			return title
		}
		if fallback == "" && !strings.EqualFold(title, "Search Results") {
			fallback = title
		}
	}
	return fallback
}

func parseReleaseDate(body []byte) string {
	if match := isoDateRE.FindSubmatch(body); match != nil {
		raw := strings.TrimSpace(string(match[1]))
		if t, err := time.Parse("2006-01-02", raw); err == nil {
			return t.Format("2006-01-02")
		}
	}

	text := compactSpaces(stripHTML(string(body)))
	match := releaseDateTextRE.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	day, _ := strconv.Atoi(match[1])
	month, ok := monthNumber(match[2])
	if !ok {
		return ""
	}
	year, _ := strconv.Atoi(match[3])
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

func parseKeyRates(body []byte) (ParsedRates, error) {
	text := stripHTML(string(body))
	for _, candidate := range rateCandidates(text) {
		if parsed, ok, err := parseRatesCandidate(candidate); ok || err != nil {
			return parsed, err
		}
	}
	if parsed, ok, err := parseRatesFromSplitPatterns(text); ok || err != nil {
		return parsed, err
	}

	return ParsedRates{}, errors.New("key ECB interest rates sentence not found or could not be parsed")
}

func rateCandidates(text string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, block := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		block = compactSpaces(block)
		if block == "" {
			continue
		}
		lower := strings.ToLower(block)
		if strings.Contains(lower, "deposit facility") &&
			strings.Contains(lower, "main refinancing operations") &&
			strings.Contains(lower, "marginal lending facility") {
			if _, ok := seen[block]; !ok {
				seen[block] = struct{}{}
				out = append(out, block)
			}
		}
	}

	compact := compactSpaces(text)
	lower := strings.ToLower(compact)
	for _, needle := range []string{"deposit facility", "main refinancing operations", "marginal lending facility"} {
		idx := strings.Index(lower, needle)
		if idx < 0 {
			continue
		}
		start := idx - 180
		if start < 0 {
			start = 0
		}
		end := idx + 520
		if end > len(compact) {
			end = len(compact)
		}
		candidate := compactSpaces(compact[start:end])
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func parseRatesCandidate(candidate string) (ParsedRates, bool, error) {
	normalized := compactSpaces(candidate)
	lower := strings.ToLower(normalized)
	if !strings.Contains(lower, "deposit facility") ||
		!strings.Contains(lower, "main refinancing operations") ||
		!strings.Contains(lower, "marginal lending facility") {
		return ParsedRates{}, false, nil
	}

	rateOrder := orderedRateNames(lower)
	if len(rateOrder) != 3 {
		return ParsedRates{}, false, nil
	}

	rawPercents := percentRE.FindAllString(normalized, -1)
	if len(rawPercents) < 3 {
		return ParsedRates{}, false, nil
	}

	values := make([]float64, 0, 3)
	for _, raw := range rawPercents[:3] {
		value, err := parsePercent(raw)
		if err != nil {
			return ParsedRates{}, true, fmt.Errorf("parse ECB rate %q: %w", raw, err)
		}
		values = append(values, value)
	}

	parsed := map[string]float64{}
	for i, name := range rateOrder {
		parsed[name] = values[i]
	}
	deposit, okDeposit := parsed["deposit"]
	mro, okMRO := parsed["mro"]
	mlf, okMLF := parsed["mlf"]
	if !okDeposit || !okMRO || !okMLF {
		return ParsedRates{}, false, nil
	}
	return ParsedRates{
		DepositFacility:          deposit,
		MainRefinancingOperation: mro,
		MarginalLendingFacility:  mlf,
		Sentence:                 normalized,
	}, true, nil
}

type rateHit struct {
	name string
	pos  int
}

type rateAccumulator struct {
	deposit    float64
	hasDeposit bool
	mro        float64
	hasMRO     bool
	mlf        float64
	hasMLF     bool
	sentences  []string
}

func parseRatesFromSplitPatterns(text string) (ParsedRates, bool, error) {
	var acc rateAccumulator
	for _, candidate := range partialRateCandidates(text) {
		normalized := compactSpaces(candidate)
		hits := rateHits(strings.ToLower(normalized))
		if len(hits) == 0 {
			continue
		}
		valueMatches := percentRE.FindAllString(normalized[hits[0].pos:], -1)
		if len(valueMatches) < len(hits) {
			continue
		}
		for i, hit := range hits {
			value, err := parsePercent(valueMatches[i])
			if err != nil {
				return ParsedRates{}, true, fmt.Errorf("parse ECB rate %q: %w", valueMatches[i], err)
			}
			acc.set(hit.name, value)
		}
		acc.sentences = appendUnique(acc.sentences, normalized)
	}
	if !acc.hasMRO || !acc.hasDeposit || !acc.hasMLF {
		return ParsedRates{}, false, nil
	}
	return ParsedRates{
		DepositFacility:          acc.deposit,
		MainRefinancingOperation: acc.mro,
		MarginalLendingFacility:  acc.mlf,
		Sentence:                 strings.Join(acc.sentences, " "),
	}, true, nil
}

func partialRateCandidates(text string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, block := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		block = compactSpaces(block)
		if block == "" || !strings.Contains(block, "%") {
			continue
		}
		lower := strings.ToLower(block)
		if strings.Contains(lower, "deposit facility") ||
			strings.Contains(lower, "main refinancing operations") ||
			strings.Contains(lower, "marginal lending facility") {
			if _, ok := seen[block]; !ok {
				seen[block] = struct{}{}
				out = append(out, block)
			}
		}
	}
	return out
}

func rateHits(lower string) []rateHit {
	hits := []rateHit{
		{name: "mro", pos: strings.Index(lower, "main refinancing operations")},
		{name: "mlf", pos: strings.Index(lower, "marginal lending facility")},
		{name: "deposit", pos: strings.Index(lower, "deposit facility")},
	}
	out := make([]rateHit, 0, len(hits))
	for _, hit := range hits {
		if hit.pos >= 0 {
			out = append(out, hit)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].pos < out[j].pos
	})
	return out
}

func (a *rateAccumulator) set(name string, value float64) {
	switch name {
	case "deposit":
		if !a.hasDeposit {
			a.deposit = value
			a.hasDeposit = true
		}
	case "mro":
		if !a.hasMRO {
			a.mro = value
			a.hasMRO = true
		}
	case "mlf":
		if !a.hasMLF {
			a.mlf = value
			a.hasMLF = true
		}
	}
}

func orderedRateNames(lowerSentence string) []string {
	type hit struct {
		name string
		pos  int
	}
	hits := []hit{
		{name: "deposit", pos: strings.Index(lowerSentence, "deposit facility")},
		{name: "mro", pos: strings.Index(lowerSentence, "main refinancing operations")},
		{name: "mlf", pos: strings.Index(lowerSentence, "marginal lending facility")},
	}
	for _, item := range hits {
		if item.pos < 0 {
			return nil
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].pos < hits[j].pos
	})
	out := make([]string, 0, len(hits))
	for _, item := range hits {
		out = append(out, item.name)
	}
	return out
}

func parsePercent(raw string) (float64, error) {
	clean := strings.ReplaceAll(raw, "%", "")
	clean = strings.TrimSpace(clean)
	return strconv.ParseFloat(clean, 64)
}

func actualFromRates(rates ParsedRates) string {
	return formatPercent(rates.MainRefinancingOperation)
}

func printDecision(result RateDecision) {
	fmt.Println("CONFIRMED")
	fmt.Printf("Country: %s\n", result.Country)
	fmt.Printf("Event: %s\n", result.EventName)
	fmt.Printf("Actual: %s%%\n", result.Actual)
	fmt.Printf("Actual Rate Field: %s\n", result.ActualRateField)
	fmt.Printf("Title: %s\n", result.Title)
	fmt.Printf("Decision Date: %s\n", result.DecisionDate)
	fmt.Printf("Release URL: %s\n", result.ReleaseURL)
	fmt.Printf("Release Link: %s\n", result.ReleaseLinkSlug)
	fmt.Printf("Deposit Facility Rate: %s%%\n", result.DepositFacilityRate)
	fmt.Printf("Main Refinancing Operations Rate: %s%%\n", result.MainRefinancingOperationsRate)
	fmt.Printf("Marginal Lending Facility Rate: %s%%\n", result.MarginalLendingFacilityRate)
	fmt.Printf("Rate Sentence: %s\n", result.RateSentence)
	fmt.Printf("Discovery Source: %s\n", result.DiscoverySourceURL)
	fmt.Printf("Discovery Snippet: %s\n", result.DiscoverySnippetURL)
	fmt.Printf("Detected At UTC: %s\n", result.DetectedAtUTC.Format(time.RFC3339Nano))
	fmt.Printf("Detection Latency From Event MS: %d\n", result.DetectionLatencyFromEventMillis)
	fmt.Printf("Fetch Latency MS: %d\n", result.FetchLatencyMillis)
	fmt.Printf("TTFB MS: %d\n", result.TimeToFirstByteMillis)
	fmt.Printf("Server Date: %s\n", blankNA(result.ServerDateHeader))
	fmt.Printf("ETag: %s\n", blankNA(result.ETag))
	fmt.Printf("Last Modified: %s\n", blankNA(result.LastModified))
	fmt.Printf("Confidence: %s\n", result.Confidence)
	fmt.Printf("Warnings: %s\n", formatWarnings(result.Warnings))
}

func monthNumber(s string) (int, bool) {
	key := strings.ToLower(strings.Trim(s, ". "))
	if len(key) > 3 {
		key = key[:3]
	}
	switch key {
	case "jan":
		return 1, true
	case "feb":
		return 2, true
	case "mar":
		return 3, true
	case "apr":
		return 4, true
	case "may":
		return 5, true
	case "jun":
		return 6, true
	case "jul":
		return 7, true
	case "aug":
		return 8, true
	case "sep":
		return 9, true
	case "oct":
		return 10, true
	case "nov":
		return 11, true
	case "dec":
		return 12, true
	default:
		return 0, false
	}
}

func formatPercent(v float64) string {
	if math.Abs(v) == 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

const keyRatesURL = "https://www.ecb.europa.eu/stats/policy_and_exchange_rates/key_ecb_interest_rates/html/index.en.html"

var (
	keyRatesRowRE  = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	keyRatesCellRE = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]>`)
	rateNumberRE   = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)
)

// Source 3: ECB key interest rates table, used as official confirmation backup.
type KeyRatesPageResult struct {
	SourceURL                     string
	EffectiveDate                 string
	DepositFacilityRate           string
	MainRefinancingOperationsRate string
	MarginalLendingFacilityRate   string
	Actual                        string
	FetchLatencyMillis            int64
	TimeToFirstByteMillis         int64
	ServerDateHeader              string
	ETag                          string
}

func fetchAndParseKeyInterestRates(ctx context.Context, client *http.Client, timeout time.Duration) (KeyRatesPageResult, error) {
	body, meta, err := fetchWithRetry(ctx, client, keyRatesURL, timeout, releaseAttempts)
	if err != nil {
		return KeyRatesPageResult{}, fmt.Errorf("fetch ECB key interest rates page: %w", err)
	}
	parsed, err := parseKeyInterestRatesPage(body)
	if err != nil {
		return KeyRatesPageResult{}, err
	}
	parsed.FetchLatencyMillis = meta.Latency.Milliseconds()
	parsed.TimeToFirstByteMillis = meta.TimeToFirstByte.Milliseconds()
	parsed.ServerDateHeader = meta.ServerDate
	parsed.ETag = meta.ETag
	return parsed, nil
}

func parseKeyInterestRatesPage(body []byte) (KeyRatesPageResult, error) {
	for _, rowMatch := range keyRatesRowRE.FindAllSubmatch(body, -1) {
		cells := htmlCells(rowMatch[1])
		if len(cells) < 6 {
			continue
		}

		year := compactSpaces(cells[0])
		if !yearRE.MatchString(year) { // use package-level compiled regex
			continue
		}
		effectiveDate, err := parseEffectiveDate(year, cells[1])
		if err != nil {
			continue
		}

		deposit, okDeposit := parseRateCell(cells[2])
		fixedMRO, okFixedMRO := parseRateCell(cells[3])
		variableMRO, okVariableMRO := parseRateCell(cells[4])
		marginal, okMarginal := parseRateCell(cells[5])
		if !okDeposit || !okMarginal || (!okFixedMRO && !okVariableMRO) {
			continue
		}
		mro := fixedMRO
		if !okFixedMRO {
			mro = variableMRO
		}

		return KeyRatesPageResult{
			SourceURL:                     keyRatesURL,
			EffectiveDate:                 effectiveDate,
			DepositFacilityRate:           formatPercent(deposit),
			MainRefinancingOperationsRate: formatPercent(mro),
			MarginalLendingFacilityRate:   formatPercent(marginal),
			Actual:                        formatPercent(mro),
		}, nil
	}

	return KeyRatesPageResult{}, errors.New("latest ECB key interest rates table row not found")
}

func htmlCells(row []byte) []string {
	matches := keyRatesCellRE.FindAllSubmatch(row, -1)
	cells := make([]string, 0, len(matches))
	for _, match := range matches {
		cells = append(cells, compactSpaces(stripHTML(string(match[1]))))
	}
	return cells
}

func parseRateCell(cell string) (float64, bool) {
	cell = strings.TrimSpace(cell)
	if cell == "" || cell == "-" {
		return 0, false
	}
	raw := rateNumberRE.FindString(cell)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseEffectiveDate(year string, dayMonth string) (string, error) {
	match := effDateRE.FindStringSubmatch(dayMonth) // use package-level compiled regex
	if match == nil {
		return "", fmt.Errorf("invalid effective date %q %q", year, dayMonth)
	}
	day, _ := strconv.Atoi(match[1])
	month, ok := monthNumber(match[2])
	if !ok {
		return "", fmt.Errorf("invalid effective month %q", match[2])
	}
	return fmt.Sprintf("%s-%02d-%02d", year, month, day), nil
}

func source3ConfirmsDecision(result RateDecision, keyRates KeyRatesPageResult) bool {
	return result.Actual == keyRates.Actual &&
		result.MainRefinancingOperationsRate == keyRates.MainRefinancingOperationsRate &&
		result.DepositFacilityRate == keyRates.DepositFacilityRate &&
		result.MarginalLendingFacilityRate == keyRates.MarginalLendingFacilityRate
}

func printSource3Current(keyRates KeyRatesPageResult, err error, result *RateDecision) {
	if err != nil {
		fmt.Printf("  FAIL [Source 3 current] %s\n", err)
		return
	}
	confirmed := "N/A"
	if result != nil {
		confirmed = fmt.Sprintf("%t", source3ConfirmsDecision(*result, keyRates))
	}
	fmt.Printf("  OK   [Source 3 current] Effective: %s Actual: %s%%    MRO: %s%% Deposit: %s%% Marginal: %s%%    Confirms Source 2: %s\n",
		keyRates.EffectiveDate,
		keyRates.Actual,
		keyRates.MainRefinancingOperationsRate,
		keyRates.DepositFacilityRate,
		keyRates.MarginalLendingFacilityRate,
		confirmed,
	)
}

func printKeyRatesConfirmation(result RateDecision, keyRates KeyRatesPageResult, err error) {
	if err != nil {
		fmt.Printf("Source 3 Confirmation: FAILED - %s\n", err)
		return
	}
	fmt.Printf("Source 3 Confirmation: %t\n", source3ConfirmsDecision(result, keyRates))
	fmt.Printf("Source 3 Effective Date: %s\n", keyRates.EffectiveDate)
	fmt.Printf("Source 3 Actual: %s%%\n", keyRates.Actual)
	fmt.Printf("Source 3 Main Refinancing Operations Rate: %s%%\n", keyRates.MainRefinancingOperationsRate)
	fmt.Printf("Source 3 Deposit Facility Rate: %s%%\n", keyRates.DepositFacilityRate)
	fmt.Printf("Source 3 Marginal Lending Facility Rate: %s%%\n", keyRates.MarginalLendingFacilityRate)
}
