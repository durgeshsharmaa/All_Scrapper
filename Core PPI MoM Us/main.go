package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata"
)

// ============================================================================
// EVENT CONFIGURATION - CHANGE THIS FOR NEXT PPI RELEASE
// ============================================================================
//
// Event group: U.S. PPI / Core PPI MoM and YoY
// Release Time: 08:30 Eastern Time
// IST = UTC + 5:30
//
// Format: "YYYY-MM-DD HH:MM:SS" in UTC.
var eventTimeUTC = "2026-06-11 12:30:00"

// ============================================================================

const (
	country         = "US"
	officialRelease = "Producer Price Indexes"
	publisher       = "U.S. Bureau of Labor Statistics"
	tableName       = "Table 1"
	unitPercent     = "%"
	valueMethod     = "direct_table_value"
	pdfValueMethod  = "direct_pdf_table_value"
	summaryMethod   = "release_confirmation_only"

	tableURL    = "https://www.bls.gov/news.release/ppi.t01.htm"
	pdfURL      = "https://www.bls.gov/news.release/pdf/ppi.pdf"
	summaryURL  = "https://www.bls.gov/news.release/ppi.nr0.htm"
	scheduleURL = "https://www.bls.gov/schedule/news_release/ppi.htm"

	httpTimeout          = 12 * time.Second
	requestTimeout       = 10 * time.Second
	headerRequestTimeout = 2 * time.Second
	pollCadence          = 500 * time.Millisecond
	contentEveryPolls    = 5
	testLead             = 1 * time.Minute
	sniperLead           = 2 * time.Second
	pollWindow           = 3 * time.Minute
	confirmationGrace    = 1500 * time.Millisecond
	userAgent            = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 PPI-Group-Sniper/1.0"
)

type ValueKind string

const (
	ValueMoM ValueKind = "mom"
	ValueYoY ValueKind = "yoy"
)

type MeasureTarget struct {
	EventName   string
	RowText     string
	GroupCode   string
	ItemCode    string
	ValueKind   ValueKind
	Seasonality string
}

var measureTargets = []MeasureTarget{
	{
		EventName:   "PPI (MoM)",
		RowText:     "Final demand",
		GroupCode:   "FD",
		ItemCode:    "4",
		ValueKind:   ValueMoM,
		Seasonality: "seasonally adjusted",
	},
	{
		EventName:   "PPI (YoY)",
		RowText:     "Final demand",
		GroupCode:   "FD",
		ItemCode:    "4",
		ValueKind:   ValueYoY,
		Seasonality: "unadjusted",
	},
	{
		EventName:   "Core PPI (MoM)",
		RowText:     "Final demand less foods and energy",
		GroupCode:   "FD",
		ItemCode:    "49104",
		ValueKind:   ValueMoM,
		Seasonality: "seasonally adjusted",
	},
	{
		EventName:   "Core PPI (YoY)",
		RowText:     "Final demand less foods and energy",
		GroupCode:   "FD",
		ItemCode:    "49104",
		ValueKind:   ValueYoY,
		Seasonality: "unadjusted",
	},
}

type Source struct {
	Name        string
	URL         string
	SourceType  string
	Kind        string
	ValueSource bool
	ValueMethod string
	Confidence  string
	Primary     bool
}

var primarySource = Source{
	Name:        "BLS PPI Table 1 HTML",
	URL:         tableURL,
	SourceType:  "html_table",
	Kind:        "html_table",
	ValueSource: true,
	ValueMethod: valueMethod,
	Confidence:  "HIGH",
	Primary:     true,
}

var pdfSource = Source{
	Name:        "BLS PPI PDF Release",
	URL:         pdfURL,
	SourceType:  "pdf",
	Kind:        "pdf",
	ValueSource: true,
	ValueMethod: pdfValueMethod,
	Confidence:  "MEDIUM",
}

var summarySource = Source{
	Name:        "BLS PPI Release Summary HTML",
	URL:         summaryURL,
	SourceType:  "html_release_summary",
	Kind:        "summary",
	ValueSource: false,
	ValueMethod: summaryMethod,
	Confidence:  "CONFIRMATION",
}

type Latency struct {
	Total    int64 `json:"total"`
	TTFB     int64 `json:"ttfb"`
	BodyRead int64 `json:"body_read"`
	Parse    int64 `json:"parse"`
}

type ParsedRow struct {
	Period             string
	FromPeriod         string
	LatestMoMColumn    string
	YoYColumn          string
	RowText            string
	GroupCode          string
	ItemCode           string
	RelativeImportance float64
	YoY                float64
	LatestMoM          float64
	PreviousMoM        float64
}

type ParsedSnapshot struct {
	Period           string
	FromPeriod       string
	LatestMoMColumn  string
	YoYColumn        string
	Rows             map[string]ParsedRow
	Measures         []MeasureResult
	ReleaseConfirmed bool
}

type MeasureResult struct {
	Country     string    `json:"country"`
	EventName   string    `json:"event_name"`
	Table       string    `json:"table"`
	RowText     string    `json:"row_text"`
	GroupCode   string    `json:"group_code"`
	ItemCode    string    `json:"item_code"`
	Column      string    `json:"column"`
	Period      string    `json:"period"`
	FromPeriod  string    `json:"from_period,omitempty"`
	Actual      string    `json:"actual"`
	ActualValue float64   `json:"-"`
	Previous    string    `json:"previous,omitempty"`
	Unit        string    `json:"unit"`
	Seasonality string    `json:"seasonality"`
	ValueMethod string    `json:"value_method"`
	Confidence  string    `json:"confidence"`
	Timestamp   time.Time `json:"-"`
}

type SnapshotResult struct {
	Source           string
	URL              string
	SourceType       string
	Period           string
	FromPeriod       string
	LatestMoMColumn  string
	YoYColumn        string
	Timestamp        time.Time
	EventLatencyMs   int64
	DetectionMethod  string
	Confidence       string
	ETag             string
	LastModified     string
	CacheControl     string
	ServerDate       string
	ContentHash      string
	StatusCode       int
	Error            string
	Warnings         []string
	Latency          Latency
	Measures         []MeasureResult
	ReleaseConfirmed bool
	UseForValues     bool
}

type Baseline struct {
	ETag         string
	LastModified string
	ContentHash  string
	Period       string
	Signature    string
}

type SourceResult struct {
	Name     string
	Source   Source
	Baseline *Baseline
	FirstHit *SnapshotResult
	Latest   *SnapshotResult
	Detected bool
	Mu       sync.Mutex
}

