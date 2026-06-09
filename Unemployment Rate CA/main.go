package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
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
)

// Event configuration.
//
// Next Canada Labour Force Survey release:
// July 10, 2026 at 18:00 IST / 12:30 UTC.
//
// Update these two values for the next event.
const (
	eventName             = "Canada Unemployment Rate"
	dataName              = "Unemployment Rate"
	country               = "CA"
	eventTimeUTCString    = "2026-07-10 12:30:00"
	expectedReleasePeriod = "2026-06"

	// Latest known LFS article used only for startup/current-data display before
	// the next target article exists.
	latestKnownDailyArticleURL = "https://www150.statcan.gc.ca/n1/daily-quotidien/260605/dq260605a-eng.htm"

	statCanDailyBaseURL  = "https://www150.statcan.gc.ca/n1/daily-quotidien/"
	statCanTableURL      = "https://www150.statcan.gc.ca/t1/tbl1/en/tv.action?pid=1410028701"
	statCanDeveloperWDS  = "https://www.statcan.gc.ca/en/developers/wds"
	wdsVectorDataURL     = "https://www150.statcan.gc.ca/t1/wds/rest/getDataFromVectorsAndLatestNPeriods"
	wdsCoordDataURL      = "https://www150.statcan.gc.ca/t1/wds/rest/getDataFromCubePidCoordAndLatestNPeriods"
	wdsSeriesInfoURL     = "https://www150.statcan.gc.ca/t1/wds/rest/getSeriesInfoFromCubePidCoord"
	statCanProductID     = 14100287
	statCanVectorID      = 2062815
	statCanCoordinate    = "1.7.1.1.1.1.0.0.0.0"
	expectedSeriesTitle  = "Canada;Unemployment rate;Total - Gender;15 years and over;Estimate;Seasonally adjusted"
	latestPeriodsToFetch = 2

	testConnectionLead = 1 * time.Minute
	sniperLead         = 2 * time.Second
	pollWindow         = 3 * time.Minute
	pollEvery          = 500 * time.Millisecond
	requestTimeout     = 5 * time.Second

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0 Safari/537.36"
)

var (
	primarySource = Source{
		Name:        "Source 1 - Statistics Canada The Daily LFS article",
		URL:         statCanDailyBaseURL,
		SourceType:  "HTML",
		Kind:        "daily-article",
		Priority:    1,
		ValueMethod: "Direct sentence parse",
	}
	tableVectorSource = Source{
		Name:        "Source 2 - Statistics Canada Table 14-10-0287-01 vector",
		URL:         statCanTableURL,
		SourceType:  "Official WDS JSON",
		Kind:        "wds-vector",
		Priority:    2,
		ValueMethod: "Direct official table value",
	}
	wdsCoordinateSource = Source{
		Name:        "Source 3 - Statistics Canada WDS coordinate backup",
		URL:         statCanDeveloperWDS,
		SourceType:  "Official WDS JSON",
		Kind:        "wds-coordinate",
		Priority:    3,
		ValueMethod: "WDS product-coordinate latest-period parse",
	}

	scriptStyleRE      = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	breakTagRE         = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockTagRE         = regexp.MustCompile(`(?is)</?(p|div|section|article|header|footer|main|h[1-6]|li|tr|table|thead|tbody|tfoot|caption)\b[^>]*>`)
	tagRE              = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE            = regexp.MustCompile(`\s+`)
	h1RE               = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	releasedRE         = regexp.MustCompile(`(?i)\bReleased:\s*(\d{4}-\d{2}-\d{2})\b`)
	lfsTitleRE         = regexp.MustCompile(`(?i)\bLabou?r Force Survey\b`)
	titlePeriodRE      = regexp.MustCompile(`(?i)\bLabou?r Force Survey,\s*([A-Za-z]+)\s+(\d{4})\b`)
	metricBlockRE      = regexp.MustCompile(`(?i)\bUnemployment rate\s*[-]\s*Canada\s+([0-9]+\.[0-9])\s*%\s+([A-Za-z]+)\s+(\d{4})\b`)
	directSentenceRE   = regexp.MustCompile(`(?i)\bunemployment rate\s+(?:increased|rose|was|remained|declined|fell)\b.{0,220}?\b(?:to|at)\s+([0-9]+\.[0-9])\s*%`)
	sentenceBoundaryRE = regexp.MustCompile(`(?s).*?([^.?!]*\bunemployment rate\s+(?:increased|rose|was|remained|declined|fell)\b.{0,220}?\b(?:to|at)\s+[0-9]+\.[0-9]\s*%[^.?!]*[.?!])`)
	anchorRE           = regexp.MustCompile(`(?is)<a\s+[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	yyyymmRE           = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`)
	monthYearRE        = regexp.MustCompile(`(?i)^([A-Za-z]+)\s+(\d{4})$`)
)

type Source struct {
	Name        string
	URL         string
	SourceType  string
	Kind        string
	Priority    int
	ValueMethod string
}

type HeaderState struct {
	Source       string
	URL          string
	Status       string
	StatusCode   int
	ETag         string
	LastModified string
	ServerDate   string
	Timestamp    time.Time
	Latency      time.Duration
	Error        string
}

