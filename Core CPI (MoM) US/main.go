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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata"
)

// ============================================================================
// EVENT CONFIGURATION - CHANGE THIS FOR NEXT CPI RELEASE
// ============================================================================
//
// Event: US CPI Table 1 group
// Release Time: 08:30 Eastern Time
// IST = UTC + 5:30
//
// Format: "YYYY-MM-DD HH:MM:SS" in UTC.
var eventTimeUTC = "2026-06-10 12:30:00"

// ============================================================================

const (
	country         = "US"
	eventName       = "US CPI Table 1"
	officialRelease = "Consumer Price Index Summary"
	publisher       = "U.S. Bureau of Labor Statistics"
	seriesID        = "CUSR0000SA0L1E"
	tableName       = "Table 1"
	indexName       = "CPI-U, U.S. city average"
	allItemsField   = "All items"
	targetField     = "All items less food and energy"
	primaryMetricID = "core_cpi_mom"
	unitPercent     = "%"

	tableURL     = "https://www.bls.gov/news.release/cpi.t01.htm"
	summaryURL   = "https://www.bls.gov/news.release/cpi.nr0.htm"
	pdfURL       = "https://www.bls.gov/news.release/pdf/cpi.pdf"
	apiURL       = "https://api.bls.gov/publicAPI/v2/timeseries/data/CUSR0000SA0L1E"
	investingURL = "https://www.investing.com/economic-calendar/"
	scheduleURL  = "https://www.bls.gov/cpi/"

	httpTimeout        = 5 * time.Second
	requestTimeout     = 5 * time.Second
	fastRequestTimeout = 1500 * time.Millisecond
	pollCadence        = 500 * time.Millisecond
	fastPollCadence    = 100 * time.Millisecond
	contentEveryPolls  = 5
	testLead           = 1 * time.Minute
	sniperLead         = 2 * time.Second
	pollWindow         = 3 * time.Minute
	userAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 US-CPI-Table1-Sniper/1.0"
)

type SourceType string

const (
	SourceHTMLTable SourceType = "html_table"
	SourceSummary   SourceType = "html_summary"
	SourcePDF       SourceType = "pdf"
	SourceAPI       SourceType = "json_api"
	SourceInvesting SourceType = "investing_confirmation"
	SourceSchedule  SourceType = "html_schedule"
)

type Latency struct {
	Total    int64 `json:"total"`
	TTFB     int64 `json:"ttfb"`
	BodyRead int64 `json:"body_read"`
	Parse    int64 `json:"parse"`
}

type Result struct {
	Source          string
	URL             string
	SourceType      string
	Period          string
	Value           string
	NumericValue    float64
	Metrics         map[string]MetricValue
	Unit            string
	Timestamp       time.Time
	EventLatencyMs  int64
	DetectionMethod string
	Confidence      string
	ETag            string
	LastModified    string
	CacheControl    string
	ServerDate      string
	ContentHash     string
	StatusCode      int
	Error           string
	Warnings        []string
	Latency         Latency
	ValueMethod     string
}

type Baseline struct {
	ETag         string
	LastModified string
	ContentHash  string
	Period       string
	Value        string
}

type SourceResult struct {
	Name     string
	Baseline *Baseline
	FirstHit *Result
	Latest   *Result
	Detected bool
}

type Scraper interface {
	Name() string
	URL() string
	SourceType() SourceType
	FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error)
	Parse(body []byte, headers http.Header) (*Result, error)
}

type tableScraper struct{}
type summaryScraper struct{}
type pdfScraper struct{}
type apiScraper struct{}
type investingScraper struct{}

type JSONResult struct {
	Country          string        `json:"country"`
	EventName        string        `json:"event_name"`
	OfficialRelease  string        `json:"official_release"`
	SourceURL        string        `json:"source_url"`
	SourceType       string        `json:"source_type"`
	Table            string        `json:"table"`
	Index            string        `json:"index"`
	SeriesID         string        `json:"series_id,omitempty"`
	Field            string        `json:"field"`
	Period           string        `json:"period"`
	Actual           string        `json:"actual"`
	Unit             string        `json:"unit"`
	ValueMethod      string        `json:"value_method"`
	Metrics          []MetricValue `json:"metrics"`
	Confidence       string        `json:"confidence"`
	ServerDateHeader string        `json:"server_date_header"`
	ETag             string        `json:"etag"`
	LastModified     string        `json:"last_modified"`
	CacheControl     string        `json:"cache_control"`
	LatencyMS        Latency       `json:"latency_ms"`
	MatchedSources   []string      `json:"matched_sources"`
	Warnings         []string      `json:"warnings"`
}

type blsAPIResponse struct {
	Status   string          `json:"status"`
	Messages json.RawMessage `json:"message"`
	Results  struct {
		Series []struct {
			SeriesID string `json:"seriesID"`
			Data     []struct {
				Year       string `json:"year"`
				Period     string `json:"period"`
				PeriodName string `json:"periodName"`
				Value      string `json:"value"`
				Latest     string `json:"latest"`
			} `json:"data"`
		} `json:"series"`
	} `json:"Results"`
}

type apiPoint struct {
	Period string
	Value  float64
}

type MetricDefinition struct {
	ID          string
	EventName   string
	Row         string
	Column      string
	NumberIndex int
}

type InvestingMetricDefinition struct {
	MetricID       string
	URL            string
	ExpectedTitles []string
}

type MetricValue struct {
	ID                string                   `json:"id"`
	EventName         string                   `json:"event_name"`
	Row               string                   `json:"row"`
	Column            string                   `json:"column"`
	Period            string                   `json:"period"`
	Actual            string                   `json:"actual"`
	NumericValue      float64                  `json:"numeric_value"`
	Unit              string                   `json:"unit"`
	ValueMethod       string                   `json:"value_method"`
	Forecast          string                   `json:"forecast,omitempty"`
	Previous          string                   `json:"previous,omitempty"`
	LatestReleaseDate string                   `json:"latest_release_date,omitempty"`
	SourceURL         string                   `json:"source_url,omitempty"`
	HistoricalRows    []InvestingHistoricalRow `json:"historical_rows,omitempty"`
}

type InvestingHistoricalRow struct {
	ReleaseDate string `json:"release_date"`
	Time        string `json:"time"`
	Period      string `json:"period"`
	Actual      string `json:"actual,omitempty"`
	Forecast    string `json:"forecast,omitempty"`
	Previous    string `json:"previous,omitempty"`
}

var table1Metrics = []MetricDefinition{
	{
		ID:          "cpi_mom",
		EventName:   "CPI (MoM)",
		Row:         allItemsField,
		Column:      "latest seasonally adjusted 1-month percent change",
		NumberIndex: 8,
	},
	{
		ID:          "cpi_yoy",
		EventName:   "CPI (YoY)",
		Row:         allItemsField,
		Column:      "unadjusted 12-month percent change",
		NumberIndex: 4,
	},
	{
		ID:          primaryMetricID,
		EventName:   "Core CPI (MoM)",
		Row:         targetField,
		Column:      "latest seasonally adjusted 1-month percent change",
		NumberIndex: 8,
	},
	{
		ID:          "core_cpi_yoy",
		EventName:   "Core CPI (YoY)",
		Row:         targetField,
		Column:      "unadjusted 12-month percent change",
		NumberIndex: 4,
	},
}

var investingMetrics = []InvestingMetricDefinition{
	{
		MetricID: "cpi_mom",
		URL:      "https://www.investing.com/economic-calendar/cpi-69",
		ExpectedTitles: []string{
			"U.S. Consumer Price Index (CPI) MoM",
			"United States Consumer Price Index (CPI) MoM",
		},
	},
	{
		MetricID: "cpi_yoy",
		URL:      "https://www.investing.com/economic-calendar/cpi-733",
		ExpectedTitles: []string{
			"U.S. Consumer Price Index (CPI) YoY",
			"United States Consumer Price Index (CPI) YoY",
		},
	},
	{
		MetricID: "core_cpi_mom",
		URL:      "https://www.investing.com/economic-calendar/united-states-core-consumer-price-index-%28cpi%29-mom-56",
		ExpectedTitles: []string{
			"U.S. Core Consumer Price Index (CPI) MoM",
			"United States Core Consumer Price Index (CPI) MoM",
		},
	},
	{
		MetricID: "core_cpi_yoy",
		URL:      "https://www.investing.com/economic-calendar/united-states-core-consumer-price-index-%28cpi%29-yoy-736",
		ExpectedTitles: []string{
			"U.S. Core Consumer Price Index (CPI) YoY",
			"United States Core Consumer Price Index (CPI) YoY",
		},
	},
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
	sources := valueSources()
	primarySources := primaryValueSources()
	expectedPeriod := expectedReleasePeriod(eventTime)

	printBanner(eventTime)

	if warning := validateConfiguredSchedule(client, eventTime, logger); warning != "" {
		fmt.Printf("WARNING: %s\n", warning)
	}

	fmt.Println("Fetching Current Published Data...")
	currentSources := sources
	if time.Until(eventTime) <= testLead {
		currentSources = primarySources
		fmt.Println("Close to release: using BLS Table 1 only for preflight speed.")
	}
	current := fetchCurrentPublishedData(client, currentSources, logger)
	printCurrentData(current)
	fmt.Println("Current data captured. Waiting for new release...")
	fmt.Println()

	testTime := eventTime.Add(-testLead)
	sniperStart := eventTime.Add(-sniperLead)
	pollEnd := eventTime.Add(pollWindow)

	if time.Now().Before(testTime) {
		fmt.Printf("Countdown to Test Connection: ")
		countdownTo(testTime, "Will test connections 1 minute before event", time.Second)
	}

	fmt.Println("Testing connections...")
	fmt.Println("   Capturing primary BLS Table 1 baseline for fast detection...")
	baselines := captureBaselines(client, primarySources, logger)
	printBaselines(baselines)
	fmt.Println()

	if time.Now().Before(sniperStart) {
		fmt.Printf("Countdown to Sniper Mode: ")
		countdownTo(sniperStart, "Sniper mode activates 2 seconds before event", 100*time.Millisecond)
	}

	if time.Now().After(pollEnd) {
		fmt.Println("Polling window already ended for configured event time.")
		printPerformanceTable(make(map[string]*SourceResult), eventTime)
		fmt.Println("NOT_CONFIRMED")
		return
	}

	fmt.Println("SNIPER MODE ACTIVATED!")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("FAST PATH: BLS Table 1 content polling every 100ms")
	fmt.Println("Confirmation sources are excluded from the critical output path")
	fmt.Println(strings.Repeat("=", 72))

	sourceResults := runFastPrimarySniperMode(client, baselines, eventTime, pollEnd, expectedPeriod, logger)
	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("Fast path complete")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()

	printPerformanceTable(sourceResults, eventTime)

	confirmed, err := mergeConfirmed(sourceResults, expectedPeriod, investingReleaseDate(eventTime))
	if err != nil {
		if strings.Contains(err.Error(), "INVESTING_MISMATCH") {
			fmt.Println("INVESTING_MISMATCH")
		}
		if strings.Contains(err.Error(), "INVESTING_NOT_UPDATED") {
			fmt.Println("INVESTING_NOT_UPDATED")
		}
		fmt.Println("NOT_CONFIRMED")
		logger.Printf("final value not confirmed: %v", err)
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(confirmed); err != nil {
		fmt.Println("NOT_CONFIRMED")
		logger.Printf("json encode failed: %v", err)
	}
}

func valueSources() []Scraper {
	return []Scraper{
		tableScraper{},
		summaryScraper{},
		pdfScraper{},
		apiScraper{},
		investingScraper{},
	}
}

func primaryValueSources() []Scraper {
	return []Scraper{tableScraper{}}
}

func (tableScraper) Name() string           { return "BLS CPI Table 1 HTML" }
func (tableScraper) URL() string            { return tableURL }
func (tableScraper) SourceType() SourceType { return SourceHTMLTable }
func (tableScraper) FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error) {
	return fetchHeadersOnly(ctx, client, tableScraper{})
}
func (tableScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	parsed, warnings, err := parseTable1HTML(body)
	if err != nil {
		return nil, err
	}
	return parsedValueToResult(tableScraper{}, parsed, warnings), nil
}