type JSONSnapshot struct {
	Country          string          `json:"country"`
	OfficialRelease  string          `json:"official_release"`
	Publisher        string          `json:"publisher"`
	Source           string          `json:"source"`
	SourceURL        string          `json:"source_url"`
	SourceType       string          `json:"source_type"`
	Table            string          `json:"table"`
	Period           string          `json:"period"`
	FromPeriod       string          `json:"from_period"`
	LatestMoMColumn  string          `json:"latest_mom_column"`
	YoYColumn        string          `json:"yoy_column"`
	ValueMethod      string          `json:"value_method"`
	Confidence       string          `json:"confidence"`
	ServerDateHeader string          `json:"server_date_header"`
	ETag             string          `json:"etag"`
	LastModified     string          `json:"last_modified"`
	CacheControl     string          `json:"cache_control"`
	LatencyMS        Latency         `json:"latency_ms"`
	MatchedSources   []string        `json:"matched_sources"`
	Warnings         []string        `json:"warnings"`
	Events           []MeasureResult `json:"events"`
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	eventTime, err := parseConfiguredEventTime()
	if err != nil {
		fmt.Println("NOT_CONFIRMED")
		logger.Printf("invalid eventTimeUTC: %v", err)
		os.Exit(1)
	}

	client := newHTTPClient()
	sources := productionSources()
	expectedPeriod := expectedReleasePeriod(eventTime)
	printBanner(eventTime)

	if warning := validateConfiguredSchedule(client, eventTime, logger); warning != "" {
		fmt.Printf("WARNING: %s\n", warning)
	}

	fmt.Println("Fetching Current Published Data...")
	current := fetchAllContent(context.Background(), client, sources, logger, false)
	printCurrentData(current)
	fmt.Println("Current data captured. Waiting for new release...")
	fmt.Println()

	testTime := eventTime.Add(-testLead)
	sniperStart := eventTime.Add(-sniperLead)
	pollEnd := eventTime.Add(pollWindow)

	if time.Now().UTC().Before(testTime) {
		countdownTo(testTime, "Countdown to Test Connection", "Will test connection 1 minute before event", time.Second)
	}

	fmt.Println("Testing connection...")
	fmt.Println("   Capturing baseline headers + content for hybrid detection...")
	states := captureBaselines(client, sources, logger)
	printBaselines(states)
	fmt.Println()

	if time.Now().UTC().Before(sniperStart) {
		countdownTo(sniperStart, "Countdown to Sniper Mode", "Sniper mode activates 2 seconds before event", 100*time.Millisecond)
	}

	if time.Now().UTC().After(pollEnd) {
		fmt.Println("Polling window already ended for configured event time.")
		fmt.Println("NOT_CONFIRMED")
		return
	}

	fmt.Println("SNIPER MODE ACTIVATED!")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("Using HYBRID detection: Headers every 500ms + Content every 5th poll")
	fmt.Println(strings.Repeat("=", 72))

	states = runSniperMode(client, sources, states, eventTime, pollEnd, expectedPeriod, logger)
	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("Polling window complete")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()

	printPerformanceTable(states, eventTime)
	finalResults := firstHits(states)
	confirmed, matched, err := mergeConfirmed(finalResults, expectedPeriod)
	if err != nil {
		fmt.Println("NOT_CONFIRMED")
		logger.Printf("final values failed confirmation: %v", err)
		return
	}
	printJSON(toJSON(confirmed, matched))
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   800 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          96,
			MaxIdleConnsPerHost:   24,
			MaxConnsPerHost:       24,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   1500 * time.Millisecond,
			ResponseHeaderTimeout: 4 * time.Second,
			ExpectContinueTimeout: 250 * time.Millisecond,
		},
	}
}

func productionSources() []Source {
	return []Source{
		primarySource,
		pdfSource,
		summarySource,
	}
}

func fetchAllContent(parent context.Context, client *http.Client, sources []Source, logger *log.Logger, logDetail bool) map[string]*SnapshotResult {
	type item struct {
		source Source
		result *SnapshotResult
		err    error
	}
	ch := make(chan item, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		source := source
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fetchAndParse(parent, client, source, logger, logDetail)
			ch <- item{source: source, result: result, err: err}
		}()
	}
	wg.Wait()
	close(ch)

	results := make(map[string]*SnapshotResult, len(sources))
	for item := range ch {
		if item.err != nil {
			if item.result == nil {
				item.result = resultFromHeaders(item.source, http.Header{}, 0, Latency{})
			}
			item.result.Error = item.err.Error()
		}
		results[item.source.Name] = item.result
	}
	return results
}

func captureBaselines(client *http.Client, sources []Source, logger *log.Logger) map[string]*SourceResult {
	results := fetchAllContent(context.Background(), client, sources, logger, false)
	states := make(map[string]*SourceResult, len(sources))
	for _, source := range sources {
		result := results[source.Name]
		state := &SourceResult{Name: source.Name, Source: source, Latest: result}
		if result != nil && result.Error == "" {
			state.Baseline = baselineFromResult(result)
		}
		states[source.Name] = state
	}
	return states
}