type Result struct {
	Source         string
	SourceType     string
	URL            string
	FetchURL       string
	Period         string
	PeriodYYYYMM   string
	Value          string
	ValuePercent   float64
	Previous       string
	PreviousPeriod string
	Timestamp      time.Time
	Latency        time.Duration
	ETag           string
	LastModified   string
	Method         string
	Detail         string
	Sentence       string
	ReleasedDate   string
}

type ParsedValue struct {
	Source            Source
	Method            string
	Field             string
	Seasonality       string
	ValuePercent      float64
	Value             string
	MetricValue       string
	MetricValueSet    bool
	Period            string
	PeriodYYYYMM      string
	ReleasedDate      string
	ArticleURL        string
	Sentence          string
	Confidence        string
	Warnings          []string
	ValidationFailure string
}

type Baseline struct {
	Header HeaderState
	Result *Result
	Error  string
}

type Snapshot struct {
	Source string
	Result *Result
	Header HeaderState
	Error  string
}

type Detection struct {
	Result           *Result
	Method           string
	PollCount        int
	DetectedAt       time.Time
	LatencyFromEvent time.Duration
	Error            string
}

type Scraper interface {
	Name() string
	URL() string
	Priority() int
	ContentEvery() int
	FetchHeaders(context.Context, *http.Client) HeaderState
	FetchCurrent(context.Context, *http.Client) (*Result, error)
	FetchTarget(context.Context, *http.Client) (*Result, error)
}

type DailyScraper struct {
	source       Source
	targetURL    string
	currentURL   string
	contentEvery int
}

type VectorScraper struct {
	source Source
}

type CoordinateScraper struct {
	source Source
}

type wdsDataResponse struct {
	Status string `json:"status"`
	Object struct {
		ResponseStatusCode int    `json:"responseStatusCode"`
		ProductID          int    `json:"productId"`
		Coordinate         string `json:"coordinate"`
		VectorID           int    `json:"vectorId"`
		VectorDataPoint    []struct {
			RefPer            string  `json:"refPer"`
			RefPerRaw         string  `json:"refPerRaw"`
			Value             float64 `json:"value"`
			Decimals          int     `json:"decimals"`
			ScalarFactorCode  int     `json:"scalarFactorCode"`
			SymbolCode        int     `json:"symbolCode"`
			StatusCode        int     `json:"statusCode"`
			SecurityLevelCode int     `json:"securityLevelCode"`
			ReleaseTime       string  `json:"releaseTime"`
			FrequencyCode     int     `json:"frequencyCode"`
		} `json:"vectorDataPoint"`
	} `json:"object"`
}

type wdsSeriesInfoResponse struct {
	Status string `json:"status"`
	Object struct {
		ResponseStatusCode int    `json:"responseStatusCode"`
		ProductID          int    `json:"productId"`
		Coordinate         string `json:"coordinate"`
		VectorID           int    `json:"vectorId"`
		FrequencyCode      int    `json:"frequencyCode"`
		ScalarFactorCode   int    `json:"scalarFactorCode"`
		Decimals           int    `json:"decimals"`
		Terminated         int    `json:"terminated"`
		SeriesTitleEn      string `json:"SeriesTitleEn"`
		MemberUomCode      int    `json:"memberUomCode"`
	} `json:"object"`
}

type wdsObservation struct {
	PeriodYYYYMM  string
	PeriodDisplay string
	Value         float64
	ValueDisplay  string
	ReleaseTime   string
	Decimals      int
}

func main() {
	eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTCString, time.UTC)
	if err != nil {
		fmt.Printf("Configuration error: invalid event time: %v\n", err)
		os.Exit(1)
	}
	expectedPeriod, err := normalizeExpectedPeriod(expectedReleasePeriod)
	if err != nil {
		fmt.Printf("Configuration error: invalid expected period: %v\n", err)
		os.Exit(1)
	}

	client := newHTTPClient()
	scrapers := buildScrapers(eventTime)

	printHeader(eventTime)
	fmt.Println("Fetching current published data from all official sources...")
	snapshots := fetchCurrentSnapshots(client, scrapers)
	printCurrentSnapshots(snapshots)
	fmt.Println("Current data captured. Waiting for new release.")

	now := time.Now().UTC()
	testTime := eventTime.Add(-testConnectionLead)
	sniperStart := eventTime.Add(-sniperLead)
	endTime := eventTime.Add(pollWindow)

	if now.After(endTime) {
		fmt.Println("Event polling window is already over. Showing current snapshots only.")
		printFinalTable(eventTime, scrapers, nil)
		return
	}

	if now.Before(testTime) {
		fmt.Printf("Countdown to test connection: %s\n", testTime.Sub(now).Round(time.Second))
		countdownUntil(testTime, time.Second, "Time remaining to connection test")
	}

	fmt.Println()
	fmt.Println("Testing connections 1 minute before event...")
	fmt.Println("Capturing baseline headers and content for hybrid detection...")
	baselines := captureBaselines(client, scrapers)
	printBaselines(scrapers, baselines)

	now = time.Now().UTC()
	if now.Before(sniperStart) {
		fmt.Printf("Final countdown to sniper mode: %s\n", sniperStart.Sub(now).Round(time.Millisecond))
		countdownUntil(sniperStart, 100*time.Millisecond, "Starting sniper mode in")
	}

	ctx, cancel := context.WithDeadline(context.Background(), endTime)
	defer cancel()

	fmt.Println()
	fmt.Println("SNIPER MODE ACTIVE")
	fmt.Println("Using hybrid detection: headers every 500ms, content by source cadence.")

	resultsCh := make(chan Detection, len(scrapers))
	var wg sync.WaitGroup
	for _, scraper := range scrapers {
		wg.Add(1)
		go func(s Scraper) {
			defer wg.Done()
			pollScraper(ctx, client, s, baselines[s.Name()], expectedPeriod, eventTime, resultsCh)
		}(scraper)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	detections := make([]Detection, 0, len(scrapers))
	for detection := range resultsCh {
		if detection.Error != "" {
			continue
		}
		detections = append(detections, detection)
		fmt.Printf("[%s] UPDATED! [%s] Period: %s | Value: %s (Detected by: %s)\n",
			detection.DetectedAt.Format("15:04:05.000"),
			detection.Result.Source,
			detection.Result.Period,
			detection.Result.Value,
			detection.Method)
	}

	fmt.Println()
	fmt.Printf("Polling window complete (%s elapsed)\n", pollWindow)
	printFinalTable(eventTime, scrapers, detections)
	if len(detections) == 0 {
		fmt.Println("No official source detected the expected release during the polling window.")
		return
	}
	sortDetections(detections)
	winner := detections[0]
	fmt.Printf("Winner: %s\n", winner.Result.Source)
	fmt.Printf("Updated Period: %s\n", winner.Result.Period)
	fmt.Printf("%s: %s\n", dataName, winner.Result.Value)
	fmt.Printf("Detection Latency: %s from event time\n", formatLatency(winner.LatencyFromEvent))
	fmt.Printf("Value Method: %s\n", winner.Result.Method)
	fmt.Printf("Source URL: %s\n", winner.Result.FetchURL)
}

