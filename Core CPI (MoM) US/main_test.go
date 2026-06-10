package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseTable1HTML(t *testing.T) {
	body := []byte(`
<html><head><title>Consumer Price Index Summary - 2026 M04 Results</title></head>
<body>
<table>
<caption>Table 1. Consumer Price Index for All Urban Consumers (CPI-U): U.S. city average, by expenditure category, April 2026 [1982-84=100, unless otherwise noted]</caption>
<thead>
<tr><th rowspan="2">Expenditure category</th><th colspan="5">Unadjusted indexes</th><th colspan="4">Seasonally adjusted percent change</th></tr>
<tr><th>Relative importance Mar. 2026</th><th>Apr. 2025</th><th>Mar. 2026</th><th>Apr. 2026</th><th>Unadjusted 12-month percent change</th><th>Jan. 2026-Feb. 2026</th><th>Feb. 2026-Mar. 2026</th><th>Mar. 2026-Apr. 2026</th></tr>
</thead>
<tbody>
<tr><th>All items</th><td>100.000</td><td>320.795</td><td>330.213</td><td>333.020</td><td>3.8</td><td>0.9</td><td>0.3</td><td>0.9</td><td>0.6</td></tr>
<tr><th>All items less food and energy</th><td>79.351</td><td>326.815</td><td>334.391</td><td>335.803</td><td>2.8</td><td>0.4</td><td>0.3</td><td>0.2</td><td>0.4</td></tr>
<tr><th>Food</th><td>13.000</td><td>320.000</td><td>321.000</td><td>322.000</td><td>2.0</td><td>0.3</td><td>0.1</td><td>0.2</td><td>0.3</td></tr>
</tbody>
</table>
</body></html>`)

	got, warnings, err := parseTable1HTML(body)
	if err != nil {
		t.Fatalf("parseTable1HTML returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseTable1HTML warnings=%v", warnings)
	}
	if got.Period != "2026-04" {
		t.Fatalf("Period=%q, want 2026-04", got.Period)
	}
	if got.ValueString != "0.4" {
		t.Fatalf("ValueString=%q, want 0.4", got.ValueString)
	}
	if got.Method != "direct_table_value" {
		t.Fatalf("Method=%q", got.Method)
	}
	wantMetrics := map[string]string{
		"cpi_mom":      "0.6%",
		"cpi_yoy":      "3.8%",
		"core_cpi_mom": "0.4%",
		"core_cpi_yoy": "2.8%",
	}
	for id, want := range wantMetrics {
		metric, ok := got.Metrics[id]
		if !ok {
			t.Fatalf("missing metric %q", id)
		}
		if metric.Actual != want {
			t.Fatalf("metric %s Actual=%q, want %q", id, metric.Actual, want)
		}
		if metric.ValueMethod != "direct_table_value" {
			t.Fatalf("metric %s ValueMethod=%q", id, metric.ValueMethod)
		}
	}
}

func TestParseNumberFootnoteRejection(t *testing.T) {
	tests := []struct {
		input   string
		wantVal float64
		wantOk  bool
	}{
		{"100.000", 100.0, true},
		{"0.4%", 0.4, true},
		{"-0.2", -0.2, true},
		{"\u22120.3", -0.3, true},
		{"(1)", 0.0, false},
		{"(2)", 0.0, false},
		{" (12) ", 0.0, false},
		{"(p)", 0.0, false},
		{"0.3(p)", 0.3, true},
	}
	for _, tc := range tests {
		gotVal, gotOk := parseNumber(tc.input)
		if gotOk != tc.wantOk || (gotOk && gotVal != tc.wantVal) {
			t.Errorf("parseNumber(%q) = (%v, %v), want (%v, %v)", tc.input, gotVal, gotOk, tc.wantVal, tc.wantOk)
		}
	}
}