func fetchHeadersOnly(parent context.Context, client *http.Client, source Source) (*SnapshotResult, error) {
	ctx, cancel := context.WithTimeout(parent, headerRequestTimeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source.URL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := resultFromHeaders(source, resp.Header, resp.StatusCode, Latency{
		Total: time.Since(start).Milliseconds(),
		TTFB:  time.Since(start).Milliseconds(),
	})
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden {
		return fetchHeadersViaGET(parent, client, source)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return result, fmt.Errorf("%s header request status=%s", source.Name, resp.Status)
	}
	return result, nil
}

func fetchHeadersViaGET(parent context.Context, client *http.Client, source Source) (*SnapshotResult, error) {
	ctx, cancel := context.WithTimeout(parent, headerRequestTimeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := resultFromHeaders(source, resp.Header, resp.StatusCode, Latency{
		Total: time.Since(start).Milliseconds(),
		TTFB:  time.Since(start).Milliseconds(),
	})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return result, fmt.Errorf("%s header fallback status=%s", source.Name, resp.Status)
	}
	return result, nil
}

func fetchAndParse(parent context.Context, client *http.Client, source Source, logger *log.Logger, logDetail bool) (*SnapshotResult, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	start := time.Now()
	if logDetail {
		logger.Printf("%s request start url=%s", source.Name, source.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	ttfb := time.Since(start).Milliseconds()
	headers := resp.Header.Clone()
	if logDetail {
		logger.Printf("%s response headers status=%s Date=%q ETag=%q Last-Modified=%q Cache-Control=%q",
			source.Name, resp.Status, headers.Get("Date"), headers.Get("ETag"), headers.Get("Last-Modified"), headers.Get("Cache-Control"))
	}

	base := resultFromHeaders(source, headers, resp.StatusCode, Latency{TTFB: ttfb})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		base.Latency.Total = time.Since(start).Milliseconds()
		return base, fmt.Errorf("%s content request status=%s", source.Name, resp.Status)
	}

	bodyStart := time.Now()
	body, err := io.ReadAll(resp.Body)
	bodyReadMS := time.Since(bodyStart).Milliseconds()
	if err != nil {
		base.Latency.Total = time.Since(start).Milliseconds()
		base.Latency.BodyRead = bodyReadMS
		return base, err
	}
	hash := sha256.Sum256(body)
	contentHash := hex.EncodeToString(hash[:])
	if logDetail {
		logger.Printf("%s body read complete bytes=%d sha256=%s body_read_ms=%d", source.Name, len(body), contentHash, bodyReadMS)
	}

	parseStart := time.Now()
	parsed, warnings, err := parseSourceBody(source, body)
	parseMS := time.Since(parseStart).Milliseconds()
	if err != nil {
		base.ContentHash = contentHash
		base.Latency = Latency{Total: time.Since(start).Milliseconds(), TTFB: ttfb, BodyRead: bodyReadMS, Parse: parseMS}
		return base, err
	}

	result := parsedSnapshotToResult(source, parsed, warnings)
	result.ETag = headers.Get("ETag")
	result.LastModified = headers.Get("Last-Modified")
	result.CacheControl = headers.Get("Cache-Control")
	result.ServerDate = headers.Get("Date")
	result.StatusCode = resp.StatusCode
	result.ContentHash = contentHash
	result.Timestamp = time.Now().UTC()
	result.Latency = Latency{Total: time.Since(start).Milliseconds(), TTFB: ttfb, BodyRead: bodyReadMS, Parse: parseMS}
	for i := range result.Measures {
		result.Measures[i].Timestamp = result.Timestamp
	}
	if logDetail {
		logger.Printf("%s parse complete period=%q signature=%q parse_ms=%d total_ms=%d",
			source.Name, result.Period, measureSignature(result.Measures), parseMS, result.Latency.Total)
	}
	return result, nil
}

func parsedSnapshotToResult(source Source, parsed ParsedSnapshot, warnings []string) *SnapshotResult {
	return &SnapshotResult{
		Source:           source.Name,
		URL:              source.URL,
		SourceType:       source.SourceType,
		Period:           parsed.Period,
		FromPeriod:       parsed.FromPeriod,
		LatestMoMColumn:  parsed.LatestMoMColumn,
		YoYColumn:        parsed.YoYColumn,
		Confidence:       source.Confidence,
		Warnings:         warnings,
		Measures:         parsed.Measures,
		ReleaseConfirmed: parsed.ReleaseConfirmed,
		UseForValues:     source.ValueSource,
	}
}

func parseSourceBody(source Source, body []byte) (ParsedSnapshot, []string, error) {
	switch source.Kind {
	case "html_table":
		parsed, warnings, err := parsePPIHTML(body)
		if err != nil {
			return ParsedSnapshot{}, warnings, err
		}
		applyMeasureMetadata(parsed.Measures, source.ValueMethod, source.Confidence)
		return parsed, warnings, nil
	case "pdf":
		parsed, warnings, err := parsePPIPDF(body)
		if err != nil {
			return ParsedSnapshot{}, warnings, err
		}
		applyMeasureMetadata(parsed.Measures, source.ValueMethod, source.Confidence)
		return parsed, warnings, nil
	case "summary":
		return parseSummaryConfirmation(body)
	default:
		return ParsedSnapshot{}, nil, fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

func applyMeasureMetadata(measures []MeasureResult, method, confidence string) {
	for i := range measures {
		measures[i].ValueMethod = method
		measures[i].Confidence = confidence
	}
}

func resultFromHeaders(source Source, headers http.Header, statusCode int, latency Latency) *SnapshotResult {
	return &SnapshotResult{
		Source:       source.Name,
		URL:          source.URL,
		SourceType:   source.SourceType,
		Timestamp:    time.Now().UTC(),
		ETag:         headers.Get("ETag"),
		LastModified: headers.Get("Last-Modified"),
		CacheControl: headers.Get("Cache-Control"),
		ServerDate:   headers.Get("Date"),
		StatusCode:   statusCode,
		Latency:      latency,
	}
}

func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
}

func parseConfiguredEventTime() (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTC, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func expectedReleasePeriod(eventTime time.Time) string {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.FixedZone("ET", -4*3600)
	}
	local := eventTime.In(ny)
	year, month := local.Year(), int(local.Month())-1
	if month == 0 {
		year--
		month = 12
	}
	return fmt.Sprintf("%04d-%02d", year, month)
}

func printBanner(eventTime time.Time) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("US PPI Group Scraper - SNIPER MODE")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("Publisher: %s\n", publisher)
	fmt.Printf("Primary Source: %s\n", tableURL)
	fmt.Println("Targets:")
	for _, target := range measureTargets {
		fmt.Printf("  %s | Row: %s | Code: %s %s | Kind: %s\n",
			target.EventName, target.RowText, target.GroupCode, target.ItemCode, strings.ToUpper(string(target.ValueKind)))
	}
	fmt.Printf("Event Time (IST): %s\n", eventTime.In(ist).Format("2006-01-02 15:04:05"))
	fmt.Printf("Event Time (UTC): %s\n", eventTime.UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected PPI Period: %s\n", prettyPeriod(expectedReleasePeriod(eventTime)))
	fmt.Println(strings.Repeat("=", 72))
}

func printCurrentData(results map[string]*SnapshotResult) {
	fmt.Println(strings.Repeat("-", 176))
	fmt.Printf("%-32s %-8s %-10s %-102s %-7s %-7s %-18s %s\n",
		"SOURCE", "STATUS", "PERIOD", "VALUES / ROLE", "TTFB", "TOTAL", "ETAG", "LAST-MODIFIED")
	fmt.Println(strings.Repeat("-", 176))
	for _, source := range productionSources() {
		result := results[source.Name]
		if result == nil || result.Error != "" {
			errText := "unavailable"
			if result != nil && result.Error != "" {
				errText = result.Error
			}
			fmt.Printf("%-32s %-8s %-10s %-102s %-7s %-7s %-18s %s\n",
				trimForConsole(source.Name, 32), "NO", "-", trimForConsole(errText, 102), "-", "-", "-", "-")
			continue
		}
		period := prettyPeriod(result.Period)
		ttfb := fmt.Sprintf("%dms", result.Latency.TTFB)
		total := fmt.Sprintf("%dms", result.Latency.Total)
		if source.ValueSource {
			fmt.Printf("%-32s %-8s %-10s %-102s %-7s %-7s %-18s %s\n",
				trimForConsole(source.Name, 32), "OK", period, trimForConsole(measureSignature(result.Measures), 102),
				ttfb, total, trimForConsole(result.ETag, 18), trimForConsole(result.LastModified, 29))
			continue
		}
		status := "confirmed"
		if !result.ReleaseConfirmed {
			status = "not confirmed"
		}
		fmt.Printf("%-32s %-8s %-10s %-102s %-7s %-7s %-18s %s\n",
			trimForConsole(source.Name, 32), "OK", period, status, ttfb, total, trimForConsole(result.ETag, 18), trimForConsole(result.LastModified, 29))
	}
	fmt.Println(strings.Repeat("-", 176))
}

func countdownTo(target time.Time, label, doneMessage string, step time.Duration) {
	for time.Now().UTC().Before(target) {
		remaining := time.Until(target)
		if step < time.Second {
			fmt.Printf("\r%s: %.3f seconds   ", label, remaining.Seconds())
		} else {
			fmt.Printf("\r%s: %02dh%02dm%02ds   ", label, int(remaining.Hours()), int(remaining.Minutes())%60, int(remaining.Seconds())%60)
		}
		time.Sleep(step)
	}
	fmt.Printf("\r%s%s\n\n", doneMessage, strings.Repeat(" ", 24))
}

func baselineFromResult(result *SnapshotResult) *Baseline {
	return &Baseline{
		ETag:         result.ETag,
		LastModified: result.LastModified,
		ContentHash:  result.ContentHash,
		Period:       result.Period,
		Signature:    measureSignature(result.Measures),
	}
}

func printBaselines(states map[string]*SourceResult) {
	fmt.Println(strings.Repeat("-", 176))
	fmt.Printf("%-32s %-8s %-10s %-102s %-18s %s\n",
		"SOURCE", "STATUS", "PERIOD", "BASELINE VALUES / ROLE", "ETAG", "LAST-MODIFIED")
	fmt.Println(strings.Repeat("-", 176))
	for _, source := range productionSources() {
		state := states[source.Name]
		if state == nil || state.Baseline == nil || state.Latest == nil || state.Latest.Error != "" {
			errText := "baseline unavailable"
			if state != nil && state.Latest != nil && state.Latest.Error != "" {
				errText = state.Latest.Error
			}
			fmt.Printf("%-32s %-8s %-10s %-102s %-18s %s\n",
				trimForConsole(source.Name, 32), "NO", "-", trimForConsole(errText, 102), "-", "-")
			continue
		}
		period := prettyPeriod(state.Baseline.Period)
		if source.ValueSource {
			fmt.Printf("%-32s %-8s %-10s %-102s %-18s %s\n",
				trimForConsole(source.Name, 32), "OK", period, trimForConsole(state.Baseline.Signature, 102),
				trimForConsole(state.Baseline.ETag, 18), trimForConsole(state.Latest.LastModified, 29))
			continue
		}
		role := fmt.Sprintf("release_confirmed=%v", state.Latest.ReleaseConfirmed)
		fmt.Printf("%-32s %-8s %-10s %-102s %-18s %s\n",
			trimForConsole(source.Name, 32), "OK", period, role,
			trimForConsole(state.Baseline.ETag, 18), trimForConsole(state.Latest.LastModified, 29))
	}
	fmt.Println(strings.Repeat("-", 176))
}

func runSniperMode(client *http.Client, sources []Source, states map[string]*SourceResult, eventTime, pollEnd time.Time, expectedPeriod string, logger *log.Logger) map[string]*SourceResult {
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup

	for _, source := range sources {
		source := source
		state := states[source.Name]
		if state == nil {
			state = &SourceResult{Name: source.Name, Source: source}
			states[source.Name] = state
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pollSource(client, source, state, eventTime, pollEnd, expectedPeriod, stopCh, &stopOnce, logger)
		}()
	}

	wg.Wait()
	return states
}

func pollSource(client *http.Client, source Source, state *SourceResult, eventTime, pollEnd time.Time, expectedPeriod string, stopCh chan struct{}, stopOnce *sync.Once, logger *log.Logger) {
	pollCount := 0
	for time.Now().UTC().Before(pollEnd) {
		select {
		case <-stopCh:
			return
		default:
		}

		pollCount++
		checkContent := pollCount%contentEveryPolls == 0
		headerResult, err := fetchHeadersOnly(context.Background(), client, source)
		if err != nil {
			logger.Printf("%s header poll failed: %v", source.Name, err)
			sleepOrStop(stopCh, pollCadence)
			continue
		}

		state.Mu.Lock()
		baseline := state.Baseline
		state.Mu.Unlock()

		headersChanged := false
		if baseline != nil {
			if headerResult.ETag != "" && headerResult.ETag != baseline.ETag {
				headersChanged = true
			}
			if headerResult.LastModified != "" && headerResult.LastModified != baseline.LastModified {
				headersChanged = true
			}
		}

		if headersChanged || checkContent {
			result, err := fetchAndParse(context.Background(), client, source, logger, false)
			if err != nil {
				logger.Printf("%s content poll failed: %v", source.Name, err)
				sleepOrStop(stopCh, pollCadence)
				continue
			}

			state.Mu.Lock()
			state.Latest = result
			state.Mu.Unlock()

			contentChanged := baseline == nil ||
				result.ContentHash != baseline.ContentHash ||
				result.Period != baseline.Period ||
				measureSignature(result.Measures) != baseline.Signature

			if contentChanged && result.Period == expectedPeriod {
				method := "content"
				if headersChanged {
					method = "headers+content"
				}
				result.DetectionMethod = method
				result.Timestamp = time.Now().UTC()
				result.EventLatencyMs = result.Timestamp.Sub(eventTime).Milliseconds()

				state.Mu.Lock()
				if !state.Detected {
					state.FirstHit = result
					state.Detected = true
					printUpdate(result, source)
					if source.Primary {
						go func() {
							time.Sleep(confirmationGrace)
							stopOnce.Do(func() { close(stopCh) })
						}()
					}
				}
				state.Mu.Unlock()
			}
		}

		sleepOrStop(stopCh, pollCadence)
	}
}

func sleepOrStop(stopCh <-chan struct{}, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-stopCh:
	case <-timer.C:
	}
}

func printUpdate(result *SnapshotResult, source Source) {
	if source.ValueSource {
		fmt.Printf("[%s] UPDATED! [%s] Period: %s | Values: %s | Detected by: %s\n",
			result.Timestamp.Format("15:04:05.000"), source.Name, prettyPeriod(result.Period), trimForConsole(measureSignature(result.Measures), 120), result.DetectionMethod)
		return
	}
	fmt.Printf("[%s] UPDATED! [%s] Period: %s | Release confirmed: %v | Detected by: %s\n",
		result.Timestamp.Format("15:04:05.000"), source.Name, prettyPeriod(result.Period), result.ReleaseConfirmed, result.DetectionMethod)
}

func printPerformanceTable(states map[string]*SourceResult, eventTime time.Time) {
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Println(strings.Repeat("=", 176))
	fmt.Printf("%-5s %-24s %-19s %-10s %-10s %-102s %-18s %-10s\n",
		"RANK", "SOURCE", "UPDATE TIME UTC", "LATENCY", "PERIOD", "VALUES", "METHOD", "STATUS")
	fmt.Println(strings.Repeat("-", 176))

	hits := detectedHits(states)
	if len(hits) == 0 {
		for _, source := range productionSources() {
			fmt.Printf("%-5s %-24s %-19s %-10s %-10s %-102s %-18s %-10s\n",
				"-", trimForConsole(source.Name, 24), "-", "-", "-", "-", "-", "not detected")
		}
		fmt.Println(strings.Repeat("=", 176))
		return
	}
	for i, hit := range hits {
		values := measureSignature(hit.Measures)
		if values == "" {
			values = fmt.Sprintf("release_confirmed=%v", hit.ReleaseConfirmed)
		}
		fmt.Printf("%-5d %-24s %-19s %-10s %-10s %-102s %-18s %-10s\n",
			i+1, trimForConsole(hit.Source, 24), hit.Timestamp.UTC().Format("15:04:05.000"),
			formatLatency(hit.EventLatencyMs), prettyPeriod(hit.Period), trimForConsole(values, 102), hit.DetectionMethod, hit.Confidence)
	}
	fmt.Println(strings.Repeat("=", 176))
	winner := hits[0]
	fmt.Printf("Winner: %s\n", winner.Source)
	fmt.Printf("Updated Period: %s\n", prettyPeriod(winner.Period))
	fmt.Printf("Detection Latency: %s from event time\n", formatLatency(winner.Timestamp.Sub(eventTime).Milliseconds()))
	fmt.Println(strings.Repeat("=", 176))
}

func detectedHits(states map[string]*SourceResult) []*SnapshotResult {
	var hits []*SnapshotResult
	for _, state := range states {
		state.Mu.Lock()
		if state.FirstHit != nil {
			hits = append(hits, state.FirstHit)
		}
		state.Mu.Unlock()
	}
	sortResultsByLatency(hits)
	return hits
}

func sortResultsByLatency(results []*SnapshotResult) {
	for i := 1; i < len(results); i++ {
		j := i
		for j > 0 && results[j].EventLatencyMs < results[j-1].EventLatencyMs {
			results[j], results[j-1] = results[j-1], results[j]
			j--
		}
	}
}

func firstHits(states map[string]*SourceResult) map[string]*SnapshotResult {
	out := make(map[string]*SnapshotResult, len(states))
	for name, state := range states {
		state.Mu.Lock()
		if state.FirstHit != nil {
			out[name] = state.FirstHit
		} else if state.Latest != nil {
			out[name] = state.Latest
		}
		state.Mu.Unlock()
	}
	return out
}

func mergeConfirmed(results map[string]*SnapshotResult, expectedPeriod string) (*SnapshotResult, []string, error) {
	html := results[primarySource.Name]
	pdf := results[pdfSource.Name]
	summary := results[summarySource.Name]

	var warnings []string
	var selected *SnapshotResult
	if isUsableValueResult(html, expectedPeriod) {
		selected = html
	} else if isUsableValueResult(pdf, expectedPeriod) {
		selected = pdf
		warnings = append(warnings, "primary HTML value source unavailable; using official PDF backup")
	}
	if selected == nil {
		return nil, nil, errors.New("no updated official value source available")
	}
	if err := validateSnapshot(selected, expectedPeriod); err != nil {
		return nil, nil, err
	}

	matched := []string{selected.Source}
	if selected.Source != pdfSource.Name && isUsableValueResult(pdf, selected.Period) {
		if !sameMeasures(selected.Measures, pdf.Measures) {
			return nil, nil, fmt.Errorf("official value sources disagree: %s=%s %s=%s", selected.Source, measureSignature(selected.Measures), pdf.Source, measureSignature(pdf.Measures))
		}
		matched = append(matched, pdf.Source)
	}
	if selected.Source != primarySource.Name && isUsableValueResult(html, selected.Period) {
		if !sameMeasures(selected.Measures, html.Measures) {
			return nil, nil, fmt.Errorf("official value sources disagree: %s=%s %s=%s", selected.Source, measureSignature(selected.Measures), html.Source, measureSignature(html.Measures))
		}
		matched = append(matched, html.Source)
	}

	if summary != nil && summary.Error == "" {
		if expectedPeriod != "" && summary.Period != "" && summary.Period != expectedPeriod {
			warnings = append(warnings, fmt.Sprintf("Source 3 summary period %s has not reached expected period %s", summary.Period, expectedPeriod))
		}
		if summary.ReleaseConfirmed && (expectedPeriod == "" || summary.Period == expectedPeriod) {
			matched = append(matched, summary.Source)
		}
	}

	cp := cloneSnapshot(selected)
	cp.Warnings = uniqueStrings(append(cp.Warnings, warnings...))
	return cp, uniqueStrings(matched), nil
}

func isUsableValueResult(result *SnapshotResult, expectedPeriod string) bool {
	if result == nil || result.Error != "" || len(result.Measures) == 0 {
		return false
	}
	if expectedPeriod != "" && result.Period != expectedPeriod {
		return false
	}
	return true
}

func sameMeasures(a, b []MeasureResult) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]string, len(a))
	for _, measure := range a {
		byName[measure.EventName] = measure.Actual
	}
	for _, measure := range b {
		if byName[measure.EventName] != measure.Actual {
			return false
		}
	}
	return true
}