func buildScrapers(eventTime time.Time) []Scraper {
	targetURL := dailyArticleCandidateURLs(eventTime)[0]
	scrapers := []Scraper{
		DailyScraper{
			source:       primarySource,
			targetURL:    targetURL,
			currentURL:   latestKnownDailyArticleURL,
			contentEvery: 5,
		},
		VectorScraper{source: tableVectorSource},
		CoordinateScraper{source: wdsCoordinateSource},
	}
	sort.Slice(scrapers, func(i, j int) bool {
		return scrapers[i].Priority() < scrapers[j].Priority()
	})
	return scrapers
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   1500 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   1500 * time.Millisecond,
			ExpectContinueTimeout: 500 * time.Millisecond,
		},
		Timeout: requestTimeout,
	}
}

func fetchCurrentSnapshots(client *http.Client, scrapers []Scraper) []Snapshot {
	out := make(chan Snapshot, len(scrapers))
	for _, scraper := range scrapers {
		go func(s Scraper) {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			header := s.FetchHeaders(ctx, client)
			result, err := s.FetchCurrent(ctx, client)
			snapshot := Snapshot{Source: s.Name(), Header: header, Result: result}
			if err != nil {
				snapshot.Error = err.Error()
			}
			out <- snapshot
		}(scraper)
	}

	snapshots := make([]Snapshot, 0, len(scrapers))
	for range scrapers {
		snapshots = append(snapshots, <-out)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return sourcePriority(scrapers, snapshots[i].Source) < sourcePriority(scrapers, snapshots[j].Source)
	})
	return snapshots
}

func captureBaselines(client *http.Client, scrapers []Scraper) map[string]Baseline {
	out := make(chan Snapshot, len(scrapers))
	for _, scraper := range scrapers {
		go func(s Scraper) {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			header := s.FetchHeaders(ctx, client)
			result, err := s.FetchCurrent(ctx, client)
			snapshot := Snapshot{Source: s.Name(), Header: header, Result: result}
			if err != nil {
				snapshot.Error = err.Error()
			}
			out <- snapshot
		}(scraper)
	}

	baselines := make(map[string]Baseline, len(scrapers))
	for range scrapers {
		snapshot := <-out
		baselines[snapshot.Source] = Baseline{
			Header: snapshot.Header,
			Result: snapshot.Result,
			Error:  snapshot.Error,
		}
	}
	return baselines
}

