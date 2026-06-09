package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
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
// EVENT CONFIGURATION - CHANGE THIS FOR NEXT UK MONTHLY GDP RELEASE
// ============================================================================
//
// Event: GDP monthly estimate, UK
// Publisher: Office for National Statistics
// Next release from March 2026 bulletin: 12 June 2026, 07:00 UK time.
// During BST this is 06:00 UTC / 11:30 IST.
//
// Format: "YYYY-MM-DD HH:MM:SS" in UTC.
const eventTimeUTC = "2026-06-12 06:00:00"
const expectedReleaseDate = "2026-06-12"
const expectedPeriod = "2026-04"

// ============================================================================

const (
	country         = "UK"
	eventName       = "UK GDP MoM and YoY"
	officialRelease = "GDP monthly estimate, UK"
	publisher       = "Office for National Statistics"
	unitPercent     = "%"

	primarySourceURL   = "https://www.ons.gov.uk/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk"
	latestBulletinURL  = primarySourceURL + "/latest"
	datasetPageURL     = "https://www.ons.gov.uk/economy/grossdomesticproductgdp/datasets/gdpmonthlyestimateuktimeseriesdataset/current"
	datasetCSVURL      = "https://www.ons.gov.uk/file?uri=/economy/grossdomesticproductgdp/datasets/gdpmonthlyestimateuktimeseriesdataset/current/mgdp.csv"
	releasesURL        = "https://www.ons.gov.uk/releases"
	targetFieldMoM     = "Monthly real GDP growth"
	targetFieldYoY     = "GDP compared with same month a year earlier"
	httpTimeout        = 5 * time.Second
	testConnectionLead = 1 * time.Minute
	sniperLead         = 2 * time.Second
	pollWindow         = 3 * time.Minute
	pollEvery          = 500 * time.Millisecond
	contentEveryPolls  = 5
	userAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) UK-GDP-Monthly-Sniper/1.0"
)