func (summaryScraper) Name() string           { return "BLS CPI Summary HTML" }
func (summaryScraper) URL() string            { return summaryURL }
func (summaryScraper) SourceType() SourceType { return SourceSummary }
func (summaryScraper) FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error) {
	return fetchHeadersOnly(ctx, client, summaryScraper{})
}
func (summaryScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	parsed, warnings, err := parseCPISummaryHTML(body)
	if err != nil {
		return nil, err
	}
	return parsedValueToResult(summaryScraper{}, parsed, warnings), nil
}

func (pdfScraper) Name() string           { return "BLS CPI PDF Release" }
func (pdfScraper) URL() string            { return pdfURL }
func (pdfScraper) SourceType() SourceType { return SourcePDF }
func (pdfScraper) FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error) {
	return fetchHeadersOnly(ctx, client, pdfScraper{})
}
func (pdfScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	parsed, warnings, err := parseCPIPDF(body)
	if err != nil {
		return nil, err
	}
	return parsedValueToResult(pdfScraper{}, parsed, warnings), nil
}

func (apiScraper) Name() string           { return "BLS API CUSR0000SA0L1E" }
func (apiScraper) URL() string            { return apiURL }
func (apiScraper) SourceType() SourceType { return SourceAPI }
func (apiScraper) FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error) {
	return fetchHeadersOnly(ctx, client, apiScraper{})
}
func (apiScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	parsed, warnings, err := parseBLSAPI(body)
	if err != nil {
		return nil, err
	}
	return parsedValueToResult(apiScraper{}, parsed, warnings), nil
}

func (investingScraper) Name() string           { return "Investing.com CPI Group" }
func (investingScraper) URL() string            { return investingURL }
func (investingScraper) SourceType() SourceType { return SourceInvesting }
func (investingScraper) FetchWithHeaders(ctx context.Context, client *http.Client) (*Result, error) {
	return fetchHeadersOnly(ctx, client, investingScraper{})
}
func (investingScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	return nil, errors.New("Investing.com group parser requires fetching all event pages")
}

type fullContentFetcher interface {
	FetchAndParseContent(ctx context.Context, client *http.Client, logger *log.Logger, logDetail bool) (*Result, error)
}

type ParsedValue struct {
	Period      string
	Value       float64
	ValueString string
	Field       string
	Method      string
	Metrics     map[string]MetricValue
}

func parsedValueToResult(scraper Scraper, parsed ParsedValue, warnings []string) *Result {
	confidence := "HIGH"
	if scraper.SourceType() == SourcePDF {
		confidence = "MEDIUM"
	}
	if scraper.SourceType() == SourceInvesting {
		confidence = "MEDIUM"
	}
	return &Result{
		Source:       scraper.Name(),
		URL:          scraper.URL(),
		SourceType:   string(scraper.SourceType()),
		Period:       parsed.Period,
		Value:        formatValueWithUnit(parsed.Value),
		NumericValue: parsed.Value,
		Metrics:      cloneMetrics(parsed.Metrics),
		Unit:         unitPercent,
		Confidence:   confidence,
		Warnings:     warnings,
		ValueMethod:  parsed.Method,
	}
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   1200 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   12,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   1200 * time.Millisecond,
			ResponseHeaderTimeout: 2500 * time.Millisecond,
			ExpectContinueTimeout: 250 * time.Millisecond,
		},
	}
}

func fetchHeadersOnly(parent context.Context, client *http.Client, scraper Scraper) (*Result, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, scraper.URL(), nil)
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
	result := resultFromHeaders(scraper, resp.Header, resp.StatusCode, Latency{
		Total: time.Since(start).Milliseconds(),
		TTFB:  ttfb,
	})
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden {
		return fetchHeadersViaGET(parent, client, scraper)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return result, fmt.Errorf("%s header request status=%s", scraper.Name(), resp.Status)
	}
	return result, nil
}

func fetchHeadersViaGET(parent context.Context, client *http.Client, scraper Scraper) (*Result, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scraper.URL(), nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := resultFromHeaders(scraper, resp.Header, resp.StatusCode, Latency{
		Total: time.Since(start).Milliseconds(),
		TTFB:  time.Since(start).Milliseconds(),
	})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return result, fmt.Errorf("%s header fallback status=%s", scraper.Name(), resp.Status)
	}
	return result, nil
}

func fetchAndParse(parent context.Context, client *http.Client, scraper Scraper, logger *log.Logger, logDetail bool) (*Result, error) {
	if custom, ok := scraper.(fullContentFetcher); ok {
		return custom.FetchAndParseContent(parent, client, logger, logDetail)
	}
	return fetchAndParseWithTimeout(parent, client, scraper, logger, logDetail, requestTimeout)
}

func fetchAndParseWithTimeout(parent context.Context, client *http.Client, scraper Scraper, logger *log.Logger, logDetail bool, timeout time.Duration) (*Result, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	if logDetail {
		logger.Printf("%s request start url=%s", scraper.Name(), scraper.URL())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scraper.URL(), nil)
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
		logger.Printf("%s response headers received status=%s Date=%q ETag=%q Last-Modified=%q Cache-Control=%q",
			scraper.Name(), resp.Status, headers.Get("Date"), headers.Get("ETag"), headers.Get("Last-Modified"), headers.Get("Cache-Control"))
	}

	base := resultFromHeaders(scraper, headers, resp.StatusCode, Latency{TTFB: ttfb})
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		base.Latency.Total = time.Since(start).Milliseconds()
		return base, fmt.Errorf("%s content request status=%s", scraper.Name(), resp.Status)
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
		logger.Printf("%s body read complete bytes=%d sha256=%s body_read_ms=%d", scraper.Name(), len(body), contentHash, bodyReadMS)
	}

	parseStart := time.Now()
	parsed, err := scraper.Parse(body, headers)
	parseMS := time.Since(parseStart).Milliseconds()
	if err != nil {
		base.ContentHash = contentHash
		base.Latency = Latency{Total: time.Since(start).Milliseconds(), TTFB: ttfb, BodyRead: bodyReadMS, Parse: parseMS}
		return base, err
	}

	parsed.ETag = headers.Get("ETag")
	parsed.LastModified = headers.Get("Last-Modified")
	parsed.CacheControl = headers.Get("Cache-Control")
	parsed.ServerDate = headers.Get("Date")
	parsed.StatusCode = resp.StatusCode
	parsed.ContentHash = contentHash
	parsed.Timestamp = time.Now().UTC()
	parsed.Latency = Latency{Total: time.Since(start).Milliseconds(), TTFB: ttfb, BodyRead: bodyReadMS, Parse: parseMS}
	if logDetail {
		logger.Printf("%s parse complete period=%q value=%q parse_ms=%d total_ms=%d", scraper.Name(), parsed.Period, parsed.Value, parseMS, parsed.Latency.Total)
	}
	return parsed, nil
}

func (investingScraper) FetchAndParseContent(parent context.Context, client *http.Client, logger *log.Logger, logDetail bool) (*Result, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout*4)
	defer cancel()

	start := time.Now()
	metrics := make(map[string]MetricValue, len(investingMetrics))
	var warnings []string
	var combined bytes.Buffer
	var firstHeaders http.Header
	statusCode := 0
	var ttfbMS int64
	var bodyReadMS int64
	var parseMS int64

	pageResults := fetchInvestingMetricPages(ctx, client, logger, logDetail)
	for _, page := range pageResults {
		if firstHeaders == nil && page.Headers != nil {
			firstHeaders = page.Headers.Clone()
			statusCode = page.StatusCode
		}
		ttfbMS += page.Latency.TTFB
		bodyReadMS += page.Latency.BodyRead
		parseMS += page.Latency.Parse
		if page.Err != nil {
			return resultFromHeaders(investingScraper{}, firstHeadersOrEmpty(firstHeaders), statusCode, Latency{
				Total:    time.Since(start).Milliseconds(),
				TTFB:     ttfbMS,
				BodyRead: bodyReadMS,
				Parse:    parseMS,
			}), page.Err
		}
		combined.Write(page.Body)
		warnings = append(warnings, page.Warnings...)
		metrics[page.Def.MetricID] = page.Metric
	}

	if err := validateInvestingGroupPeriods(metrics); err != nil {
		return resultFromHeaders(investingScraper{}, firstHeadersOrEmpty(firstHeaders), statusCode, Latency{
			Total:    time.Since(start).Milliseconds(),
			TTFB:     ttfbMS,
			BodyRead: bodyReadMS,
			Parse:    parseMS,
		}), err
	}

	hash := sha256.Sum256(combined.Bytes())
	primary := metrics[primaryMetricID]
	result := &Result{
		Source:       (investingScraper{}).Name(),
		URL:          investingURL,
		SourceType:   string(SourceInvesting),
		Period:       primary.Period,
		Value:        primary.Actual,
		NumericValue: primary.NumericValue,
		Metrics:      metrics,
		Unit:         unitPercent,
		Timestamp:    time.Now().UTC(),
		Confidence:   "MEDIUM",
		Warnings:     warnings,
		ETag:         firstHeaders.Get("ETag"),
		LastModified: firstHeaders.Get("Last-Modified"),
		CacheControl: firstHeaders.Get("Cache-Control"),
		ServerDate:   firstHeaders.Get("Date"),
		ContentHash:  hex.EncodeToString(hash[:]),
		StatusCode:   statusCode,
		Latency: Latency{
			Total:    time.Since(start).Milliseconds(),
			TTFB:     ttfbMS,
			BodyRead: bodyReadMS,
			Parse:    parseMS,
		},
		ValueMethod: "investing_html_confirmation",
	}
	return result, nil
}