func pollScraper(ctx context.Context, client *http.Client, scraper Scraper, baseline Baseline, expectedPeriod string, eventTime time.Time, out chan<- Detection) {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	pollCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pollCount++
		header := scraper.FetchHeaders(ctx, client)
		if header.Error == "" && headerIndicatesUpdate(baseline.Header, header) {
			if result, err := scraper.FetchTarget(ctx, client); err == nil && isValidUpdate(result, baseline, expectedPeriod) {
				out <- Detection{
					Result:           result,
					Method:           "headers",
					PollCount:        pollCount,
					DetectedAt:       time.Now().UTC(),
					LatencyFromEvent: time.Now().UTC().Sub(eventTime),
				}
				return
			}
		}

		if pollCount%scraper.ContentEvery() == 0 {
			if result, err := scraper.FetchTarget(ctx, client); err == nil && isValidUpdate(result, baseline, expectedPeriod) {
				out <- Detection{
					Result:           result,
					Method:           "content",
					PollCount:        pollCount,
					DetectedAt:       time.Now().UTC(),
					LatencyFromEvent: time.Now().UTC().Sub(eventTime),
				}
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func headerIndicatesUpdate(base HeaderState, current HeaderState) bool {
	if current.StatusCode == 0 {
		return false
	}
	if base.StatusCode == 0 && current.StatusCode == http.StatusOK {
		return true
	}
	if base.StatusCode != 0 && current.StatusCode != base.StatusCode {
		return true
	}
	if base.ETag != "" && current.ETag != "" && current.ETag != base.ETag {
		return true
	}
	if base.LastModified != "" && current.LastModified != "" && current.LastModified != base.LastModified {
		return true
	}
	return false
}

func isValidUpdate(result *Result, baseline Baseline, expectedPeriod string) bool {
	if result == nil || result.PeriodYYYYMM != expectedPeriod {
		return false
	}
	if result.ValuePercent < 0 || result.ValuePercent > 30 {
		return false
	}
	if baseline.Result == nil {
		return true
	}
	return result.PeriodYYYYMM != baseline.Result.PeriodYYYYMM || result.Value != baseline.Result.Value
}

func printHeader(eventTime time.Time) {
	ist := eventTime.In(time.FixedZone("IST", 5*60*60+30*60))
	fmt.Println("CANADA UNEMPLOYMENT RATE SCRAPER - SNIPER MODE")
	fmt.Printf("Event Time (IST): %s\n", ist.Format("2006-01-02 15:04:05"))
	fmt.Printf("Event Time (UTC): %s\n", eventTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected Period: %s\n", periodDisplay(expectedReleasePeriod))
	fmt.Println("FIELD=National unemployment rate")
	fmt.Println("SEASONALITY=Seasonally adjusted")
	fmt.Println("GEOGRAPHY=Canada")
	fmt.Println("SEX=Both sexes")
	fmt.Println("AGE_GROUP=15 years and over")
	fmt.Println("STATISTIC=Unemployment rate")
	fmt.Println()
	fmt.Println("Sources:")
	fmt.Printf("1. %s | %s | %s\n", primarySource.Name, primarySource.SourceType, primarySource.ValueMethod)
	fmt.Printf("2. %s | %s | %s\n", tableVectorSource.Name, tableVectorSource.SourceType, tableVectorSource.ValueMethod)
	fmt.Printf("3. %s | %s | %s\n", wdsCoordinateSource.Name, wdsCoordinateSource.SourceType, wdsCoordinateSource.ValueMethod)
	fmt.Println()
}

func printCurrentSnapshots(snapshots []Snapshot) {
	for _, snapshot := range snapshots {
		if snapshot.Error != "" {
			fmt.Printf("  WAIT [%-58s] %s\n", snapshot.Source, shortError(snapshot.Error))
			fmt.Println("       Value: not scraped yet")
			continue
		}
		fmt.Printf("  OK   [%-58s] Period: %-12s Value: %-6s Method: %s\n",
			snapshot.Source,
			snapshot.Result.Period,
			snapshot.Result.Value,
			snapshot.Result.Method)
	}
}

func printBaselines(scrapers []Scraper, baselines map[string]Baseline) {
	for _, scraper := range scrapers {
		baseline := baselines[scraper.Name()]
		if baseline.Error != "" {
			fmt.Printf("  WAIT [%s] %s\n", scraper.Name(), shortError(baseline.Error))
			continue
		}
		headerStatus := baseline.Header.Status
		if headerStatus == "" {
			headerStatus = "N/A"
		}
		fmt.Printf("  OK   [%s] Period: %s | Value: %s | Header: %s | ETag: %s\n",
			scraper.Name(),
			blankNA(resultPeriod(baseline.Result)),
			blankNA(resultValue(baseline.Result)),
			headerStatus,
			blankNA(baseline.Header.ETag))
	}
}

func printFinalTable(eventTime time.Time, scrapers []Scraper, detections []Detection) {
	sortDetections(detections)
	fmt.Println()
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Printf("%-6s %-58s %-18s %-14s %-10s %-10s\n", "RANK", "SOURCE", "UPDATE UTC", "LATENCY", "VALUE", "METHOD")
	detected := make(map[string]Detection, len(detections))
	for _, detection := range detections {
		detected[detection.Result.Source] = detection
	}
	for i, detection := range detections {
		fmt.Printf("%-6s %-58s %-18s %-14s %-10s %-10s\n",
			fmt.Sprintf("#%d", i+1),
			detection.Result.Source,
			detection.DetectedAt.Format("15:04:05.000"),
			formatLatency(detection.LatencyFromEvent),
			detection.Result.Value,
			detection.Method)
	}
	for _, scraper := range scrapers {
		if _, ok := detected[scraper.Name()]; ok {
			continue
		}
		fmt.Printf("%-6s %-58s %-18s %-14s %-10s %-10s\n", "-", scraper.Name(), "-", "Pending", "Pending", "-")
	}
	fmt.Printf("Event UTC: %s\n", eventTime.Format("15:04:05.000"))
}

func sortDetections(detections []Detection) {
	sort.Slice(detections, func(i, j int) bool {
		if detections[i].DetectedAt.Equal(detections[j].DetectedAt) {
			return detections[i].Result.Source < detections[j].Result.Source
		}
		return detections[i].DetectedAt.Before(detections[j].DetectedAt)
	})
}

func sourcePriority(scrapers []Scraper, source string) int {
	for _, scraper := range scrapers {
		if scraper.Name() == source {
			return scraper.Priority()
		}
	}
	return 999
}

func countdownUntil(target time.Time, step time.Duration, label string) {
	for {
		now := time.Now().UTC()
		if !now.Before(target) {
			fmt.Println()
			return
		}
		remaining := target.Sub(now)
		fmt.Printf("\r%s: %s   ", label, formatCountdown(remaining))
		sleepFor := step
		if remaining < sleepFor {
			sleepFor = remaining
		}
		time.Sleep(sleepFor)
	}
}

func formatCountdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
	total := int(d.Round(time.Second).Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatLatency(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return fmt.Sprintf("%s%.3fs", sign, float64(d.Microseconds())/1000000)
}

func resultPeriod(result *Result) string {
	if result == nil {
		return ""
	}
	return result.Period
}

func resultValue(result *Result) string {
	if result == nil {
		return ""
	}
	return result.Value
}

func blankNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/A"
	}
	return s
}

func shortError(s string) string {
	const max = 180
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (s DailyScraper) Name() string      { return s.source.Name }
func (s DailyScraper) URL() string       { return s.targetURL }
func (s DailyScraper) Priority() int     { return s.source.Priority }
func (s DailyScraper) ContentEvery() int { return s.contentEvery }
func (s DailyScraper) FetchHeaders(ctx context.Context, client *http.Client) HeaderState {
	return fetchHeaders(ctx, client, s.Name(), s.targetURL)
}
func (s DailyScraper) FetchCurrent(ctx context.Context, client *http.Client) (*Result, error) {
	result, err := s.fetchArticle(ctx, client, s.targetURL)
	if err == nil {
		return result, nil
	}
	if s.currentURL == "" || s.currentURL == s.targetURL {
		return nil, err
	}
	return s.fetchArticle(ctx, client, s.currentURL)
}
func (s DailyScraper) FetchTarget(ctx context.Context, client *http.Client) (*Result, error) {
	return s.fetchArticle(ctx, client, s.targetURL)
}
func (s DailyScraper) fetchArticle(ctx context.Context, client *http.Client, url string) (*Result, error) {
	body, meta, err := fetchBytes(ctx, client, http.MethodGet, url, nil, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if err != nil {
		return nil, err
	}
	parsed, err := parseDailyArticle(s.source, body, url)
	if err != nil {
		return nil, err
	}
	return &Result{
		Source:       s.Name(),
		SourceType:   s.source.SourceType,
		URL:          s.source.URL,
		FetchURL:     url,
		Period:       parsed.Period,
		PeriodYYYYMM: parsed.PeriodYYYYMM,
		Value:        parsed.Value,
		ValuePercent: parsed.ValuePercent,
		Timestamp:    time.Now().UTC(),
		Latency:      meta.Latency,
		ETag:         meta.ETag,
		LastModified: meta.LastModified,
		Method:       parsed.Method,
		Detail:       "Statistics Canada The Daily Labour Force Survey article",
		Sentence:     parsed.Sentence,
		ReleasedDate: parsed.ReleasedDate,
	}, nil
}

func (s VectorScraper) Name() string      { return s.source.Name }
func (s VectorScraper) URL() string       { return s.source.URL }
func (s VectorScraper) Priority() int     { return s.source.Priority }
func (s VectorScraper) ContentEvery() int { return 1 }
func (s VectorScraper) FetchHeaders(ctx context.Context, client *http.Client) HeaderState {
	return fetchHeaders(ctx, client, s.Name(), statCanTableURL)
}
func (s VectorScraper) FetchCurrent(ctx context.Context, client *http.Client) (*Result, error) {
	return s.FetchTarget(ctx, client)
}
func (s VectorScraper) FetchTarget(ctx context.Context, client *http.Client) (*Result, error) {
	payload := []map[string]int{{"vectorId": statCanVectorID, "latestN": latestPeriodsToFetch}}
	var parsed []wdsDataResponse
	meta, err := postJSON(ctx, client, wdsVectorDataURL, payload, &parsed)
	if err != nil {
		return nil, err
	}
	return resultFromWDS(s.source, wdsVectorDataURL, meta, parsed)
}

func (s CoordinateScraper) Name() string      { return s.source.Name }
func (s CoordinateScraper) URL() string       { return s.source.URL }
func (s CoordinateScraper) Priority() int     { return s.source.Priority }
func (s CoordinateScraper) ContentEvery() int { return 1 }
func (s CoordinateScraper) FetchHeaders(ctx context.Context, client *http.Client) HeaderState {
	return fetchHeaders(ctx, client, s.Name(), statCanDeveloperWDS)
}
func (s CoordinateScraper) FetchCurrent(ctx context.Context, client *http.Client) (*Result, error) {
	return s.FetchTarget(ctx, client)
}
func (s CoordinateScraper) FetchTarget(ctx context.Context, client *http.Client) (*Result, error) {
	if err := verifyCoordinateSeries(ctx, client); err != nil {
		return nil, err
	}
	payload := []map[string]interface{}{{"productId": statCanProductID, "coordinate": statCanCoordinate, "latestN": latestPeriodsToFetch}}
	var parsed []wdsDataResponse
	meta, err := postJSON(ctx, client, wdsCoordDataURL, payload, &parsed)
	if err != nil {
		return nil, err
	}
	return resultFromWDS(s.source, wdsCoordDataURL, meta, parsed)
}

func fetchHeaders(ctx context.Context, client *http.Client, sourceName, url string) HeaderState {
	started := time.Now().UTC()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return HeaderState{Source: sourceName, URL: url, Timestamp: time.Now().UTC(), Error: err.Error()}
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := client.Do(req)
	received := time.Now().UTC()
	if err != nil {
		return HeaderState{Source: sourceName, URL: url, Timestamp: received, Latency: received.Sub(started), Error: err.Error()}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return HeaderState{
		Source:       sourceName,
		URL:          url,
		Status:       fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		StatusCode:   resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ServerDate:   resp.Header.Get("Date"),
		Timestamp:    received,
		Latency:      received.Sub(started),
	}
}

type fetchMeta struct {
	Status       string
	StatusCode   int
	ETag         string
	LastModified string
	ServerDate   string
	Latency      time.Duration
}

func fetchBytes(ctx context.Context, client *http.Client, method, url string, body io.Reader, accept string) ([]byte, fetchMeta, error) {
	started := time.Now().UTC()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fetchMeta{}, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-CA,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fetchMeta{Latency: time.Since(started)}, err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	meta := fetchMeta{
		Status:       fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		StatusCode:   resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ServerDate:   resp.Header.Get("Date"),
		Latency:      time.Since(started),
	}
	if readErr != nil {
		return respBody, meta, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return respBody, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(respBody))
	}
	return respBody, meta, nil
}

func postJSON(ctx context.Context, client *http.Client, url string, payload interface{}, out interface{}) (fetchMeta, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return fetchMeta{}, err
	}
	respBody, meta, err := fetchBytes(ctx, client, http.MethodPost, url, bytes.NewReader(body), "application/json")
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return meta, err
	}
	return meta, nil
}

func resultFromWDS(source Source, fetchURL string, meta fetchMeta, parsed []wdsDataResponse) (*Result, error) {
	if len(parsed) == 0 {
		return nil, errors.New("empty WDS response")
	}
	response := parsed[0]
	if err := validateWDSData(response); err != nil {
		return nil, err
	}
	points := response.Object.VectorDataPoint
	if len(points) == 0 {
		return nil, errors.New("WDS response has no observations")
	}

	observations := make([]wdsObservation, 0, len(points))
	for _, point := range points {
		periodYYYYMM, periodDisplay, err := wdsPeriod(point.RefPer)
		if err != nil {
			return nil, err
		}
		observations = append(observations, wdsObservation{
			PeriodYYYYMM:  periodYYYYMM,
			PeriodDisplay: periodDisplay,
			Value:         point.Value,
			ValueDisplay:  fmt.Sprintf("%.*f%%", point.Decimals, point.Value),
			ReleaseTime:   point.ReleaseTime,
			Decimals:      point.Decimals,
		})
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].PeriodYYYYMM < observations[j].PeriodYYYYMM
	})

	latest := observations[len(observations)-1]
	previous := wdsObservation{PeriodDisplay: "N/A", ValueDisplay: "N/A"}
	if len(observations) >= 2 {
		previous = observations[len(observations)-2]
	}
	return &Result{
		Source:         source.Name,
		SourceType:     source.SourceType,
		URL:            source.URL,
		FetchURL:       fetchURL,
		Period:         latest.PeriodDisplay,
		PeriodYYYYMM:   latest.PeriodYYYYMM,
		Value:          latest.ValueDisplay,
		ValuePercent:   latest.Value,
		Previous:       previous.ValueDisplay,
		PreviousPeriod: previous.PeriodDisplay,
		Timestamp:      time.Now().UTC(),
		Latency:        meta.Latency,
		ETag:           meta.ETag,
		LastModified:   meta.LastModified,
		Method:         source.ValueMethod,
		Detail:         fmt.Sprintf("Product ID %d, coordinate %s, vector %d", response.Object.ProductID, response.Object.Coordinate, response.Object.VectorID),
		ReleasedDate:   latest.ReleaseTime,
	}, nil
}

func validateWDSData(response wdsDataResponse) error {
	if response.Status != "SUCCESS" {
		return fmt.Errorf("WDS status %q", response.Status)
	}
	if response.Object.ProductID != statCanProductID {
		return fmt.Errorf("product ID %d does not match expected %d", response.Object.ProductID, statCanProductID)
	}
	if response.Object.Coordinate != statCanCoordinate {
		return fmt.Errorf("coordinate %s does not match expected %s", response.Object.Coordinate, statCanCoordinate)
	}
	if response.Object.VectorID != statCanVectorID {
		return fmt.Errorf("vector ID %d does not match expected %d", response.Object.VectorID, statCanVectorID)
	}
	return nil
}

var seriesInfoOnce struct {
	sync.Once
	err error
}

func verifyCoordinateSeries(ctx context.Context, client *http.Client) error {
	seriesInfoOnce.Do(func() {
		payload := []map[string]interface{}{{"productId": statCanProductID, "coordinate": statCanCoordinate}}
		var parsed []wdsSeriesInfoResponse
		_, err := postJSON(ctx, client, wdsSeriesInfoURL, payload, &parsed)
		if err != nil {
			seriesInfoOnce.err = err
			return
		}
		if len(parsed) == 0 {
			seriesInfoOnce.err = errors.New("empty WDS series-info response")
			return
		}
		seriesInfoOnce.err = validateSeriesInfo(parsed[0])
	})
	return seriesInfoOnce.err
}

func validateSeriesInfo(info wdsSeriesInfoResponse) error {
	if info.Status != "SUCCESS" {
		return fmt.Errorf("WDS series-info status %q", info.Status)
	}
	if info.Object.ProductID != statCanProductID {
		return fmt.Errorf("series-info product ID %d does not match expected %d", info.Object.ProductID, statCanProductID)
	}
	if info.Object.Coordinate != statCanCoordinate {
		return fmt.Errorf("series-info coordinate %s does not match expected %s", info.Object.Coordinate, statCanCoordinate)
	}
	if info.Object.VectorID != statCanVectorID {
		return fmt.Errorf("series-info vector ID %d does not match expected %d", info.Object.VectorID, statCanVectorID)
	}
	if info.Object.Terminated != 0 {
		return fmt.Errorf("series is terminated: %d", info.Object.Terminated)
	}
	if info.Object.SeriesTitleEn != expectedSeriesTitle {
		return fmt.Errorf("unexpected WDS series title %q", info.Object.SeriesTitleEn)
	}
	return nil
}

func parseDailyArticle(source Source, body []byte, articleURL string) (*ParsedValue, error) {
	text := compactSpaces(stripHTMLBytes(body))
	title := extractH1(body)
	if !lfsTitleRE.MatchString(title) {
		return nil, fmt.Errorf("not a Labour Force Survey article: h1=%q", title)
	}

	periodDisplay, periodYYYYMM, err := periodFromLFSTitle(title)
	if err != nil {
		return nil, err
	}
	releasedDate := ""
	if match := releasedRE.FindStringSubmatch(text); match != nil {
		releasedDate = match[1]
	}

	value, sentence, directOK, err := parseUnemploymentSentence(text)
	if err != nil {
		return nil, err
	}
	method := "Direct sentence parse"
	warnings := []string{}
	if !directOK {
		var metricPeriod string
		value, metricPeriod, err = parseCanadaMetricBlock(text)
		if err != nil {
			return nil, fmt.Errorf("direct sentence parse failed and metric block fallback failed: %w", err)
		}
		if metricPeriod != "" && metricPeriod != periodYYYYMM {
			return nil, fmt.Errorf("metric period %s does not match article period %s", metricPeriod, periodYYYYMM)
		}
		method = "Metric block fallback"
		warnings = append(warnings, "Direct sentence pattern was not found; used Canada metric block fallback.")
	}

	metricValue, metricPeriod, metricErr := parseCanadaMetricBlock(text)
	metricValueSet := metricErr == nil
	if metricValueSet {
		if metricPeriod != "" && metricPeriod != periodYYYYMM {
			return nil, fmt.Errorf("metric period %s does not match article period %s", metricPeriod, periodYYYYMM)
		}
		if math.Abs(metricValue-value) > 0.000001 {
			return nil, fmt.Errorf("direct sentence value %.1f%% does not match Canada metric block %.1f%%", value, metricValue)
		}
	}

	return &ParsedValue{
		Source:         source,
		Method:         method,
		Field:          "National unemployment rate",
		Seasonality:    "Seasonally adjusted",
		ValuePercent:   value,
		Value:          formatPercent(value),
		MetricValue:    formatPercent(metricValue),
		MetricValueSet: metricValueSet,
		Period:         periodDisplay,
		PeriodYYYYMM:   periodYYYYMM,
		ReleasedDate:   releasedDate,
		ArticleURL:     articleURL,
		Sentence:       sentence,
		Confidence:     "HIGH",
		Warnings:       warnings,
	}, nil
}

func parseUnemploymentSentence(text string) (float64, string, bool, error) {
	match := directSentenceRE.FindStringSubmatch(text)
	if match == nil {
		return 0, "", false, nil
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, "", false, err
	}
	sentence := compactSpaces(match[0])
	if sentenceMatch := sentenceBoundaryRE.FindStringSubmatch(text); sentenceMatch != nil {
		sentence = compactSpaces(sentenceMatch[1])
	}
	return value, sentence, true, nil
}

func parseCanadaMetricBlock(text string) (float64, string, error) {
	match := metricBlockRE.FindStringSubmatch(text)
	if match == nil {
		return 0, "", errors.New("Canada unemployment-rate metric block not found")
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, "", err
	}
	month, err := monthNumber(match[2])
	if err != nil {
		return 0, "", err
	}
	return value, fmt.Sprintf("%s-%02d", match[3], month), nil
}

func extractLatestLFSArticleURL(body []byte) (string, error) {
	for _, match := range anchorRE.FindAllSubmatch(body, -1) {
		href := strings.TrimSpace(string(match[1]))
		text := compactSpaces(stripHTMLBytes(match[2]))
		if lfsTitleRE.MatchString(text) {
			return absoluteStatCanURL(href), nil
		}
	}
	return "", errors.New("no Labour Force Survey article link found on Daily index")
}

func absoluteStatCanURL(href string) string {
	if strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://") {
		return href
	}
	if strings.HasPrefix(href, "/") {
		return "https://www150.statcan.gc.ca" + href
	}
	return "https://www150.statcan.gc.ca/n1/" + strings.TrimLeft(href, "/")
}

