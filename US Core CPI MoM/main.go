package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

// ═══════════════════════════════════════════════════════════════
// 📅 EVENT CONFIGURATION - CHANGE THIS FOR NEXT CPI RELEASE
// ═══════════════════════════════════════════════════════════════
//
// Event: US Core CPI MoM
// Release Time: 08:30 Eastern Time
// IST = UTC + 5:30
//
// Format: "YYYY-MM-DD HH:MM:SS" in UTC
//
var eventTimeUTC = "2026-06-10 12:30:00"

// ═══════════════════════════════════════════════════════════════

type Result struct {
	Source          string    `json:"source"`
	URL             string    `json:"source_url"`
	SourceType      string    `json:"source_type"`
	Period          string    `json:"period"`
	Value           string    `json:"actual"`
	NumericValue    float64   `json:"-"`
	Unit            string    `json:"unit"`
	Timestamp       time.Time `json:"-"`
	EventLatencyMs  int64     `json:"-"`
	DetectionMethod string    `json:"-"`
	Confidence      string    `json:"confidence"`
	ETag            string    `json:"etag"`
	LastModified    string    `json:"last_modified"`
	CacheControl    string    `json:"cache_control"`
	ServerDate      string    `json:"server_date_header"`
	ContentHash     string    `json:"-"`
	StatusCode      int       `json:"-"`
	Error           string    `json:"-"`
	Warnings        []string  `json:"warnings"`
	LatencyMs       struct {
		Total    int64 `json:"total"`
		TTFB     int64 `json:"ttfb"`
		BodyRead int64 `json:"body_read"`
		Parse    int64 `json:"parse"`
	} `json:"latency_ms"`

	Country         string   `json:"country"`
	EventName       string   `json:"event_name"`
	OfficialRelease string   `json:"official_release"`
	SeriesID        string   `json:"series_id"`
	Field           string   `json:"field"`
	ValueMethod     string   `json:"value_method"`
	MatchedSources  []string `json:"matched_sources"`
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
	Mu       sync.Mutex
}

type Scraper interface {
	Name() string
	URL() string
	Type() string
	FetchWithHeaders(ctx context.Context, client *http.Client, skipContent bool) (*Result, error)
	Parse(body []byte, headers http.Header) (*Result, error)
}

// -------------------------------------------------------------------------
// HTTP Client setup
// -------------------------------------------------------------------------

func newHTTPClient() *http.Client {
	t := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // Allow fast handshake
	}
	return &http.Client{
		Transport: t,
		Timeout:   5 * time.Second,
	}
}

func doRequest(ctx context.Context, client *http.Client, urlStr string, skipContent bool) (*Result, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Connection", "keep-alive")
// removed manual Accept-Encoding to let http.Transport handle gzip auto-decompression

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)

	res := &Result{
		StatusCode:   resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		CacheControl: resp.Header.Get("Cache-Control"),
		ServerDate:   resp.Header.Get("Date"),
	}
	res.LatencyMs.TTFB = ttfb.Milliseconds()

	if skipContent {
		res.LatencyMs.Total = time.Since(start).Milliseconds()
		return res, nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return res, nil, err
	}
	bodyRead := time.Since(start)
	res.LatencyMs.BodyRead = (bodyRead - ttfb).Milliseconds()

	hash := sha256.Sum256(body)
	res.ContentHash = hex.EncodeToString(hash[:])
	res.LatencyMs.Total = time.Since(start).Milliseconds()

	return res, body, nil
}

// -------------------------------------------------------------------------
// BLS HTML Table 1 Scraper
// -------------------------------------------------------------------------

type HTMLScraper struct{}

func (s *HTMLScraper) Name() string { return "BLS CPI Table 1 HTML" }
func (s *HTMLScraper) URL() string  { return "https://www.bls.gov/news.release/cpi.t01.htm" }
func (s *HTMLScraper) Type() string { return "HTML" }

func (s *HTMLScraper) FetchWithHeaders(ctx context.Context, client *http.Client, skipContent bool) (*Result, error) {
	res, body, err := doRequest(ctx, client, s.URL(), skipContent)
	if err != nil {
		return nil, err
	}
	if skipContent || res.StatusCode != 200 {
		return res, nil
	}

	parseStart := time.Now()
	parsedRes, err := s.Parse(body, nil)
	if err != nil {
		res.Error = err.Error()
	} else if parsedRes != nil {
		res.Period = parsedRes.Period
		res.Value = parsedRes.Value
		res.NumericValue = parsedRes.NumericValue
		res.Warnings = parsedRes.Warnings
	}
	res.LatencyMs.Parse = time.Since(parseStart).Milliseconds()
	return res, nil
}