type investingPageResult struct {
	Def        InvestingMetricDefinition
	Metric     MetricValue
	Warnings   []string
	Body       []byte
	Headers    http.Header
	StatusCode int
	Latency    Latency
	Err        error
}

func fetchInvestingMetricPages(ctx context.Context, client *http.Client, logger *log.Logger, logDetail bool) []investingPageResult {
	ch := make(chan investingPageResult, len(investingMetrics))
	var wg sync.WaitGroup
	for _, def := range investingMetrics {
		def := def
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- fetchInvestingMetricPage(ctx, client, logger, logDetail, def)
		}()
	}
	wg.Wait()
	close(ch)

	byID := make(map[string]investingPageResult, len(investingMetrics))
	for result := range ch {
		byID[result.Def.MetricID] = result
	}
	ordered := make([]investingPageResult, 0, len(investingMetrics))
	for _, def := range investingMetrics {
		if result, ok := byID[def.MetricID]; ok {
			ordered = append(ordered, result)
		}
	}
	return ordered
}

func fetchInvestingMetricPage(ctx context.Context, client *http.Client, logger *log.Logger, logDetail bool, def InvestingMetricDefinition) investingPageResult {
	pageStart := time.Now()
	if logDetail {
		logger.Printf("%s request start url=%s", (investingScraper{}).Name(), def.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, def.URL, nil)
	if err != nil {
		return investingPageResult{Def: def, Err: err}
	}
	setHeaders(req)
	req.Header.Set("Referer", investingURL)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	ttfb := time.Since(pageStart).Milliseconds()
	if err != nil {
		return investingPageResult{Def: def, Latency: Latency{Total: time.Since(pageStart).Milliseconds(), TTFB: ttfb}, Err: err}
	}
	defer resp.Body.Close()

	headers := resp.Header.Clone()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return investingPageResult{
			Def:        def,
			Headers:    headers,
			StatusCode: resp.StatusCode,
			Latency:    Latency{Total: time.Since(pageStart).Milliseconds(), TTFB: ttfb},
			Err:        fmt.Errorf("%s content request status=%s url=%s", (investingScraper{}).Name(), resp.Status, def.URL),
		}
	}

	bodyStart := time.Now()
	body, err := io.ReadAll(resp.Body)
	bodyRead := time.Since(bodyStart).Milliseconds()
	if err != nil {
		return investingPageResult{
			Def:        def,
			Headers:    headers,
			StatusCode: resp.StatusCode,
			Latency:    Latency{Total: time.Since(pageStart).Milliseconds(), TTFB: ttfb, BodyRead: bodyRead},
			Err:        err,
		}
	}

	parseStart := time.Now()
	metric, warnings, err := parseInvestingMetricPage(body, def)
	parse := time.Since(parseStart).Milliseconds()
	return investingPageResult{
		Def:        def,
		Metric:     metric,
		Warnings:   warnings,
		Body:       body,
		Headers:    headers,
		StatusCode: resp.StatusCode,
		Latency:    Latency{Total: time.Since(pageStart).Milliseconds(), TTFB: ttfb, BodyRead: bodyRead, Parse: parse},
		Err:        err,
	}
}

func firstHeadersOrEmpty(headers http.Header) http.Header {
	if headers == nil {
		return http.Header{}
	}
	return headers
}

func resultFromHeaders(scraper Scraper, headers http.Header, statusCode int, latency Latency) *Result {
	return &Result{
		Source:       scraper.Name(),
		URL:          scraper.URL(),
		SourceType:   string(scraper.SourceType()),
		Unit:         unitPercent,
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
	req.Header.Set("Accept", "*/*")
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
	fmt.Println("US CPI Table 1 Scraper - SNIPER MODE")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("Publisher: %s\n", publisher)
	fmt.Printf("Event Time (IST): %s\n", eventTime.In(ist).Format("2006-01-02 15:04:05"))
	fmt.Printf("Event Time (UTC): %s\n", eventTime.UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected CPI Period: %s\n", prettyPeriod(expectedReleasePeriod(eventTime)))
	fmt.Println(strings.Repeat("=", 72))
}

func fetchCurrentPublishedData(client *http.Client, sources []Scraper, logger *log.Logger) map[string]*Result {
	return fetchAllContent(context.Background(), client, sources, logger, true)
}

func fetchAllContent(ctx context.Context, client *http.Client, sources []Scraper, logger *log.Logger, logDetail bool) map[string]*Result {
	type item struct {
		name   string
		result *Result
		err    error
	}
	ch := make(chan item, len(sources))
	var wg sync.WaitGroup
	for _, scraper := range sources {
		scraper := scraper
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fetchAndParse(ctx, client, scraper, logger, logDetail)
			ch <- item{name: scraper.Name(), result: result, err: err}
		}()
	}
	wg.Wait()
	close(ch)

	out := make(map[string]*Result, len(sources))
	for item := range ch {
		if item.err != nil {
			if item.result == nil {
				item.result = &Result{Source: item.name, Error: item.err.Error()}
			} else {
				item.result.Error = item.err.Error()
			}
		}
		out[item.name] = item.result
	}
	return out
}

func printCurrentData(results map[string]*Result) {
	for _, name := range sourceOrder() {
		if _, exists := results[name]; !exists {
			continue
		}
		result := results[name]
		if result == nil || result.Error != "" {
			errText := "unavailable"
			if result != nil && result.Error != "" {
				errText = result.Error
			}
			fmt.Printf("NO  [%-27s] %s\n", name, errText)
			continue
		}
		fmt.Printf("OK  [%-27s] Period: %-12s Value: %-7s ETag: %-18s Last-Modified: %s\n",
			name, prettyPeriod(result.Period), result.Value, trimForConsole(result.ETag, 18), trimForConsole(result.LastModified, 29))
		if summary := metricConsoleSummary(result.Metrics); summary != "" {
			fmt.Printf("    Metrics: %s\n", summary)
		}
	}
	fmt.Println(strings.Repeat("-", 72))
}

func captureBaselines(client *http.Client, sources []Scraper, logger *log.Logger) map[string]*SourceResult {
	results := fetchAllContent(context.Background(), client, sources, logger, true)
	out := make(map[string]*SourceResult, len(sources))
	for _, scraper := range sources {
		result := results[scraper.Name()]
		state := &SourceResult{Name: scraper.Name(), Latest: result}
		if result != nil && result.Error == "" {
			state.Baseline = &Baseline{
				ETag:         result.ETag,
				LastModified: result.LastModified,
				ContentHash:  result.ContentHash,
				Period:       result.Period,
				Value:        result.Value,
			}
		}
		out[scraper.Name()] = state
	}
	return out
}

func printBaselines(states map[string]*SourceResult) {
	for _, name := range sourceOrder() {
		if _, exists := states[name]; !exists {
			continue
		}
		state := states[name]
		if state == nil || state.Baseline == nil || state.Latest == nil || state.Latest.Error != "" {
			errText := "baseline unavailable"
			if state != nil && state.Latest != nil && state.Latest.Error != "" {
				errText = state.Latest.Error
			}
			fmt.Printf("NO  [%s] %s\n", name, errText)
			continue
		}
		fmt.Printf("OK  [%s] Connected | Period: %s | Value: %s | ETag: %s\n",
			name, prettyPeriod(state.Baseline.Period), state.Baseline.Value, trimForConsole(state.Baseline.ETag, 24))
		if summary := metricConsoleSummary(state.Latest.Metrics); summary != "" {
			fmt.Printf("    Metrics: %s\n", summary)
		}
	}
}

func runFastPrimarySniperMode(client *http.Client, baselines map[string]*SourceResult, eventTime, pollEnd time.Time, expectedPeriod string, logger *log.Logger) map[string]*SourceResult {
	ctx, cancel := context.WithDeadline(context.Background(), pollEnd)
	defer cancel()

	scraper := tableScraper{}
	state := baselines[scraper.Name()]
	if state == nil {
		state = &SourceResult{Name: scraper.Name()}
	}
	results := map[string]*SourceResult{scraper.Name(): state}
	ticker := time.NewTicker(fastPollCadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return results
		default:
		}

		candidate, err := fetchAndParseWithTimeout(ctx, client, scraper, logger, false, fastRequestTimeout)
		if candidate != nil && err != nil {
			candidate.Error = err.Error()
			state.Latest = candidate
		}
		if candidate != nil && candidate.Error == "" {
			state.Latest = candidate
			contentChanged := hasContentChange(state.Baseline, candidate)
			if err := validateResult(candidate, expectedPeriod); err == nil {
				if err := validateMetricPayload(candidate, expectedPeriod, ""); err == nil {
					candidate.DetectionMethod = "fast_table_content"
					if contentChanged {
						candidate.DetectionMethod = "fast_table_content_changed"
					}
					candidate.EventLatencyMs = candidate.Timestamp.Sub(eventTime).Milliseconds()
					state.FirstHit = cloneResult(candidate)
					state.Detected = true
					fmt.Printf("[%s] UPDATED! [%s] Period: %s | Value: %s | Detected by: %s\n",
						candidate.Timestamp.UTC().Format("15:04:05.000"), candidate.Source, prettyPeriod(candidate.Period), candidate.Value, candidate.DetectionMethod)
					if summary := metricConsoleSummary(candidate.Metrics); summary != "" {
						fmt.Printf("    Metrics: %s\n", summary)
					}
					return results
				}
			}
		}

		select {
		case <-ctx.Done():
			return results
		case <-ticker.C:
		}
	}
}