func dailyArticleCandidateURLs(eventTime time.Time) []string {
	dateCode := eventTime.Format("060102")
	return []string{fmt.Sprintf("https://www150.statcan.gc.ca/n1/daily-quotidien/%s/dq%sa-eng.htm", dateCode, dateCode)}
}

func extractH1(body []byte) string {
	match := h1RE.FindSubmatch(body)
	if match == nil {
		return ""
	}
	return compactSpaces(stripHTMLBytes(match[1]))
}

func periodFromLFSTitle(title string) (string, string, error) {
	match := titlePeriodRE.FindStringSubmatch(title)
	if match == nil {
		return "", "", fmt.Errorf("could not parse LFS article period from title %q", title)
	}
	month, err := monthNumber(match[1])
	if err != nil {
		return "", "", err
	}
	display := fmt.Sprintf("%s %s", canonicalMonthName(month), match[2])
	return display, fmt.Sprintf("%s-%02d", match[2], month), nil
}

func stripHTMLBytes(body []byte) string {
	s := string(body)
	s = scriptStyleRE.ReplaceAllString(s, " ")
	s = breakTagRE.ReplaceAllString(s, " ")
	s = blockTagRE.ReplaceAllString(s, " ")
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return normalizeWhitespace(s)
}

func normalizeWhitespace(s string) string {
	replacer := strings.NewReplacer(
		"\u00a0", " ",
		"\u202f", " ",
		"\u2009", " ",
		"\u2014", "-",
		"\u2013", "-",
		"\r", " ",
		"\n", " ",
		"\t", " ",
	)
	return spaceRE.ReplaceAllString(replacer.Replace(s), " ")
}