func (s *HTMLScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	var targetRow *html.Node
	var findRow func(*html.Node)
	findRow = func(n *html.Node) {
		if targetRow != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			text := extractText(n)
			if strings.Contains(strings.ToLower(text), "all items less food and energy") {
				targetRow = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findRow(c)
		}
	}
	findRow(doc)

	if targetRow == nil {
		os.WriteFile("failed_table1.html", body, 0644)
		return nil, fmt.Errorf("target row 'All items less food and energy' not found")
	}

	var tds []string
	for c := targetRow.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
			t := strings.TrimSpace(extractText(c))
			if t != "" {
				tds = append(tds, t)
			}
		}
	}

	if len(tds) < 2 {
		return nil, fmt.Errorf("not enough columns in target row")
	}

	// Usually the last few columns are seasonally adjusted 1-month changes.
	// We extract the last valid float we find.
	var lastVal float64
	var found bool
	for i := len(tds) - 1; i >= 0; i-- {
		clean := strings.ReplaceAll(tds[i], "%", "")
		if val, err := strconv.ParseFloat(clean, 64); err == nil {
			lastVal = val
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("could not parse any numeric value in target row")
	}

	// Attempt to find period from table header, or use current month logic.
	// We'll leave period extraction as generic or fallback.
	period := "Unknown"
	re := regexp.MustCompile(`(?i)(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[\w]*\s+20\d{2}`)
	matches := re.FindAllString(string(body), -1)
	if len(matches) > 0 {
		// Just take the most prevalent or last mentioned recent month
		period = normalizePeriod(matches[len(matches)-1])
	}

	return &Result{
		Period:       period,
		Value:        fmt.Sprintf("%.1f%%", lastVal),
		NumericValue: lastVal,
	}, nil
}

// -------------------------------------------------------------------------
// BLS PDF Scraper
// -------------------------------------------------------------------------

type PDFScraper struct{}

func (s *PDFScraper) Name() string { return "BLS CPI PDF Release" }
func (s *PDFScraper) URL() string  { return "https://www.bls.gov/news.release/pdf/cpi.pdf" }
func (s *PDFScraper) Type() string { return "PDF" }

func (s *PDFScraper) FetchWithHeaders(ctx context.Context, client *http.Client, skipContent bool) (*Result, error) {
	res, body, err := doRequest(ctx, client, s.URL(), skipContent)
	if err != nil {
		return nil, err
	}
	if skipContent || res.StatusCode != 200 {
		return res, nil
	}

	parseStart := time.Now()
	parsedRes, err := s.Parse(body, nil)
	if err != nil {
		res.Error = err.Error()
	} else if parsedRes != nil {
		res.Period = parsedRes.Period
		res.Value = parsedRes.Value
		res.NumericValue = parsedRes.NumericValue
		res.Warnings = parsedRes.Warnings
	}
	res.LatencyMs.Parse = time.Since(parseStart).Milliseconds()
	return res, nil
}

func (s *PDFScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	pdfReader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	b, err := pdfReader.GetPlainText()
	if err != nil {
		return nil, err
	}

	textBytes, err := io.ReadAll(b)
	if err != nil {
		return nil, err
	}
	text := string(textBytes)
	lines := strings.Split(text, "\n")
	var targetLine string
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "all items less food and energy") {
			targetLine = l
			break
		}
	}

	if targetLine == "" {
		return nil, fmt.Errorf("target string not found in PDF")
	}

	re := regexp.MustCompile(`[-+]?\d*\.\d+|\d+`)
	nums := re.FindAllString(targetLine, -1)
	if len(nums) < 1 {
		return nil, fmt.Errorf("no numbers found in PDF row")
	}

	val, err := strconv.ParseFloat(nums[len(nums)-1], 64)
	if err != nil {
		return nil, err
	}

	return &Result{
		Period:       "Unknown",
		Value:        fmt.Sprintf("%.1f%%", val),
		NumericValue: val,
		Warnings:     []string{"PDF parsing is heuristic-based"},
	}, nil
}

// -------------------------------------------------------------------------
// BLS API Scraper
// -------------------------------------------------------------------------