func cloneSnapshot(result *SnapshotResult) *SnapshotResult {
	if result == nil {
		return nil
	}
	cp := *result
	cp.Warnings = append([]string(nil), result.Warnings...)
	cp.Measures = append([]MeasureResult(nil), result.Measures...)
	return &cp
}

func validateSnapshot(result *SnapshotResult, expectedPeriod string) error {
	if result == nil {
		return errors.New("missing result")
	}
	if expectedPeriod != "" && result.Period != expectedPeriod {
		return fmt.Errorf("stale source period=%s, expected %s", result.Period, expectedPeriod)
	}
	if len(result.Measures) != len(measureTargets) {
		return fmt.Errorf("measure count=%d, expected %d", len(result.Measures), len(measureTargets))
	}

	byName := map[string]MeasureResult{}
	for _, measure := range result.Measures {
		if measure.ValueMethod != valueMethod && measure.ValueMethod != pdfValueMethod {
			return fmt.Errorf("%s value method=%q, expected official direct table/PDF value", measure.EventName, measure.ValueMethod)
		}
		if measure.Actual == "" {
			return fmt.Errorf("%s empty actual value", measure.EventName)
		}
		if measure.ActualValue < -50 || measure.ActualValue > 50 {
			return fmt.Errorf("%s actual %.1f is outside reasonable PPI bounds", measure.EventName, measure.ActualValue)
		}
		byName[measure.EventName] = measure
	}
	for _, target := range measureTargets {
		measure, ok := byName[target.EventName]
		if !ok {
			return fmt.Errorf("missing event %q", target.EventName)
		}
		if normalizeLabel(measure.RowText) != normalizeLabel(target.RowText) {
			return fmt.Errorf("%s row=%q, expected %q", target.EventName, measure.RowText, target.RowText)
		}
		if measure.GroupCode != target.GroupCode {
			return fmt.Errorf("%s group=%q, expected %q", target.EventName, measure.GroupCode, target.GroupCode)
		}
		if measure.ItemCode != target.ItemCode {
			return fmt.Errorf("%s item=%q, expected %q", target.EventName, measure.ItemCode, target.ItemCode)
		}
	}
	return nil
}