func compactSpaces(s string) string {
	return strings.TrimSpace(normalizeWhitespace(s))
}

func validate(parsed *ParsedValue, expectedRaw string) (string, []string, error) {
	warnings := []string{}
	if parsed == nil {
		return "LOW", warnings, errors.New("no parsed value")
	}
	if parsed.PeriodYYYYMM == "" {
		return "LOW", warnings, errors.New("release period could not be validated")
	}
	if parsed.ValuePercent < 0 || parsed.ValuePercent > 30 {
		return "LOW", warnings, fmt.Errorf("unemployment rate %.1f%% is outside expected bounds", parsed.ValuePercent)
	}
	expected, err := normalizeExpectedPeriod(expectedRaw)
	if err != nil {
		return "LOW", warnings, err
	}
	if expected != "" && parsed.PeriodYYYYMM != expected {
		return "LOW", warnings, fmt.Errorf("stale source: expected %s but source period is %s", periodDisplay(expected), parsed.Period)
	}
	if parsed.Method != "Direct sentence parse" {
		warnings = append(warnings, "Primary direct sentence was unavailable; fallback parser was used.")
		return "MEDIUM", warnings, nil
	}
	if !parsed.MetricValueSet {
		warnings = append(warnings, "Canada metric block was not found for cross-check.")
		return "MEDIUM", warnings, nil
	}
	return "HIGH", warnings, nil
}

