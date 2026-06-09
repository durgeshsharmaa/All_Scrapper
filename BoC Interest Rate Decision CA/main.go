package main

import (
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
	"time"

	_ "time/tzdata"
)

// ============================================================================
// EVENT CONFIGURATION - CHANGE THIS FOR NEXT BOC RELEASE
// ============================================================================
//
// Event: Bank of Canada Interest Rate Decision
// Screenshot schedule: Jun 10, 2026 19:15 IST
// Official source schedule: Bank of Canada policy interest-rate page
// Format: "YYYY-MM-DD HH:MM:SS" in UTC
const eventTimeUTC = "2026-06-10 13:45:00"
const expectedReleaseDate = "2026-06-10"

// ============================================================================

const (
	country              = "CA"
	eventName            = "BoC Interest Rate Decision"
	officialRelease      = "Bank of Canada policy rate announcement"
	targetField          = "Target for the overnight rate"
	unitPercent          = "%"
	defaultReqTimeout    = 5 * time.Second
	testConnectionLead   = 1 * time.Minute
	sniperLead           = 2 * time.Second
	pollWindow           = 3 * time.Minute
	pollEvery            = 500 * time.Millisecond
	browserLikeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

	pressReleaseListURL     = "https://www.bankofcanada.ca/press/press-releases/"
	policyRatePageURL       = "https://www.bankofcanada.ca/core-functions/monetary-policy/key-interest-rate/"
	policyInstrumentPageURL = "https://www.bankofcanada.ca/rates/indicators/key-variables/policy-instrument/"
	policyInstrumentDataURL = "https://www.bankofcanada.ca/valet/observations/group/ATABLE_POLICY_INSTRUMENT/json?recent=1"
	policyInstrumentSeries  = "STATIC_ATABLE_V39079"
)