func TestExtractTable1Block(t *testing.T) {
	htmlStr := `
	<html>
	<body>
	<div class="nav-table">
	  <table>
	    <tr><td>All items</td><td>Some unrelated menu cell</td></tr>
	  </table>
	</div>
	<div class="content">
	  <table>
	    <caption>Table 1. Consumer Price Index for All Urban Consumers (CPI-U): U.S. city average, April 2026 [1982-84=100]</caption>
	    <thead>
	      <tr><th>Expenditure category</th><th>Unadjusted 12-month percent change</th><th>Seasonally adjusted percent change</th></tr>
	    </thead>
	    <tbody>
	      <tr><th>All items</th><td>100.000</td><td>320.795</td><td>330.213</td><td>333.020</td><td>3.8</td><td>0.9</td><td>0.3</td><td>0.9</td><td>0.6</td></tr>
	      <tr><th>All items less food and energy</th><td>79.351</td><td>326.815</td><td>334.391</td><td>335.803</td><td>2.8</td><td>0.4</td><td>0.3</td><td>0.2</td><td>0.4</td></tr>
	    </tbody>
	  </table>
	</div>
	</body>
	</html>`

	got := extractTable1Block(htmlStr)
	if !strings.Contains(got, "caption") || !strings.Contains(got, "Table 1.") {
		t.Fatalf("expected Table 1 to be isolated, got: %s", got)
	}
	if strings.Contains(got, "nav-table") {
		t.Fatalf("expected nav-table to be excluded from Table 1 block")
	}

	parsed, _, err := parseTable1HTML([]byte(htmlStr))
	if err != nil {
		t.Fatalf("parseTable1HTML failed on multi-table HTML: %v", err)
	}
	if parsed.Period != "2026-04" || parsed.ValueString != "0.4" {
		t.Fatalf("incorrect parsed values: Period=%s, Value=%s", parsed.Period, parsed.ValueString)
	}
}

func TestParseTable1HTMLRejectsAmbiguousTargetRows(t *testing.T) {
	body := []byte(`
<table>
<caption>Table 1. Consumer Price Index for All Urban Consumers (CPI-U), April 2026 [1982-84=100]</caption>
<tr><th>Expenditure category</th><th>Unadjusted percent change</th><th>Seasonally adjusted percent change</th></tr>
<tr><th>All items</th><td>100.000</td><td>320.795</td><td>330.213</td><td>333.020</td><td>3.8</td><td>0.9</td><td>0.3</td><td>0.9</td><td>0.6</td></tr>
<tr><th>All items less food and energy</th><td>79.351</td><td>326.815</td><td>334.391</td><td>335.803</td><td>2.8</td><td>0.4</td><td>0.3</td><td>0.2</td><td>0.4</td></tr>
<tr><th>All items less food and energy</th><td>79.351</td><td>326.815</td><td>334.391</td><td>335.803</td><td>2.8</td><td>0.4</td><td>0.3</td><td>0.2</td><td>0.4</td></tr>
</table>`)
	if _, _, err := parseTable1HTML(body); err == nil {
		t.Fatal("expected ambiguous target row error")
	}
}

func TestParseCPIPDFText(t *testing.T) {
	body := []byte(`Consumer Price Index Summary - 2026 M04 Results
Table 1. Consumer Price Index for All Urban Consumers (CPI-U): U.S. city average, by expenditure category, April 2026 [1982-84=100]
All items 100.000 320.795 330.213 333.020 3.8 0.9 0.3 0.9 0.6
All items less food and energy 79.351 326.815 334.391 335.803 2.8 0.4 0.3 0.2 0.4`)

	got, warnings, err := parseCPIPDF(body)
	if err != nil {
		t.Fatalf("parseCPIPDF returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseCPIPDF warnings=%v", warnings)
	}
	if got.Period != "2026-04" {
		t.Fatalf("Period=%q, want 2026-04", got.Period)
	}
	if got.ValueString != "0.4" {
		t.Fatalf("ValueString=%q, want 0.4", got.ValueString)
	}
	wantMetrics := map[string]string{
		"cpi_mom":      "0.6%",
		"cpi_yoy":      "3.8%",
		"core_cpi_mom": "0.4%",
		"core_cpi_yoy": "2.8%",
	}
	for id, want := range wantMetrics {
		metric, ok := got.Metrics[id]
		if !ok {
			t.Fatalf("missing metric %q", id)
		}
		if metric.Actual != want {
			t.Fatalf("metric %s Actual=%q, want %q", id, metric.Actual, want)
		}
		if metric.ValueMethod != "direct_pdf_table_value" {
			t.Fatalf("metric %s ValueMethod=%q", id, metric.ValueMethod)
		}
	}
}