type APIScraper struct{}

func (s *APIScraper) Name() string { return "BLS API CUSR0000SA0L1E" }
func (s *APIScraper) URL() string  { return "https://api.bls.gov/publicAPI/v2/timeseries/data/CUSR0000SA0L1E" }
func (s *APIScraper) Type() string { return "API" }

func (s *APIScraper) FetchWithHeaders(ctx context.Context, client *http.Client, skipContent bool) (*Result, error) {
	res, body, err := doRequest(ctx, client, s.URL(), skipContent)
	if err != nil {
		return nil, err
	}
	if skipContent || res.StatusCode != 200 {
		return res, nil
	}

	parseStart := time.Now()
	parsedRes, err := s.Parse(body, nil)
	if err != nil {
		res.Error = err.Error()
	} else if parsedRes != nil {
		res.Period = parsedRes.Period
		res.Value = parsedRes.Value
		res.NumericValue = parsedRes.NumericValue
		res.Warnings = parsedRes.Warnings
	}
	res.LatencyMs.Parse = time.Since(parseStart).Milliseconds()
	return res, nil
}

func (s *APIScraper) Parse(body []byte, headers http.Header) (*Result, error) {
	type APIResponse struct {
		Results struct {
			Series []struct {
				Data []struct {
					Year       string `json:"year"`
					PeriodName string `json:"periodName"`
					Value      string `json:"value"`
				} `json:"data"`
			} `json:"series"`
		} `json:"Results"`
	}

	var apiRes APIResponse
	if err := json.Unmarshal(body, &apiRes); err != nil {
		return nil, err
	}

	if len(apiRes.Results.Series) == 0 || len(apiRes.Results.Series[0].Data) < 2 {
		return nil, fmt.Errorf("insufficient data in API response")
	}

	data := apiRes.Results.Series[0].Data
	currStr := data[0].Value
	prevStr := data[1].Value
	period := data[0].PeriodName + " " + data[0].Year

	curr, err1 := strconv.ParseFloat(currStr, 64)
	prev, err2 := strconv.ParseFloat(prevStr, 64)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("error parsing float values")
	}

	mom := ((curr / prev) - 1) * 100
	mom = math.Round(mom*10) / 10

	return &Result{
		Period:       period,
		Value:        fmt.Sprintf("%.1f%%", mom),
		NumericValue: mom,
	}, nil
}

// -------------------------------------------------------------------------
// Utility Functions
// -------------------------------------------------------------------------

func extractText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(extractText(c))
	}
	return b.String()
}

func normalizePeriod(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 3 {
		return s[:3] + " " + s[len(s)-4:]
	}
	return s
}

func validateResult(r *Result) error {
	if r.NumericValue < -10 || r.NumericValue > 10 {
		return fmt.Errorf("value %v is outside reasonable bounds (-10%% to +10%%)", r.NumericValue)
	}
	if r.Value == "" {
		return fmt.Errorf("empty value")
	}
	return nil
}

// -------------------------------------------------------------------------
// Main Execution
// -------------------------------------------------------------------------