var (
	sources = []Source{
		{
			Name:        "Bank of Canada press-release listing",
			URL:         pressReleaseListURL,
			SourceType:  "Official HTML",
			Kind:        "primary",
			Priority:    1,
			ValueMethod: "Direct overnight-rate sentence parse",
		},
		{
			Name:        "Bank of Canada policy interest rate page",
			URL:         policyRatePageURL,
			SourceType:  "Official HTML",
			Kind:        "policy-rate-page",
			Priority:    2,
			ValueMethod: "Recent data Target (%) + schedule validation",
		},
		{
			Name:        "Bank of Canada policy instrument",
			URL:         policyInstrumentPageURL,
			FetchURL:    policyInstrumentDataURL,
			SourceType:  "Official JSON data",
			Kind:        "policy-instrument",
			Priority:    3,
			ValueMethod: "Direct current value",
		},
	}

	anchorRE        = regexp.MustCompile(`(?is)<a\s+[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	scriptStyleRE   = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	breakTagRE      = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockTagRE      = regexp.MustCompile(`(?is)</?(p|div|section|article|header|footer|main|h[1-6]|li|tr|table|thead|tbody|tfoot|caption)\b[^>]*>`)
	tagRE           = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE         = regexp.MustCompile(`\s+`)
	dateRE          = regexp.MustCompile(`(?i)\b(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2}),\s+(20\d{2})\b`)
	maintainTitleRE = regexp.MustCompile(`(?i)^Bank of Canada maintains policy rate at\s+\S+\s*%`)
	changeTitleRE   = regexp.MustCompile(`(?i)^Bank of Canada (?:reduces|increases) policy rate by\s+\d+(?:\.\d+)?\s+basis points?\b`)
	scheduleYearRE  = regexp.MustCompile(`(?i)\bSchedule\s+for\s+(20\d{2})\b`)

	fractionChars       = "\u00bc\u00bd\u00be\u215b\u215c\u215d\u215e"
	rateValuePattern    = `([0-9]+(?:\.[0-9]+)?|[0-9]+[` + fractionChars + `])`
	overnightSentenceRE = regexp.MustCompile(`(?is)\b(The\s+Bank\s+of\s+Canada\s+today\s+[^.]{0,500}?\btarget\s+for\s+(?:the\s+)?overnight\s+rate\s+(?:at|to)\s+` + rateValuePattern + `\s*%(?:[^.]|\.[0-9]){0,500}\.)`)
	policyRateRowRE     = regexp.MustCompile(`(?i)\b(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2}),\s+(20\d{2})\s+` + rateValuePattern + `\s+(?:---|[-+]?\d+(?:\.\d+)?)`)
	scheduleDateRE      = regexp.MustCompile(`(?i)\b(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2})\s+Interest\s+rate\s+announcement\b`)
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
	RequestStarted    time.Time
	ResponseReceived  time.Time
	ResponseSizeBytes int
	HashSHA256        string
	LatencyMS         Latency
}

type Result struct {
	Source            Source
	ReleaseDate       string
	Title             string
	FetchURL          string
	Field             string
	Value             string
	NumericValue      float64
	Unit              string
	ValueMethod       string
	Sentence          string
	Confidence        string
	ScheduleYear      int
	ScheduledDates    []string
	ScheduleConfirmed bool
	DetectionMethod   string
	DetectedAt        time.Time
	EventLatencyMS    int64
	Warnings          []string
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
	ReleaseDate  string
	Value        string
}

type Detection struct {
	Result           *Result
	Meta             FetchMeta
	PollCount        int
	DetectedAt       time.Time
	LatencyFromEvent time.Duration
	Error            string
}

type ConsoleResult struct {
	Country           string   `json:"country"`
	EventName         string   `json:"event_name"`
	OfficialRelease   string   `json:"official_release"`
	Source            string   `json:"source"`
	SourceURL         string   `json:"source_url"`
	FetchURL          string   `json:"fetch_url"`
	SourceType        string   `json:"source_type"`
	ReleaseDate       string   `json:"release_date"`
	Title             string   `json:"title"`
	Field             string   `json:"field"`
	Actual            string   `json:"actual"`
	Unit              string   `json:"unit"`
	ValueMethod       string   `json:"value_method"`
	Confidence        string   `json:"confidence"`
	DetectionMethod   string   `json:"detection_method"`
	EventLatencyMS    int64    `json:"event_latency_ms"`
	Sentence          string   `json:"sentence"`
	ServerDateHeader  string   `json:"server_date_header"`
	ETag              string   `json:"etag"`
	LastModified      string   `json:"last_modified"`
	CacheControl      string   `json:"cache_control"`
	ContentHash       string   `json:"content_hash"`
	LatencyMS         Latency  `json:"latency_ms"`
	MatchedSources    []string `json:"matched_sources"`
	Warnings          []string `json:"warnings"`
	ScheduleConfirmed bool     `json:"schedule_confirmed"`
	ScheduledDates    []string `json:"scheduled_dates"`
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

	client := newHTTPClient()
	printHeader(eventTime)

	fmt.Println("Fetching current published data from official sources...")
	current := fetchAllSources(client, defaultReqTimeout)
	printSourceTable(current, expectedReleaseDate)
	if current["primary"].Result == nil {
		fmt.Printf("Startup error: primary official source unavailable: %s\n", blankNA(current["primary"].Error))
		return
	}
	if current["policy-rate-page"].Result == nil {
		fmt.Printf("Startup error: official schedule source unavailable: %s\n", blankNA(current["policy-rate-page"].Error))
		return
	}
	fmt.Println("Current data captured. Waiting for the configured announcement time.")
	fmt.Println()

	policySnapshot := current["policy-rate-page"]
	if policySnapshot.Result != nil && !policySnapshot.Result.ScheduleConfirmed {
		fmt.Printf("Configuration error: %s is not listed on the official 2026 Bank of Canada schedule.\n", expectedReleaseDate)
		return
	}

	testTime := eventTime.Add(-testConnectionLead)
	sniperStart := eventTime.Add(-sniperLead)
	endTime := eventTime.Add(pollWindow)
	now := time.Now().UTC()

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
	baselines := fetchAllSources(client, defaultReqTimeout)
	printSourceTable(baselines, expectedReleaseDate)
	if baselinePolicy := baselines["policy-rate-page"]; baselinePolicy.Result != nil && !baselinePolicy.Result.ScheduleConfirmed {
		fmt.Printf("Configuration error: %s is not listed on the official Bank of Canada schedule.\n", expectedReleaseDate)
		return
	}

	primaryBaseline := baselineFromSnapshot(baselines["primary"])
	now = time.Now().UTC()
	if now.Before(sniperStart) {
		fmt.Printf("Final countdown to sniper mode: %s\n", sniperStart.Sub(now).Round(time.Millisecond))
		countdownUntil(sniperStart, 100*time.Millisecond, "Starting sniper mode in")
	}

	fmt.Println()
	fmt.Println("SNIPER MODE ACTIVE")
	fmt.Println("Primary trigger: official Bank of Canada press-release listing")
	fmt.Println("Hybrid detection: headers every 500ms, content every 5th poll, immediate content fetch on header change")
	fmt.Println()

	ctx, cancel := context.WithDeadline(context.Background(), endTime)
	defer cancel()
	detection := pollPrimary(ctx, client, primaryBaseline, eventTime, logger)

	fmt.Println()
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Printf("%-6s %-42s %-18s %-14s %-12s %-10s\n", "RANK", "SOURCE", "UPDATE UTC", "LATENCY", "VALUE", "METHOD")
	if detection.Error != "" || detection.Result == nil {
		fmt.Printf("%-6s %-42s %-18s %-14s %-12s %-10s\n", "-", primarySource().Name, "-", "Pending", "Pending", "-")
		fmt.Printf("NOT_CONFIRMED: no confirmed %s decision detected for %s during the polling window.\n", eventName, expectedReleaseDate)
		if detection.Error != "" {
			fmt.Printf("Last error: %s\n", detection.Error)
		}
		return
	}

	backups := fetchAllSources(client, defaultReqTimeout)
	warnings, validationErr := validateConfirmation(detection.Result, backups)
	if validationErr != nil {
		fmt.Printf("%-6s %-42s %-18s %-14s %-12s %-10s\n", "#1", detection.Result.Source.Name, detection.DetectedAt.Format("15:04:05.000"), formatLatency(detection.LatencyFromEvent), detection.Result.Value, detection.Result.DetectionMethod)
		fmt.Printf("NOT_CONFIRMED: %v\n", validationErr)
		return
	}
	detection.Result.Warnings = append(detection.Result.Warnings, warnings...)
	detection.Result.Confidence = "HIGH"

	fmt.Printf("%-6s %-42s %-18s %-14s %-12s %-10s\n",
		"#1",
		detection.Result.Source.Name,
		detection.DetectedAt.Format("15:04:05.000"),
		formatLatency(detection.LatencyFromEvent),
		detection.Result.Value,
		detection.Result.DetectionMethod,
	)
	fmt.Printf("Winner: %s\n", detection.Result.Source.Name)
	fmt.Printf("Release Date: %s\n", detection.Result.ReleaseDate)
	fmt.Printf("%s: %s\n", targetField, detection.Result.Value)
	fmt.Printf("Detection Latency: %s from event time\n", formatLatency(detection.LatencyFromEvent))
	fmt.Printf("Decision Title: %s\n", detection.Result.Title)
	fmt.Printf("Decision URL: %s\n", detection.Result.FetchURL)
	fmt.Println()
	fmt.Println("JSON OUTPUT:")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(consoleResult(detection.Result, detection.Meta, backups)); err != nil {
		logger.Printf("json encode failed: %v", err)
	}
}

func primarySource() Source {
	return sources[0]
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   1200 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   1200 * time.Millisecond,
			ResponseHeaderTimeout: 1800 * time.Millisecond,
			ExpectContinueTimeout: 250 * time.Millisecond,
		},
	}
}

func fetchAllSources(client *http.Client, timeout time.Duration) map[string]Snapshot {
	out := make(map[string]Snapshot, len(sources))
	for _, source := range sources {
		out[source.Kind] = fetchSnapshot(client, source, timeout)
	}
	return out
}

func fetchSnapshot(client *http.Client, source Source, timeout time.Duration) Snapshot {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	body, meta, err := fetch(ctx, client, source.RequestURL(), true)
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}

	parseStart := time.Now()
	result, err := parseSource(source, body)
	meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
	if err != nil {
		return Snapshot{Meta: meta, Error: err.Error()}
	}
	result.Source = source
	return Snapshot{Result: result, Meta: meta}
}

func fetch(ctx context.Context, client *http.Client, url string, readBody bool) ([]byte, FetchMeta, error) {
	start := time.Now()
	meta := FetchMeta{RequestStarted: start.UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, meta, err
	}
	req.Header.Set("User-Agent", browserLikeUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-CA,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

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
	meta.LatencyMS.TTFB = time.Since(start).Milliseconds()

	if !readBody {
		meta.ResponseReceived = time.Now().UTC()
		meta.LatencyMS.Total = time.Since(start).Milliseconds()
		return nil, meta, nil
	}

	bodyStart := time.Now()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	meta.ResponseReceived = time.Now().UTC()
	meta.ResponseSizeBytes = len(body)
	meta.LatencyMS.BodyRead = time.Since(bodyStart).Milliseconds()
	meta.LatencyMS.Total = time.Since(start).Milliseconds()
	hash := sha256.Sum256(body)
	meta.HashSHA256 = hex.EncodeToString(hash[:])

	if readErr != nil {
		return body, meta, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return body, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(body))
	}
	return body, meta, nil
}

func parseSource(source Source, body []byte) (*Result, error) {
	switch source.Kind {
	case "primary":
		return parsePressReleaseListing(source, body)
	case "policy-rate-page":
		return parsePolicyRatePage(source, body)
	case "policy-instrument":
		return parsePolicyInstrumentData(source, body)
	default:
		return nil, fmt.Errorf("unknown source kind %q", source.Kind)
	}
}

func parsePressReleaseListing(source Source, body []byte) (*Result, error) {
	raw := string(body)
	var parserErrors []string
	for _, match := range anchorRE.FindAllStringSubmatchIndex(raw, -1) {
		href := raw[match[2]:match[3]]
		titleHTML := raw[match[4]:match[5]]
		title := compactSpaces(stripHTML(titleHTML))
		if !isTargetDecisionTitle(title) {
			continue
		}

		before := raw[max(0, match[0]-1800):match[0]]
		releaseDate, err := parseLastReleaseDate(stripHTML(before))
		if err != nil {
			parserErrors = append(parserErrors, fmt.Sprintf("%q: %v", title, err))
			continue
		}

		after := raw[match[1]:min(len(raw), match[1]+3000)]
		sentence, value, valueDisplay, err := parseOvernightRateSentence(stripHTML(after))
		if err != nil {
			parserErrors = append(parserErrors, fmt.Sprintf("%q: %v", title, err))
			continue
		}

		return &Result{
			Source:       source,
			ReleaseDate:  releaseDate,
			Title:        title,
			FetchURL:     absoluteBankURL(href),
			Field:        targetField,
			Value:        valueDisplay,
			NumericValue: value,
			Unit:         unitPercent,
			ValueMethod:  source.ValueMethod,
			Sentence:     sentence,
			Confidence:   "HIGH",
		}, nil
	}
	if len(parserErrors) > 0 {
		return nil, errors.New(strings.Join(parserErrors, "; "))
	}
	return nil, errors.New("no Bank of Canada policy-rate decision title found on press-release listing")
}

func parsePolicyRatePage(source Source, body []byte) (*Result, error) {
	text := compactSpaces(stripHTML(string(body)))
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "policy interest rate") {
		return nil, errors.New("policy interest rate page heading not found")
	}
	if !strings.Contains(lower, "target for the overnight rate") {
		return nil, errors.New("target for the overnight rate text not found")
	}

	currentDate, currentValue, currentDisplay, err := parsePolicyCurrentRate(text)
	if err != nil {
		return nil, err
	}
	scheduleYear, scheduleDates, err := parsePolicySchedule(text)
	if err != nil {
		return nil, err
	}

	return &Result{
		Source:            source,
		ReleaseDate:       currentDate,
		FetchURL:          source.URL,
		Field:             targetField,
		Value:             currentDisplay,
		NumericValue:      currentValue,
		Unit:              unitPercent,
		ValueMethod:       source.ValueMethod,
		Confidence:        "HIGH",
		ScheduleYear:      scheduleYear,
		ScheduledDates:    scheduleDates,
		ScheduleConfirmed: containsString(scheduleDates, expectedReleaseDate),
	}, nil
}

func parsePolicyCurrentRate(text string) (string, float64, string, error) {
	section := sectionBetween(text, "Recent data", "*As of")
	if section == "" {
		section = sectionBetween(text, "Recent data", "More data")
	}
	if section == "" {
		return "", 0, "", errors.New("Recent data section not found on policy-rate page")
	}
	match := policyRateRowRE.FindStringSubmatch(section)
	if match == nil {
		return "", 0, "", errors.New("current Target (%) row not found on policy-rate page")
	}
	month, err := monthNumber(match[1])
	if err != nil {
		return "", 0, "", err
	}
	day, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, "", err
	}
	year, err := strconv.Atoi(match[3])
	if err != nil {
		return "", 0, "", err
	}
	value, err := parseRatePercent(match[4])
	if err != nil {
		return "", 0, "", err
	}
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	return date, value, formatRateFromRaw(match[4], value), nil
}

func parsePolicySchedule(text string) (int, []string, error) {
	section := text
	lower := strings.ToLower(text)
	if start := strings.Index(lower, "schedule for"); start >= 0 {
		section = text[start:]
	}
	if end := strings.Index(strings.ToLower(section), "see blackout"); end >= 0 {
		section = section[:end]
	}
	yearMatch := scheduleYearRE.FindStringSubmatch(section)
	if yearMatch == nil {
		return 0, nil, errors.New("policy-rate schedule year not found")
	}
	year, err := strconv.Atoi(yearMatch[1])
	if err != nil {
		return 0, nil, err
	}

	var dates []string
	for _, match := range scheduleDateRE.FindAllStringSubmatch(section, -1) {
		month, err := monthNumber(match[1])
		if err != nil {
			return 0, nil, err
		}
		day, err := strconv.Atoi(match[2])
		if err != nil {
			return 0, nil, err
		}
		date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		dates = append(dates, date)
	}
	if len(dates) == 0 {
		return 0, nil, errors.New("no interest-rate announcement dates found in policy-rate schedule")
	}
	return year, dates, nil
}

type policyInstrumentResponse struct {
	SeriesDetail map[string]struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"seriesDetail"`
	Observations []struct {
		Date   string `json:"d"`
		Target struct {
			Value string `json:"v"`
		} `json:"STATIC_ATABLE_V39079"`
	} `json:"observations"`
}