func toJSON(result *SnapshotResult, matched []string) JSONSnapshot {
	events := make([]MeasureResult, len(result.Measures))
	copy(events, result.Measures)
	return JSONSnapshot{
		Country:          country,
		OfficialRelease:  officialRelease,
		Publisher:        publisher,
		Source:           result.Source,
		SourceURL:        result.URL,
		SourceType:       result.SourceType,
		Table:            tableName,
		Period:           result.Period,
		FromPeriod:       result.FromPeriod,
		LatestMoMColumn:  result.LatestMoMColumn,
		YoYColumn:        result.YoYColumn,
		ValueMethod:      resultValueMethod(result),
		Confidence:       result.Confidence,
		ServerDateHeader: result.ServerDate,
		ETag:             result.ETag,
		LastModified:     result.LastModified,
		CacheControl:     result.CacheControl,
		LatencyMS:        result.Latency,
		MatchedSources:   matched,
		Warnings:         uniqueStrings(result.Warnings),
		Events:           events,
	}
}

func printJSON(result JSONSnapshot) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Println("NOT_CONFIRMED")
	}
}

func resultValueMethod(result *SnapshotResult) string {
	if result == nil || len(result.Measures) == 0 {
		return ""
	}
	return result.Measures[0].ValueMethod
}

func parsePPIPDF(body []byte) (ParsedSnapshot, []string, error) {
	text, warnings := extractPDFText(body)
	text = normalizePDFText(text)
	lower := asciiLower(text)
	if !strings.Contains(lower, "producer price indexes") {
		return ParsedSnapshot{}, warnings, errors.New("Producer Price Index release PDF not confirmed")
	}
	if !strings.Contains(lower, "table 1. producer price index") {
		return ParsedSnapshot{}, warnings, errors.New("Table 1 marker not found in PDF text")
	}
	if !strings.Contains(lower, "seasonally adjusted 1-month percent change") {
		return ParsedSnapshot{}, warnings, errors.New("seasonally adjusted 1-month percent change header not found in PDF text")
	}

	tableText, err := table1Text(text)
	if err != nil {
		return ParsedSnapshot{}, warnings, err
	}
	period := parseLatestPeriod(tableText)
	if period == "" {
		period = parseLatestPeriod(text)
	}
	if period == "" {
		return ParsedSnapshot{}, warnings, errors.New("latest release period not found in PDF")
	}
	fromPeriod := previousMonth(period)
	if fromPeriod == "" {
		return ParsedSnapshot{}, warnings, fmt.Errorf("could not compute previous period for %q", period)
	}

	rows := make(map[string]ParsedRow)
	for _, target := range uniqueRowTargets() {
		row, err := parsePDFTargetRow(tableText, target, period, fromPeriod)
		if err != nil {
			return ParsedSnapshot{}, warnings, err
		}
		rows[rowKey(target.RowText, target.GroupCode, target.ItemCode)] = row
	}

	snapshot := ParsedSnapshot{
		Period:          period,
		FromPeriod:      fromPeriod,
		LatestMoMColumn: latestMoMColumnFromPeriod(period),
		YoYColumn:       yoyColumnFromPeriod(period),
		Rows:            rows,
	}
	for _, target := range measureTargets {
		row := rows[rowKey(target.RowText, target.GroupCode, target.ItemCode)]
		measure, err := measureFromRow(target, row)
		if err != nil {
			return ParsedSnapshot{}, warnings, err
		}
		snapshot.Measures = append(snapshot.Measures, measure)
	}
	return snapshot, warnings, nil
}