func countdownTo(target time.Time, note string, tick time.Duration) {
	if note != "" {
		fmt.Printf("%s\n", note)
	}
	for {
		remaining := time.Until(target)
		if remaining <= 0 {
			fmt.Print("\r")
			return
		}
		if remaining > 10*time.Second {
			fmt.Printf("\r%v remaining", remaining.Truncate(time.Second))
			time.Sleep(time.Second)
			continue
		}
		fmt.Printf("\r%v remaining", remaining.Truncate(time.Millisecond))
		time.Sleep(tick)
	}
}

func runSniperMode(client *http.Client, sources []Scraper, baselines map[string]*SourceResult, eventTime, pollEnd time.Time, expectedPeriod string, logger *log.Logger) map[string]*SourceResult {
	ctx, cancel := context.WithDeadline(context.Background(), pollEnd)
	defer cancel()

	results := make(map[string]*SourceResult, len(sources))
	for _, scraper := range sources {
		if existing := baselines[scraper.Name()]; existing != nil {
			results[scraper.Name()] = existing
		} else {
			results[scraper.Name()] = &SourceResult{Name: scraper.Name()}
		}
	}

	hits := make(chan *Result, len(sources)*4)
	var wg sync.WaitGroup
	for _, scraper := range sources {
		scraper := scraper
		state := results[scraper.Name()]
		wg.Add(1)
		go func() {
			defer wg.Done()
			pollSource(ctx, client, scraper, state, eventTime, expectedPeriod, hits, logger)
		}()
	}

	go func() {
		wg.Wait()
		close(hits)
	}()

	for hit := range hits {
		fmt.Printf("[%s] UPDATED! [%s] Period: %s | Value: %s | Detected by: %s\n",
			hit.Timestamp.UTC().Format("15:04:05.000"), hit.Source, prettyPeriod(hit.Period), hit.Value, hit.DetectionMethod)
		if summary := metricConsoleSummary(hit.Metrics); summary != "" {
			fmt.Printf("    Metrics: %s\n", summary)
		}
	}

	return results
}

