package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	country              = "US"
	eventName            = "Private Nonfarm Payrolls"
	totalPrivateSeriesID = "CES0500000001"

	eventTimeUTCString    = "2026-06-05 12:30:00"
	expectedReleasePeriod = "2026-05"
	expectedForecastValue = "85K"

	testConnectionLead = 1 * time.Minute
	sniperLead         = 2 * time.Second
	pollWindow         = 3 * time.Minute
	headerPollEvery    = 500 * time.Millisecond
	contentEveryNPolls = 5
	requestTimeout     = 5 * time.Second
	apiRequestTimeout  = 15 * time.Second
	apiMaxAttempts     = 2
	thirdPartyTimeout  = 2 * time.Minute
	thirdPartyEvery    = 1 * time.Second

	officialUserAgent    = "PrivateNonfarmPayrollsUS/1.0 (official BLS sniper scraper; contact=research@example.com)"
	browserLikeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0 Safari/537.36"
)

var (
	primarySource = Source{
		Name:       "BLS Table B-1 official news release table",
		URL:        "https://www.bls.gov/news.release/empsit.t17.htm",
		SourceType: "HTML table",
		Kind:       "bls-table-b1",
		Primary:    true,
		Official:   true,
		Priority:   1,
	}
	fallbackSource = Source{
		Name:       "BLS CES flat text file, total private employment",
		URL:        "https://download.bls.gov/pub/time.series/ce/ce.data.05a.TotalPrivate.Employment",
		SourceType: "BLS flat text time-series",
		Kind:       "ces-flat-file",
		Primary:    false,
		Official:   true,
		Priority:   3,
	}
	apiSource = Source{
		Name:       "BLS Public API CES0500000001",
		URL:        "https://api.bls.gov/publicAPI/v2/timeseries/data/CES0500000001",
		SourceType: "BLS Public Data API JSON",
		Kind:       "bls-api",
		Primary:    false,
		Official:   true,
		Priority:   2,
	}
	flatAllCESSource = Source{
		Name:       "BLS CES flat file AllCESSeries",
		URL:        "https://download.bls.gov/pub/time.series/ce/ce.data.0.AllCESSeries",
		SourceType: "BLS CES flat text time-series",
		Kind:       "ces-all-flat-file",
		Primary:    false,
		Official:   true,
		Priority:   3,
	}
	thirdPartySource = Source{
		Name:       "Investing.com economic calendar",
		URL:        "https://www.investing.com/economic-calendar/private-nonfarm-payrolls-528",
		SourceType: "Third-party HTML",
		Kind:       "investing-calendar",
		Primary:    false,
		Official:   false,
		Priority:   99,
	}
	numberRE        = regexp.MustCompile(`[-+]?\d[\d,]*(?:\.\d+)?`)
	preRE           = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`)
	scriptStyleRE   = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	breakTagRE      = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockCloseRE    = regexp.MustCompile(`(?is)</(tr|div|p|h[1-6]|li|table|thead|tbody|tfoot|caption)>`)
	cellCloseRE     = regexp.MustCompile(`(?is)</t[dh]>`)
	tagRE           = regexp.MustCompile(`(?is)<[^>]+>`)
	tableRowRE      = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	headerCellRE    = regexp.MustCompile(`(?is)<th\b[^>]*>(.*?)</th>`)
	dataCellRE      = regexp.MustCompile(`(?is)<td\b[^>]*>(.*?)</td>`)
	prelimMarkerRE  = regexp.MustCompile(`(?i)\([pr]\)`)
	pagePeriodRE    = regexp.MustCompile(`(?i)(\d{4})\s+M(0[1-9]|1[0-2])\s+Results`)
	changePeriodRE  = regexp.MustCompile(`(?i)Change from:\s*([A-Za-z]{3,9})\.?\s*(\d{4})\s*-\s*([A-Za-z]{3,9})\.?\s*(\d{4})`)
	rowLabelRE      = regexp.MustCompile(`(?i)^Total private(?:\s+|$)(.*)$`)
	investingDateRE = regexp.MustCompile(`(?i)^([A-Za-z]{3,9})\.?\s+(\d{1,2}),\s+(\d{4})\s+\(([A-Za-z]{3,9})\.?\)$`)
)

type Source struct {
	Name       string
	URL        string
	SourceType string
	Kind       string
	Primary    bool
	Official   bool
	Priority   int
}

type FetchMeta struct {
	Status               string
	StatusCode           int
	ServerDate           string
	LastModified         string
	ETag                 string
	RequestStarted       time.Time
	FirstByteReceived    time.Time
	ResponseReceived     time.Time
	Latency              time.Duration
	TimeToFirstByte      time.Duration
	ResponseSizeBytes    int
	NetworkErrorOccurred bool
}

type ParsedValue struct {
	Source            Source
	Method            string
	Table             string
	Row               string
	Column            string
	Unit              string
	Period            string
	PeriodYYYYMM      string
	FromPeriod        string
	FromPeriodYYYYMM  string
	PagePeriodYYYYMM  string
	ActualThousands   float64
	PreviousThousands float64
	ForecastThousands float64
	ForecastAvailable bool
	Forecast          string
	Previous          string
	Confidence        string
	Warnings          []string
	ValidationFailure string
}

type periodInfo struct {
	FromDisplay string
	FromYYYYMM  string
	ToDisplay   string
	ToYYYYMM    string
}

type cesObservation struct {
	YYYYMM  string
	Display string
	Value   float64
}

type Baseline struct {
	Source          Source
	ETag            string
	LastModified    string
	PeriodYYYYMM    string
	Period          string
	ActualThousands float64
	ValueSet        bool
	Error           string
}

type Detection struct {
	Source           Source
	Result           *ParsedValue
	Meta             FetchMeta
	Method           string
	PollCount        int
	DetectedAt       time.Time
	LatencyFromEvent time.Duration
	Error            string
}

type ThirdPartyCheck struct {
	DetectedAt      time.Time
	DelayMS         int64
	ValueMatched    bool
	ForecastMatched string
	PreviousMatched string
	Actual          string
	Forecast        string
	Previous        string
	Period          string
	Error           string
}

func main() {
	eventTime, err := time.ParseInLocation("2006-01-02 15:04:05", eventTimeUTCString, time.UTC)
	if err != nil {
		fmt.Printf("Configuration error: invalid eventTimeUTCString: %v\n", err)
		os.Exit(1)
	}
	expectedPeriod, err := normalizeExpectedPeriod(expectedReleasePeriod)
	if err != nil {
		fmt.Printf("Configuration error: invalid expectedReleasePeriod: %v\n", err)
		os.Exit(1)
	}
	expectedForecast, expectedForecastSet, err := parseKValue(expectedForecastValue)
	if err != nil {
		fmt.Printf("Configuration error: invalid expectedForecastValue: %v\n", err)
		os.Exit(1)
	}

	client := newHTTPClient()
	sources := []Source{primarySource, apiSource, flatAllCESSource}

	printHeader(eventTime)
	fmt.Println("Fetching current published official data...")
	current := fetchAllCurrent(client, sources)
	printCurrentSnapshot(current)

	testTime := eventTime.Add(-testConnectionLead)
	sniperStart := eventTime.Add(-sniperLead)
	endTime := eventTime.Add(pollWindow)
	now := time.Now().UTC()

	if now.Before(testTime) {
		fmt.Printf("Countdown to connection test: %s\n", testTime.Sub(now).Round(time.Second))
		countdownUntil(testTime, time.Second)
	}

	fmt.Println("Testing connections and capturing baseline headers + content...")
	baselines := captureBaselines(client, sources)
	printBaselines(baselines)

	now = time.Now().UTC()
	if now.Before(sniperStart) {
		fmt.Printf("Final countdown to sniper mode: %s\n", sniperStart.Sub(now).Round(time.Millisecond))
		countdownUntil(sniperStart, 100*time.Millisecond)
	}

	if time.Now().UTC().After(endTime) {
		fmt.Println("Event polling window is already over. Update eventTimeUTCString for the next release.")
		printFinalTable(eventTime, sources, nil)
		return
	}

	fmt.Println("SNIPER MODE ACTIVE: official sources only trigger; third-party is verification only.")
	ctx, cancel := context.WithDeadline(context.Background(), endTime)
	defer cancel()

	resultsCh := make(chan Detection, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			pollOfficialSource(ctx, client, src, baselines[src.Name], expectedPeriod, eventTime, resultsCh)
		}(source)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var detections []Detection
	var firstOfficial *Detection
	var thirdParty *ThirdPartyCheck
	for detection := range resultsCh {
		detections = append(detections, detection)
		if detection.Error != "" || detection.Result == nil {
			continue
		}
		fmt.Printf("[%s] UPDATED [%s] Period: %s | Value: %s | Detected by: %s\n",
			detection.DetectedAt.Format("15:04:05.000"),
			detection.Source.Name,
			detection.Result.Period,
			formatK(detection.Result.ActualThousands),
			detection.Method,
		)
		if firstOfficial == nil {
			copyDetection := detection
			firstOfficial = &copyDetection
			check := verifyThirdParty(client, detection, expectedForecast, expectedForecastSet)
			thirdParty = &check
		}
	}

	printFinalTable(eventTime, sources, detections)
	if firstOfficial != nil {
		fmt.Printf("Winner: %s\n", firstOfficial.Source.Name)
		fmt.Printf("Updated Period: %s\n", firstOfficial.Result.Period)
		fmt.Printf("%s: %s\n", eventName, formatK(firstOfficial.Result.ActualThousands))
		fmt.Printf("Detection Latency: %+.3fs from event time\n", firstOfficial.LatencyFromEvent.Seconds())
	}
	if thirdParty != nil {
		printThirdPartyCheck(*thirdParty)
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
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func fetch(ctx context.Context, client *http.Client, source Source, userAgent string) ([]byte, FetchMeta, error) {
	meta := FetchMeta{RequestStarted: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, meta, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", acceptForSource(source))
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
		meta.NetworkErrorOccurred = true
		return nil, meta, err
	}
	defer resp.Body.Close()

	meta.ResponseReceived = time.Now().UTC()
	meta.Latency = meta.ResponseReceived.Sub(meta.RequestStarted)
	if !meta.FirstByteReceived.IsZero() {
		meta.TimeToFirstByte = meta.FirstByteReceived.Sub(meta.RequestStarted)
	}
	meta.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	meta.StatusCode = resp.StatusCode
	meta.ServerDate = resp.Header.Get("Date")
	meta.LastModified = resp.Header.Get("Last-Modified")
	meta.ETag = resp.Header.Get("ETag")

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	meta.ResponseSizeBytes = len(body)
	if readErr != nil {
		return body, meta, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return body, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(body))
	}
	return body, meta, nil
}

func fetchHeaders(ctx context.Context, client *http.Client, source Source) (FetchMeta, error) {
	meta := FetchMeta{RequestStarted: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source.URL, nil)
	if err != nil {
		return meta, err
	}
	req.Header.Set("User-Agent", userAgentForSource(source))
	req.Header.Set("Accept", acceptForSource(source))
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
		meta.NetworkErrorOccurred = true
		return meta, err
	}
	defer resp.Body.Close()

	meta.ResponseReceived = time.Now().UTC()
	meta.Latency = meta.ResponseReceived.Sub(meta.RequestStarted)
	if !meta.FirstByteReceived.IsZero() {
		meta.TimeToFirstByte = meta.FirstByteReceived.Sub(meta.RequestStarted)
	}
	meta.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	meta.StatusCode = resp.StatusCode
	meta.ServerDate = resp.Header.Get("Date")
	meta.LastModified = resp.Header.Get("Last-Modified")
	meta.ETag = resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return meta, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	return meta, nil
}

func fetchSourceContent(ctx context.Context, client *http.Client, source Source) (*ParsedValue, FetchMeta, error) {
	switch source.Kind {
	case "ces-all-flat-file":
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		return fetchAndParseCESAllSeries(reqCtx, client, source)
	case "bls-api":
		return fetchBLSAPIWithFallback(ctx, client, source)
	default:
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		body, meta, err := fetch(reqCtx, client, source, userAgentForSource(source))
		if err != nil {
			return nil, meta, err
		}
		parsed, err := parseSource(source, body)
		return parsed, meta, err
	}
}

func fetchBLSAPIWithFallback(ctx context.Context, client *http.Client, source Source) (*ParsedValue, FetchMeta, error) {
	var lastMeta FetchMeta
	var lastErr error
	for attempt := 1; attempt <= apiMaxAttempts; attempt++ {
		body, meta, err := fetchSourceAttemptTimeout(ctx, client, source, apiRequestTimeout)
		lastMeta = meta
		if err == nil {
			if attempt > 1 {
				meta.Status = fmt.Sprintf("%s (API attempt %d/%d)", meta.Status, attempt, apiMaxAttempts)
			}
			parsed, parseErr := parseSource(source, body)
			return parsed, meta, parseErr
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, lastMeta, ctx.Err()
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		}
	}
	return nil, lastMeta, fmt.Errorf("BLS API HTTPS unavailable after %d attempt(s); HTTP redirects back to HTTPS: %w", apiMaxAttempts, lastErr)
}

func fetchSourceAttempt(ctx context.Context, client *http.Client, source Source) ([]byte, FetchMeta, error) {
	return fetchSourceAttemptTimeout(ctx, client, source, requestTimeout)
}

func fetchSourceAttemptTimeout(ctx context.Context, client *http.Client, source Source, timeout time.Duration) ([]byte, FetchMeta, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fetch(reqCtx, client, source, userAgentForSource(source))
}

func parseSource(source Source, body []byte) (*ParsedValue, error) {
	switch source.Kind {
	case "bls-table-b1":
		return parseBLSTableB1(source, body)
	case "bls-api":
		return parseBLSAPI(source, body)
	case "ces-flat-file":
		return parseCESFlatFile(source, body)
	case "ces-all-flat-file":
		return parseCESFlatFile(source, body)
	default:
		return nil, fmt.Errorf("unsupported source kind %q", source.Kind)
	}
}

func userAgentForSource(source Source) string {
	if !source.Official {
		return browserLikeUserAgent
	}
	return officialUserAgent
}

func acceptForSource(source Source) string {
	switch source.Kind {
	case "bls-api":
		return "application/json,*/*;q=0.8"
	case "ces-flat-file", "ces-all-flat-file":
		return "text/plain,application/octet-stream,*/*;q=0.5"
	default:
		return "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5"
	}
}

func parseBLSTableB1(source Source, body []byte) (*ParsedValue, error) {
	raw := string(body)
	if looksLikeAccessDenied(raw) {
		return nil, errors.New("BLS returned an access-denied page")
	}

	text := extractText(raw)
	section := tableSection(text)
	sectionCompact := compactSpaces(section)
	if !strings.Contains(strings.ToLower(sectionCompact), "table b-1") ||
		!strings.Contains(strings.ToLower(sectionCompact), "employees on nonfarm payrolls") {
		return nil, errors.New("official Table B-1 heading not found")
	}
	if !strings.Contains(strings.ToLower(sectionCompact), "in thousands") {
		return nil, errors.New("Table B-1 unit marker [In thousands] not found")
	}

	changePeriod, err := extractChangePeriod(section)
	if err != nil {
		return nil, err
	}
	values, err := extractTotalPrivateValuesFromHTML(raw)
	if err != nil {
		values, err = extractTotalPrivateValues(section)
	}
	if err != nil {
		return nil, err
	}
	actual := values[len(values)-1]
	levelDiff := values[7] - values[6]
	if math.Abs(levelDiff-actual) > 0.05 {
		return nil, fmt.Errorf("Total private final change %.1f does not match seasonally adjusted level difference %.1f", actual, levelDiff)
	}
	previousChange := values[6] - values[5]

	return &ParsedValue{
		Source:            source,
		Method:            "Direct official table value",
		Table:             "B-1",
		Row:               "Total private",
		Column:            "Change from previous month",
		Unit:              "thousands",
		Period:            changePeriod.ToDisplay,
		PeriodYYYYMM:      changePeriod.ToYYYYMM,
		FromPeriod:        changePeriod.FromDisplay,
		FromPeriodYYYYMM:  changePeriod.FromYYYYMM,
		PagePeriodYYYYMM:  extractPagePeriod(raw),
		ActualThousands:   actual,
		PreviousThousands: previousChange,
		Forecast:          "N/A",
		Previous:          formatK(previousChange),
	}, nil
}

func parseCESFlatFile(source Source, body []byte) (*ParsedValue, error) {
	raw := string(body)
	if looksLikeAccessDenied(raw) {
		return nil, errors.New("BLS returned an access-denied page")
	}

	var observations []cesObservation
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[0] != totalPrivateSeriesID {
			continue
		}
		if len(fields[2]) != 3 || fields[2][0] != 'M' || fields[2] == "M13" {
			continue
		}
		year, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid CES year %q: %w", fields[1], err)
		}
		month, err := strconv.Atoi(strings.TrimPrefix(fields[2], "M"))
		if err != nil || month < 1 || month > 12 {
			return nil, fmt.Errorf("invalid CES period %q", fields[2])
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(fields[3], ",", ""), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid CES value %q: %w", fields[3], err)
		}
		yyyymm := fmt.Sprintf("%04d-%02d", year, month)
		observations = append(observations, cesObservation{
			YYYYMM:  yyyymm,
			Display: periodDisplay(yyyymm),
			Value:   value,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(observations) < 2 {
		return nil, fmt.Errorf("fewer than two %s observations found", totalPrivateSeriesID)
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].YYYYMM < observations[j].YYYYMM
	})
	latest := observations[len(observations)-1]
	previous := observations[len(observations)-2]

	return &ParsedValue{
		Source:            source,
		Method:            "Calculated official backup from CES seasonally adjusted levels",
		Table:             "CES flat file",
		Row:               "Total private",
		Column:            "Latest monthly level minus previous monthly level",
		Unit:              "thousands",
		Period:            latest.Display,
		PeriodYYYYMM:      latest.YYYYMM,
		FromPeriod:        previous.Display,
		FromPeriodYYYYMM:  previous.YYYYMM,
		ActualThousands:   latest.Value - previous.Value,
		PreviousThousands: previousMonthChange(observations),
		Forecast:          "N/A",
		Previous:          formatK(previousMonthChange(observations)),
		Warnings:          []string{"Fallback source used; the primary Table B-1 source is preferred because it publishes the change directly."},
	}, nil
}

type blsAPIResponse struct {
	Status        string        `json:"status"`
	ResponseMS    int           `json:"responseTime"`
	Messages      []string      `json:"message"`
	Results       blsAPIResults `json:"Results"`
	ErrorMessages []string      `json:"errorMessages"`
}

type blsAPIResults struct {
	Series []blsAPISeries `json:"series"`
}

type blsAPISeries struct {
	SeriesID string              `json:"seriesID"`
	Data     []blsAPIObservation `json:"data"`
}

type blsAPIObservation struct {
	Year       string `json:"year"`
	Period     string `json:"period"`
	PeriodName string `json:"periodName"`
	Latest     string `json:"latest"`
	Value      string `json:"value"`
}

func parseBLSAPI(source Source, body []byte) (*ParsedValue, error) {
	var response blsAPIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid BLS API JSON: %w", err)
	}
	if response.Status != "REQUEST_SUCCEEDED" {
		messages := append([]string{}, response.Messages...)
		messages = append(messages, response.ErrorMessages...)
		if len(messages) == 0 {
			messages = append(messages, "no BLS API message supplied")
		}
		return nil, fmt.Errorf("BLS API status %q: %s", response.Status, strings.Join(messages, "; "))
	}
	if len(response.Results.Series) != 1 {
		return nil, fmt.Errorf("expected one BLS API series, got %d", len(response.Results.Series))
	}
	series := response.Results.Series[0]
	if series.SeriesID != totalPrivateSeriesID {
		return nil, fmt.Errorf("expected series %s, got %s", totalPrivateSeriesID, series.SeriesID)
	}

	var observations []cesObservation
	for _, item := range series.Data {
		if item.Period == "M13" {
			continue
		}
		year, err := strconv.Atoi(item.Year)
		if err != nil {
			return nil, fmt.Errorf("invalid BLS API year %q: %w", item.Year, err)
		}
		month, err := strconv.Atoi(strings.TrimPrefix(item.Period, "M"))
		if err != nil || month < 1 || month > 12 {
			return nil, fmt.Errorf("invalid BLS API period %q", item.Period)
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(item.Value, ",", ""), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid BLS API value %q: %w", item.Value, err)
		}
		yyyymm := fmt.Sprintf("%04d-%02d", year, month)
		observations = append(observations, cesObservation{
			YYYYMM:  yyyymm,
			Display: periodDisplay(yyyymm),
			Value:   value,
		})
	}
	return buildCalculatedResult(source, "Calculated official backup from BLS API levels", "BLS Public API", observations)
}

func fetchAndParseCESAllSeries(ctx context.Context, client *http.Client, source Source) (*ParsedValue, FetchMeta, error) {
	meta := FetchMeta{RequestStarted: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, meta, err
	}
	req.Header.Set("User-Agent", userAgentForSource(source))
	req.Header.Set("Accept", acceptForSource(source))
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
		meta.NetworkErrorOccurred = true
		return nil, meta, err
	}
	defer resp.Body.Close()

	meta.ResponseReceived = time.Now().UTC()
	meta.Latency = meta.ResponseReceived.Sub(meta.RequestStarted)
	if !meta.FirstByteReceived.IsZero() {
		meta.TimeToFirstByte = meta.FirstByteReceived.Sub(meta.RequestStarted)
	}
	meta.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	meta.StatusCode = resp.StatusCode
	meta.ServerDate = resp.Header.Get("Date")
	meta.LastModified = resp.Header.Get("Last-Modified")
	meta.ETag = resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, meta, fmt.Errorf("unexpected HTTP status %s: %s", resp.Status, firstLine(body))
	}

	counting := &countingReader{reader: resp.Body}
	scanner := bufio.NewScanner(counting)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	var observations []cesObservation
	seenTarget := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[0] == "series_id" {
			continue
		}
		if seenTarget && fields[0] > totalPrivateSeriesID {
			break
		}
		if fields[0] != totalPrivateSeriesID {
			continue
		}
		seenTarget = true
		if fields[2] == "M13" {
			continue
		}
		year, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, meta, fmt.Errorf("invalid CES flat year %q: %w", fields[1], err)
		}
		month, err := strconv.Atoi(strings.TrimPrefix(fields[2], "M"))
		if err != nil || month < 1 || month > 12 {
			return nil, meta, fmt.Errorf("invalid CES flat period %q", fields[2])
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(fields[3], ",", ""), 64)
		if err != nil {
			return nil, meta, fmt.Errorf("invalid CES flat value %q: %w", fields[3], err)
		}
		yyyymm := fmt.Sprintf("%04d-%02d", year, month)
		observations = append(observations, cesObservation{
			YYYYMM:  yyyymm,
			Display: periodDisplay(yyyymm),
			Value:   value,
		})
	}
	meta.ResponseSizeBytes = int(counting.count)
	if err := scanner.Err(); err != nil {
		return nil, meta, err
	}
	parsed, err := buildCalculatedResult(source, "Calculated official backup from streamed CES flat-file levels", "CES flat file", observations)
	return parsed, meta, err
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func buildCalculatedResult(source Source, method string, table string, observations []cesObservation) (*ParsedValue, error) {
	if len(observations) < 2 {
		return nil, fmt.Errorf("fewer than two %s observations found", totalPrivateSeriesID)
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].YYYYMM < observations[j].YYYYMM
	})
	for i := 1; i < len(observations); i++ {
		if observations[i].YYYYMM == observations[i-1].YYYYMM {
			return nil, fmt.Errorf("duplicate observation for %s", observations[i].Display)
		}
	}
	latest := observations[len(observations)-1]
	previous := observations[len(observations)-2]
	expectedPrevious, err := previousYYYYMM(latest.YYYYMM)
	if err != nil {
		return nil, err
	}
	if previous.YYYYMM != expectedPrevious {
		return nil, fmt.Errorf("latest two observations are not consecutive: %s and %s", previous.Display, latest.Display)
	}
	previousChange := previousMonthChange(observations)
	return &ParsedValue{
		Source:            source,
		Method:            method,
		Table:             table,
		Row:               "Total private",
		Column:            "Latest monthly level minus previous monthly level",
		Unit:              "thousands",
		Period:            latest.Display,
		PeriodYYYYMM:      latest.YYYYMM,
		FromPeriod:        previous.Display,
		FromPeriodYYYYMM:  previous.YYYYMM,
		ActualThousands:   latest.Value - previous.Value,
		PreviousThousands: previousChange,
		Forecast:          "N/A",
		Previous:          formatK(previousChange),
	}, nil
}

func previousMonthChange(observations []cesObservation) float64 {
	if len(observations) < 3 {
		return 0
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].YYYYMM < observations[j].YYYYMM
	})
	previous := observations[len(observations)-2]
	prior := observations[len(observations)-3]
	expectedPrior, err := previousYYYYMM(previous.YYYYMM)
	if err != nil || prior.YYYYMM != expectedPrior {
		return 0
	}
	return previous.Value - prior.Value
}

type Snapshot struct {
	Source Source
	Result *ParsedValue
	Meta   FetchMeta
	Error  string
}

func printHeader(eventTime time.Time) {
	ist := time.FixedZone("IST", 5*60*60+30*60)
	fmt.Println("============================================================")
	fmt.Printf("%s Scraper - SNIPER MODE\n", eventName)
	fmt.Println("============================================================")
	fmt.Printf("Event Time (IST): %s\n", eventTime.In(ist).Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("Event Time (UTC): %s\n", eventTime.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("Expected Period: %s\n", periodDisplay(expectedReleasePeriod))
	fmt.Printf("Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05.000 MST"))
	fmt.Println("Official trigger sources: BLS Table B-1, BLS API, BLS CES flat files")
	fmt.Println("Third-party page: verification only, never primary trigger")
	fmt.Println("============================================================")
}

func fetchAllCurrent(client *http.Client, sources []Source) []Snapshot {
	var wg sync.WaitGroup
	out := make(chan Snapshot, len(sources))
	for _, source := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			result, meta, err := fetchSourceContent(context.Background(), client, src)
			snapshot := Snapshot{Source: src, Result: result, Meta: meta}
			if err != nil {
				snapshot.Error = err.Error()
			}
			out <- snapshot
		}(source)
	}
	wg.Wait()
	close(out)
	var snapshots []Snapshot
	for snapshot := range out {
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Source.Priority < snapshots[j].Source.Priority
	})
	return snapshots
}

func printCurrentSnapshot(snapshots []Snapshot) {
	fmt.Println("Current published data:")
	for _, snapshot := range snapshots {
		if snapshot.Error != "" {
			fmt.Printf("  FAIL [%-36s] %s\n", snapshot.Source.Name, snapshot.Error)
			continue
		}
		fmt.Printf("  OK   [%-36s] Period: %-8s Value: %-8s Status: %s\n",
			snapshot.Source.Name,
			snapshot.Result.Period,
			formatK(snapshot.Result.ActualThousands),
			blankNA(snapshot.Meta.Status),
		)
	}
	fmt.Println("------------------------------------------------------------")
}

func captureBaselines(client *http.Client, sources []Source) map[string]Baseline {
	baselines := make(map[string]Baseline, len(sources))
	snapshots := fetchAllCurrent(client, sources)
	for _, snapshot := range snapshots {
		baseline := Baseline{
			Source:       snapshot.Source,
			ETag:         snapshot.Meta.ETag,
			LastModified: snapshot.Meta.LastModified,
		}
		if snapshot.Error != "" {
			baseline.Error = snapshot.Error
		}
		if snapshot.Result != nil {
			baseline.Period = snapshot.Result.Period
			baseline.PeriodYYYYMM = snapshot.Result.PeriodYYYYMM
			baseline.ActualThousands = snapshot.Result.ActualThousands
			baseline.ValueSet = true
		}
		baselines[snapshot.Source.Name] = baseline
	}
	return baselines
}

func printBaselines(baselines map[string]Baseline) {
	keys := make([]string, 0, len(baselines))
	for key := range baselines {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		baseline := baselines[key]
		if baseline.Error != "" {
			fmt.Printf("  FAIL [%s] %s\n", baseline.Source.Name, baseline.Error)
			continue
		}
		fmt.Printf("  OK   [%s] Period: %s | Value: %s | ETag: %s | Last-Modified: %s\n",
			baseline.Source.Name,
			blankNA(baseline.Period),
			formatKMaybe(baseline.ActualThousands, baseline.ValueSet),
			blankNA(baseline.ETag),
			blankNA(baseline.LastModified),
		)
	}
	fmt.Println("Baseline captured. Waiting for new release.")
}

func countdownUntil(target time.Time, interval time.Duration) {
	for {
		remaining := time.Until(target)
		if remaining <= 0 {
			fmt.Print("\rTime remaining: 00:00:00.000\n")
			return
		}
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		fmt.Printf("\rTime remaining: %s", formatDuration(remaining))
		sleep := interval
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

func pollOfficialSource(ctx context.Context, client *http.Client, source Source, baseline Baseline, expectedPeriod string, eventTime time.Time, out chan<- Detection) {
	pollCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pollCount++
		contentCheck := pollCount%contentEveryNPolls == 0
		method := "content"
		if !contentCheck {
			reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			meta, err := fetchHeaders(reqCtx, client, source)
			cancel()
			if err == nil && headersChanged(baseline, meta) {
				contentCheck = true
				method = "headers"
			}
		}

		if contentCheck {
			result, meta, err := fetchSourceContent(ctx, client, source)
			if err == nil && isValidOfficialUpdate(result, baseline, expectedPeriod) {
				detectedAt := time.Now().UTC()
				select {
				case out <- Detection{
					Source:           source,
					Result:           result,
					Meta:             meta,
					Method:           method,
					PollCount:        pollCount,
					DetectedAt:       detectedAt,
					LatencyFromEvent: detectedAt.Sub(eventTime),
				}:
				case <-ctx.Done():
				}
				return
			}
		}

		timer := time.NewTimer(headerPollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func headersChanged(baseline Baseline, meta FetchMeta) bool {
	if baseline.ETag != "" && meta.ETag != "" && baseline.ETag != meta.ETag {
		return true
	}
	if baseline.LastModified != "" && meta.LastModified != "" && baseline.LastModified != meta.LastModified {
		return true
	}
	return false
}

func isValidOfficialUpdate(result *ParsedValue, baseline Baseline, expectedPeriod string) bool {
	if result == nil || result.PeriodYYYYMM == "" {
		return false
	}
	if expectedPeriod != "" && result.PeriodYYYYMM != expectedPeriod {
		return false
	}
	if !baseline.ValueSet {
		return true
	}
	return result.PeriodYYYYMM != baseline.PeriodYYYYMM || !valuesEqualK(result.ActualThousands, baseline.ActualThousands)
}

func printFinalTable(eventTime time.Time, sources []Source, detections []Detection) {
	sort.Slice(detections, func(i, j int) bool {
		if detections[i].DetectedAt.Equal(detections[j].DetectedAt) {
			return detections[i].Source.Priority < detections[j].Source.Priority
		}
		return detections[i].DetectedAt.Before(detections[j].DetectedAt)
	})
	fmt.Println("============================================================")
	fmt.Println("FINAL PERFORMANCE TABLE")
	fmt.Println("============================================================")
	fmt.Printf("%-5s %-38s %-16s %-12s %-10s %-10s\n", "RANK", "SOURCE", "UPDATE UTC", "LATENCY", "VALUE", "METHOD")
	fmt.Println("------------------------------------------------------------")
	seen := map[string]bool{}
	for i, detection := range detections {
		if detection.Result == nil || detection.Error != "" {
			continue
		}
		seen[detection.Source.Name] = true
		fmt.Printf("%-5d %-38s %-16s %+8.3fs   %-10s %-10s\n",
			i+1,
			detection.Source.Name,
			detection.DetectedAt.Format("15:04:05.000"),
			detection.DetectedAt.Sub(eventTime).Seconds(),
			formatK(detection.Result.ActualThousands),
			detection.Method,
		)
	}
	for _, source := range sources {
		if seen[source.Name] {
			continue
		}
		fmt.Printf("%-5s %-38s %-16s %-12s %-10s %-10s\n", "-", source.Name, "-", "Pending", "Pending", "-")
	}
	fmt.Println("============================================================")
}

func verifyThirdParty(client *http.Client, official Detection, expectedForecast float64, expectedForecastSet bool) ThirdPartyCheck {
	deadline := time.Now().Add(thirdPartyTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		body, _, err := fetch(ctx, client, thirdPartySource, browserLikeUserAgent)
		cancel()
		if err == nil {
			rows, parseErr := parseInvestingRows(body)
			if parseErr == nil {
				for _, row := range rows {
					if row.PeriodYYYYMM != official.Result.PeriodYYYYMM || !row.ActualSet {
						continue
					}
					if !valuesEqualK(row.Actual, official.Result.ActualThousands) {
						continue
					}
					detectedAt := time.Now().UTC()
					return ThirdPartyCheck{
						DetectedAt:      detectedAt,
						DelayMS:         detectedAt.Sub(official.DetectedAt).Milliseconds(),
						ValueMatched:    true,
						ForecastMatched: compareOptionalForecast(row, expectedForecast, expectedForecastSet),
						PreviousMatched: compareOptionalValue(row.Previous, row.PreviousSet, official.Result.PreviousThousands),
						Actual:          formatK(row.Actual),
						Forecast:        formatKMaybe(row.Forecast, row.ForecastSet),
						Previous:        formatKMaybe(row.Previous, row.PreviousSet),
						Period:          row.Period,
					}
				}
			} else {
				return ThirdPartyCheck{Error: parseErr.Error()}
			}
		}
		time.Sleep(thirdPartyEvery)
	}
	return ThirdPartyCheck{Error: "third-party page did not show the official actual before timeout"}
}

func printThirdPartyCheck(check ThirdPartyCheck) {
	fmt.Println("THIRD-PARTY VERIFICATION")
	fmt.Printf("Source: %s\n", thirdPartySource.Name)
	fmt.Printf("Detected Time: %s\n", formatTime(check.DetectedAt))
	fmt.Printf("Delay vs Official MS: %d\n", check.DelayMS)
	fmt.Printf("Value Matched Official: %t\n", check.ValueMatched)
	fmt.Printf("Forecast Matched: %s\n", blankNA(check.ForecastMatched))
	fmt.Printf("Previous Matched Official: %s\n", blankNA(check.PreviousMatched))
	fmt.Printf("Third-Party Period: %s\n", blankNA(check.Period))
	fmt.Printf("Third-Party Actual: %s\n", blankNA(check.Actual))
	fmt.Printf("Third-Party Forecast: %s\n", blankNA(check.Forecast))
	fmt.Printf("Third-Party Previous: %s\n", blankNA(check.Previous))
	if check.Error != "" {
		fmt.Printf("Third-Party Error: %s\n", check.Error)
	}
}

func validate(parsed *ParsedValue, expectedRaw string, fallback bool) (string, []string, error) {
	var warnings []string
	if parsed.PeriodYYYYMM == "" || parsed.FromPeriodYYYYMM == "" {
		return "LOW", warnings, errors.New("release period could not be validated")
	}
	expectedPrevious, err := previousYYYYMM(parsed.PeriodYYYYMM)
	if err != nil {
		return "LOW", warnings, err
	}
	if parsed.FromPeriodYYYYMM != expectedPrevious {
		return "LOW", warnings, fmt.Errorf("change period is %s to %s, not the previous calendar month", parsed.FromPeriod, parsed.Period)
	}
	if parsed.PagePeriodYYYYMM != "" && parsed.PagePeriodYYYYMM != parsed.PeriodYYYYMM {
		return "LOW", warnings, fmt.Errorf("page title period %s does not match change-column period %s", periodDisplay(parsed.PagePeriodYYYYMM), parsed.Period)
	}
	if math.IsNaN(parsed.ActualThousands) || math.IsInf(parsed.ActualThousands, 0) {
		return "LOW", warnings, errors.New("actual value is not numeric")
	}

	expected, err := normalizeExpectedPeriod(expectedRaw)
	if err != nil {
		return "LOW", warnings, err
	}
	if expected == "" {
		warnings = append(warnings, "No expected period supplied; pass -expected-period to reject a page that has not rolled to the target release month.")
	} else if parsed.PeriodYYYYMM != expected {
		return "LOW", warnings, fmt.Errorf("stale source: expected %s but source period is %s", periodDisplay(expected), parsed.Period)
	}

	if fallback {
		warnings = append(warnings, "Official backup value is calculated from levels; primary Table B-1 is the direct source.")
		return "MEDIUM", warnings, nil
	}
	if expected == "" {
		return "MEDIUM", warnings, nil
	}
	return "HIGH", warnings, nil
}

func printResult(result *ParsedValue, meta FetchMeta) {
	actual := "REJECTED"
	if result.ValidationFailure == "" && result.Method != "Rejected" {
		actual = formatK(result.ActualThousands)
	}
	fmt.Printf("Country: %s\n", country)
	fmt.Printf("Event: %s\n", eventName)
	fmt.Printf("Source: %s\n", result.Source.Name)
	fmt.Printf("Source URL: %s\n", result.Source.URL)
	fmt.Printf("Source Type: %s\n", result.Source.SourceType)
	fmt.Printf("Method: %s\n", result.Method)
	fmt.Printf("Table: %s\n", blankNA(result.Table))
	fmt.Printf("Row: %s\n", blankNA(result.Row))
	fmt.Printf("Column: %s\n", blankNA(result.Column))
	fmt.Printf("Unit: %s\n", blankNA(result.Unit))
	fmt.Printf("Period: %s\n", blankNA(result.Period))
	fmt.Printf("Actual: %s\n", actual)
	fmt.Printf("Forecast: %s\n", blankNA(result.Forecast))
	fmt.Printf("Previous: %s\n", blankNA(result.Previous))
	fmt.Printf("Confidence: %s\n", blankNA(result.Confidence))
	fmt.Printf("Request Started: %s\n", formatTime(meta.RequestStarted))
	fmt.Printf("Response Received: %s\n", formatTime(meta.ResponseReceived))
	fmt.Printf("Latency MS: %d\n", meta.Latency.Milliseconds())
	fmt.Printf("TTFB MS: %d\n", meta.TimeToFirstByte.Milliseconds())
	fmt.Printf("HTTP Status: %s\n", blankNA(meta.Status))
	fmt.Printf("Server Date: %s\n", blankNA(meta.ServerDate))
	fmt.Printf("ETag: %s\n", blankNA(meta.ETag))
	fmt.Printf("Last Modified: %s\n", blankNA(meta.LastModified))
	fmt.Printf("Response Bytes: %d\n", meta.ResponseSizeBytes)
	fmt.Printf("Warnings: %s\n", formatWarnings(result.Warnings))
	if result.ValidationFailure != "" {
		fmt.Printf("Error: %s\n", result.ValidationFailure)
	}
}

func extractText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	preMatches := preRE.FindAllStringSubmatch(raw, -1)
	if len(preMatches) > 0 {
		var blocks []string
		for _, match := range preMatches {
			block := stripHTML(match[1])
			if strings.Contains(strings.ToLower(block), "table b-1") || strings.Contains(strings.ToLower(block), "total private") {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
			return strings.Join(blocks, "\n")
		}
	}
	return stripHTML(raw)
}

func stripHTML(raw string) string {
	s := scriptStyleRE.ReplaceAllString(raw, " ")
	s = breakTagRE.ReplaceAllString(s, "\n")
	s = blockCloseRE.ReplaceAllString(s, "\n")
	s = cellCloseRE.ReplaceAllString(s, " ")
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return s
}

func tableSection(text string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "table b-1")
	if idx < 0 {
		return text
	}
	return text[idx:]
}

func extractPagePeriod(raw string) string {
	clean := compactSpaces(stripHTML(raw))
	match := pagePeriodRE.FindStringSubmatch(clean)
	if match == nil {
		return ""
	}
	return match[1] + "-" + match[2]
}

func extractChangePeriod(text string) (periodInfo, error) {
	clean := prelimMarkerRE.ReplaceAllString(text, "")
	clean = compactSpaces(clean)
	match := changePeriodRE.FindStringSubmatch(clean)
	if match == nil {
		return periodInfo{}, errors.New("Change from period header not found")
	}
	fromMonth, err := monthNumber(match[1])
	if err != nil {
		return periodInfo{}, err
	}
	toMonth, err := monthNumber(match[3])
	if err != nil {
		return periodInfo{}, err
	}
	fromYear, _ := strconv.Atoi(match[2])
	toYear, _ := strconv.Atoi(match[4])
	from := fmt.Sprintf("%04d-%02d", fromYear, fromMonth)
	to := fmt.Sprintf("%04d-%02d", toYear, toMonth)
	return periodInfo{
		FromDisplay: periodDisplay(from),
		FromYYYYMM:  from,
		ToDisplay:   periodDisplay(to),
		ToYYYYMM:    to,
	}, nil
}

func extractTotalPrivateValues(text string) ([]float64, error) {
	lines := normalizedLines(text)
	var matches [][]float64
	var candidateErrors []string

	for i, line := range lines {
		compact := compactSpaces(line)
		if compact == "" {
			continue
		}
		match := rowLabelRE.FindStringSubmatch(compact)
		if match == nil {
			continue
		}
		rest := strings.TrimSpace(match[1])
		if rest != "" && !startsWithNumber(rest) {
			continue
		}
		valuesLine := rest
		if valuesLine == "" {
			for j := i + 1; j < len(lines) && j <= i+5; j++ {
				next := compactSpaces(lines[j])
				if next == "" {
					continue
				}
				if !startsWithNumber(next) {
					candidateErrors = append(candidateErrors, fmt.Sprintf("line after Total private is not numeric: %q", next))
					break
				}
				valuesLine = next
				break
			}
		}
		if valuesLine == "" {
			continue
		}
		values, err := parseNumbers(valuesLine)
		if err != nil {
			candidateErrors = append(candidateErrors, err.Error())
			continue
		}
		if len(values) != 9 {
			candidateErrors = append(candidateErrors, fmt.Sprintf("Total private row has %d numeric columns, expected 9", len(values)))
			continue
		}
		matches = append(matches, values)
	}

	if len(matches) == 0 {
		if len(candidateErrors) > 0 {
			return nil, fmt.Errorf("Total private row found but not usable: %s", strings.Join(candidateErrors, "; "))
		}
		return nil, errors.New("Total private row not found")
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous Total private rows: %d matches", len(matches))
	}
	return matches[0], nil
}

func extractTotalPrivateValuesFromHTML(raw string) ([]float64, error) {
	rows := tableRowRE.FindAllStringSubmatch(raw, -1)
	if len(rows) == 0 {
		return nil, errors.New("no HTML table rows found")
	}

	var matches [][]float64
	var candidateErrors []string
	for _, row := range rows {
		header := headerCellRE.FindStringSubmatch(row[1])
		if header == nil {
			continue
		}
		label := compactSpaces(prelimMarkerRE.ReplaceAllString(stripHTML(header[1]), ""))
		if !strings.EqualFold(label, "Total private") {
			continue
		}

		cells := dataCellRE.FindAllStringSubmatch(row[1], -1)
		values := make([]float64, 0, len(cells))
		for _, cell := range cells {
			cellText := compactSpaces(stripHTML(cell[1]))
			numbers, err := parseNumbers(cellText)
			if err != nil {
				candidateErrors = append(candidateErrors, fmt.Sprintf("Total private cell %q: %v", cellText, err))
				continue
			}
			if len(numbers) != 1 {
				candidateErrors = append(candidateErrors, fmt.Sprintf("Total private cell %q has %d numbers", cellText, len(numbers)))
				continue
			}
			values = append(values, numbers[0])
		}
		if len(values) != 9 {
			candidateErrors = append(candidateErrors, fmt.Sprintf("Total private HTML row has %d numeric columns, expected 9", len(values)))
			continue
		}
		matches = append(matches, values)
	}

	if len(matches) == 0 {
		if len(candidateErrors) > 0 {
			return nil, fmt.Errorf("Total private HTML row found but not usable: %s", strings.Join(candidateErrors, "; "))
		}
		return nil, errors.New("Total private HTML row not found")
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous Total private HTML rows: %d matches", len(matches))
	}
	return matches[0], nil
}

func parseNumbers(s string) ([]float64, error) {
	rawTokens := numberRE.FindAllString(s, -1)
	if len(rawTokens) == 0 {
		return nil, errors.New("no numeric tokens found")
	}
	values := make([]float64, 0, len(rawTokens))
	for _, token := range rawTokens {
		value, err := strconv.ParseFloat(strings.ReplaceAll(token, ",", ""), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q: %w", token, err)
		}
		values = append(values, value)
	}
	return values, nil
}

func normalizedLines(s string) []string {
	rawLines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

func compactSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func startsWithNumber(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return (s[0] >= '0' && s[0] <= '9') || s[0] == '-' || s[0] == '+'
}

type thirdPartyRow struct {
	ReleaseDateText string
	Period          string
	PeriodYYYYMM    string
	Actual          float64
	ActualSet       bool
	Forecast        float64
	ForecastSet     bool
	Previous        float64
	PreviousSet     bool
}

func parseInvestingRows(body []byte) ([]thirdPartyRow, error) {
	raw := string(body)
	if strings.Contains(strings.ToLower(raw), "access denied") && strings.Contains(strings.ToLower(raw), "investing") {
		return nil, errors.New("Investing.com returned an access-denied page")
	}

	var rows []thirdPartyRow
	for _, rowMatch := range tableRowRE.FindAllStringSubmatch(raw, -1) {
		cells := dataCellRE.FindAllStringSubmatch(rowMatch[1], -1)
		if len(cells) < 5 {
			continue
		}
		dateText := compactSpaces(stripHTML(cells[0][1]))
		periodYYYYMM, periodDisplayText, ok := parseInvestingPeriod(dateText)
		if !ok {
			continue
		}
		actual, actualSet, err := parseKValue(compactSpaces(stripHTML(cells[2][1])))
		if err != nil {
			continue
		}
		forecast, forecastSet, err := parseKValue(compactSpaces(stripHTML(cells[3][1])))
		if err != nil {
			continue
		}
		previous, previousSet, err := parseKValue(compactSpaces(stripHTML(cells[4][1])))
		if err != nil {
			continue
		}
		rows = append(rows, thirdPartyRow{
			ReleaseDateText: dateText,
			Period:          periodDisplayText,
			PeriodYYYYMM:    periodYYYYMM,
			Actual:          actual,
			ActualSet:       actualSet,
			Forecast:        forecast,
			ForecastSet:     forecastSet,
			Previous:        previous,
			PreviousSet:     previousSet,
		})
	}
	if len(rows) == 0 {
		return nil, errors.New("no Investing.com historical rows found")
	}
	return rows, nil
}

func parseInvestingPeriod(text string) (string, string, bool) {
	match := investingDateRE.FindStringSubmatch(compactSpaces(text))
	if match == nil {
		return "", "", false
	}
	releaseMonth, err := monthNumber(match[1])
	if err != nil {
		return "", "", false
	}
	releaseYear, err := strconv.Atoi(match[3])
	if err != nil {
		return "", "", false
	}
	periodMonth, err := monthNumber(match[4])
	if err != nil {
		return "", "", false
	}
	periodYear := releaseYear
	if periodMonth > releaseMonth {
		periodYear--
	}
	yyyymm := fmt.Sprintf("%04d-%02d", periodYear, periodMonth)
	return yyyymm, periodDisplay(yyyymm), true
}

func parseKValue(input string) (float64, bool, error) {
	s := compactSpaces(strings.TrimSpace(input))
	if s == "" || s == "-" || s == "--" || strings.EqualFold(s, "N/A") {
		return 0, false, nil
	}
	s = strings.ReplaceAll(s, ",", "")
	multiplier := 1.0
	switch s[len(s)-1] {
	case 'K', 'k':
		s = s[:len(s)-1]
	case 'M', 'm':
		s = s[:len(s)-1]
		multiplier = 1000
	case 'B', 'b':
		s = s[:len(s)-1]
		multiplier = 1000000
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, err
	}
	return value * multiplier, true, nil
}

func valuesEqualK(a float64, b float64) bool {
	return math.Abs(a-b) < 0.5
}

func compareOptionalForecast(row thirdPartyRow, expected float64, expectedSet bool) string {
	if !expectedSet {
		return "N/A"
	}
	return compareOptionalValue(row.Forecast, row.ForecastSet, expected)
}

func compareOptionalValue(value float64, valueSet bool, expected float64) string {
	if !valueSet {
		return "N/A"
	}
	return fmt.Sprintf("%t", valuesEqualK(value, expected))
}

func formatKMaybe(value float64, ok bool) string {
	if !ok {
		return "N/A"
	}
	return formatK(value)
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

func normalizeExpectedPeriod(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	if match := regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`).FindStringSubmatch(input); match != nil {
		return input, nil
	}
	match := regexp.MustCompile(`(?i)^([A-Za-z]{3,9})\.?\s+(\d{4})$`).FindStringSubmatch(input)
	if match == nil {
		return "", fmt.Errorf("invalid expected period %q; use YYYY-MM or Mon YYYY", input)
	}
	month, err := monthNumber(match[1])
	if err != nil {
		return "", err
	}
	year, _ := strconv.Atoi(match[2])
	return fmt.Sprintf("%04d-%02d", year, month), nil
}