func normalizeExpectedPeriod(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	if yyyymmRE.MatchString(input) {
		return input, nil
	}
	match := monthYearRE.FindStringSubmatch(input)
	if match == nil {
		return "", fmt.Errorf("expected period must be YYYY-MM or Month YYYY, got %q", input)
	}
	month, err := monthNumber(match[1])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%02d", match[2], month), nil
}

func periodDisplay(yyyymm string) string {
	normalized, err := normalizeExpectedPeriod(yyyymm)
	if err == nil {
		yyyymm = normalized
	}
	t, err := time.Parse("2006-01", yyyymm)
	if err != nil {
		return yyyymm
	}
	return t.Format("January 2006")
}

func wdsPeriod(refPer string) (string, string, error) {
	t, err := time.Parse("2006-01-02", refPer)
	if err != nil {
		return "", "", fmt.Errorf("invalid reference period %q: %w", refPer, err)
	}
	return t.Format("2006-01"), t.Format("January 2006"), nil
}

func monthNumber(name string) (int, error) {
	switch strings.ToLower(strings.TrimSuffix(name, ".")) {
	case "jan", "january":
		return 1, nil
	case "feb", "february":
		return 2, nil
	case "mar", "march":
		return 3, nil
	case "apr", "april":
		return 4, nil
	case "may":
		return 5, nil
	case "jun", "june":
		return 6, nil
	case "jul", "july":
		return 7, nil
	case "aug", "august":
		return 8, nil
	case "sep", "sept", "september":
		return 9, nil
	case "oct", "october":
		return 10, nil
	case "nov", "november":
		return 11, nil
	case "dec", "december":
		return 12, nil
	default:
		return 0, fmt.Errorf("unknown month %q", name)
	}
}

func canonicalMonthName(month int) string {
	names := []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	if month < 1 || month > 12 {
		return ""
	}
	return names[month]
}

func formatPercent(value float64) string {
	return fmt.Sprintf("%.1f%%", value)
}

func firstLine(body []byte) string {
	line := strings.TrimSpace(string(body))
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = line[:idx]
	}
	const max = 200
	if len(line) > max {
		line = line[:max] + "..."
	}
	return line
}