func pollSource(ctx context.Context, client *http.Client, scraper Scraper, state *SourceResult, eventTime time.Time, expectedPeriod string, hits chan<- *Result, logger *log.Logger) {
	ticker := time.NewTicker(pollCadence)
	defer ticker.Stop()

	pollNo := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pollNo++
		checkContent := pollNo%contentEveryPolls == 0 || state.Baseline == nil
		var candidate *Result
		var err error
		headersChanged := false
		contentChecked := false

		if checkContent {
			contentChecked = true
			candidate, err = fetchAndParse(ctx, client, scraper, logger, false)
		} else {
			headerResult, headerErr := scraper.FetchWithHeaders(ctx, client)
			if headerErr != nil {
				err = headerErr
				candidate = headerResult
			} else {
				headersChanged = hasHeaderChange(state.Baseline, headerResult)
				if headersChanged {
					contentChecked = true
					candidate, err = fetchAndParse(ctx, client, scraper, logger, false)
				} else {
					state.Latest = headerResult
				}
			}
		}

		if candidate != nil && err != nil {
			candidate.Error = err.Error()
			state.Latest = candidate
		}

		if candidate != nil && candidate.Error == "" && contentChecked {
			contentChanged := hasContentChange(state.Baseline, candidate)
			if headersChanged || contentChanged {
				method := detectionMethod(headersChanged, contentChanged)
				if err := validateResult(candidate, expectedPeriod); err == nil {
					candidate.DetectionMethod = method
					candidate.EventLatencyMs = candidate.Timestamp.Sub(eventTime).Milliseconds()
					state.Latest = candidate
					if state.FirstHit == nil {
						state.FirstHit = cloneResult(candidate)
						state.Detected = true
						hits <- cloneResult(candidate)
					}
				} else {
					state.Latest = candidate
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func hasHeaderChange(b *Baseline, r *Result) bool {
	if b == nil || r == nil {
		return false
	}
	return changedHeader(b.ETag, r.ETag) || changedHeader(b.LastModified, r.LastModified)
}

func changedHeader(base, current string) bool {
	base = strings.TrimSpace(base)
	current = strings.TrimSpace(current)
	if base == "" && current == "" {
		return false
	}
	return base != current
}

func hasContentChange(b *Baseline, r *Result) bool {
	if b == nil || r == nil {
		return true
	}
	return b.Period != r.Period || b.Value != r.Value || changedHeader(b.ContentHash, r.ContentHash)
}

func detectionMethod(headersChanged, contentChanged bool) string {
	switch {
	case headersChanged && contentChanged:
		return "headers+content"
	case headersChanged:
		return "headers"
	default:
		return "content"
	}
}

func validateResult(r *Result, expectedPeriod string) error {
	if r == nil {
		return errors.New("nil result")
	}
	if !isOfficialBLSURL(r.URL) {
		return fmt.Errorf("non-official BLS URL: %s", r.URL)
	}
	if r.Period == "" || r.Value == "" {
		return errors.New("empty period or value")
	}
	if expectedPeriod != "" && r.Period != expectedPeriod {
		return fmt.Errorf("stale or wrong period: got %s expected %s", r.Period, expectedPeriod)
	}
	if strings.EqualFold(r.Value, "-") || strings.Contains(strings.ToLower(r.Value), "nan") {
		return errors.New("placeholder value")
	}
	if math.Abs(r.NumericValue) > 5 {
		return fmt.Errorf("impossible monthly value %.2f", r.NumericValue)
	}
	if r.Confidence == "LOW" {
		return errors.New("low confidence result")
	}
	return nil
}

func isOfficialBLSURL(url string) bool {
	return url == tableURL || url == summaryURL || url == pdfURL || url == apiURL || url == investingURL || url == scheduleURL
}

func mergeConfirmed(states map[string]*SourceResult, expectedPeriod, expectedReleaseDate string) (JSONResult, error) {
	var hits []*Result
	for _, name := range sourceOrder() {
		state := states[name]
		if state != nil && state.FirstHit != nil && state.FirstHit.Error == "" {
			hits = append(hits, state.FirstHit)
		}
	}
	if len(hits) == 0 {
		return JSONResult{}, errors.New("no source detected the expected period")
	}

	sort.Slice(hits, func(i, j int) bool {
		return hits[i].EventLatencyMs < hits[j].EventLatencyMs
	})

	for _, hit := range hits {
		if err := validateResult(hit, expectedPeriod); err != nil {
			return JSONResult{}, err
		}
	}

	var tableHit *Result
	for _, hit := range hits {
		if hit.Source == (tableScraper{}).Name() {
			tableHit = hit
			break
		}
	}
	if tableHit == nil {
		return JSONResult{}, errors.New("BLS Table 1 HTML did not detect the expected period")
	}
	if err := validateMetricPayload(tableHit, expectedPeriod, ""); err != nil {
		return JSONResult{}, err
	}
	for _, hit := range hits {
		if hit.Value != tableHit.Value {
			if hit.SourceType == string(SourceInvesting) {
				return JSONResult{}, fmt.Errorf("INVESTING_MISMATCH: %s core_cpi_mom BLS=%s Investing=%s", hit.Source, tableHit.Value, hit.Value)
			}
			return JSONResult{}, fmt.Errorf("official source disagreement: %s=%s Table1=%s", hit.Source, hit.Value, tableHit.Value)
		}
	}
	for _, hit := range hits {
		if hit.Source == tableHit.Source || len(hit.Metrics) == 0 {
			continue
		}
		if err := validateMetricPayload(hit, expectedPeriod, expectedReleaseDate); err != nil {
			return JSONResult{}, err
		}
		if err := compareMetricPayloads(tableHit, hit); err != nil {
			return JSONResult{}, err
		}
	}

	selected := tableHit
	primary := selected.Metrics[primaryMetricID]

	matched := make([]string, 0, len(hits))
	var warnings []string
	for _, hit := range hits {
		matched = append(matched, hit.Source)
		warnings = append(warnings, hit.Warnings...)
	}
	if len(hits) == 1 {
		warnings = append(warnings, "only one official value source detected during polling window")
	}

	confidence := selected.Confidence
	if selected.Source == (tableScraper{}).Name() && len(hits) >= 2 {
		confidence = "HIGH"
	}

	return JSONResult{
		Country:          country,
		EventName:        eventName,
		OfficialRelease:  officialRelease,
		SourceURL:        selected.URL,
		SourceType:       selected.SourceType,
		Table:            tableName,
		Index:            indexName,
		Field:            primary.Row,
		Period:           selected.Period,
		Actual:           primary.Actual,
		Unit:             unitPercent,
		ValueMethod:      primary.ValueMethod,
		Metrics:          orderedMetrics(selected.Metrics),
		Confidence:       confidence,
		ServerDateHeader: selected.ServerDate,
		ETag:             selected.ETag,
		LastModified:     selected.LastModified,
		CacheControl:     selected.CacheControl,
		LatencyMS:        selected.Latency,
		MatchedSources:   matched,
		Warnings:         uniqueStrings(warnings),
	}, nil
}

func validateMetricPayload(r *Result, expectedPeriod, expectedReleaseDate string) error {
	if r == nil {
		return errors.New("nil metric result")
	}
	if len(r.Metrics) == 0 {
		return fmt.Errorf("%s did not include CPI metric payload", r.Source)
	}
	for _, def := range table1Metrics {
		metric, ok := r.Metrics[def.ID]
		if !ok {
			return fmt.Errorf("%s missing metric %s", r.Source, def.ID)
		}
		if metric.Row != def.Row {
			return fmt.Errorf("metric %s row=%q expected %q", def.ID, metric.Row, def.Row)
		}
		if metric.Column != def.Column {
			return fmt.Errorf("metric %s column=%q expected %q", def.ID, metric.Column, def.Column)
		}
		if expectedPeriod != "" && metric.Period != expectedPeriod {
			return fmt.Errorf("metric %s period=%s expected %s", def.ID, metric.Period, expectedPeriod)
		}
		if metric.LatestReleaseDate != "" && expectedReleaseDate != "" && metric.LatestReleaseDate != expectedReleaseDate {
			if r.SourceType == string(SourceInvesting) {
				return fmt.Errorf("INVESTING_NOT_UPDATED: metric %s latest_release=%s expected=%s", def.ID, metric.LatestReleaseDate, expectedReleaseDate)
			}
			return fmt.Errorf("metric %s latest_release=%s expected %s", def.ID, metric.LatestReleaseDate, expectedReleaseDate)
		}
		if metric.Actual == "" || strings.EqualFold(metric.Actual, "-") || strings.Contains(strings.ToLower(metric.Actual), "nan") {
			return fmt.Errorf("metric %s has invalid value %q", def.ID, metric.Actual)
		}
	}
	return nil
}

func compareMetricPayloads(primary, confirmation *Result) error {
	for _, def := range table1Metrics {
		left := primary.Metrics[def.ID]
		right := confirmation.Metrics[def.ID]
		if left.Actual != right.Actual {
			if confirmation.SourceType == string(SourceInvesting) {
				return fmt.Errorf("INVESTING_MISMATCH: %s BLS=%s Investing=%s", def.ID, left.Actual, right.Actual)
			}
			return fmt.Errorf("metric disagreement: %s %s=%s %s=%s", def.ID, primary.Source, left.Actual, confirmation.Source, right.Actual)
		}
	}
	return nil
}

func orderedMetrics(metrics map[string]MetricValue) []MetricValue {
	out := make([]MetricValue, 0, len(table1Metrics))
	for _, def := range table1Metrics {
		if metric, ok := metrics[def.ID]; ok {
			out = append(out, metric)
		}
	}
	return out
}

func metricDefinition(id string) (MetricDefinition, bool) {
	for _, def := range table1Metrics {
		if def.ID == id {
			return def, true
		}
	}
	return MetricDefinition{}, false
}

func printPerformanceTable(states map[string]*SourceResult, eventTime time.Time) {
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("%-5s %-28s %-19s %-10s %-10s %-8s %-15s %-10s\n",
		"RANK", "SOURCE", "UPDATE TIME UTC", "LATENCY", "PERIOD", "VALUE", "METHOD", "STATUS")
	fmt.Println(strings.Repeat("-", 110))

	var hits []*Result
	for _, name := range sourceOrder() {
		if state := states[name]; state != nil && state.FirstHit != nil {
			hits = append(hits, state.FirstHit)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].EventLatencyMs < hits[j].EventLatencyMs
	})

	printed := map[string]bool{}
	for i, hit := range hits {
		printed[hit.Source] = true
		rank := fmt.Sprintf("%d", i+1)
		fmt.Printf("%-5s %-28s %-19s %-10s %-10s %-8s %-15s %-10s\n",
			rank, hit.Source, hit.Timestamp.UTC().Format("15:04:05.000"),
			formatLatency(hit.EventLatencyMs), prettyPeriod(hit.Period), hit.Value, hit.DetectionMethod, hit.Confidence)
	}
	for _, name := range sourceOrder() {
		if printed[name] {
			continue
		}
		status := "not detected"
		if state := states[name]; state != nil && state.Latest != nil && state.Latest.Error != "" {
			status = trimForConsole(state.Latest.Error, 34)
		}
		fmt.Printf("%-5s %-28s %-19s %-10s %-10s %-8s %-15s %-10s\n",
			"-", name, "-", "-", "-", "-", "-", status)
	}
	fmt.Println(strings.Repeat("=", 72))

	if len(hits) > 0 {
		winner := hits[0]
		fmt.Printf("Winner: %s\n", winner.Source)
		fmt.Printf("Updated Period: %s\n", prettyPeriod(winner.Period))
		fmt.Printf("Core CPI MoM: %s\n", winner.Value)
		if summary := metricConsoleSummary(winner.Metrics); summary != "" {
			fmt.Printf("Table 1 Metrics: %s\n", summary)
		}
		fmt.Printf("Detection Latency: %s from event time\n", formatLatency(winner.Timestamp.Sub(eventTime).Milliseconds()))
	}
	fmt.Println(strings.Repeat("=", 72))
}

func printInvestingConfirmationStatus(states map[string]*SourceResult, expectedPeriod, expectedReleaseDate string) {
	state := states[(investingScraper{}).Name()]
	if state == nil {
		return
	}
	if state.FirstHit == nil {
		if investingNotUpdated(state.Latest, expectedPeriod, expectedReleaseDate) {
			fmt.Println("INVESTING_NOT_UPDATED")
		}
		return
	}
	if investingNotUpdated(state.FirstHit, expectedPeriod, expectedReleaseDate) {
		fmt.Println("INVESTING_NOT_UPDATED")
	}
}

func investingNotUpdated(result *Result, expectedPeriod, expectedReleaseDate string) bool {
	if result == nil || result.SourceType != string(SourceInvesting) {
		return false
	}
	if expectedPeriod != "" && result.Period != "" && result.Period != expectedPeriod {
		return true
	}
	for _, metric := range result.Metrics {
		if expectedPeriod != "" && metric.Period != "" && metric.Period != expectedPeriod {
			return true
		}
		if expectedReleaseDate != "" && metric.LatestReleaseDate != "" && metric.LatestReleaseDate != expectedReleaseDate {
			return true
		}
	}
	return false
}

func metricConsoleSummary(metrics map[string]MetricValue) string {
	ordered := orderedMetrics(metrics)
	if len(ordered) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ordered))
	for _, metric := range ordered {
		part := fmt.Sprintf("%s=%s", metric.EventName, metric.Actual)
		if metric.Forecast != "" || metric.Previous != "" {
			var details []string
			if metric.Forecast != "" {
				details = append(details, "Forecast "+metric.Forecast)
			}
			if metric.Previous != "" {
				details = append(details, "Previous "+metric.Previous)
			}
			part += " (" + strings.Join(details, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}

func formatLatency(ms int64) string {
	sign := "+"
	if ms < 0 {
		sign = "-"
		ms = -ms
	}
	return fmt.Sprintf("%s%.3fs", sign, float64(ms)/1000)
}

func sourceOrder() []string {
	return []string{
		(tableScraper{}).Name(),
		(summaryScraper{}).Name(),
		(pdfScraper{}).Name(),
		(apiScraper{}).Name(),
		(investingScraper{}).Name(),
	}
}

func investingReleaseDate(eventTime time.Time) string {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.FixedZone("ET", -4*3600)
	}
	return eventTime.In(ny).Format("2006-01-02")
}

func validateConfiguredSchedule(client *http.Client, eventTime time.Time, logger *log.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheduleURL, nil)
	if err != nil {
		return "could not create schedule request"
	}
	setHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "BLS schedule validation unavailable: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "BLS schedule validation read failed"
	}
	schedule, err := parseReleaseSchedule(normalizeSpace(stripHTML(string(body))))
	if err != nil {
		return "BLS schedule sentence not parsed"
	}
	if !schedule.Equal(eventTime) {
		logger.Printf("BLS schedule release time=%s configured=%s", schedule.UTC().Format(time.RFC3339), eventTime.UTC().Format(time.RFC3339))
		return fmt.Sprintf("configured eventTimeUTC differs from BLS schedule (%s UTC)", schedule.UTC().Format("2006-01-02 15:04:05"))
	}
	return ""
}

func parseReleaseSchedule(text string) (time.Time, error) {
	re := regexp.MustCompile(`(?i)Consumer Price Index for\s+(January|February|March|April|May|June|July|August|September|October|November|December)\s+20\d{2}\s+is scheduled to be released on\s+(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2}),\s+(20\d{2}),\s+at\s+(\d{1,2}):(\d{2})\s+A\.?M\.?\s+Eastern Time`)
	match := re.FindStringSubmatch(text)
	if len(match) != 7 {
		return time.Time{}, errors.New("schedule sentence not found")
	}
	releaseMonth := monthNumber(match[2])
	releaseDay, _ := strconv.Atoi(match[3])
	releaseYear, _ := strconv.Atoi(match[4])
	releaseHour, _ := strconv.Atoi(match[5])
	releaseMinute, _ := strconv.Atoi(match[6])
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(releaseYear, time.Month(releaseMonth), releaseDay, releaseHour, releaseMinute, 0, 0, loc).UTC(), nil
}

func parseTable1HTML(body []byte) (ParsedValue, []string, error) {
	doc := string(body)
	plain := normalizeSpace(stripHTML(doc))
	lower := strings.ToLower(plain)
	if !strings.Contains(lower, "table 1") || !strings.Contains(lower, strings.ToLower(allItemsField)) || !strings.Contains(lower, strings.ToLower(targetField)) {
		return ParsedValue{}, nil, errors.New("table 1 or required CPI rows not found")
	}
	if !strings.Contains(lower, "unadjusted percent change") && !strings.Contains(lower, "12-month percent change") {
		return ParsedValue{}, nil, errors.New("unadjusted 12-month percent change header not found")
	}
	if !strings.Contains(lower, "seasonally adjusted percent change") {
		return ParsedValue{}, nil, errors.New("seasonally adjusted percent change header not found")
	}

	rows := htmlRows(doc)
	period := parseLatestPeriod(doc)
	if period == "" {
		return ParsedValue{}, nil, errors.New("latest release month not found in table")
	}

	metrics, err := parseTable1Metrics(rows, period)
	if err != nil {
		return ParsedValue{}, nil, err
	}
	primary, ok := metrics[primaryMetricID]
	if !ok {
		return ParsedValue{}, nil, fmt.Errorf("primary metric %q not found", primaryMetricID)
	}
	value := primary.NumericValue
	return ParsedValue{
		Period:      period,
		Value:       value,
		ValueString: formatPercent(value),
		Field:       targetField,
		Method:      "direct_table_value",
		Metrics:     metrics,
	}, nil, nil
}

func parseTable1Metrics(rows [][]string, period string) (map[string]MetricValue, error) {
	rowNumbers := make(map[string][]float64)
	for _, metric := range table1Metrics {
		if _, ok := rowNumbers[metric.Row]; ok {
			continue
		}
		numbers, err := table1RowNumbers(rows, metric.Row)
		if err != nil {
			return nil, err
		}
		rowNumbers[metric.Row] = numbers
	}

	metrics := make(map[string]MetricValue, len(table1Metrics))
	for _, def := range table1Metrics {
		numbers := rowNumbers[def.Row]
		if len(numbers) <= def.NumberIndex {
			return nil, fmt.Errorf("%s row had %d numeric cells, expected index %d", def.Row, len(numbers), def.NumberIndex)
		}
		value := round1(numbers[def.NumberIndex])
		metrics[def.ID] = MetricValue{
			ID:           def.ID,
			EventName:    def.EventName,
			Row:          def.Row,
			Column:       def.Column,
			Period:       period,
			Actual:       formatValueWithUnit(value),
			NumericValue: value,
			Unit:         unitPercent,
			ValueMethod:  "direct_table_value",
		}
	}
	return metrics, nil
}

func table1RowNumbers(rows [][]string, field string) ([]float64, error) {
	var matched [][]string
	for _, cells := range rows {
		if len(cells) > 0 && sameField(cells[0], field) {
			matched = append(matched, cells)
		}
	}
	if len(matched) != 1 {
		return nil, fmt.Errorf("ambiguous %q row count=%d", field, len(matched))
	}
	numbers := numbersFromCells(matched[0][1:])
	if len(numbers) < 9 {
		return nil, fmt.Errorf("%s row had %d numeric cells, expected at least 9", field, len(numbers))
	}
	return numbers, nil
}

func parseCPISummaryHTML(body []byte) (ParsedValue, []string, error) {
	text := normalizeSpace(stripHTML(string(body)))
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "consumer price index summary") {
		return ParsedValue{}, nil, errors.New("CPI summary title not found")
	}
	if !strings.Contains(lower, strings.ToLower(allItemsField)) || !strings.Contains(lower, strings.ToLower(targetField)) {
		return ParsedValue{}, nil, errors.New("required CPI summary rows not found")
	}

	period := parseLatestPeriod(text)
	if period == "" {
		return ParsedValue{}, nil, errors.New("latest release month not found in CPI summary")
	}

	metrics, warnings, err := parseSummaryMetrics(text, period)
	if err != nil {
		return ParsedValue{}, warnings, err
	}
	primary, ok := metrics[primaryMetricID]
	if !ok {
		return ParsedValue{}, warnings, fmt.Errorf("primary metric %q not found in CPI summary", primaryMetricID)
	}
	return ParsedValue{
		Period:      period,
		Value:       primary.NumericValue,
		ValueString: formatPercent(primary.NumericValue),
		Field:       targetField,
		Method:      primary.ValueMethod,
		Metrics:     metrics,
	}, warnings, nil
}