func parsePolicyInstrumentData(source Source, body []byte) (*Result, error) {
	var payload policyInstrumentResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("policy-instrument JSON parse failed: %w", err)
	}
	detail, ok := payload.SeriesDetail[policyInstrumentSeries]
	if !ok {
		return nil, fmt.Errorf("policy-instrument target series %s not found", policyInstrumentSeries)
	}
	label := strings.ToLower(detail.Label + " " + detail.Description)
	if !strings.Contains(label, "overnight rate") || !strings.Contains(label, "target") {
		return nil, fmt.Errorf("policy-instrument series %s is not the overnight-rate target", policyInstrumentSeries)
	}

	for i := len(payload.Observations) - 1; i >= 0; i-- {
		observation := payload.Observations[i]
		rawValue := strings.TrimSpace(observation.Target.Value)
		if rawValue == "" {
			continue
		}
		date := strings.TrimSpace(observation.Date)
		if _, err := parseISODate(date); err != nil {
			return nil, fmt.Errorf("policy-instrument observation date invalid: %w", err)
		}
		value, err := parseRatePercent(rawValue)
		if err != nil {
			return nil, err
		}
		return &Result{
			Source:       source,
			ReleaseDate:  date,
			FetchURL:     source.RequestURL(),
			Field:        "Target for the Overnight Rate",
			Value:        formatRateFromRaw(rawValue, value),
			NumericValue: value,
			Unit:         unitPercent,
			ValueMethod:  source.ValueMethod,
			Confidence:   "HIGH",
		}, nil
	}
	return nil, errors.New("current target value not found on policy-instrument data page")
}