func table1Text(text string) (string, error) {
	lower := asciiLower(text)
	start := strings.Index(lower, "table 1. producer price index")
	if start < 0 {
		return "", errors.New("Table 1 start not found")
	}
	endRel := strings.Index(lower[start:], "table 2.")
	if endRel < 0 {
		return text[start:], nil
	}
	return text[start : start+endRel], nil
}

func parsePDFTargetRow(tableText string, target MeasureTarget, period, fromPeriod string) (ParsedRow, error) {
	labelPattern := flexibleLabelPattern(target.RowText)
	pattern := `(?i)` + labelPattern + `\s*(?:\d+(?:\s*,\s*\d+)?)?\s*(?:\.\s*)*\b` +
		regexp.QuoteMeta(target.GroupCode) + `\s+` + regexp.QuoteMeta(target.ItemCode) + `\b`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringIndex(tableText, -1)
	if len(matches) != 1 {
		return ParsedRow{}, fmt.Errorf("PDF Table 1 target row match count=%d for row=%q group=%s item=%s", len(matches), target.RowText, target.GroupCode, target.ItemCode)
	}

	snippet := tableText[matches[0][1]:]
	if len(snippet) > 350 {
		snippet = snippet[:350]
	}
	numbers := numbersFromText(snippet)
	if len(numbers) < 7 {
		return ParsedRow{}, fmt.Errorf("PDF Table 1 row %s %s had %d numeric values after item code, expected at least 7", target.GroupCode, target.ItemCode, len(numbers))
	}

	return ParsedRow{
		Period:          period,
		FromPeriod:      fromPeriod,
		LatestMoMColumn: latestMoMColumnFromPeriod(period),
		YoYColumn:       yoyColumnFromPeriod(period),
		RowText:         target.RowText,
		GroupCode:       target.GroupCode,
		ItemCode:        target.ItemCode,
		YoY:             round1(numbers[1]),
		LatestMoM:       round1(numbers[6]),
		PreviousMoM:     round1(numbers[5]),
	}, nil
}

func parseSummaryConfirmation(body []byte) (ParsedSnapshot, []string, error) {
	doc := string(body)
	plain := normalizeSpace(stripHTML(doc))
	lower := strings.ToLower(plain)
	if !strings.Contains(lower, "producer price indexes") {
		return ParsedSnapshot{}, nil, errors.New("Producer Price Index release summary not confirmed")
	}
	period := parseReleasePeriod(plain)
	var warnings []string
	if period == "" {
		warnings = append(warnings, "summary release period not parsed")
	}
	if strings.Contains(lower, "final demand less foods, energy, and trade services") {
		warnings = append(warnings, "summary mentions final demand less foods, energy, and trade services; not used for Core PPI FD 49104")
	}
	if !exactRowPresent(doc, "Final demand", "FD", "4") || !exactRowPresent(doc, "Final demand less foods and energy", "FD", "49104") {
		warnings = append(warnings, "summary does not clearly expose exact grouped rows FD 4 and FD 49104; confirmation only")
	}
	return ParsedSnapshot{
		Period:           period,
		ReleaseConfirmed: true,
	}, warnings, nil
}

func parseReleasePeriod(text string) string {
	re := regexp.MustCompile(`(?i)\bPRODUCER PRICE INDEXES\s*[-–]\s*([A-Za-z]+)\s+(20\d{2})\b`)
	match := re.FindStringSubmatch(text)
	if len(match) == 3 {
		return fmt.Sprintf("%s-%02d", match[2], monthNumber(match[1]))
	}
	return ""
}

func exactRowPresent(doc, rowText, groupCode, itemCode string) bool {
	for _, cells := range htmlRows(doc) {
		if len(cells) < 3 {
			continue
		}
		if normalizeLabel(cells[0]) == normalizeLabel(rowText) &&
			strings.EqualFold(strings.TrimSpace(cells[1]), groupCode) &&
			strings.TrimSpace(cells[2]) == itemCode {
			return true
		}
	}
	return false
}

func extractPDFText(body []byte) (string, []string) {
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("%PDF")) {
		return string(body), nil
	}

	var warnings []string
	streamRe := regexp.MustCompile(`(?s)<<(.*?)>>\s*stream\r?\n(.*?)\r?\nendstream`)
	streams := streamRe.FindAllSubmatch(body, -1)
	var parts []string
	for _, stream := range streams {
		dict := string(stream[1])
		data := bytes.Trim(stream[2], "\r\n")
		if strings.Contains(dict, "/FlateDecode") {
			reader, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				continue
			}
			decoded, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				continue
			}
			parts = append(parts, pdfContentText(decoded))
			continue
		}
		parts = append(parts, pdfContentText(data))
	}
	if len(parts) == 0 {
		warnings = append(warnings, "no readable PDF text streams found")
		parts = append(parts, pdfContentText(body))
	}
	return strings.Join(parts, " "), warnings
}

func pdfContentText(data []byte) string {
	s := string(data)
	var parts []string

	literalRe := regexp.MustCompile(`\((?:\\.|[^\\()])*\)`)
	for _, token := range literalRe.FindAllString(s, -1) {
		parts = append(parts, decodePDFLiteral(token[1:len(token)-1]))
	}

	hexRe := regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)
	for _, match := range hexRe.FindAllStringSubmatch(s, -1) {
		raw := strings.Join(strings.Fields(match[1]), "")
		if len(raw)%2 != 0 {
			raw += "0"
		}
		if decoded, err := hex.DecodeString(raw); err == nil {
			parts = append(parts, string(decoded))
		}
	}
	return strings.Join(parts, " ")
}

func decodePDFLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '(', ')', '\\':
			b.WriteByte(s[i])
		default:
			if s[i] >= '0' && s[i] <= '7' {
				end := i + 1
				for end < len(s) && end < i+3 && s[end] >= '0' && s[end] <= '7' {
					end++
				}
				value, err := strconv.ParseInt(s[i:end], 8, 32)
				if err == nil {
					b.WriteByte(byte(value))
				}
				i = end - 1
			} else {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

func normalizePDFText(s string) string {
	replacements := map[string]string{
		"\x00":   "",
		"\x02":   "",
		"\u2212": "-",
		"\u2013": "-",
		"\u2014": "-",
		"\u00a0": " ",
	}
	for old, replacement := range replacements {
		s = strings.ReplaceAll(s, old, replacement)
	}
	return normalizeSpace(s)
}

func numbersFromText(s string) []float64 {
	re := regexp.MustCompile(`[-+]?\d[\d,]*(?:\.\d+)?`)
	raw := re.FindAllString(s, -1)
	values := make([]float64, 0, len(raw))
	for _, item := range raw {
		value, err := strconv.ParseFloat(strings.ReplaceAll(item, ",", ""), 64)
		if err == nil {
			values = append(values, value)
		}
	}
	return values
}

func flexibleLabelPattern(label string) string {
	words := strings.Fields(label)
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, regexp.QuoteMeta(word))
	}
	return strings.Join(parts, `\s+`)
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + 32
		}
	}
	return string(b)
}