func parseSummaryMetrics(text, period string) (map[string]MetricValue, []string, error) {
	if metrics, err := parseSummaryTableAMetrics(text, period); err == nil {
		return metrics, nil, nil
	}
	metrics, err := parseSummaryNarrativeMetrics(text, period)
	if err != nil {
		return nil, nil, err
	}
	return metrics, []string{"CPI summary metrics parsed from narrative fallback"}, nil
}

func parseSummaryTableAMetrics(text, period string) (map[string]MetricValue, error) {
	tableText, ok := summaryTableASection(text)
	if !ok {
		return nil, errors.New("Table A section not found in CPI summary")
	}
	allItemsNumbers, err := summaryTableRowNumbers(tableText, allItemsField, "Food")
	if err != nil {
		return nil, err
	}
	coreNumbers, err := summaryTableRowNumbers(tableText, targetField, "Commodities less food and energy commodities")
	if err != nil {
		return nil, err
	}
	values := map[string]float64{
		"cpi_mom":      allItemsNumbers[len(allItemsNumbers)-2],
		"cpi_yoy":      allItemsNumbers[len(allItemsNumbers)-1],
		"core_cpi_mom": coreNumbers[len(coreNumbers)-2],
		"core_cpi_yoy": coreNumbers[len(coreNumbers)-1],
	}
	return metricPayloadFromValues(period, values, "direct_summary_table_a_value"), nil
}

func summaryTableASection(text string) (string, bool) {
	lower := strings.ToLower(text)
	start := strings.Index(lower, "table a. percent changes in cpi")
	if start < 0 {
		start = strings.Index(lower, "table a.")
	}
	if start < 0 {
		return "", false
	}
	section := text[start:]
	sectionLower := lower[start:]
	end := len(section)
	for _, marker := range []string{"footnotes", "note:", "food the index for food"} {
		if idx := strings.Index(sectionLower, marker); idx >= 0 && idx < end {
			end = idx
		}
	}
	return section[:end], true
}

func summaryTableRowNumbers(tableText, rowLabel, nextLabel string) ([]float64, error) {
	lower := strings.ToLower(tableText)
	start := strings.Index(lower, strings.ToLower(rowLabel))
	if start < 0 {
		return nil, fmt.Errorf("summary Table A row %q not found", rowLabel)
	}
	rest := tableText[start+len(rowLabel):]
	if nextLabel != "" {
		if next := strings.Index(strings.ToLower(rest), strings.ToLower(nextLabel)); next >= 0 {
			rest = rest[:next]
		}
	}
	numbers := numbersFromText(rest)
	if len(numbers) < 2 {
		return nil, fmt.Errorf("summary Table A row %q had %d numeric values, expected at least 2", rowLabel, len(numbers))
	}
	return numbers, nil
}

func parseSummaryNarrativeMetrics(text, period string) (map[string]MetricValue, error) {
	values, err := parseNarrativeMetricValues(text)
	if err != nil {
		return nil, err
	}
	return metricPayloadFromValues(period, values, "direct_summary_text_value"), nil
}

func parseNarrativeMetricValues(text string) (map[string]float64, error) {
	values := make(map[string]float64, len(table1Metrics))
	patterns := map[string][]summaryValuePattern{
		"cpi_mom": {
			{regexp.MustCompile(`(?i)\bConsumer Price Index for All Urban Consumers\s*\(CPI-U\)\s+(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+on a seasonally adjusted basis\s+in\s+[A-Za-z]+`), 1},
			{regexp.MustCompile(`(?i)\bConsumer Price Index for All Urban Consumers\s*\(CPI-U\)\s+(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+on a seasonally adjusted basis\s+in\s+[A-Za-z]+`), -1},
		},
		"cpi_yoy": {
			{regexp.MustCompile(`(?i)\ball items index\s+(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+before seasonal adjustment`), 1},
			{regexp.MustCompile(`(?i)\ball items index\s+(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+for the 12 months ending\s+[A-Za-z]+`), 1},
			{regexp.MustCompile(`(?i)\ball items index\s+(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+before seasonal adjustment`), -1},
			{regexp.MustCompile(`(?i)\ball items index\s+(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+for the 12 months ending\s+[A-Za-z]+`), -1},
		},
		"core_cpi_mom": {
			{regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy(?:\s+index)?\s+(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+in\s+[A-Za-z]+`), 1},
			{regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy(?:\s+index)?\s+(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+in\s+[A-Za-z]+`), -1},
		},
		"core_cpi_yoy": {
			{regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy(?:\s+index)?\s+(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+(?:over the year|over the past 12 months|for the 12 months ending\s+[A-Za-z]+)`), 1},
			{regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy(?:\s+index)?\s+(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+(?:over the year|over the past 12 months|for the 12 months ending\s+[A-Za-z]+)`), -1},
		},
	}
	for _, def := range table1Metrics {
		value, ok := parseSummaryValue(text, patterns[def.ID])
		if !ok {
			return nil, fmt.Errorf("CPI summary narrative metric %s not found", def.ID)
		}
		values[def.ID] = value
	}
	return values, nil
}

type summaryValuePattern struct {
	re   *regexp.Regexp
	sign float64
}

func parseSummaryValue(text string, patterns []summaryValuePattern) (float64, bool) {
	for _, pattern := range patterns {
		match := pattern.re.FindStringSubmatch(text)
		if len(match) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		return round1(value * pattern.sign), true
	}
	return 0, false
}

func metricPayloadFromValues(period string, values map[string]float64, method string) map[string]MetricValue {
	metrics := make(map[string]MetricValue, len(table1Metrics))
	for _, def := range table1Metrics {
		value := round1(values[def.ID])
		metrics[def.ID] = MetricValue{
			ID:           def.ID,
			EventName:    def.EventName,
			Row:          def.Row,
			Column:       def.Column,
			Period:       period,
			Actual:       formatValueWithUnit(value),
			NumericValue: value,
			Unit:         unitPercent,
			ValueMethod:  method,
		}
	}
	return metrics
}

func parseCPIPDF(body []byte) (ParsedValue, []string, error) {
	text, warnings := extractPDFText(body)
	text = normalizeSpace(text)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, strings.ToLower(allItemsField)) || !strings.Contains(lower, strings.ToLower(targetField)) {
		return ParsedValue{}, warnings, errors.New("required CPI rows not found in PDF text")
	}
	period := parseLatestPeriod(text)
	if period == "" {
		return ParsedValue{}, warnings, errors.New("latest release month not found in PDF text")
	}

	if metrics, err := parsePDFTable1Metrics(text, period); err == nil {
		primary := metrics[primaryMetricID]
		return ParsedValue{
			Period:      period,
			Value:       primary.NumericValue,
			ValueString: formatPercent(primary.NumericValue),
			Field:       targetField,
			Method:      primary.ValueMethod,
			Metrics:     metrics,
		}, warnings, nil
	}
	if metrics, err := parsePDFNarrativeMetrics(text, period); err == nil {
		warnings = append(warnings, "PDF metrics parsed from narrative fallback")
		primary := metrics[primaryMetricID]
		return ParsedValue{
			Period:      period,
			Value:       primary.NumericValue,
			ValueString: formatPercent(primary.NumericValue),
			Field:       targetField,
			Method:      primary.ValueMethod,
			Metrics:     metrics,
		}, warnings, nil
	}

	if value, err := parseTextTableRowValue(text); err == nil {
		return ParsedValue{
			Period:      period,
			Value:       value,
			ValueString: formatPercent(value),
			Field:       targetField,
			Method:      "direct_pdf_table_value",
		}, warnings, nil
	}
	if value, ok := parseSummarySentenceValue(text); ok {
		return ParsedValue{
			Period:      period,
			Value:       value,
			ValueString: formatPercent(value),
			Field:       targetField,
			Method:      "direct_pdf_text_value",
		}, warnings, nil
	}
	return ParsedValue{}, warnings, errors.New("PDF target monthly core value not found")
}