func TestParseCPIPDFSummarySentence(t *testing.T) {
	body := []byte(`Consumer Price Index Summary - 2026 M04 Results
The index for all items less food and energy increased 0.4 percent in April (SA); up 2.8 percent over the year (NSA).`)

	got, _, err := parseCPIPDF(body)
	if err != nil {
		t.Fatalf("parseCPIPDF returned error: %v", err)
	}
	if got.ValueString != "0.4" {
		t.Fatalf("ValueString=%q, want 0.4", got.ValueString)
	}
}

func TestParseCPISummaryHTML(t *testing.T) {
	body := []byte(`
<html><head><title>Consumer Price Index Summary - 2026 M04 Results</title></head>
<body>
<h1>Consumer Price Index Summary</h1>
<p>CONSUMER PRICE INDEX - APRIL 2026</p>
<p>The Consumer Price Index for All Urban Consumers (CPI-U) increased 0.6 percent on a seasonally adjusted basis in April.
Over the last 12 months, the all items index increased 3.8 percent before seasonal adjustment.</p>
<p>The index for all items less food and energy rose 0.4 percent in April.</p>
<p>The all items less food and energy index rose 2.8 percent over the year.</p>
<p>Table A. Percent changes in CPI for All Urban Consumers (CPI-U): U.S. city average</p>
<p>All items - - 0.3 0.2 0.3 0.9 0.6 3.8</p>
<p>Food - - 0.7 0.2 0.4 0.0 0.5 3.2</p>
<p>All items less food and energy - - 0.2 0.3 0.2 0.2 0.4 2.8</p>
<p>Commodities less food and energy commodities - - 0.0 0.0 0.1 0.1 0.0 1.1</p>
<p>Footnotes</p>
</body></html>`)

	got, warnings, err := parseCPISummaryHTML(body)
	if err != nil {
		t.Fatalf("parseCPISummaryHTML returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseCPISummaryHTML warnings=%v", warnings)
	}
	if got.Period != "2026-04" {
		t.Fatalf("Period=%q, want 2026-04", got.Period)
	}
	wantMetrics := map[string]string{
		"cpi_mom":      "0.6%",
		"cpi_yoy":      "3.8%",
		"core_cpi_mom": "0.4%",
		"core_cpi_yoy": "2.8%",
	}
	for id, want := range wantMetrics {
		metric, ok := got.Metrics[id]
		if !ok {
			t.Fatalf("missing metric %q", id)
		}
		if metric.Actual != want {
			t.Fatalf("metric %s Actual=%q, want %q", id, metric.Actual, want)
		}
		if metric.ValueMethod != "direct_summary_table_a_value" {
			t.Fatalf("metric %s ValueMethod=%q", id, metric.ValueMethod)
		}
	}
}

func TestParseBLSAPI(t *testing.T) {
	body := []byte(`{
  "status": "REQUEST_SUCCEEDED",
  "Results": {
    "series": [{
      "seriesID": "CUSR0000SA0L1E",
      "data": [
        {"year": "2026", "period": "M04", "periodName": "April", "latest": "true", "value": "335.803"},
        {"year": "2026", "period": "M03", "periodName": "March", "value": "334.391"},
        {"year": "2025", "period": "M10", "periodName": "October", "value": "-"}
      ]
    }]
  }
}`)

	got, _, err := parseBLSAPI(body)
	if err != nil {
		t.Fatalf("parseBLSAPI returned error: %v", err)
	}
	if got.Period != "2026-04" {
		t.Fatalf("Period=%q, want 2026-04", got.Period)
	}
	if got.ValueString != "0.4" {
		t.Fatalf("ValueString=%q, want 0.4", got.ValueString)
	}
	if got.Method != "calculated_index_change" {
		t.Fatalf("Method=%q", got.Method)
	}
}