func parsePPIHTML(body []byte) (ParsedSnapshot, []string, error) {
	doc := string(body)
	plain := normalizeSpace(stripHTML(doc))
	lower := strings.ToLower(plain)

	if !strings.Contains(lower, "table 1") {
		return ParsedSnapshot{}, nil, errors.New("Table 1 marker not found")
	}
	if !strings.Contains(lower, "seasonally adjusted 1-month percent change") {
		return ParsedSnapshot{}, nil, errors.New("seasonally adjusted 1-month percent change header not found")
	}
	for _, target := range uniqueRowTargets() {
		if !strings.Contains(lower, strings.ToLower(target.RowText)) {
			return ParsedSnapshot{}, nil, fmt.Errorf("target row text not found: %s", target.RowText)
		}
	}

	period := parseLatestPeriod(doc)
	if period == "" {
		return ParsedSnapshot{}, nil, errors.New("latest release period not found")
	}
	fromPeriod := previousMonth(period)
	if fromPeriod == "" {
		return ParsedSnapshot{}, nil, fmt.Errorf("could not compute previous period for %q", period)
	}

	rows := htmlRows(doc)
	parsedRows := make(map[string]ParsedRow)
	for _, rowTarget := range uniqueRowTargets() {
		row, err := parseTargetRow(rows, rowTarget, period, fromPeriod)
		if err != nil {
			return ParsedSnapshot{}, nil, err
		}
		parsedRows[rowKey(rowTarget.RowText, rowTarget.GroupCode, rowTarget.ItemCode)] = row
	}

	snapshot := ParsedSnapshot{
		Period:          period,
		FromPeriod:      fromPeriod,
		LatestMoMColumn: latestMoMColumnFromPeriod(period),
		YoYColumn:       yoyColumnFromPeriod(period),
		Rows:            parsedRows,
	}
	for _, target := range measureTargets {
		row := parsedRows[rowKey(target.RowText, target.GroupCode, target.ItemCode)]
		measure, err := measureFromRow(target, row)
		if err != nil {
			return ParsedSnapshot{}, nil, err
		}
		snapshot.Measures = append(snapshot.Measures, measure)
	}
	return snapshot, nil, nil
}

func uniqueRowTargets() []MeasureTarget {
	seen := map[string]struct{}{}
	var out []MeasureTarget
	for _, target := range measureTargets {
		key := rowKey(target.RowText, target.GroupCode, target.ItemCode)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, target)
	}
	return out
}

func parseTargetRow(rows [][]string, target MeasureTarget, period, fromPeriod string) (ParsedRow, error) {
	var matched [][]string
	for _, cells := range rows {
		if len(cells) < 4 {
			continue
		}
		rowText := normalizeLabel(cells[0])
		groupCode := strings.ToUpper(strings.TrimSpace(cells[1]))
		itemCode := strings.TrimSpace(cells[2])
		if rowText == normalizeLabel(target.RowText) && groupCode == target.GroupCode && itemCode == target.ItemCode {
			matched = append(matched, cells)
		}
	}
	if len(matched) != 1 {
		return ParsedRow{}, fmt.Errorf("target row match count=%d for row=%q group=%s item=%s", len(matched), target.RowText, target.GroupCode, target.ItemCode)
	}

	numbers := numbersFromCells(matched[0][3:])
	if len(numbers) < 7 {
		return ParsedRow{}, fmt.Errorf("target row %s %s had %d numeric cells after item code, expected at least 7", target.GroupCode, target.ItemCode, len(numbers))
	}

	return ParsedRow{
		Period:             period,
		FromPeriod:         fromPeriod,
		LatestMoMColumn:    latestMoMColumnFromPeriod(period),
		YoYColumn:          yoyColumnFromPeriod(period),
		RowText:            stripTrailingFootnote(matched[0][0]),
		GroupCode:          target.GroupCode,
		ItemCode:           target.ItemCode,
		RelativeImportance: round3(numbers[0]),
		YoY:                round1(numbers[1]),
		LatestMoM:          round1(numbers[len(numbers)-1]),
		PreviousMoM:        round1(numbers[len(numbers)-2]),
	}, nil
}

func measureFromRow(target MeasureTarget, row ParsedRow) (MeasureResult, error) {
	measure := MeasureResult{
		Country:     country,
		EventName:   target.EventName,
		Table:       tableName,
		RowText:     row.RowText,
		GroupCode:   row.GroupCode,
		ItemCode:    row.ItemCode,
		Period:      row.Period,
		Unit:        unitPercent,
		Seasonality: target.Seasonality,
		ValueMethod: valueMethod,
		Confidence:  "HIGH",
	}

	switch target.ValueKind {
	case ValueMoM:
		measure.Column = row.LatestMoMColumn
		measure.FromPeriod = row.FromPeriod
		measure.ActualValue = row.LatestMoM
		measure.Actual = formatValueWithUnit(row.LatestMoM)
		measure.Previous = formatValueWithUnit(row.PreviousMoM)
	case ValueYoY:
		measure.Column = row.YoYColumn
		measure.ActualValue = row.YoY
		measure.Actual = formatValueWithUnit(row.YoY)
	default:
		return MeasureResult{}, fmt.Errorf("unknown value kind %q", target.ValueKind)
	}
	return measure, nil
}

func rowKey(rowText, groupCode, itemCode string) string {
	return normalizeLabel(rowText) + "|" + strings.ToUpper(strings.TrimSpace(groupCode)) + "|" + strings.TrimSpace(itemCode)
}