func parsePDFTable1Metrics(text, period string) (map[string]MetricValue, error) {
	allItemsNumbers, err := parsePDFTable1RowNumbers(text, allItemsField, 99, 101)
	if err != nil {
		return nil, err
	}
	coreNumbers, err := parsePDFTable1RowNumbers(text, targetField, 70, 90)
	if err != nil {
		return nil, err
	}
	values := map[string]float64{
		"cpi_mom":      allItemsNumbers[8],
		"cpi_yoy":      allItemsNumbers[4],
		"core_cpi_mom": coreNumbers[8],
		"core_cpi_yoy": coreNumbers[4],
	}
	return metricPayloadFromValues(period, values, "direct_pdf_table_value"), nil
}

func parsePDFTable1RowNumbers(text, field string, relMin, relMax float64) ([]float64, error) {
	targetRe := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(field))
	matches := targetRe.FindAllStringIndex(text, -1)
	var firstErr error
	for _, match := range matches {
		if field == allItemsField && hasLongerAllItemsLabelAt(text, match[0]) {
			continue
		}
		snippet := text[match[0]:]
		if len(snippet) > 700 {
			snippet = snippet[:700]
		}
		values, err := parsePDFTable1NumbersFromSnippet(snippet, relMin, relMax)
		if err == nil {
			return values, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("PDF Table 1 row %q not found", field)
}

func hasLongerAllItemsLabelAt(text string, start int) bool {
	tail := strings.ToLower(text[start:])
	for _, longer := range []string{
		"all items less",
		"all items index",
		"all items cpi",
	} {
		if strings.HasPrefix(tail, longer) {
			return true
		}
	}
	return false
}

func parsePDFTable1NumbersFromSnippet(snippet string, relMin, relMax float64) ([]float64, error) {
	values := numbersFromText(snippet)
	if len(values) < 9 {
		return nil, fmt.Errorf("PDF Table 1 row had %d numeric values, expected at least 9", len(values))
	}
	values = values[:9]
	if values[0] < relMin || values[0] > relMax || values[1] < 100 || values[2] < 100 || values[3] < 100 {
		return nil, errors.New("PDF Table 1 row did not match index-level structure")
	}
	return values, nil
}

func parsePDFNarrativeMetrics(text, period string) (map[string]MetricValue, error) {
	values, err := parseNarrativeMetricValues(text)
	if err != nil {
		return nil, err
	}
	return metricPayloadFromValues(period, values, "direct_pdf_text_value"), nil
}

func parseInvestingMetricPage(body []byte, investingDef InvestingMetricDefinition) (MetricValue, []string, error) {
	text := normalizeSpace(stripHTML(string(body)))
	if !investingTitleMatches(text, investingDef.ExpectedTitles) {
		return MetricValue{}, nil, fmt.Errorf("Investing.com title mismatch for %s", investingDef.MetricID)
	}

	latestReleaseDisplay, latestReleaseISO, err := parseInvestingLatestReleaseDate(text)
	if err != nil {
		return MetricValue{}, nil, err
	}
	cardText := investingLatestCardText(text)
	actual, numeric, err := parseInvestingCardValue(cardText, "Actual")
	if err != nil {
		return MetricValue{}, nil, err
	}
	forecast, _, err := parseInvestingCardValue(cardText, "Forecast")
	if err != nil {
		return MetricValue{}, nil, err
	}
	previous, _, err := parseInvestingCardValue(cardText, "Previous")
	if err != nil {
		return MetricValue{}, nil, err
	}

	rows := parseInvestingHistoricalRows(text)
	row, ok := investingHistoricalRowForRelease(rows, latestReleaseISO)
	if !ok {
		return MetricValue{}, nil, fmt.Errorf("Investing.com historical row for latest release %s not found", latestReleaseDisplay)
	}
	if row.Period == "" {
		return MetricValue{}, nil, fmt.Errorf("Investing.com historical row for latest release %s missing bracket period", latestReleaseDisplay)
	}

	var warnings []string
	if row.Actual != "" && row.Actual != actual {
		warnings = append(warnings, fmt.Sprintf("Investing.com %s top-card actual %s differs from history row actual %s", investingDef.MetricID, actual, row.Actual))
	}
	if row.Forecast != "" && row.Forecast != forecast {
		warnings = append(warnings, fmt.Sprintf("Investing.com %s top-card forecast %s differs from history row forecast %s", investingDef.MetricID, forecast, row.Forecast))
	}
	if row.Previous != "" && row.Previous != previous {
		warnings = append(warnings, fmt.Sprintf("Investing.com %s top-card previous %s differs from history row previous %s", investingDef.MetricID, previous, row.Previous))
	}

	tableDef, ok := metricDefinition(investingDef.MetricID)
	if !ok {
		return MetricValue{}, warnings, fmt.Errorf("unknown Investing.com metric %s", investingDef.MetricID)
	}
	metric := MetricValue{
		ID:                tableDef.ID,
		EventName:         tableDef.EventName,
		Row:               tableDef.Row,
		Column:            tableDef.Column,
		Period:            row.Period,
		Actual:            actual,
		NumericValue:      numeric,
		Unit:              unitPercent,
		ValueMethod:       "investing_html_confirmation",
		Forecast:          forecast,
		Previous:          previous,
		LatestReleaseDate: latestReleaseISO,
		SourceURL:         investingDef.URL,
		HistoricalRows:    rows,
	}
	return metric, warnings, nil
}

func investingTitleMatches(text string, expectedTitles []string) bool {
	normalizedText := normalizeComparableText(text)
	for _, title := range expectedTitles {
		if strings.Contains(normalizedText, normalizeComparableText(title)) {
			return true
		}
	}
	return false
}

func normalizeComparableText(s string) string {
	s = strings.ToLower(normalizeSpace(s))
	s = strings.ReplaceAll(s, "u.s.", "united states")
	s = strings.ReplaceAll(s, "us ", "united states ")
	return s
}

func parseInvestingLatestReleaseDate(text string) (string, string, error) {
	re := regexp.MustCompile(`(?i)\bLatest Release\s+([A-Za-z]{3,9}\s+\d{1,2},\s+20\d{2})\b`)
	match := re.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", "", errors.New("Investing.com latest release date not found")
	}
	iso, err := parseInvestingDateISO(match[1])
	if err != nil {
		return "", "", err
	}
	return match[1], iso, nil
}

func investingLatestCardText(text string) string {
	start := strings.Index(strings.ToLower(text), "latest release")
	if start < 0 {
		return text
	}
	card := text[start:]
	cardLower := strings.ToLower(card)
	end := len(card)
	for _, marker := range []string{"importance:", "release date time actual forecast previous"} {
		if idx := strings.Index(cardLower, marker); idx >= 0 && idx < end {
			end = idx
		}
	}
	return card[:end]
}

func parseInvestingCardValue(text, label string) (string, float64, error) {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(label) + `\s+([-+]?\d+(?:\.\d+)?%)`)
	match := re.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", 0, fmt.Errorf("Investing.com %s value not found", label)
	}
	value, ok := percentStringToFloat(match[1])
	if !ok {
		return "", 0, fmt.Errorf("Investing.com %s value parse failed: %s", label, match[1])
	}
	return formatValueWithUnit(value), value, nil
}

func parseInvestingHistoricalRows(text string) []InvestingHistoricalRow {
	headerRe := regexp.MustCompile(`(?i)\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Sept|Oct|Nov|Dec)[a-z]*\s+\d{1,2},\s+20\d{2}\s+\(([A-Za-z]{3,9})\)`)
	matches := headerRe.FindAllStringSubmatchIndex(text, -1)
	rows := make([]InvestingHistoricalRow, 0, len(matches))
	for i, match := range matches {
		start := match[0]
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		segment := text[start:end]
		header := text[match[0]:match[1]]
		dateText, bracket, ok := parseInvestingHistoryHeader(header)
		if !ok {
			continue
		}
		releaseISO, err := parseInvestingDateISO(dateText)
		if err != nil {
			continue
		}
		period := investingPeriodFromBracket(releaseISO, bracket)
		timeText := ""
		afterHeader := segment[len(header):]
		if timeMatch := regexp.MustCompile(`\b\d{1,2}:\d{2}\b`).FindStringIndex(afterHeader); len(timeMatch) == 2 {
			timeText = afterHeader[timeMatch[0]:timeMatch[1]]
			afterHeader = afterHeader[timeMatch[1]:]
		}
		values := percentStringsFromText(afterHeader)
		row := InvestingHistoricalRow{
			ReleaseDate: releaseISO,
			Time:        timeText,
			Period:      period,
		}
		switch {
		case len(values) >= 3:
			row.Actual = formatPercentString(values[0])
			row.Forecast = formatPercentString(values[1])
			row.Previous = formatPercentString(values[2])
		case len(values) == 2:
			row.Forecast = formatPercentString(values[0])
			row.Previous = formatPercentString(values[1])
		case len(values) == 1:
			row.Actual = formatPercentString(values[0])
		}
		rows = append(rows, row)
	}
	return rows
}

func parseInvestingHistoryHeader(header string) (string, string, bool) {
	re := regexp.MustCompile(`(?i)\b((?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Sept|Oct|Nov|Dec)[a-z]*\s+\d{1,2},\s+20\d{2})\s+\(([A-Za-z]{3,9})\)`)
	match := re.FindStringSubmatch(header)
	if len(match) != 3 {
		return "", "", false
	}
	return match[1], match[2], true
}

func investingHistoricalRowForRelease(rows []InvestingHistoricalRow, latestReleaseISO string) (InvestingHistoricalRow, bool) {
	for _, row := range rows {
		if row.ReleaseDate == latestReleaseISO {
			return row, true
		}
	}
	return InvestingHistoricalRow{}, false
}