func pollPrimary(ctx context.Context, client *http.Client, baseline Baseline, eventTime time.Time, logger *log.Logger) Detection {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	var lastErr string
	pollCount := 0
	source := primarySource()
	for {
		select {
		case <-ctx.Done():
			return Detection{Error: firstNonEmpty(lastErr, ctx.Err().Error())}
		default:
		}

		pollCount++
		checkContent := pollCount%5 == 0
		reqCtx, cancel := context.WithTimeout(ctx, defaultReqTimeout)
		body, meta, err := fetch(reqCtx, client, source.URL, checkContent)
		cancel()
		if err != nil {
			lastErr = err.Error()
			logger.Printf("poll %d failed: %v", pollCount, err)
		} else {
			headersChanged := (meta.ETag != "" && meta.ETag != baseline.ETag) ||
				(meta.LastModified != "" && meta.LastModified != baseline.LastModified)

			var result *Result
			var parseErr error
			contentChanged := false
			if checkContent {
				parseStart := time.Now()
				result, parseErr = parsePressReleaseListing(source, body)
				meta.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
				if parseErr != nil {
					lastErr = parseErr.Error()
					logger.Printf("poll %d parse failed: %v", pollCount, parseErr)
				} else {
					contentChanged = meta.HashSHA256 != baseline.ContentHash ||
						result.ReleaseDate != baseline.ReleaseDate ||
						result.Value != baseline.Value
				}
			}

			if headersChanged && !checkContent {
				reqCtx2, cancel2 := context.WithTimeout(ctx, defaultReqTimeout)
				body2, meta2, err2 := fetch(reqCtx2, client, source.URL, true)
				cancel2()
				if err2 == nil {
					parseStart := time.Now()
					result, parseErr = parsePressReleaseListing(source, body2)
					meta2.LatencyMS.Parse = time.Since(parseStart).Milliseconds()
					if parseErr == nil {
						meta = meta2
						contentChanged = meta.HashSHA256 != baseline.ContentHash ||
							result.ReleaseDate != baseline.ReleaseDate ||
							result.Value != baseline.Value
					}
				} else {
					lastErr = err2.Error()
				}
			}

			if result != nil && (headersChanged || contentChanged) {
				method := detectionMethod(headersChanged, contentChanged)
				validationErr := validatePrimary(result)
				if validationErr != nil {
					lastErr = validationErr.Error()
					logger.Printf("poll %d not confirmed: %v", pollCount, validationErr)
				} else {
					detectedAt := time.Now().UTC()
					result.DetectionMethod = method
					result.DetectedAt = detectedAt
					result.EventLatencyMS = detectedAt.Sub(eventTime).Milliseconds()
					return Detection{
						Result:           result,
						Meta:             meta,
						PollCount:        pollCount,
						DetectedAt:       detectedAt,
						LatencyFromEvent: detectedAt.Sub(eventTime),
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return Detection{Error: firstNonEmpty(lastErr, ctx.Err().Error())}
		case <-ticker.C:
		}
	}
}

func validatePrimary(result *Result) error {
	if result == nil {
		return errors.New("no parsed primary result")
	}
	if result.ReleaseDate != expectedReleaseDate {
		return fmt.Errorf("stale primary source: expected %s but latest decision is %s", expectedReleaseDate, result.ReleaseDate)
	}
	if result.NumericValue < -1 || result.NumericValue > 25 {
		return fmt.Errorf("overnight target %.4f%% is outside expected bounds", result.NumericValue)
	}
	return nil
}

func validateConfirmation(primary *Result, snapshots map[string]Snapshot) ([]string, error) {
	if err := validatePrimary(primary); err != nil {
		return nil, err
	}
	var warnings []string
	var matched []string
	matched = append(matched, primary.Source.Name)

	policy := snapshots["policy-rate-page"]
	if policy.Error != "" || policy.Result == nil {
		warnings = append(warnings, "Source 2 unavailable; schedule/current-rate validation skipped.")
	} else {
		if !policy.Result.ScheduleConfirmed {
			return nil, fmt.Errorf("expected release date %s is not listed in Source 2 schedule", expectedReleaseDate)
		}
		warnings = append(warnings, fmt.Sprintf("Source 2 schedule confirmed %s.", expectedReleaseDate))
		if !floatEqual(primary.NumericValue, policy.Result.NumericValue) {
			return nil, fmt.Errorf("Source 1 overnight target %s differs from Source 2 current target %s", primary.Value, policy.Result.Value)
		}
		if policy.Result.ReleaseDate != primary.ReleaseDate {
			warnings = append(warnings, fmt.Sprintf("Source 2 current target is dated %s; primary decision date is %s.", policy.Result.ReleaseDate, primary.ReleaseDate))
		}
		matched = append(matched, policy.Result.Source.Name)
	}

	instrument := snapshots["policy-instrument"]
	if instrument.Error != "" || instrument.Result == nil {
		warnings = append(warnings, "Source 3 unavailable; direct current-target validation skipped.")
	} else {
		if !floatEqual(primary.NumericValue, instrument.Result.NumericValue) {
			return nil, fmt.Errorf("Source 1 overnight target %s differs from Source 3 current target %s", primary.Value, instrument.Result.Value)
		}
		if instrument.Result.ReleaseDate != primary.ReleaseDate {
			warnings = append(warnings, fmt.Sprintf("Source 3 current target is dated %s; primary decision date is %s.", instrument.Result.ReleaseDate, primary.ReleaseDate))
		}
		matched = append(matched, instrument.Result.Source.Name)
	}

	primary.Warnings = append(primary.Warnings, "Matched official sources: "+strings.Join(matched, ", "))
	return warnings, nil
}

func consoleResult(result *Result, meta FetchMeta, snapshots map[string]Snapshot) ConsoleResult {
	matched := []string{result.Source.Name}
	if policy := snapshots["policy-rate-page"]; policy.Result != nil && policy.Error == "" && floatEqual(policy.Result.NumericValue, result.NumericValue) {
		matched = append(matched, policy.Result.Source.Name)
	}
	if instrument := snapshots["policy-instrument"]; instrument.Result != nil && instrument.Error == "" && floatEqual(instrument.Result.NumericValue, result.NumericValue) {
		matched = append(matched, instrument.Result.Source.Name)
	}
	scheduledDates := []string{}
	scheduleConfirmed := false
	if policy := snapshots["policy-rate-page"]; policy.Result != nil {
		scheduledDates = policy.Result.ScheduledDates
		scheduleConfirmed = policy.Result.ScheduleConfirmed
	}

	return ConsoleResult{
		Country:           country,
		EventName:         eventName,
		OfficialRelease:   officialRelease,
		Source:            result.Source.Name,
		SourceURL:         result.Source.URL,
		FetchURL:          result.FetchURL,
		SourceType:        result.Source.SourceType,
		ReleaseDate:       result.ReleaseDate,
		Title:             result.Title,
		Field:             result.Field,
		Actual:            result.Value,
		Unit:              result.Unit,
		ValueMethod:       result.ValueMethod,
		Confidence:        result.Confidence,
		DetectionMethod:   result.DetectionMethod,
		EventLatencyMS:    result.EventLatencyMS,
		Sentence:          result.Sentence,
		ServerDateHeader:  meta.ServerDate,
		ETag:              meta.ETag,
		LastModified:      meta.LastModified,
		CacheControl:      meta.CacheControl,
		ContentHash:       meta.HashSHA256,
		LatencyMS:         meta.LatencyMS,
		MatchedSources:    uniqueStrings(matched),
		Warnings:          uniqueStrings(result.Warnings),
		ScheduleConfirmed: scheduleConfirmed,
		ScheduledDates:    scheduledDates,
	}
}

func baselineFromSnapshot(snapshot Snapshot) Baseline {
	b := Baseline{}
	b.ETag = snapshot.Meta.ETag
	b.LastModified = snapshot.Meta.LastModified
	b.ContentHash = snapshot.Meta.HashSHA256
	if snapshot.Result != nil {
		b.ReleaseDate = snapshot.Result.ReleaseDate
		b.Value = snapshot.Result.Value
	}
	return b
}

func printHeader(eventTime time.Time) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	et, _ := time.LoadLocation("America/Toronto")
	fmt.Println("=======================================================================")
	fmt.Println("BoC Interest Rate Decision CA - SNIPER MODE")
	fmt.Println("=======================================================================")
	fmt.Printf("Event Time (IST): %s\n", eventTime.In(ist).Format("2006-01-02 15:04:05"))
	if et != nil {
		fmt.Printf("Event Time (ET):  %s\n", eventTime.In(et).Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Printf("Event Time (UTC): %s\n", eventTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Current Time UTC: %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Printf("Expected Release Date: %s\n", expectedReleaseDate)
	fmt.Printf("Field: %s\n", targetField)
	fmt.Println("Ignored: Bank Rate, deposit rate")
	fmt.Println("=======================================================================")
	fmt.Println()
}

func printSourceTable(snapshots map[string]Snapshot, expectedDate string) {
	ordered := append([]Source(nil), sources...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	for _, source := range ordered {
		snapshot := snapshots[source.Kind]
		if snapshot.Error != "" {
			fmt.Printf("  ERR  [%-44s] %s\n", source.Name, shortError(snapshot.Error))
			continue
		}
		if snapshot.Result == nil {
			fmt.Printf("  WAIT [%-44s] no parsed result\n", source.Name)
			continue
		}
		extra := ""
		if source.Kind == "policy-rate-page" {
			extra = fmt.Sprintf(" Schedule %s: %t", expectedDate, snapshot.Result.ScheduleConfirmed)
		}
		fmt.Printf("  OK   [%-44s] Date: %-10s Value: %-8s%s\n",
			source.Name,
			snapshot.Result.ReleaseDate,
			snapshot.Result.Value,
			extra,
		)
	}
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

func detectionMethod(headersChanged, contentChanged bool) string {
	if headersChanged && contentChanged {
		return "headers+content"
	}
	if headersChanged {
		return "headers"
	}
	return "content"
}

func isTargetDecisionTitle(title string) bool {
	title = compactSpaces(title)
	return maintainTitleRE.MatchString(title) || changeTitleRE.MatchString(title)
}

func parseOvernightRateSentence(text string) (string, float64, string, error) {
	text = compactSpaces(text)
	match := overnightSentenceRE.FindStringSubmatch(text)
	if match == nil {
		return "", 0, "", errors.New("target-for-the-overnight-rate sentence not found")
	}
	value, err := parseRatePercent(match[2])
	if err != nil {
		return "", 0, "", err
	}
	return compactSpaces(match[1]), value, formatRateFromRaw(match[2], value), nil
}

func parseRatePercent(raw string) (float64, error) {
	s := strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	if s == "" {
		return 0, errors.New("empty rate value")
	}
	var numeric strings.Builder
	fraction := 0.0
	for _, r := range s {
		if value, ok := unicodeFractionValue(r); ok {
			fraction += value
			continue
		}
		numeric.WriteRune(r)
	}
	baseText := strings.TrimSpace(numeric.String())
	base := 0.0
	if baseText != "" {
		parsed, err := strconv.ParseFloat(baseText, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid rate value %q: %w", raw, err)
		}
		base = parsed
	}
	return base + fraction, nil
}

func unicodeFractionValue(r rune) (float64, bool) {
	switch r {
	case '\u00bc':
		return 0.25, true
	case '\u00bd':
		return 0.50, true
	case '\u00be':
		return 0.75, true
	case '\u215b':
		return 0.125, true
	case '\u215c':
		return 0.375, true
	case '\u215d':
		return 0.625, true
	case '\u215e':
		return 0.875, true
	default:
		return 0, false
	}
}

func formatRateFromRaw(raw string, value float64) string {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	if raw != "" && !containsUnicodeFraction(raw) {
		return raw + "%"
	}
	return trimFloat(value, 3) + "%"
}

func containsUnicodeFraction(s string) bool {
	for _, r := range s {
		if _, ok := unicodeFractionValue(r); ok {
			return true
		}
	}
	return false
}

func parseLastReleaseDate(text string) (string, error) {
	matches := dateRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return "", errors.New("release date not found before decision title")
	}
	match := matches[len(matches)-1]
	month, err := monthNumber(match[1])
	if err != nil {
		return "", err
	}
	day, err := strconv.Atoi(match[2])
	if err != nil {
		return "", err
	}
	year, err := strconv.Atoi(match[3])
	if err != nil {
		return "", err
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), nil
}

func parseISODate(input string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(input))
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, got %q", input)
	}
	return t, nil
}

func monthNumber(name string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "january":
		return 1, nil
	case "february":
		return 2, nil
	case "march":
		return 3, nil
	case "april":
		return 4, nil
	case "may":
		return 5, nil
	case "june":
		return 6, nil
	case "july":
		return 7, nil
	case "august":
		return 8, nil
	case "september":
		return 9, nil
	case "october":
		return 10, nil
	case "november":
		return 11, nil
	case "december":
		return 12, nil
	default:
		return 0, fmt.Errorf("unknown month %q", name)
	}
}

func absoluteBankURL(href string) string {
	href = strings.TrimSpace(html.UnescapeString(href))
	if strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://") {
		return href
	}
	if strings.HasPrefix(href, "/") {
		return "https://www.bankofcanada.ca" + href
	}
	return "https://www.bankofcanada.ca/" + strings.TrimLeft(href, "/")
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
		"\r", " ",
		"\n", " ",
		"\t", " ",
	)
	return spaceRE.ReplaceAllString(replacer.Replace(s), " ")
}

func compactSpaces(s string) string {
	return strings.TrimSpace(normalizeWhitespace(s))
}

func sectionBetween(text, startMarker, endMarker string) string {
	lower := strings.ToLower(text)
	start := strings.Index(lower, strings.ToLower(startMarker))
	if start < 0 {
		return ""
	}
	sectionStart := start + len(startMarker)
	end := -1
	if strings.TrimSpace(endMarker) != "" {
		end = strings.Index(strings.ToLower(text[sectionStart:]), strings.ToLower(endMarker))
	}
	if end < 0 {
		return strings.TrimSpace(text[sectionStart:])
	}
	return strings.TrimSpace(text[sectionStart : sectionStart+end])
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

func trimFloat(value float64, decimals int) string {
	if math.Abs(value) == 0 {
		value = 0
	}
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-0" {
		return "0"
	}
	return text
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