func htmlRows(doc string) [][]string {
	rowRe := regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	cellRe := regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]>`)
	rowMatches := rowRe.FindAllStringSubmatch(doc, -1)
	rows := make([][]string, 0, len(rowMatches))
	for _, rowMatch := range rowMatches {
		cellMatches := cellRe.FindAllStringSubmatch(rowMatch[1], -1)
		if len(cellMatches) == 0 {
			continue
		}
		cells := make([]string, 0, len(cellMatches))
		for _, cellMatch := range cellMatches {
			cells = append(cells, normalizeSpace(stripHTML(cellMatch[1])))
		}
		rows = append(rows, cells)
	}
	return rows
}

func numbersFromCells(cells []string) []float64 {
	var numbers []float64
	for _, cell := range cells {
		if value, ok := parseNumber(cell); ok {
			numbers = append(numbers, value)
		}
	}
	return numbers
}

func parseNumber(s string) (float64, bool) {
	clean := strings.ReplaceAll(s, "\u2212", "-")
	clean = strings.ReplaceAll(clean, "%", "")
	re := regexp.MustCompile(`[-+]?\d[\d,]*(?:\.\d+)?`)
	match := re.FindString(clean)
	if match == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(match, ",", ""), 64)
	return value, err == nil
}

func stripHTML(s string) string {
	scriptRe := regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	styleRe := regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	breakRe := regexp.MustCompile(`(?i)<\s*(br|/p|/tr|/th|/td|/li|/div)\b[^>]*>`)
	tagRe := regexp.MustCompile(`(?s)<[^>]+>`)
	s = scriptRe.ReplaceAllString(s, " ")
	s = styleRe.ReplaceAllString(s, " ")
	s = breakRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	return html.UnescapeString(s)
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func stripTrailingFootnote(label string) string {
	return strings.TrimSpace(regexp.MustCompile(`\s*\(\s*\d+\s*\)\s*$`).ReplaceAllString(normalizeSpace(label), ""))
}

func normalizeLabel(label string) string {
	return strings.ToLower(stripTrailingFootnote(label))
}

func parseLatestPeriod(s string) string {
	if period := parseMResultsPeriod(s); period != "" {
		return period
	}
	if period := parseTableCaptionPeriod(s); period != "" {
		return period
	}
	if period := parseLatestMonthYearPeriod(s); period != "" {
		return period
	}
	return ""
}

func parseMResultsPeriod(s string) string {
	re := regexp.MustCompile(`(?i)\b(20\d{2})\s+M(0[1-9]|1[0-2])\s+Results\b`)
	match := re.FindStringSubmatch(s)
	if len(match) == 3 {
		return match[1] + "-" + match[2]
	}
	return ""
}

func parseTableCaptionPeriod(s string) string {
	re := regexp.MustCompile(`(?i)\[\s*(January|February|March|April|May|June|July|August|September|October|November|December)\s+(20\d{2})\s*\]`)
	match := re.FindStringSubmatch(s)
	if len(match) == 3 {
		return fmt.Sprintf("%s-%02d", match[2], monthNumber(match[1]))
	}
	return ""
}

func parseLatestMonthYearPeriod(s string) string {
	re := regexp.MustCompile(`(?i)\b(January|February|March|April|May|June|July|August|September|October|November|December)\s+(20\d{2})\b`)
	matches := re.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return ""
	}
	match := matches[len(matches)-1]
	return fmt.Sprintf("%s-%02d", match[2], monthNumber(match[1]))
}

func validateConfiguredSchedule(client *http.Client, eventTime time.Time, logger *log.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheduleURL, nil)
	if err != nil {
		return "could not create BLS PPI schedule request"
	}
	setHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "BLS PPI schedule validation unavailable: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "BLS PPI schedule validation read failed"
	}

	schedule, err := parsePPISchedule(normalizeSpace(stripHTML(string(body))), expectedReleasePeriod(eventTime))
	if err != nil {
		return "BLS PPI schedule row not parsed"
	}
	if !schedule.Equal(eventTime) {
		logger.Printf("BLS PPI schedule release time=%s configured=%s", schedule.UTC().Format(time.RFC3339), eventTime.UTC().Format(time.RFC3339))
		return fmt.Sprintf("configured eventTimeUTC differs from BLS PPI schedule (%s UTC)", schedule.UTC().Format("2006-01-02 15:04:05"))
	}
	return ""
}

func parsePPISchedule(text, referencePeriod string) (time.Time, error) {
	reference := longPeriod(referencePeriod)
	if reference == "" {
		return time.Time{}, fmt.Errorf("invalid reference period %q", referencePeriod)
	}
	pattern := `(?i)\b` + regexp.QuoteMeta(reference) + `\s+` +
		`(Jan\.?|Feb\.?|Mar\.?|Apr\.?|May|Jun\.?|Jul\.?|Aug\.?|Sep\.?|Oct\.?|Nov\.?|Dec\.?)\s+` +
		`(\d{1,2}),\s+(20\d{2})\s+(\d{1,2}):(\d{2})\s+(AM|PM)\b`
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) != 7 {
		return time.Time{}, errors.New("PPI schedule row not found")
	}

	releaseMonth := monthNumber(match[1])
	releaseDay, _ := strconv.Atoi(match[2])
	releaseYear, _ := strconv.Atoi(match[3])
	hour, _ := strconv.Atoi(match[4])
	minute, _ := strconv.Atoi(match[5])
	if strings.EqualFold(match[6], "PM") && hour != 12 {
		hour += 12
	}
	if strings.EqualFold(match[6], "AM") && hour == 12 {
		hour = 0
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(releaseYear, time.Month(releaseMonth), releaseDay, hour, minute, 0, 0, loc).UTC(), nil
}

func monthNumber(name string) int {
	clean := strings.ToLower(strings.Trim(strings.TrimSpace(name), "."))
	switch clean {
	case "jan", "january":
		return 1
	case "feb", "february":
		return 2
	case "mar", "march":
		return 3
	case "apr", "april":
		return 4
	case "may":
		return 5
	case "jun", "june":
		return 6
	case "jul", "july":
		return 7
	case "aug", "august":
		return 8
	case "sep", "sept", "september":
		return 9
	case "oct", "october":
		return 10
	case "nov", "november":
		return 11
	case "dec", "december":
		return 12
	default:
		return 0
	}
}

func previousMonth(period string) string {
	parts := strings.Split(period, "-")
	if len(parts) != 2 {
		return ""
	}
	year, errY := strconv.Atoi(parts[0])
	month, errM := strconv.Atoi(parts[1])
	if errY != nil || errM != nil || month < 1 || month > 12 {
		return ""
	}
	month--
	if month == 0 {
		year--
		month = 12
	}
	return fmt.Sprintf("%04d-%02d", year, month)
}

func latestMoMColumnFromPeriod(period string) string {
	from := previousMonth(period)
	if from == "" {
		return ""
	}
	return fmt.Sprintf("%s to %s(p)", monthAbbrevFromPeriod(from), monthAbbrevFromPeriod(period))
}

func yoyColumnFromPeriod(period string) string {
	parts := strings.Split(period, "-")
	if len(parts) != 2 {
		return ""
	}
	year, errY := strconv.Atoi(parts[0])
	month, errM := strconv.Atoi(parts[1])
	if errY != nil || errM != nil || month < 1 || month > 12 {
		return ""
	}
	return fmt.Sprintf("%s %d to %s %d(p)", monthAbbrevFromPeriod(period), year-1, monthAbbrevFromPeriod(period), year)
}

func monthAbbrevFromPeriod(period string) string {
	parts := strings.Split(period, "-")
	if len(parts) != 2 {
		return period
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return period
	}
	names := []string{"", "Jan.", "Feb.", "Mar.", "Apr.", "May", "Jun.", "Jul.", "Aug.", "Sep.", "Oct.", "Nov.", "Dec."}
	return names[month]
}

func prettyPeriod(period string) string {
	parts := strings.Split(period, "-")
	if len(parts) != 2 {
		return period
	}
	year := parts[0]
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return period
	}
	names := []string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	return fmt.Sprintf("%s %s", names[month], year)
}

func longPeriod(period string) string {
	parts := strings.Split(period, "-")
	if len(parts) != 2 {
		return ""
	}
	year := parts[0]
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return ""
	}
	names := []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	return fmt.Sprintf("%s %s", names[month], year)
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func formatPercent(v float64) string {
	if math.Abs(v) == 0 {
		v = 0
	}
	return strconv.FormatFloat(round1(v), 'f', 1, 64)
}

func formatValueWithUnit(v float64) string {
	return formatPercent(v) + "%"
}

func measureSignature(measures []MeasureResult) string {
	parts := make([]string, 0, len(measures))
	for _, measure := range measures {
		parts = append(parts, fmt.Sprintf("%s=%s", measure.EventName, measure.Actual))
	}
	return strings.Join(parts, ", ")
}

func formatLatency(ms int64) string {
	sign := "+"
	if ms < 0 {
		sign = "-"
		ms = -ms
	}
	return fmt.Sprintf("%s%.3fs", sign, float64(ms)/1000)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func trimForConsole(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