func TestParseInvestingMetricPage(t *testing.T) {
	body := []byte(`
<html><head><title>U.S. Consumer Price Index (CPI) MoM</title></head>
<body>
<h1>U.S. Consumer Price Index (CPI) MoM</h1>
<div>Latest Release May 12, 2026</div>
<div>Actual 0.6%</div>
<div>Forecast 0.6%</div>
<div>Previous 0.9%</div>
<div>Importance:Country:Currency:Source:</div>
<table>
<tr><th>Release date</th><th>Time</th><th>Actual</th><th>Forecast</th><th>Previous</th></tr>
<tr><td>Jun 10, 2026 (May)</td><td>12:30</td><td></td><td>0.3%</td><td>0.6%</td></tr>
<tr><td>May 12, 2026 (Apr)</td><td>12:30</td><td>0.6%</td><td>0.6%</td><td>0.9%</td></tr>
<tr><td>Apr 10, 2026 (Mar)</td><td>12:30</td><td>0.9%</td><td>1.0%</td><td>0.3%</td></tr>
</table>
</body></html>`)

	got, warnings, err := parseInvestingMetricPage(body, investingMetrics[0])
	if err != nil {
		t.Fatalf("parseInvestingMetricPage returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseInvestingMetricPage warnings=%v", warnings)
	}
	if got.ID != "cpi_mom" {
		t.Fatalf("ID=%q, want cpi_mom", got.ID)
	}
	if got.Period != "2026-04" {
		t.Fatalf("Period=%q, want 2026-04", got.Period)
	}
	if got.LatestReleaseDate != "2026-05-12" {
		t.Fatalf("LatestReleaseDate=%q, want 2026-05-12", got.LatestReleaseDate)
	}
	if got.Actual != "0.6%" || got.Forecast != "0.6%" || got.Previous != "0.9%" {
		t.Fatalf("values actual=%q forecast=%q previous=%q", got.Actual, got.Forecast, got.Previous)
	}
	if len(got.HistoricalRows) != 3 {
		t.Fatalf("HistoricalRows=%d, want 3", len(got.HistoricalRows))
	}
}

func TestParseScheduleAndExpectedLatestPeriod(t *testing.T) {
	body := []byte(`The Consumer Price Index for May 2026 is scheduled to be released on June 10, 2026, at 8:30 A.M. Eastern Time.`)

	got, err := parseReleaseSchedule(normalizeSpace(string(body)))
	if err != nil {
		t.Fatalf("parseReleaseSchedule returned error: %v", err)
	}
	if got.UTC().Format("2006-01-02 15:04:05") != "2026-06-10 12:30:00" {
		t.Fatalf("release time=%s, want 2026-06-10 12:30:00 UTC", got.UTC().Format("2006-01-02 15:04:05"))
	}
	if expectedReleasePeriod(got) != "2026-05" {
		t.Fatalf("expectedReleasePeriod=%q, want 2026-05", expectedReleasePeriod(got))
	}
}

func TestMergeConfirmedRequiresConsistency(t *testing.T) {
	states := map[string]*SourceResult{
		(tableScraper{}).Name(): {
			Name:     (tableScraper{}).Name(),
			FirstHit: testHit((tableScraper{}).Name(), tableURL, SourceHTMLTable, "2026-04", "0.4%", 0.4, 10),
		},
		(apiScraper{}).Name(): {
			Name:     (apiScraper{}).Name(),
			FirstHit: testHit((apiScraper{}).Name(), apiURL, SourceAPI, "2026-04", "0.3%", 0.3, 20),
		},
	}

	if _, err := mergeConfirmed(states, "2026-04", "2026-05-12"); err == nil {
		t.Fatal("expected NOT_CONFIRMED consistency error")
	}
}

func TestMergeConfirmedFastPathTableOnly(t *testing.T) {
	states := map[string]*SourceResult{
		(tableScraper{}).Name(): {
			Name:     (tableScraper{}).Name(),
			FirstHit: testHit((tableScraper{}).Name(), tableURL, SourceHTMLTable, "2026-04", "0.4%", 0.4, 10),
		},
	}

	got, err := mergeConfirmed(states, "2026-04", "2026-05-12")
	if err != nil {
		t.Fatalf("mergeConfirmed returned error: %v", err)
	}
	if got.Actual != "0.4%" {
		t.Fatalf("Actual=%q, want 0.4%%", got.Actual)
	}
	if len(got.MatchedSources) != 1 || got.MatchedSources[0] != (tableScraper{}).Name() {
		t.Fatalf("MatchedSources=%v, want Table 1 only", got.MatchedSources)
	}
}