func parseInvestingDateISO(s string) (string, error) {
	for _, layout := range []string{"Jan 2, 2006", "January 2, 2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("Investing.com date parse failed: %s", s)
}

func investingPeriodFromBracket(releaseDateISO, bracket string) string {
	parts := strings.Split(releaseDateISO, "-")
	if len(parts) != 3 {
		return ""
	}
	year, errY := strconv.Atoi(parts[0])
	releaseMonth, errM := strconv.Atoi(parts[1])
	periodMonth := monthNumber(bracket)
	if errY != nil || errM != nil || periodMonth == 0 {
		return ""
	}
	if periodMonth > releaseMonth {
		year--
	}
	return fmt.Sprintf("%04d-%02d", year, periodMonth)
}

func percentStringsFromText(text string) []string {
	re := regexp.MustCompile(`[-+]?\d+(?:\.\d+)?%`)
	return re.FindAllString(text, -1)
}

func formatPercentString(s string) string {
	value, ok := percentStringToFloat(s)
	if !ok {
		return strings.TrimSpace(s)
	}
	return formatValueWithUnit(value)
}

func percentStringToFloat(s string) (float64, bool) {
	value, ok := parseNumber(s)
	if !ok {
		return 0, false
	}
	return round1(value), true
}

func validateInvestingGroupPeriods(metrics map[string]MetricValue) error {
	period := ""
	releaseDate := ""
	for _, def := range table1Metrics {
		metric, ok := metrics[def.ID]
		if !ok {
			return fmt.Errorf("Investing.com missing metric %s", def.ID)
		}
		if period == "" {
			period = metric.Period
		} else if metric.Period != period {
			return fmt.Errorf("Investing.com period mismatch: %s=%s first=%s", def.ID, metric.Period, period)
		}
		if releaseDate == "" {
			releaseDate = metric.LatestReleaseDate
		} else if metric.LatestReleaseDate != releaseDate {
			return fmt.Errorf("Investing.com release date mismatch: %s=%s first=%s", def.ID, metric.LatestReleaseDate, releaseDate)
		}
	}
	return nil
}

func parseBLSAPI(body []byte) (ParsedValue, []string, error) {
	var payload blsAPIResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return ParsedValue{}, nil, fmt.Errorf("BLS API JSON parse failed: %w", err)
	}
	if payload.Status != "" && payload.Status != "REQUEST_SUCCEEDED" {
		return ParsedValue{}, nil, fmt.Errorf("BLS API status %q message=%s", payload.Status, string(payload.Messages))
	}
	if len(payload.Results.Series) != 1 {
		return ParsedValue{}, nil, fmt.Errorf("BLS API series count=%d", len(payload.Results.Series))
	}
	series := payload.Results.Series[0]
	if series.SeriesID != seriesID {
		return ParsedValue{}, nil, fmt.Errorf("BLS API seriesID=%q, expected %q", series.SeriesID, seriesID)
	}

	points := make([]apiPoint, 0, len(series.Data))
	var warnings []string
	for _, d := range series.Data {
		if !monthlyPeriod(d.Period) {
			continue
		}
		rawValue := strings.TrimSpace(d.Value)
		if rawValue == "" || rawValue == "-" {
			warnings = append(warnings, fmt.Sprintf("BLS API skipped missing value for %s %s", d.Year, d.Period))
			continue
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(rawValue, ",", ""), 64)
		if err != nil {
			return ParsedValue{}, warnings, fmt.Errorf("BLS API value parse failed for %s %s: %w", d.Year, d.Period, err)
		}
		period := apiPeriod(d.Year, d.Period)
		if period == "" {
			return ParsedValue{}, warnings, fmt.Errorf("BLS API invalid period year=%q period=%q", d.Year, d.Period)
		}
		points = append(points, apiPoint{Period: period, Value: value})
	}
	if len(points) < 2 {
		return ParsedValue{}, warnings, fmt.Errorf("BLS API monthly point count=%d", len(points))
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Period > points[j].Period })
	current, previous := points[0], points[1]
	if previousMonth(current.Period) != previous.Period {
		return ParsedValue{}, warnings, fmt.Errorf("BLS API latest periods are not consecutive: current=%s previous=%s", current.Period, previous.Period)
	}
	if previous.Value == 0 {
		return ParsedValue{}, warnings, errors.New("BLS API previous index value is zero")
	}
	mom := round1(((current.Value / previous.Value) - 1) * 100)
	return ParsedValue{
		Period:      current.Period,
		Value:       mom,
		ValueString: formatPercent(mom),
		Field:       targetField,
		Method:      "calculated_index_change",
	}, warnings, nil
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

func numbersFromText(s string) []float64 {
	clean := strings.ReplaceAll(s, "\u2212", "-")
	re := regexp.MustCompile(`[-+]?\d[\d,]*(?:\.\d+)?`)
	raw := re.FindAllString(clean, -1)
	numbers := make([]float64, 0, len(raw))
	for _, item := range raw {
		value, err := strconv.ParseFloat(strings.ReplaceAll(item, ",", ""), 64)
		if err == nil {
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

func sameField(label, expected string) bool {
	normalized := strings.ToLower(normalizeSpace(label))
	normalized = regexp.MustCompile(`\s*\(\d+\)\s*$`).ReplaceAllString(normalized, "")
	return strings.TrimSpace(normalized) == strings.ToLower(expected)
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

func parseLatestPeriod(s string) string {
	if period := parseMResultsPeriod(s); period != "" {
		return period
	}
	if period := parseTableCaptionPeriod(s); period != "" {
		return period
	}
	if period := parseCPIReleaseHeaderPeriod(s); period != "" {
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
	re := regexp.MustCompile(`(?i),\s*(January|February|March|April|May|June|July|August|September|October|November|December)\s+(20\d{2})\s*\[`)
	match := re.FindStringSubmatch(s)
	if len(match) == 3 {
		return fmt.Sprintf("%s-%02d", match[2], monthNumber(match[1]))
	}
	return ""
}

func parseCPIReleaseHeaderPeriod(s string) string {
	re := regexp.MustCompile(`(?i)\bCONSUMER PRICE INDEX\s*-\s*(January|February|March|April|May|June|July|August|September|October|November|December)\s+(20\d{2})\b`)
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

func parseSummarySentenceValue(text string) (float64, bool) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy\b[^.]{0,180}?\b(?:increased|rose|advanced|gained)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+in\s+[A-Za-z]+(?:\s+\(SA\))?`),
		regexp.MustCompile(`(?i)\b(?:index for\s+)?all items less food and energy\b[^.]{0,180}?\b(?:declined|fell|decreased)\s+([-+]?\d+(?:\.\d+)?)\s+percent\s+in\s+[A-Za-z]+(?:\s+\(SA\))?`),
	}
	for i, re := range patterns {
		match := re.FindStringSubmatch(text)
		if len(match) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		if i == 1 {
			value = -value
		}
		return round1(value), true
	}
	return 0, false
}

func parseTextTableRowValue(text string) (float64, error) {
	targetRe := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(targetField))
	matches := targetRe.FindAllStringIndex(text, -1)
	var firstErr error
	for _, match := range matches {
		snippet := text[match[0]:]
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		value, err := parseTable1NumbersFromSnippet(snippet)
		if err == nil {
			return value, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return 0, errors.New("target row not found in text table")
}

func parseTable1NumbersFromSnippet(snippet string) (float64, error) {
	re := regexp.MustCompile(`[-+]?\d[\d,]*(?:\.\d+)?`)
	raw := re.FindAllString(snippet, -1)
	if len(raw) < 9 {
		return 0, fmt.Errorf("target text table row had %d numeric values, expected at least 9", len(raw))
	}
	values := make([]float64, 0, len(raw))
	for _, item := range raw {
		value, err := strconv.ParseFloat(strings.ReplaceAll(item, ",", ""), 64)
		if err != nil {
			return 0, err
		}
		values = append(values, value)
	}
	if values[0] < 70 || values[0] > 90 || values[1] < 100 || values[2] < 100 || values[3] < 100 {
		return 0, errors.New("target text table row did not match Table 1 index-level structure")
	}
	return round1(values[8]), nil
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
		dict, data := string(stream[1]), stream[2]
		if strings.Contains(dict, "/FlateDecode") {
			reader, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				warnings = append(warnings, "PDF FlateDecode stream could not be decompressed")
				continue
			}
			decoded, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				warnings = append(warnings, "PDF FlateDecode stream read failed")
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
		if decoded, err := hex.DecodeString(strings.Join(strings.Fields(match[1]), "")); err == nil {
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

func monthlyPeriod(period string) bool {
	if len(period) != 3 || period[0] != 'M' {
		return false
	}
	month, err := strconv.Atoi(period[1:])
	return err == nil && month >= 1 && month <= 12
}

func apiPeriod(year, period string) string {
	if !monthlyPeriod(period) {
		return ""
	}
	y, err := strconv.Atoi(year)
	if err != nil || y < 1900 || y > 2200 {
		return ""
	}
	return fmt.Sprintf("%04d-%s", y, period[1:])
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

func monthNumber(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "january", "jan":
		return 1
	case "february", "feb":
		return 2
	case "march", "mar":
		return 3
	case "april", "apr":
		return 4
	case "may":
		return 5
	case "june", "jun":
		return 6
	case "july", "jul":
		return 7
	case "august", "aug":
		return 8
	case "september", "sep", "sept":
		return 9
	case "october", "oct":
		return 10
	case "november", "nov":
		return 11
	case "december", "dec":
		return 12
	default:
		return 0
	}
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

func round1(v float64) float64 {
	return math.Round(v*10) / 10
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

func cloneResult(r *Result) *Result {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Warnings = append([]string(nil), r.Warnings...)
	cp.Metrics = cloneMetrics(r.Metrics)
	return &cp
}

func cloneMetrics(metrics map[string]MetricValue) map[string]MetricValue {
	if len(metrics) == 0 {
		return nil
	}
	cp := make(map[string]MetricValue, len(metrics))
	for key, value := range metrics {
		value.HistoricalRows = append([]InvestingHistoricalRow(nil), value.HistoricalRows...)
		cp[key] = value
	}
	return cp
}