func main() {
	eventTime, err := time.Parse("2006-01-02 15:04:05", eventTimeUTC)
	if err != nil {
		log.Fatalf("Invalid eventTimeUTC format: %v", err)
	}

	istLocation := time.FixedZone("IST", 5*3600+1800)
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Println("🎯 US Core CPI MoM Scraper - SNIPER MODE")
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf("📅 Event Time (IST): %s\n", eventTime.In(istLocation).Format("2006-01-02 15:04:05"))
	fmt.Printf("📅 Event Time (UTC): %s\n", eventTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("🕐 Current Time (UTC): %s\n", time.Now().UTC().Format("2006-01-02 15:04:05"))
	fmt.Println("═══════════════════════════════════════════════════════════════════════\n")

	scrapers := []Scraper{
		&HTMLScraper{},
		&PDFScraper{},
		&APIScraper{},
	}
	client := newHTTPClient()
	results := make(map[string]*SourceResult)

	fmt.Println("📊 Fetching Current Published Data...")
	fmt.Println("───────────────────────────────────────────────────────────────────────")
	for _, s := range scrapers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := s.FetchWithHeaders(ctx, client, false)
		cancel()

		sr := &SourceResult{Name: s.Name()}
		results[s.Name()] = sr

		if err != nil || (res != nil && res.Error != "") {
			errStr := ""
			if err != nil {
				errStr = err.Error()
			} else if res != nil && res.Error != "" {
				errStr = res.Error
			}
			fmt.Printf("❌ [%-25s] Error: %s\n", s.Name(), errStr)
		} else {
			fmt.Printf("✅ [%-25s] Period: %-13s Value: %s\n", s.Name(), res.Period, res.Value)
			sr.Baseline = &Baseline{
				ETag:         res.ETag,
				LastModified: res.LastModified,
				ContentHash:  res.ContentHash,
				Period:       res.Period,
				Value:        res.Value,
			}
		}
	}
	fmt.Println("───────────────────────────────────────────────────────────────────────")
	fmt.Println("✅ Current data captured. Waiting for new release...\n")

	// Adjust for testing: if eventTime is in the past, pretend it's in 5 seconds
	if time.Now().UTC().After(eventTime) {
		eventTime = time.Now().UTC().Add(5 * time.Second)
		fmt.Println("⚠️ Event time is in the past. Adjusting to +5 seconds for demo purposes.")
	}

	testTime := eventTime.Add(-1 * time.Minute)
	sniperTime := eventTime.Add(-2 * time.Second)
	endTime := eventTime.Add(3 * time.Minute)

	// Countdown to test connection
	for time.Now().UTC().Before(testTime) {
		rem := time.Until(testTime)
		fmt.Printf("\r⏰ Countdown to Test Connection: %02dh%02dm%02ds   ", int(rem.Hours()), int(rem.Minutes())%60, int(rem.Seconds())%60)
		time.Sleep(1 * time.Second)
	}
	fmt.Printf("\r   Will test connections 1 minute before event                       \n\n")

	fmt.Println("🔌 Testing connections...")
	fmt.Println("   Capturing baseline headers + content for hybrid detection...\n")
	for _, s := range scrapers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := s.FetchWithHeaders(ctx, client, false)
		cancel()

		sr := results[s.Name()]
		if err == nil && res != nil && res.Error == "" {
			sr.Baseline.ETag = res.ETag
			sr.Baseline.LastModified = res.LastModified
			sr.Baseline.ContentHash = res.ContentHash
			sr.Baseline.Period = res.Period
			sr.Baseline.Value = res.Value
			fmt.Printf("✅ [%s] Connected | Period: %s | Value: %s | ETag: %s\n", s.Name(), res.Period, res.Value, res.ETag)
		} else {
			fmt.Printf("⚠️ [%s] Test Connection Error\n", s.Name())
		}
	}

	// Final countdown to sniper mode
	fmt.Println()
	for time.Now().UTC().Before(sniperTime) {
		rem := time.Until(sniperTime)
		fmt.Printf("\r🎯 Sniper Mode starts in: %.3f seconds  ", rem.Seconds())
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("\r🎯 SNIPER MODE ACTIVATED!                                \n")
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Println("Using HYBRID detection: Headers every 500ms + Content every 5th poll")
	fmt.Println("═══════════════════════════════════════════════════════════════════════\n")

	var wg sync.WaitGroup
	for _, s := range scrapers {
		wg.Add(1)
		go func(scraper Scraper) {
			defer wg.Done()
			pollCount := 0
			sr := results[scraper.Name()]

			for time.Now().UTC().Before(endTime) {
				sr.Mu.Lock()
				if sr.Detected {
					sr.Mu.Unlock()
					return
				}
				sr.Mu.Unlock()

				pollCount++
				checkContent := (pollCount % 5) == 0

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				res, err := scraper.FetchWithHeaders(ctx, client, !checkContent)
				cancel()

				if err == nil && res != nil && res.Error == "" {
					headersChanged := false
					if res.ETag != "" && res.ETag != sr.Baseline.ETag {
						headersChanged = true
					}
					if res.LastModified != "" && res.LastModified != sr.Baseline.LastModified {
						headersChanged = true
					}

					contentChanged := false
					if checkContent {
						if res.ContentHash != sr.Baseline.ContentHash || res.Period != sr.Baseline.Period || res.Value != sr.Baseline.Value {
							contentChanged = true
						}
					}

					if headersChanged || contentChanged {
						// If headers changed but content wasn't fetched, fetch content now
						if headersChanged && !checkContent {
							ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
							resContent, errContent := scraper.FetchWithHeaders(ctx2, client, false)
							cancel2()
							if errContent == nil && resContent != nil && resContent.Error == "" {
								res = resContent
								contentChanged = (res.ContentHash != sr.Baseline.ContentHash || res.Period != sr.Baseline.Period || res.Value != sr.Baseline.Value)
							}
						}

						if headersChanged || contentChanged {
							method := []string{}
							if headersChanged {
								method = append(method, "headers")
							}
							if contentChanged {
								method = append(method, "content")
							}

							res.DetectionMethod = strings.Join(method, "+")
							res.Timestamp = time.Now().UTC()
							res.EventLatencyMs = res.Timestamp.Sub(eventTime).Milliseconds()
							res.Source = scraper.Name()
							res.URL = scraper.URL()
							res.SourceType = scraper.Type()

							// Fill JSON expected fields
							res.Country = "US"
							res.EventName = "Core CPI MoM"
							res.OfficialRelease = "Consumer Price Index Summary"
							res.SeriesID = "CUSR0000SA0L1E"
							res.Field = "All items less food and energy"
							res.Unit = "%"
							res.ValueMethod = "direct_table_or_calculated_index_change"
							if scraper.Type() == "API" {
								res.Confidence = "HIGH"
							} else if scraper.Type() == "HTML" {
								res.Confidence = "HIGH"
							} else {
								res.Confidence = "MEDIUM"
							}

							sr.Mu.Lock()
							if !sr.Detected {
								if err := validateResult(res); err == nil {
									sr.FirstHit = res
									sr.Detected = true
									timeStr := res.Timestamp.Format("15:04:05.000")
									fmt.Printf("[%s] 🚨 UPDATED! [%s] Period: %s | Value: %s | Detected by: %s\n", timeStr, scraper.Name(), res.Period, res.Value, res.DetectionMethod)
								} else {
									fmt.Printf("⚠️ [%s] Detected change but validation failed: %v\n", scraper.Name(), err)
								}
							}
							sr.Mu.Unlock()
						}
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}(s)
	}

	wg.Wait()

	fmt.Println("\n═══════════════════════════════════════════════════════════════════════")
	fmt.Println("⏰ Polling window complete")
	fmt.Println("═══════════════════════════════════════════════════════════════════════\n")

	fmt.Println("📊 FINAL PERFORMANCE TABLE")
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf("%-5s %-25s %-21s %-11s %-10s %-7s %s\n", "RANK", "SOURCE", "UPDATE TIME UTC", "LATENCY", "PERIOD", "VALUE", "METHOD")
	fmt.Println("───────────────────────────────────────────────────────────────────────")

	var ranked []*Result
	for _, sr := range results {
		if sr.Detected && sr.FirstHit != nil {
			ranked = append(ranked, sr.FirstHit)
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Timestamp.Before(ranked[j].Timestamp)
	})

	medals := []string{"🥇", "🥈", "🥉"}
	for i, r := range ranked {
		medal := "  "
		if i < len(medals) {
			medal = medals[i]
		}
		latStr := fmt.Sprintf("+%.3fs", float64(r.EventLatencyMs)/1000.0)
		fmt.Printf("%-5s %-25s %-21s %-11s %-10s %-7s %s\n", medal, r.Source, r.Timestamp.Format("15:04:05.000"), latStr, r.Period, r.Value, r.DetectionMethod)
	}

	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	if len(ranked) > 0 {
		winner := ranked[0]
		fmt.Printf("✅ Winner: %s\n", winner.Source)
		fmt.Printf("📅 Updated Period: %s\n", winner.Period)
		fmt.Printf("📈 Core CPI MoM: %s\n", winner.Value)
		fmt.Printf("⏱ Detection Latency: +%.3fs from event time\n", float64(winner.EventLatencyMs)/1000.0)

		if winner.Confidence == "LOW" || len(ranked) > 1 && ranked[0].Value != ranked[1].Value {
			fmt.Println("\n⚠️ NOT_CONFIRMED: Official sources disagree or confidence is LOW")
		} else {
			fmt.Println("═══════════════════════════════════════════════════════════════════════")
			fmt.Println("JSON OUTPUT:")
			for _, r := range ranked {
				r.MatchedSources = []string{r.Source}
			}
			jsonOut, _ := json.MarshalIndent(winner, "", "  ")
			fmt.Println(string(jsonOut))
		}
	} else {
		fmt.Println("❌ No updates detected during the polling window.")
	}
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
}