func TestMergeConfirmedSuccess(t *testing.T) {
	states := map[string]*SourceResult{
		(tableScraper{}).Name(): {
			Name:     (tableScraper{}).Name(),
			FirstHit: testHit((tableScraper{}).Name(), tableURL, SourceHTMLTable, "2026-04", "0.4%", 0.4, 10),
		},
		(summaryScraper{}).Name(): {
			Name:     (summaryScraper{}).Name(),
			FirstHit: testHit((summaryScraper{}).Name(), summaryURL, SourceSummary, "2026-04", "0.4%", 0.4, 15),
		},
		(pdfScraper{}).Name(): {
			Name:     (pdfScraper{}).Name(),
			FirstHit: testHit((pdfScraper{}).Name(), pdfURL, SourcePDF, "2026-04", "0.4%", 0.4, 30),
		},
		(apiScraper{}).Name(): {
			Name:     (apiScraper{}).Name(),
			FirstHit: testHit((apiScraper{}).Name(), apiURL, SourceAPI, "2026-04", "0.4%", 0.4, 20),
		},
		(investingScraper{}).Name(): {
			Name:     (investingScraper{}).Name(),
			FirstHit: testHit((investingScraper{}).Name(), investingURL, SourceInvesting, "2026-04", "0.4%", 0.4, 40),
		},
	}

	got, err := mergeConfirmed(states, "2026-04", "2026-05-12")
	if err != nil {
		t.Fatalf("mergeConfirmed returned error: %v", err)
	}
	if got.Actual != "0.4%" {
		t.Fatalf("Actual=%q, want 0.4%%", got.Actual)
	}
	if len(got.Metrics) != 4 {
		t.Fatalf("Metrics=%v, want 4 Table 1 metrics", got.Metrics)
	}
	if got.Metrics[0].ID != "cpi_mom" || got.Metrics[0].Actual != "0.6%" {
		t.Fatalf("first metric=%+v, want cpi_mom 0.6%%", got.Metrics[0])
	}
	if got.Confidence != "HIGH" {
		t.Fatalf("Confidence=%q, want HIGH", got.Confidence)
	}
	if len(got.MatchedSources) != 5 {
		t.Fatalf("MatchedSources=%v, want 5 sources", got.MatchedSources)
	}
}

func testHit(source, url string, sourceType SourceType, period, value string, numeric float64, latencyMS int64) *Result {
	result := &Result{
		Source:          source,
		URL:             url,
		SourceType:      string(sourceType),
		Period:          period,
		Value:           value,
		NumericValue:    numeric,
		Unit:            "%",
		Timestamp:       time.Date(2026, 5, 12, 12, 30, 0, int(latencyMS)*int(time.Millisecond), time.UTC),
		EventLatencyMs:  latencyMS,
		DetectionMethod: "content",
		Confidence:      "HIGH",
		ETag:            `"abc"`,
		LastModified:    "Tue, 12 May 2026 12:30:00 GMT",
		CacheControl:    "max-age=0",
		ServerDate:      "Tue, 12 May 2026 12:30:00 GMT",
		ContentHash:     "hash",
		StatusCode:      200,
		Latency:         Latency{Total: 10, TTFB: 5, BodyRead: 2, Parse: 1},
		ValueMethod:     "direct_table_or_calculated_index_change",
	}
	if sourceType == SourceHTMLTable || sourceType == SourceSummary || sourceType == SourcePDF || sourceType == SourceInvesting {
		result.Metrics = testTable1Metrics(period)
	}
	if sourceType == SourceInvesting {
		for key, metric := range result.Metrics {
			metric.ValueMethod = "investing_html_confirmation"
			metric.LatestReleaseDate = "2026-05-12"
			metric.Forecast = metric.Actual
			metric.Previous = metric.Actual
			result.Metrics[key] = metric
		}
	}
	return result
}

func testTable1Metrics(period string) map[string]MetricValue {
	values := map[string]float64{
		"cpi_mom":      0.6,
		"cpi_yoy":      3.8,
		"core_cpi_mom": 0.4,
		"core_cpi_yoy": 2.8,
	}
	metrics := make(map[string]MetricValue, len(table1Metrics))
	for _, def := range table1Metrics {
		value := values[def.ID]
		metrics[def.ID] = MetricValue{
			ID:           def.ID,
			EventName:    def.EventName,
			Row:          def.Row,
			Column:       def.Column,
			Period:       period,
			Actual:       formatValueWithUnit(value),
			NumericValue: value,
			Unit:         "%",
			ValueMethod:  "direct_table_value",
		}
	}
	return metrics
}