func previousYYYYMM(yyyymm string) (string, error) {
	t, err := time.Parse("2006-01", yyyymm)
	if err != nil {
		return "", fmt.Errorf("invalid period %q: %w", yyyymm, err)
	}
	return t.AddDate(0, -1, 0).Format("2006-01"), nil
}

func periodDisplay(yyyymm string) string {
	t, err := time.Parse("2006-01", yyyymm)
	if err != nil {
		return yyyymm
	}
	return t.Format("Jan 2006")
}

func monthNumber(s string) (int, error) {
	key := strings.ToLower(strings.Trim(s, ". "))
	if len(key) > 3 {
		key = key[:3]
	}
	months := map[string]int{
		"jan": 1,
		"feb": 2,
		"mar": 3,
		"apr": 4,
		"may": 5,
		"jun": 6,
		"jul": 7,
		"aug": 8,
		"sep": 9,
		"oct": 10,
		"nov": 11,
		"dec": 12,
	}
	month, ok := months[key]
	if !ok {
		return 0, fmt.Errorf("unknown month %q", s)
	}
	return month, nil
}

func formatK(value float64) string {
	rounded := math.Round(value)
	if math.Abs(value-rounded) < 0.000001 {
		return fmt.Sprintf("%.0fK", rounded)
	}
	return fmt.Sprintf("%.1fK", value)
}

func formatWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return "[]"
	}
	return "[" + strings.Join(warnings, "; ") + "]"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "N/A"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func blankNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/A"
	}
	return s
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

func looksLikeAccessDenied(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "access denied") && strings.Contains(lower, "bls")
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func getenvDefault(key, fallback string) string {
	if value := getenv(key); value != "" {
		return value
	}
	return fallback
}

func errorString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}