var (
	sources = []Source{
		{
			Name:        "Source 1 - ONS GDP monthly estimate bulletin",
			URL:         primarySourceURL,
			FetchURL:    latestBulletinURL,
			SourceType:  "Official HTML",
			Kind:        "article",
			Priority:    1,
			ValueMethod: "Direct article value",
		},
		{
			Name:        "Source 2 - ONS Monthly gross domestic product: time series (MGDP)",
			URL:         datasetPageURL,
			FetchURL:    datasetCSVURL,
			SourceType:  "Official CSV dataset",
			Kind:        "dataset",
			Priority:    2,
			ValueMethod: "Direct official dataset value",
		},
		{
			Name:        "Source 3 - ONS releases discovery page",
			URL:         releasesURL,
			FetchURL:    releaseDiscoveryURLForPeriod(expectedPeriod),
			SourceType:  "Official HTML release page",
			Kind:        "release-discovery",
			Priority:    3,
			ValueMethod: "Official release discovery and schedule validation",
		},
	}

	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	breakTagRE    = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockTagRE    = regexp.MustCompile(`(?is)</?(p|div|section|article|header|footer|main|h[1-6]|li|tr|table|thead|tbody|tfoot|caption)\b[^>]*>`)
	tagRE         = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE       = regexp.MustCompile(`\s+`)
	h1RE          = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	canonicalRE   = regexp.MustCompile(`(?is)<link\s+rel=["']canonical["']\s+href=["']([^"']+)["']`)
	anchorRE      = regexp.MustCompile(`(?is)<a\s+[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	titleMetaRE   = regexp.MustCompile(`contentTitle:\s*htmlUnescape\("([^"]+)"\)`)
	releaseDateRE = regexp.MustCompile(`(?i)\bRelease date:\s*([0-9]{1,2}\s+[A-Za-z]+\s+20\d{2})\b`)
	releasedAtRE  = regexp.MustCompile(`(?i)\bReleased:\s*([0-9]{1,2}\s+[A-Za-z]+\s+20\d{2}\s+\d{1,2}:\d{2}\s*(?:am|pm))\b`)
	nextReleaseRE = regexp.MustCompile(`(?i)\bNext release:\s*([0-9]{1,2}\s+[A-Za-z]+\s+20\d{2})\b`)
	titlePeriodRE = regexp.MustCompile(`(?i)\bGDP monthly estimate,\s*UK:\s*([A-Za-z]+)\s+(20\d{2})\b`)
	rowPeriodRE   = regexp.MustCompile(`^(20\d{2})\s+([A-Z]{3})$`)

	momPositiveRE = regexp.MustCompile(`(?i)\bMonthly\s+GDP\s+(?:grew|increased|rose)\s+by\s+([-+]?\d+(?:\.\d+)?)\s*%\s+in\s+([A-Za-z]+\s+20\d{2})\b`)
	momNegativeRE = regexp.MustCompile(`(?i)\bMonthly\s+GDP\s+(?:fell|declined|decreased|contracted|shrank)\s+by\s+([-+]?\d+(?:\.\d+)?)\s*%\s+in\s+([A-Za-z]+\s+20\d{2})\b`)
	momZeroRE     = regexp.MustCompile(`(?i)\bMonthly\s+GDP\s+(?:was\s+flat|was\s+unchanged|showed\s+no\s+growth|had\s+no\s+growth|recorded\s+no\s+growth)\s+in\s+([A-Za-z]+\s+20\d{2})\b`)
	yoyREs        = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bGDP\s+is\s+estimated\s+to\s+be\s+([-+]?\d+(?:\.\d+)?)\s*%\s+(higher|lower)\s+in\s+([A-Za-z]+\s+20\d{2})\s+compared\s+with\s+the\s+same\s+month\s+a\s+year\s+ago\b`),
		regexp.MustCompile(`(?i)\bCompared\s+with\s+the\s+same\s+month\s+a\s+year\s+ago,\s+GDP\s+is\s+estimated\s+to\s+be\s+([-+]?\d+(?:\.\d+)?)\s*%\s+(higher|lower)\s+in\s+([A-Za-z]+\s+20\d{2})\b`),
	}
)

type Source struct {
	Name        string
	URL         string
	FetchURL    string
	SourceType  string
	Kind        string
	Priority    int
	ValueMethod string
}

func (s Source) RequestURL() string {
	if strings.TrimSpace(s.FetchURL) != "" {
		return s.FetchURL
	}
	return s.URL
}

type Latency struct {
	Total    int64 `json:"total"`
	TTFB     int64 `json:"ttfb"`
	BodyRead int64 `json:"body_read"`
	Parse    int64 `json:"parse"`
}

type FetchMeta struct {
	Status            string
	StatusCode        int
	ServerDate        string
	LastModified      string
	ETag              string
	CacheControl      string
	FinalURL          string
	RequestStarted    time.Time
	ResponseReceived  time.Time
	ResponseSizeBytes int
	HashSHA256        string
	LatencyMS         Latency
}

type MetricValue struct {
	ID           string  `json:"id"`
	EventName    string  `json:"event_name"`
	Field        string  `json:"field"`
	Period       string  `json:"period"`
	Actual       string  `json:"actual"`
	NumericValue float64 `json:"numeric_value"`
	Unit         string  `json:"unit"`
	ValueMethod  string  `json:"value_method"`
	Sentence     string  `json:"sentence,omitempty"`
	Source       string  `json:"source"`
}

type Result struct {
	Source            Source
	ReleaseDate       string
	ReleasedAtUTC     string
	NextRelease       string
	Title             string
	FetchURL          string
	ArticleURL        string
	Period            string
	PeriodYYYYMM      string
	MoM               MetricValue
	YoY               MetricValue
	HasMoM            bool
	HasYoY            bool
	Confidence        string
	DetectionMethod   string
	DetectedAt        time.Time
	EventLatencyMS    int64
	Warnings          []string
	MatchedSources    []string
	ScheduleConfirmed bool
}

type Snapshot struct {
	Result *Result
	Meta   FetchMeta
	Error  string
}

type Baseline struct {
	ETag         string
	LastModified string
	ContentHash  string
	Period       string
	MoM          string
	YoY          string
	StatusCode   int
}

type Detection struct {
	Result           *Result
	Meta             FetchMeta
	Source           Source
	PollCount        int
	DetectedAt       time.Time
	LatencyFromEvent time.Duration
	Error            string
}

type ConsoleResult struct {
	Country           string        `json:"country"`
	EventName         string        `json:"event_name"`
	OfficialRelease   string        `json:"official_release"`
	Publisher         string        `json:"publisher"`
	Source            string        `json:"source"`
	SourceURL         string        `json:"source_url"`
	FetchURL          string        `json:"fetch_url"`
	SourceType        string        `json:"source_type"`
	ReleaseDate       string        `json:"release_date"`
	ReleasedAtUTC     string        `json:"released_at_utc,omitempty"`
	NextRelease       string        `json:"next_release"`
	ArticleURL        string        `json:"article_url,omitempty"`
	Period            string        `json:"period"`
	GDPMoM            string        `json:"gdp_mom"`
	GDPYoY            string        `json:"gdp_yoy"`
	Unit              string        `json:"unit"`
	Metrics           []MetricValue `json:"metrics"`
	Confidence        string        `json:"confidence"`
	DetectionMethod   string        `json:"detection_method"`
	EventLatencyMS    int64         `json:"event_latency_ms"`
	ServerDateHeader  string        `json:"server_date_header"`
	ETag              string        `json:"etag"`
	LastModified      string        `json:"last_modified"`
	CacheControl      string        `json:"cache_control"`
	ContentHash       string        `json:"content_hash"`
	LatencyMS         Latency       `json:"latency_ms"`
	MatchedSources    []string      `json:"matched_sources"`
	ScheduleConfirmed bool          `json:"schedule_confirmed"`
	Warnings          []string      `json:"warnings"`
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTC, time.UTC)
	if err != nil {
		fmt.Printf("Configuration error: invalid eventTimeUTC: %v\n", err)
		os.Exit(1)
	}
	if _, err := parseISODate(expectedReleaseDate); err != nil {
		fmt.Printf("Configuration error: invalid expectedReleaseDate: %v\n", err)
		os.Exit(1)
	}
	if _, err := parseYYYYMM(expectedPeriod); err != nil {
		fmt.Printf("Configuration error: invalid expectedPeriod: %v\n", err)
		os.Exit(1)
	}

	client := newHTTPClient()
	printHeader(eventTime)

	fmt.Println("Fetching current published data from official ONS sources...")
	current := fetchAllSources(client, httpTimeout)
	printSourceTable(current)
	fmt.Println("Current data captured. Waiting for the configured release time.")
	fmt.Println()

	now := time.Now().UTC()
	testTime := eventTime.Add(-testConnectionLead)
	sniperStart := eventTime.Add(-sniperLead)
	endTime := eventTime.Add(pollWindow)
	if now.After(endTime) {
		fmt.Println("Event polling window is already over. Showing current snapshot only.")
		return
	}

	if now.Before(testTime) {
		fmt.Printf("Countdown to connection test: %s\n", testTime.Sub(now).Round(time.Second))
		countdownUntil(testTime, time.Second, "Time remaining to connection test")
	}

	fmt.Println()
	fmt.Println("Testing connections and capturing baseline headers + content...")
	baselines := fetchAllSources(client, httpTimeout)
	printSourceTable(baselines)

	now = time.Now().UTC()
	if now.Before(sniperStart) {
		fmt.Printf("Final countdown to sniper mode: %s\n", sniperStart.Sub(now).Round(time.Millisecond))
		countdownUntil(sniperStart, 100*time.Millisecond, "Starting sniper mode in")
	}

	fmt.Println()
	fmt.Println("SNIPER MODE ACTIVE")
	fmt.Println("Primary trigger: ONS GDP monthly estimate bulletin")
	fmt.Println("Backup/fallback: ONS MGDP CSV dataset")
	fmt.Println("Hybrid detection: headers every 500ms, content every 5th poll, immediate content fetch on header change")
	fmt.Println()

	ctx, cancel := context.WithDeadline(context.Background(), endTime)
	defer cancel()

	detections := pollSources(ctx, client, baselines, eventTime, logger)
	printPerformanceTable(detections, eventTime)

	articleDetection, datasetDetection, releaseDetection := selectBestDetections(detections)
	var article, dataset, releaseDiscovery *Result
	var articleMeta FetchMeta
	if articleDetection != nil {
		article = articleDetection.Result
		articleMeta = articleDetection.Meta
	}
	if datasetDetection != nil {
		dataset = datasetDetection.Result
	}
	if releaseDetection != nil {
		releaseDiscovery = releaseDetection.Result
	}
	if (article == nil || article.PeriodYYYYMM != expectedPeriod) && releaseDiscovery != nil && releaseDiscovery.ArticleURL != "" {
		discoveredArticle, discoveredMeta, err := fetchArticleFromDiscovery(client, releaseDiscovery.ArticleURL)
		if err == nil {
			article = discoveredArticle
			articleMeta = discoveredMeta
		} else {
			logger.Printf("exact article fetch from ONS releases page failed: %v", err)
		}
	}
	confirmed, err := composeConfirmed(article, dataset, releaseDiscovery, fetchAllSources(client, httpTimeout))
	if err != nil {
		fmt.Printf("NOT_CONFIRMED: %v\n", err)
		return
	}

	fmt.Printf("Winner: %s\n", confirmed.Source.Name)
	fmt.Printf("Release Date: %s\n", confirmed.ReleaseDate)
	fmt.Printf("Period: %s\n", confirmed.Period)
	fmt.Printf("GDP (MoM): %s\n", confirmed.MoM.Actual)
	fmt.Printf("GDP (YoY): %s\n", confirmed.YoY.Actual)
	fmt.Printf("Detection Latency: %s from event time\n", formatLatency(time.Duration(confirmed.EventLatencyMS)*time.Millisecond))
	fmt.Println()
	fmt.Println("JSON OUTPUT:")

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(consoleResult(confirmed, articleMeta)); err != nil {
		logger.Printf("json encode failed: %v", err)
	}
}

func fetchArticleFromDiscovery(client *http.Client, articleURL string) (*Result, FetchMeta, error) {
	source := sources[0]
	source.FetchURL = articleURL
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	body, meta, err := fetch(ctx, client, http.MethodGet, articleURL, true)
	if err != nil {
		return nil, meta, err
	}
	parseStart := time.Now()
	result, err := parseArticle(source, body, meta.FinalURL)
	meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
	if err != nil {
		return nil, meta, err
	}
	return result, meta, nil
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

func fetchAllSources(client *http.Client, timeout time.Duration) map[string]Snapshot {
	out := make(chan struct {
		kind     string
		snapshot Snapshot
	}, len(sources))
	for _, source := range sources {
		source := source
		go func() {
			out <- struct {
				kind     string
				snapshot Snapshot
			}{kind: source.Kind, snapshot: fetchSnapshot(client, source, timeout)}
		}()
	}

	snapshots := make(map[string]Snapshot, len(sources))
	for range sources {
		item := <-out
		snapshots[item.kind] = item.snapshot
	}
	return snapshots
}

func fetchSnapshot(client *http.Client, source Source, timeout time.Duration) Snapshot {
	if source.Kind == "release-discovery" {
		return fetchReleaseDiscoverySnapshot(client, source, timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body, meta, err := fetch(ctx, client, http.MethodGet, source.RequestURL(), true)
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}
	parseStart := time.Now()
	result, err := parseSource(source, body, meta.FinalURL)
	meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}
	return Snapshot{Result: result, Meta: meta}
}

func fetchReleaseDiscoverySnapshot(client *http.Client, source Source, timeout time.Duration) Snapshot {
	expectedSnapshot := fetchSingleReleaseDiscovery(client, source, source.RequestURL(), timeout)
	if expectedSnapshot.Error == "" && expectedSnapshot.Result != nil {
		return expectedSnapshot
	}

	priorPeriod, err := previousMonth(expectedPeriod)
	if err != nil {
		return expectedSnapshot
	}
	priorURL := releaseDiscoveryURLForPeriod(priorPeriod)
	priorSnapshot := fetchSingleReleaseDiscovery(client, source, priorURL, timeout)
	if priorSnapshot.Error != "" || priorSnapshot.Result == nil {
		return expectedSnapshot
	}
	priorSnapshot.Meta = expectedSnapshot.Meta
	priorSnapshot.Result.Warnings = append(priorSnapshot.Result.Warnings,
		fmt.Sprintf("Expected release page %s is not live yet; using previous release page %s for schedule validation.", source.RequestURL(), priorURL))
	return priorSnapshot
}

func fetchSingleReleaseDiscovery(client *http.Client, source Source, url string, timeout time.Duration) Snapshot {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body, meta, err := fetch(ctx, client, http.MethodGet, url, true)
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}
	parseStart := time.Now()
	result, err := parseReleaseDiscovery(source, body, meta.FinalURL)
	meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}
	return Snapshot{Result: result, Meta: meta}
}

func fetch(ctx context.Context, client *http.Client, method, url string, readBody bool) ([]byte, FetchMeta, error) {
	start := time.Now()
	meta := FetchMeta{RequestStarted: start.UTC()}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, meta, err
	}
	setHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		meta.ResponseReceived = time.Now().UTC()
		meta.LatencyMS.Total = time.Since(start).Milliseconds()
		return nil, meta, err
	}
	defer resp.Body.Close()

	meta.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	meta.StatusCode = resp.StatusCode
	meta.ServerDate = resp.Header.Get("Date")
	meta.LastModified = resp.Header.Get("Last-Modified")
	meta.ETag = resp.Header.Get("ETag")
	meta.CacheControl = resp.Header.Get("Cache-Control")
	meta.FinalURL = resp.Request.URL.String()
	meta.LatencyMS.TTFB = time.Since(start).Milliseconds()

	if !readBody {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		meta.ResponseReceived = time.Now().UTC()
		meta.LatencyMS.Total = time.Since(start).Milliseconds()
		return nil, meta, nil
	}

	bodyStart := time.Now()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	meta.ResponseReceived = time.Now().UTC()
	meta.ResponseSizeBytes = len(body)
	meta.LatencyMS.BodyRead = time.Since(bodyStart).Milliseconds()
	meta.LatencyMS.Total = time.Since(start).Milliseconds()
	hash := sha256.Sum256(body)
	meta.HashSHA256 = hex.EncodeToString(hash[:])

	if readErr != nil {
		return body, meta, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(body))
	}
	return body, meta, nil
}

func fetchHeaders(ctx context.Context, client *http.Client, source Source) FetchMeta {
	_, meta, err := fetch(ctx, client, http.MethodHead, source.RequestURL(), false)
	if err == nil && meta.StatusCode != http.StatusMethodNotAllowed && meta.StatusCode != http.StatusForbidden {
		return meta
	}
	_, meta, _ = fetch(ctx, client, http.MethodGet, source.RequestURL(), false)
	return meta
}

func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/csv,application/csv,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
}

func parseSource(source Source, body []byte, finalURL string) (*Result, error) {
	switch source.Kind {
	case "article":
		return parseArticle(source, body, finalURL)
	case "dataset":
		return parseDataset(source, body, finalURL)
	case "release-discovery":
		return parseReleaseDiscovery(source, body, finalURL)
	default:
		return nil, fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

func parseArticle(source Source, body []byte, finalURL string) (*Result, error) {
	raw := string(body)
	text := compactSpaces(stripHTML(raw))
	title := extractTitle(raw)
	periodDisplay, periodYYYYMM, err := periodFromTitle(title)
	if err != nil {
		return nil, err
	}
	releaseDate, err := extractReleaseDate(text)
	if err != nil {
		return nil, err
	}
	nextRelease := extractNextRelease(text)

	mom, err := parseMoM(text)
	if err != nil {
		return nil, err
	}
	if mom.Period != periodDisplay {
		return nil, fmt.Errorf("MoM sentence period %s does not match article period %s", mom.Period, periodDisplay)
	}
	mom.ID = "gdp_mom"
	mom.EventName = "GDP (MoM)"
	mom.Field = targetFieldMoM
	mom.Unit = unitPercent
	mom.ValueMethod = source.ValueMethod
	mom.Source = source.Name

	result := &Result{
		Source:       source,
		ReleaseDate:  releaseDate,
		NextRelease:  nextRelease,
		Title:        title,
		FetchURL:     firstNonEmpty(extractCanonicalURL(raw), finalURL, source.RequestURL()),
		Period:       periodDisplay,
		PeriodYYYYMM: periodYYYYMM,
		MoM:          mom,
		HasMoM:       true,
		Confidence:   "HIGH",
	}

	yoy, err := parseYoY(text)
	if err == nil {
		if yoy.Period != periodDisplay {
			return nil, fmt.Errorf("YoY sentence period %s does not match article period %s", yoy.Period, periodDisplay)
		}
		yoy.ID = "gdp_yoy"
		yoy.EventName = "GDP (YoY)"
		yoy.Field = targetFieldYoY
		yoy.Unit = unitPercent
		yoy.ValueMethod = source.ValueMethod
		yoy.Source = source.Name
		result.YoY = yoy
		result.HasYoY = true
	} else {
		result.Warnings = append(result.Warnings, "Article YoY sentence was not found; MGDP dataset fallback is required.")
	}
	return result, nil
}

func parseReleaseDiscovery(source Source, body []byte, finalURL string) (*Result, error) {
	raw := string(body)
	text := compactSpaces(stripHTML(raw))
	title := extractTitle(raw)
	periodDisplay, periodYYYYMM, err := periodFromTitle(title)
	if err != nil {
		return nil, err
	}
	releasedAt, err := extractReleasedAtUTC(text)
	if err != nil {
		return nil, err
	}
	nextRelease := extractNextRelease(text)
	articleURL, err := extractPublicationArticleURL(raw, title)
	if err != nil {
		return nil, err
	}

	return &Result{
		Source:            source,
		ReleaseDate:       releasedAt.Format("2006-01-02"),
		ReleasedAtUTC:     releasedAt.UTC().Format("2006-01-02 15:04:05"),
		NextRelease:       nextRelease,
		Title:             title,
		FetchURL:          firstNonEmpty(finalURL, source.RequestURL()),
		ArticleURL:        articleURL,
		Period:            periodDisplay,
		PeriodYYYYMM:      periodYYYYMM,
		Confidence:        "HIGH",
		ScheduleConfirmed: scheduleConfirmsExpectedRelease(periodYYYYMM, releasedAt, nextRelease),
	}, nil
}

func parseDataset(source Source, body []byte, finalURL string) (*Result, error) {
	reader := csv.NewReader(strings.NewReader(string(body)))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("MGDP CSV parse failed: %w", err)
	}
	if len(records) < 2 {
		return nil, errors.New("MGDP CSV has no data")
	}
	headers := records[0]
	titleIdx := indexOfNormalized(headers, "title")
	if titleIdx < 0 {
		return nil, errors.New("MGDP CSV Title column not found")
	}
	indexIdx := findMGDPColumn(headers, "index")
	momIdx := findMGDPColumn(headers, "mom")
	yoyIdx := findMGDPColumn(headers, "yoy")
	if momIdx < 0 {
		return nil, errors.New("MGDP monthly growth column not found")
	}
	if yoyIdx < 0 && indexIdx < 0 {
		return nil, errors.New("MGDP YoY column and index fallback column not found")
	}

	releaseDate := ""
	nextRelease := ""
	dataRows := make(map[string][]string)
	var latest []string
	for _, record := range records[1:] {
		if len(record) <= titleIdx {
			continue
		}
		label := strings.TrimSpace(record[titleIdx])
		switch strings.ToLower(label) {
		case "release date":
			releaseDate = dateCell(record, momIdx)
		case "next release":
			nextRelease = cell(record, momIdx)
		default:
			periodYYYYMM, _, ok := periodFromDatasetLabel(label)
			if !ok {
				continue
			}
			dataRows[periodYYYYMM] = record
			if latest == nil || periodYYYYMM > mustDatasetPeriod(latest[titleIdx]) {
				latest = record
			}
		}
	}
	if latest == nil {
		return nil, errors.New("MGDP CSV has no monthly data rows")
	}

	periodYYYYMM, periodDisplay, ok := periodFromDatasetLabel(latest[titleIdx])
	if !ok {
		return nil, fmt.Errorf("invalid latest MGDP row label %q", latest[titleIdx])
	}
	momValue, momMethod, err := datasetMoM(latest, dataRows, periodYYYYMM, momIdx, indexIdx)
	if err != nil {
		return nil, err
	}
	yoyValue, yoyMethod, err := datasetYoY(latest, dataRows, periodYYYYMM, yoyIdx, indexIdx)
	if err != nil {
		return nil, err
	}

	mom := MetricValue{
		ID:           "gdp_mom",
		EventName:    "GDP (MoM)",
		Field:        targetFieldMoM,
		Period:       periodDisplay,
		Actual:       formatPercent(momValue),
		NumericValue: momValue,
		Unit:         unitPercent,
		ValueMethod:  momMethod,
		Source:       source.Name,
	}
	yoy := MetricValue{
		ID:           "gdp_yoy",
		EventName:    "GDP (YoY)",
		Field:        targetFieldYoY,
		Period:       periodDisplay,
		Actual:       formatPercent(yoyValue),
		NumericValue: yoyValue,
		Unit:         unitPercent,
		ValueMethod:  yoyMethod,
		Source:       source.Name,
	}

	return &Result{
		Source:       source,
		ReleaseDate:  releaseDate,
		NextRelease:  nextRelease,
		Title:        "Monthly gross domestic product: time series",
		FetchURL:     firstNonEmpty(finalURL, source.RequestURL()),
		Period:       periodDisplay,
		PeriodYYYYMM: periodYYYYMM,
		MoM:          mom,
		YoY:          yoy,
		HasMoM:       true,
		HasYoY:       true,
		Confidence:   "HIGH",
	}, nil
}

func datasetMoM(row []string, dataRows map[string][]string, period string, momIdx, indexIdx int) (float64, string, error) {
	if raw := cell(row, momIdx); raw != "" {
		value, err := parseFloat(raw)
		if err != nil {
			return 0, "", err
		}
		return round1(value), "Direct official dataset value", nil
	}
	if indexIdx < 0 {
		return 0, "", errors.New("MGDP monthly growth missing and index fallback unavailable")
	}
	prevPeriod, err := previousMonth(period)
	if err != nil {
		return 0, "", err
	}
	currentIndex, err := parseFloat(cell(row, indexIdx))
	if err != nil {
		return 0, "", fmt.Errorf("current MGDP index invalid: %w", err)
	}
	prevRow := dataRows[prevPeriod]
	if prevRow == nil {
		return 0, "", fmt.Errorf("previous month %s row not found for MGDP MoM fallback", prevPeriod)
	}
	prevIndex, err := parseFloat(cell(prevRow, indexIdx))
	if err != nil {
		return 0, "", fmt.Errorf("previous MGDP index invalid: %w", err)
	}
	if prevIndex == 0 {
		return 0, "", errors.New("previous MGDP index is zero")
	}
	return round1(((currentIndex / prevIndex) - 1) * 100), "Calculated from official MGDP index", nil
}

func datasetYoY(row []string, dataRows map[string][]string, period string, yoyIdx, indexIdx int) (float64, string, error) {
	if yoyIdx >= 0 {
		if raw := cell(row, yoyIdx); raw != "" {
			value, err := parseFloat(raw)
			if err != nil {
				return 0, "", err
			}
			return round1(value), "Direct official dataset value", nil
		}
	}
	if indexIdx < 0 {
		return 0, "", errors.New("MGDP YoY missing and index fallback unavailable")
	}
	priorYear, err := sameMonthPriorYear(period)
	if err != nil {
		return 0, "", err
	}
	currentIndex, err := parseFloat(cell(row, indexIdx))
	if err != nil {
		return 0, "", fmt.Errorf("current MGDP index invalid: %w", err)
	}
	prevRow := dataRows[priorYear]
	if prevRow == nil {
		return 0, "", fmt.Errorf("same month prior year %s row not found for MGDP YoY fallback", priorYear)
	}
	prevIndex, err := parseFloat(cell(prevRow, indexIdx))
	if err != nil {
		return 0, "", fmt.Errorf("prior-year MGDP index invalid: %w", err)
	}
	if prevIndex == 0 {
		return 0, "", errors.New("prior-year MGDP index is zero")
	}
	return round1(((currentIndex / prevIndex) - 1) * 100), "Calculated from official MGDP index", nil
}

func parseMoM(text string) (MetricValue, error) {
	if match := momPositiveRE.FindStringSubmatchIndex(text); match != nil {
		value, err := parseFloat(text[match[2]:match[3]])
		if err != nil {
			return MetricValue{}, err
		}
		period, err := normalizeMonthYear(text[match[4]:match[5]])
		if err != nil {
			return MetricValue{}, err
		}
		return metricFromSentence("gdp_mom", period, value, sentenceAround(text, match[0], match[1])), nil
	}
	if match := momNegativeRE.FindStringSubmatchIndex(text); match != nil {
		value, err := parseFloat(text[match[2]:match[3]])
		if err != nil {
			return MetricValue{}, err
		}
		period, err := normalizeMonthYear(text[match[4]:match[5]])
		if err != nil {
			return MetricValue{}, err
		}
		return metricFromSentence("gdp_mom", period, -math.Abs(value), sentenceAround(text, match[0], match[1])), nil
	}
	if match := momZeroRE.FindStringSubmatchIndex(text); match != nil {
		period, err := normalizeMonthYear(text[match[2]:match[3]])
		if err != nil {
			return MetricValue{}, err
		}
		return metricFromSentence("gdp_mom", period, 0, sentenceAround(text, match[0], match[1])), nil
	}
	return MetricValue{}, errors.New("Monthly GDP direct sentence not found")
}

func parseYoY(text string) (MetricValue, error) {
	for _, re := range yoyREs {
		if match := re.FindStringSubmatchIndex(text); match != nil {
			value, err := parseFloat(text[match[2]:match[3]])
			if err != nil {
				return MetricValue{}, err
			}
			direction := strings.ToLower(text[match[4]:match[5]])
			if direction == "lower" {
				value = -math.Abs(value)
			}
			period, err := normalizeMonthYear(text[match[6]:match[7]])
			if err != nil {
				return MetricValue{}, err
			}
			return metricFromSentence("gdp_yoy", period, value, sentenceAround(text, match[0], match[1])), nil
		}
	}
	return MetricValue{}, errors.New("same-month-a-year-ago GDP sentence not found")
}

func metricFromSentence(id, period string, value float64, sentence string) MetricValue {
	return MetricValue{
		ID:           id,
		Period:       period,
		Actual:       formatPercent(value),
		NumericValue: round1(value),
		Unit:         unitPercent,
		Sentence:     compactSpaces(sentence),
	}
}

func pollSources(ctx context.Context, client *http.Client, baselines map[string]Snapshot, eventTime time.Time, logger *log.Logger) []Detection {
	out := make(chan Detection, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		source := source
		wg.Add(1)
		go func() {
			defer wg.Done()
			baseline := baselineFromSnapshot(baselines[source.Kind])
			out <- pollSource(ctx, client, source, baseline, eventTime, logger)
		}()
	}
	wg.Wait()
	close(out)

	var detections []Detection
	for detection := range out {
		if detection.Error == "" && detection.Result != nil {
			detections = append(detections, detection)
		}
	}
	sort.Slice(detections, func(i, j int) bool {
		return detections[i].DetectedAt.Before(detections[j].DetectedAt)
	})
	return detections
}

func pollSource(ctx context.Context, client *http.Client, source Source, baseline Baseline, eventTime time.Time, logger *log.Logger) Detection {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	pollCount := 0
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return Detection{Source: source, Error: firstNonEmpty(lastErr, ctx.Err().Error())}
		default:
		}

		pollCount++
		reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
		headerMeta := fetchHeaders(reqCtx, client, source)
		cancel()
		headersChanged := headerIndicatesUpdate(baseline, headerMeta)
		checkContent := headersChanged || pollCount%contentEveryPolls == 0
		if checkContent {
			reqCtx2, cancel2 := context.WithTimeout(ctx, httpTimeout)
			body, meta, err := fetch(reqCtx2, client, http.MethodGet, source.RequestURL(), true)
			cancel2()
			if err != nil {
				lastErr = err.Error()
				logger.Printf("%s poll %d fetch failed: %v", source.Name, pollCount, err)
			} else {
				parseStart := time.Now()
				result, parseErr := parseSource(source, body, meta.FinalURL)
				meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
				if parseErr != nil {
					lastErr = parseErr.Error()
					logger.Printf("%s poll %d parse failed: %v", source.Name, pollCount, parseErr)
				} else if isExpectedUpdate(result, baseline) {
					detectedAt := time.Now().UTC()
					method := "content"
					if headersChanged {
						method = "headers+content"
					}
					result.DetectionMethod = method
					result.DetectedAt = detectedAt
					result.EventLatencyMS = detectedAt.Sub(eventTime).Milliseconds()
					return Detection{
						Result:           result,
						Meta:             meta,
						Source:           source,
						PollCount:        pollCount,
						DetectedAt:       detectedAt,
						LatencyFromEvent: detectedAt.Sub(eventTime),
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return Detection{Source: source, Error: firstNonEmpty(lastErr, ctx.Err().Error())}
		case <-ticker.C:
		}
	}
}

func headerIndicatesUpdate(baseline Baseline, meta FetchMeta) bool {
	if meta.StatusCode == 0 {
		return false
	}
	if baseline.StatusCode != 0 && meta.StatusCode != baseline.StatusCode {
		return true
	}
	if baseline.ETag != "" && meta.ETag != "" && meta.ETag != baseline.ETag {
		return true
	}
	if baseline.LastModified != "" && meta.LastModified != "" && meta.LastModified != baseline.LastModified {
		return true
	}
	return false
}

func isExpectedUpdate(result *Result, baseline Baseline) bool {
	if result == nil {
		return false
	}
	if result.Source.Kind == "release-discovery" {
		return result.PeriodYYYYMM == expectedPeriod &&
			result.ReleaseDate == expectedReleaseDate &&
			result.ArticleURL != "" &&
			result.PeriodYYYYMM != baseline.Period
	}
	if result.PeriodYYYYMM != expectedPeriod {
		return false
	}
	if result.Source.Kind == "article" && result.ReleaseDate != expectedReleaseDate {
		return false
	}
	if !validMetricRange(result.MoM.NumericValue) || (result.HasYoY && !validMetricRange(result.YoY.NumericValue)) {
		return false
	}
	return result.PeriodYYYYMM != baseline.Period || result.MoM.Actual != baseline.MoM || result.YoY.Actual != baseline.YoY
}

func baselineFromSnapshot(snapshot Snapshot) Baseline {
	b := Baseline{
		ETag:         snapshot.Meta.ETag,
		LastModified: snapshot.Meta.LastModified,
		ContentHash:  snapshot.Meta.HashSHA256,
		StatusCode:   snapshot.Meta.StatusCode,
	}
	if snapshot.Result != nil {
		b.Period = snapshot.Result.PeriodYYYYMM
		if snapshot.Result.HasMoM {
			b.MoM = snapshot.Result.MoM.Actual
		}
		if snapshot.Result.HasYoY {
			b.YoY = snapshot.Result.YoY.Actual
		}
	}
	return b
}

func selectBestDetections(detections []Detection) (*Detection, *Detection, *Detection) {
	var article *Detection
	var dataset *Detection
	var releaseDiscovery *Detection
	for _, detection := range detections {
		if detection.Result == nil {
			continue
		}
		detection := detection
		switch detection.Result.Source.Kind {
		case "article":
			if article == nil {
				article = &detection
			}
		case "dataset":
			if dataset == nil {
				dataset = &detection
			}
		case "release-discovery":
			if releaseDiscovery == nil {
				releaseDiscovery = &detection
			}
		}
	}
	return article, dataset, releaseDiscovery
}

func composeConfirmed(article, detectedDataset, detectedReleaseDiscovery *Result, latest map[string]Snapshot) (*Result, error) {
	if article == nil {
		return nil, errors.New("primary ONS bulletin did not publish the expected release during the polling window")
	}
	if err := validateArticle(article); err != nil {
		return nil, err
	}

	dataset := detectedDataset
	if snapshot := latest["dataset"]; snapshot.Result != nil && snapshot.Error == "" {
		if snapshot.Result.PeriodYYYYMM == expectedPeriod || dataset == nil {
			dataset = snapshot.Result
		}
	}
	releaseDiscovery := detectedReleaseDiscovery
	if snapshot := latest["release-discovery"]; snapshot.Result != nil && snapshot.Error == "" {
		if snapshot.Result.PeriodYYYYMM == expectedPeriod || releaseDiscovery == nil {
			releaseDiscovery = snapshot.Result
		}
	}
	if err := validateReleaseDiscovery(releaseDiscovery); err != nil {
		return nil, fmt.Errorf("ONS release discovery validation failed: %w", err)
	}

	confirmed := cloneResult(article)
	confirmed.MatchedSources = []string{article.Source.Name}
	if !confirmed.HasYoY {
		if dataset == nil {
			return nil, errors.New("article YoY sentence missing and MGDP dataset is unavailable")
		}
		if err := validateDataset(dataset); err != nil {
			return nil, fmt.Errorf("article YoY sentence missing and MGDP fallback is invalid: %w", err)
		}
		confirmed.YoY = dataset.YoY
		confirmed.YoY.ValueMethod = "Official MGDP dataset fallback"
		confirmed.HasYoY = true
		confirmed.Warnings = append(confirmed.Warnings, "GDP YoY used MGDP dataset fallback because the direct article sentence was not found.")
	}

	if dataset != nil {
		if err := validateDataset(dataset); err != nil {
			confirmed.Warnings = append(confirmed.Warnings, "MGDP dataset validation skipped: "+err.Error())
		} else {
			if !floatEqual(confirmed.MoM.NumericValue, dataset.MoM.NumericValue) {
				return nil, fmt.Errorf("Source 1 GDP MoM %s differs from MGDP dataset %s", confirmed.MoM.Actual, dataset.MoM.Actual)
			}
			if !floatEqual(confirmed.YoY.NumericValue, dataset.YoY.NumericValue) {
				return nil, fmt.Errorf("Source 1 GDP YoY %s differs from MGDP dataset %s", confirmed.YoY.Actual, dataset.YoY.Actual)
			}
			confirmed.MatchedSources = append(confirmed.MatchedSources, dataset.Source.Name)
		}
	} else {
		confirmed.Warnings = append(confirmed.Warnings, "MGDP dataset unavailable for confirmation.")
	}
	if releaseDiscovery != nil {
		if releaseDiscovery.PeriodYYYYMM == expectedPeriod && releaseDiscovery.ArticleURL != "" && normalizeURL(releaseDiscovery.ArticleURL) != normalizeURL(confirmed.FetchURL) {
			return nil, fmt.Errorf("ONS releases page article URL %s differs from parsed article URL %s", releaseDiscovery.ArticleURL, confirmed.FetchURL)
		}
		confirmed.ReleasedAtUTC = releaseDiscovery.ReleasedAtUTC
		confirmed.ArticleURL = releaseDiscovery.ArticleURL
		confirmed.MatchedSources = append(confirmed.MatchedSources, releaseDiscovery.Source.Name)
		confirmed.ScheduleConfirmed = true
	}

	confirmed.Confidence = "HIGH"
	confirmed.MatchedSources = uniqueStrings(confirmed.MatchedSources)
	return confirmed, nil
}

func validateArticle(result *Result) error {
	if result == nil {
		return errors.New("no primary article result")
	}
	if result.PeriodYYYYMM != expectedPeriod {
		return fmt.Errorf("stale primary article: expected %s but latest article period is %s", periodDisplay(expectedPeriod), result.Period)
	}
	if result.ReleaseDate != expectedReleaseDate {
		return fmt.Errorf("stale primary article: expected release date %s but article release date is %s", expectedReleaseDate, result.ReleaseDate)
	}
	if !result.HasMoM {
		return errors.New("primary article missing GDP MoM")
	}
	if !validMetricRange(result.MoM.NumericValue) {
		return fmt.Errorf("GDP MoM %.1f%% is outside expected bounds", result.MoM.NumericValue)
	}
	if result.HasYoY && !validMetricRange(result.YoY.NumericValue) {
		return fmt.Errorf("GDP YoY %.1f%% is outside expected bounds", result.YoY.NumericValue)
	}
	return nil
}

func validateDataset(result *Result) error {
	if result == nil {
		return errors.New("no MGDP dataset result")
	}
	if result.PeriodYYYYMM != expectedPeriod {
		return fmt.Errorf("stale MGDP dataset: expected %s but latest dataset period is %s", periodDisplay(expectedPeriod), result.Period)
	}
	if !result.HasMoM || !result.HasYoY {
		return errors.New("MGDP dataset missing MoM or YoY value")
	}
	if !validMetricRange(result.MoM.NumericValue) || !validMetricRange(result.YoY.NumericValue) {
		return fmt.Errorf("MGDP values are outside expected bounds: MoM %.1f%% YoY %.1f%%", result.MoM.NumericValue, result.YoY.NumericValue)
	}
	return nil
}

func validMetricRange(value float64) bool {
	return value >= -30 && value <= 30
}

func consoleResult(result *Result, meta FetchMeta) ConsoleResult {
	return ConsoleResult{
		Country:           country,
		EventName:         eventName,
		OfficialRelease:   officialRelease,
		Publisher:         publisher,
		Source:            result.Source.Name,
		SourceURL:         result.Source.URL,
		FetchURL:          result.FetchURL,
		SourceType:        result.Source.SourceType,
		ReleaseDate:       result.ReleaseDate,
		ReleasedAtUTC:     result.ReleasedAtUTC,
		NextRelease:       result.NextRelease,
		ArticleURL:        firstNonEmpty(result.ArticleURL, result.FetchURL),
		Period:            result.Period,
		GDPMoM:            result.MoM.Actual,
		GDPYoY:            result.YoY.Actual,
		Unit:              unitPercent,
		Metrics:           []MetricValue{result.MoM, result.YoY},
		Confidence:        result.Confidence,
		DetectionMethod:   result.DetectionMethod,
		EventLatencyMS:    result.EventLatencyMS,
		ServerDateHeader:  meta.ServerDate,
		ETag:              meta.ETag,
		LastModified:      meta.LastModified,
		CacheControl:      meta.CacheControl,
		ContentHash:       meta.HashSHA256,
		LatencyMS:         meta.LatencyMS,
		MatchedSources:    uniqueStrings(result.MatchedSources),
		ScheduleConfirmed: result.ScheduleConfirmed,
		Warnings:          uniqueStrings(result.Warnings),
	}
}

func printHeader(eventTime time.Time) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	london, _ := time.LoadLocation("Europe/London")
	fmt.Println("=======================================================================")
	fmt.Println("UK GDP MoM and YoY - SNIPER MODE")
	fmt.Println("=======================================================================")
	fmt.Printf("Publisher: %s\n", publisher)
	fmt.Printf("Release: %s\n", officialRelease)
	fmt.Printf("Event Time (IST): %s\n", eventTime.In(ist).Format("2006-01-02 15:04:05"))
	if london != nil {
		fmt.Printf("Event Time (UK):  %s\n", eventTime.In(london).Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Printf("Event Time (UTC): %s\n", eventTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected Release Date: %s\n", expectedReleaseDate)
	fmt.Printf("Expected Period: %s\n", periodDisplay(expectedPeriod))
	fmt.Println("MoM method: direct ONS article sentence")
	fmt.Println("YoY method: direct ONS article sentence; MGDP dataset fallback if absent")
	fmt.Println("=======================================================================")
	fmt.Println()
}

func printSourceTable(snapshots map[string]Snapshot) {
	ordered := append([]Source(nil), sources...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	for _, source := range ordered {
		snapshot := snapshots[source.Kind]
		if snapshot.Error != "" {
			fmt.Printf("  ERR  [%-64s] %s\n", source.Name, shortError(snapshot.Error))
			continue
		}
		if snapshot.Result == nil {
			fmt.Printf("  WAIT [%-64s] no parsed result\n", source.Name)
			continue
		}
		if source.Kind == "release-discovery" {
			fmt.Printf("  OK   [%-64s] Release: %-10s Period: %-14s Next: %-10s Article: %s\n",
				source.Name,
				blankNA(snapshot.Result.ReleaseDate),
				snapshot.Result.Period,
				blankNA(snapshot.Result.NextRelease),
				blankNA(snapshot.Result.ArticleURL),
			)
			continue
		}
		yoy := "N/A"
		if snapshot.Result.HasYoY {
			yoy = snapshot.Result.YoY.Actual
		}
		fmt.Printf("  OK   [%-64s] Release: %-10s Period: %-14s MoM: %-7s YoY: %-7s\n",
			source.Name,
			blankNA(snapshot.Result.ReleaseDate),
			snapshot.Result.Period,
			snapshot.Result.MoM.Actual,
			yoy,
		)
	}
}

func printPerformanceTable(detections []Detection, eventTime time.Time) {
	fmt.Println()
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Printf("%-6s %-64s %-18s %-14s %-8s %-8s %-10s\n", "RANK", "SOURCE", "UPDATE UTC", "LATENCY", "MoM", "YoY", "METHOD")
	detected := make(map[string]Detection, len(detections))
	for _, detection := range detections {
		detected[detection.Source.Kind] = detection
	}
	for i, detection := range detections {
		yoy := "N/A"
		mom := "N/A"
		if detection.Result.HasYoY {
			yoy = detection.Result.YoY.Actual
		}
		if detection.Result.HasMoM {
			mom = detection.Result.MoM.Actual
		}
		fmt.Printf("%-6s %-64s %-18s %-14s %-8s %-8s %-10s\n",
			fmt.Sprintf("#%d", i+1),
			detection.Source.Name,
			detection.DetectedAt.Format("15:04:05.000"),
			formatLatency(detection.LatencyFromEvent),
			mom,
			yoy,
			detection.Result.DetectionMethod,
		)
	}
	for _, source := range sources {
		if _, ok := detected[source.Kind]; ok {
			continue
		}
		fmt.Printf("%-6s %-64s %-18s %-14s %-8s %-8s %-10s\n", "-", source.Name, "-", "Pending", "Pending", "Pending", "-")
	}
	fmt.Printf("Event UTC: %s\n", eventTime.Format("15:04:05.000"))
}

func countdownUntil(target time.Time, step time.Duration, label string) {
	for {
		now := time.Now().UTC()
		if !now.Before(target) {
			fmt.Println()
			return
		}
		remaining := target.Sub(now)
		fmt.Printf("\r%s: %s   ", label, formatDuration(remaining))
		sleep := step
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

func extractTitle(raw string) string {
	if match := titleMetaRE.FindStringSubmatch(raw); match != nil {
		return compactSpaces(html.UnescapeString(match[1]))
	}
	if match := h1RE.FindStringSubmatch(raw); match != nil {
		return compactSpaces(stripHTML(match[1]))
	}
	return ""
}

func extractCanonicalURL(raw string) string {
	match := canonicalRE.FindStringSubmatch(raw)
	if match == nil {
		return ""
	}
	return absoluteONSURL(html.UnescapeString(match[1]))
}

func extractReleaseDate(text string) (string, error) {
	match := releaseDateRE.FindStringSubmatch(text)
	if match == nil {
		return "", errors.New("release date not found")
	}
	t, err := parseLongDate(match[1])
	if err != nil {
		return "", err
	}
	return t.Format("2006-01-02"), nil
}

func extractNextRelease(text string) string {
	match := nextReleaseRE.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	t, err := parseLongDate(match[1])
	if err != nil {
		return compactSpaces(match[1])
	}
	return t.Format("2006-01-02")
}

func extractReleasedAtUTC(text string) (time.Time, error) {
	match := releasedAtRE.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, errors.New("release discovery timestamp not found")
	}
	return parseONSReleaseDateTime(match[1])
}

func parseONSReleaseDateTime(input string) (time.Time, error) {
	uk, err := time.LoadLocation("Europe/London")
	if err != nil {
		uk = time.FixedZone("UK", 0)
	}
	input = compactSpaces(strings.ToUpper(input))
	t, err := time.ParseInLocation("2 January 2006 3:04PM", input, uk)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid ONS release timestamp %q: %w", input, err)
	}
	return t.UTC(), nil
}

func extractPublicationArticleURL(raw, expectedTitle string) (string, error) {
	expectedTitle = compactSpaces(expectedTitle)
	for _, match := range anchorRE.FindAllStringSubmatch(raw, -1) {
		href := html.UnescapeString(strings.TrimSpace(match[1]))
		label := compactSpaces(stripHTML(match[2]))
		if label != expectedTitle {
			continue
		}
		absolute := absoluteONSURL(href)
		if strings.Contains(absolute, "/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/") {
			return absolute, nil
		}
	}
	return "", fmt.Errorf("publication article link for %q not found on release page", expectedTitle)
}

func scheduleConfirmsExpectedRelease(periodYYYYMM string, releasedAt time.Time, nextRelease string) bool {
	if periodYYYYMM == expectedPeriod {
		eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTC, time.UTC)
		return err == nil && releasedAt.UTC().Equal(eventTime.UTC())
	}
	return nextRelease == expectedReleaseDate
}

func validateReleaseDiscovery(result *Result) error {
	if result == nil {
		return errors.New("no ONS releases discovery result")
	}
	if result.Source.Kind != "release-discovery" {
		return fmt.Errorf("invalid release discovery source kind %q", result.Source.Kind)
	}
	if result.PeriodYYYYMM != expectedPeriod {
		return fmt.Errorf("ONS releases page has period %s; expected live release %s", result.Period, periodDisplay(expectedPeriod))
	}
	if result.ReleaseDate != expectedReleaseDate {
		return fmt.Errorf("ONS releases page expected release date %s but found %s", expectedReleaseDate, result.ReleaseDate)
	}
	eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTC, time.UTC)
	if err != nil {
		return err
	}
	releasedAt, err := time.ParseInLocation("2006-01-02 15:04:05", result.ReleasedAtUTC, time.UTC)
	if err != nil {
		return fmt.Errorf("invalid ONS releases UTC timestamp %q: %w", result.ReleasedAtUTC, err)
	}
	if !releasedAt.Equal(eventTime.UTC()) {
		return fmt.Errorf("ONS releases timestamp %s UTC does not match configured event %s UTC", releasedAt.Format("2006-01-02 15:04:05"), eventTime.Format("2006-01-02 15:04:05"))
	}
	if result.ArticleURL == "" {
		return errors.New("ONS releases page has no publication article URL")
	}
	return nil
}

func periodFromTitle(title string) (string, string, error) {
	match := titlePeriodRE.FindStringSubmatch(title)
	if match == nil {
		return "", "", fmt.Errorf("could not parse GDP article period from title %q", title)
	}
	month, err := monthNumber(match[1])
	if err != nil {
		return "", "", err
	}
	display := fmt.Sprintf("%s %s", canonicalMonthName(month), match[2])
	return display, fmt.Sprintf("%s-%02d", match[2], month), nil
}

func normalizeMonthYear(input string) (string, error) {
	input = compactSpaces(input)
	parts := strings.Fields(input)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid month-year %q", input)
	}
	month, err := monthNumber(parts[0])
	if err != nil {
		return "", err
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return "", fmt.Errorf("invalid year %q", parts[1])
	}
	return fmt.Sprintf("%s %s", canonicalMonthName(month), parts[1]), nil
}

func periodFromDatasetLabel(label string) (string, string, bool) {
	match := rowPeriodRE.FindStringSubmatch(strings.TrimSpace(label))
	if match == nil {
		return "", "", false
	}
	month, ok := shortMonthNumber(match[2])
	if !ok {
		return "", "", false
	}
	return fmt.Sprintf("%s-%02d", match[1], month), fmt.Sprintf("%s %s", canonicalMonthName(month), match[1]), true
}

func mustDatasetPeriod(label string) string {
	period, _, ok := periodFromDatasetLabel(label)
	if !ok {
		return ""
	}
	return period
}

func findMGDPColumn(headers []string, kind string) int {
	for i, header := range headers {
		n := normalizeHeader(header)
		if !strings.Contains(n, "gross value added - monthly") {
			continue
		}
		if strings.Contains(n, "contribution") || strings.Contains(n, "3 month") || strings.Contains(n, "3m") {
			continue
		}
		switch kind {
		case "index":
			if strings.Contains(n, "index 1dp") {
				return i
			}
		case "mom":
			if strings.Contains(n, "period on period growth") && !strings.Contains(n, "1 year ago") {
				return i
			}
		case "yoy":
			if strings.Contains(n, "period on period 1 year ago growth") {
				return i
			}
		}
	}
	return -1
}

func indexOfNormalized(values []string, want string) int {
	want = normalizeHeader(want)
	for i, value := range values {
		if normalizeHeader(value) == want {
			return i
		}
	}
	return -1
}

func normalizeHeader(s string) string {
	s = strings.ToLower(compactSpaces(s))
	s = strings.ReplaceAll(s, "growth )", "growth)")
	s = strings.ReplaceAll(s, "supply(", "supply (")
	return s
}

func sentenceAround(text string, start, end int) string {
	left := strings.LastIndexAny(text[:start], ".!?")
	if left < 0 {
		left = 0
	} else {
		left++
	}
	right := strings.IndexAny(text[end:], ".!?")
	if right < 0 {
		right = len(text)
	} else {
		right = end + right + 1
	}
	return compactSpaces(text[left:right])
}

func stripHTML(s string) string {
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
		"\u2212", "-",
		"\r", " ",
		"\n", " ",
		"\t", " ",
	)
	return spaceRE.ReplaceAllString(replacer.Replace(s), " ")
}

func compactSpaces(s string) string {
	return strings.TrimSpace(normalizeWhitespace(s))
}

func parseFloat(raw string) (float64, error) {
	s := strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, errors.New("empty numeric value")
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric value %q: %w", raw, err)
	}
	return value, nil
}

func parseLongDate(input string) (time.Time, error) {
	return time.Parse("2 January 2006", compactSpaces(input))
}

func parseISODate(input string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(input))
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, got %q", input)
	}
	return t, nil
}

func parseYYYYMM(input string) (time.Time, error) {
	t, err := time.Parse("2006-01", strings.TrimSpace(input))
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM, got %q", input)
	}
	return t, nil
}

func dateCell(record []string, idx int) string {
	value := cell(record, idx)
	if t, err := time.Parse("02-01-2006", value); err == nil {
		return t.Format("2006-01-02")
	}
	if t, err := parseLongDate(value); err == nil {
		return t.Format("2006-01-02")
	}
	return value
}

func cell(record []string, idx int) string {
	if idx < 0 || idx >= len(record) {
		return ""
	}
	return compactSpaces(record[idx])
}

func monthNumber(name string) (int, error) {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".")) {
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

func shortMonthNumber(name string) (int, bool) {
	month, err := monthNumber(name)
	return month, err == nil
}

func canonicalMonthName(month int) string {
	names := []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	if month < 1 || month > 12 {
		return ""
	}
	return names[month]
}

func periodDisplay(yyyymm string) string {
	t, err := parseYYYYMM(yyyymm)
	if err != nil {
		return yyyymm
	}
	return t.Format("January 2006")
}

func releaseDiscoveryURLForPeriod(yyyymm string) string {
	t, err := parseYYYYMM(yyyymm)
	if err != nil {
		return releasesURL
	}
	return releasesURL + "/gdpmonthlyestimateuk" + strings.ToLower(t.Format("January2006"))
}

func previousMonth(period string) (string, error) {
	t, err := parseYYYYMM(period)
	if err != nil {
		return "", err
	}
	return t.AddDate(0, -1, 0).Format("2006-01"), nil
}

func sameMonthPriorYear(period string) (string, error) {
	t, err := parseYYYYMM(period)
	if err != nil {
		return "", err
	}
	return t.AddDate(-1, 0, 0).Format("2006-01"), nil
}

func absoluteONSURL(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://") {
		return href
	}
	if strings.HasPrefix(href, "/") {
		return "https://www.ons.gov.uk" + href
	}
	return "https://www.ons.gov.uk/" + strings.TrimLeft(href, "/")
}

func normalizeURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	return strings.ToLower(raw)
}

func formatPercent(value float64) string {
	return strconv.FormatFloat(round1(value), 'f', 1, 64) + "%"
}

func round1(value float64) float64 {
	if math.Abs(value) == 0 {
		return 0
	}
	return math.Round(value*10) / 10
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	d -= time.Duration(minutes) * time.Minute
	seconds := int(d / time.Second)
	millis := int((d - time.Duration(seconds)*time.Second) / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}

func formatLatency(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return fmt.Sprintf("%s%.3fs", sign, d.Seconds())
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func blankNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/A"
	}
	return s
}

func shortError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 180 {
		return s
	}
	return s[:180] + "..."
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

func cloneResult(r *Result) *Result {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Warnings = append([]string(nil), r.Warnings...)
	cp.MatchedSources = append([]string(nil), r.MatchedSources...)
	return &cp
}
