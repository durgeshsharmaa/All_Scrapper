package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

const aprilDailyFixture = `
<html>
<body>
<h1>Labour Force Survey, April 2026</h1>
<p>Released:&nbsp;2026-05-08</p>
<div>Employment level &mdash; Canada</div>
<div>21,034,000</div>
<div>April&nbsp;2026</div>
<div>-0.1%</div>
<div>Unemployment rate &mdash; Canada</div>
<div>6.9%</div>
<div>April&nbsp;2026</div>
<div>0.2 pts</div>
<h2>Highlights</h2>
<p>Employment was little changed in April (-18,000; -0.1%) and the employment rate fell 0.1 percentage points to 60.5%.</p>
<p>The unemployment rate increased by 0.2 percentage points to 6.9%, as more people searched for work.</p>
</body>
</html>`

const indexFixture = `
<html>
<body>
<a href="/n1/daily-quotidien/260604/dq260604a-eng.htm">Other release (Released: 2026-06-04)</a>
<a href="/daily-quotidien/260508/dq260508a-eng.htm">Labour Force Survey April 2026 (Released: 2026-05-08)</a>
</body>
</html>`

const wdsFixture = `[
  {
    "status": "SUCCESS",
    "object": {
      "responseStatusCode": 0,
      "productId": 14100287,
      "coordinate": "1.7.1.1.1.1.0.0.0.0",
      "vectorId": 2062815,
      "vectorDataPoint": [
        {
          "refPer": "2026-04-01",
          "refPerRaw": "2026-04-01",
          "value": 6.9,
          "decimals": 1,
          "scalarFactorCode": 0,
          "symbolCode": 0,
          "statusCode": 0,
          "securityLevelCode": 0,
          "releaseTime": "2026-06-05T08:30",
          "frequencyCode": 6
        },
        {
          "refPer": "2026-05-01",
          "refPerRaw": "2026-05-01",
          "value": 6.6,
          "decimals": 1,
          "scalarFactorCode": 0,
          "symbolCode": 0,
          "statusCode": 0,
          "securityLevelCode": 0,
          "releaseTime": "2026-06-05T08:30",
          "frequencyCode": 6
        }
      ]
    }
  }
]`

func TestParseDailyArticleDirectSentence(t *testing.T) {
	parsed, err := parseDailyArticle(primarySource, []byte(aprilDailyFixture), "https://example.test/dq260508a-eng.htm")
	if err != nil {
		t.Fatalf("parseDailyArticle returned error: %v", err)
	}
	if parsed.Method != "Direct sentence parse" {
		t.Fatalf("method = %s, want Direct sentence parse", parsed.Method)
	}
	if parsed.PeriodYYYYMM != "2026-04" {
		t.Fatalf("period = %s, want 2026-04", parsed.PeriodYYYYMM)
	}
	if math.Abs(parsed.ValuePercent-6.9) > 0.000001 {
		t.Fatalf("actual = %.1f, want 6.9", parsed.ValuePercent)
	}
	if parsed.Field != "National unemployment rate" {
		t.Fatalf("field = %s", parsed.Field)
	}
	if parsed.Seasonality != "Seasonally adjusted" {
		t.Fatalf("seasonality = %s", parsed.Seasonality)
	}
	if !strings.Contains(parsed.Sentence, "increased by 0.2 percentage points to 6.9%") {
		t.Fatalf("unexpected parsed sentence: %q", parsed.Sentence)
	}
}

func TestValidateRejectsStalePeriod(t *testing.T) {
	parsed, err := parseDailyArticle(primarySource, []byte(aprilDailyFixture), "https://example.test/dq260508a-eng.htm")
	if err != nil {
		t.Fatalf("parseDailyArticle returned error: %v", err)
	}
	_, _, err = validate(parsed, "2026-05")
	if err == nil || !strings.Contains(err.Error(), "stale source") {
		t.Fatalf("expected stale source error, got %v", err)
	}
}

func TestExtractLatestLFSArticleURL(t *testing.T) {
	url, err := extractLatestLFSArticleURL([]byte(indexFixture))
	if err != nil {
		t.Fatalf("extractLatestLFSArticleURL returned error: %v", err)
	}
	want := "https://www150.statcan.gc.ca/daily-quotidien/260508/dq260508a-eng.htm"
	if url != want {
		t.Fatalf("url = %s, want %s", url, want)
	}
}

func TestDailyArticleCandidateURLs(t *testing.T) {
	eventTime := time.Date(2026, 6, 5, 12, 30, 0, 0, time.UTC)
	urls := dailyArticleCandidateURLs(eventTime)
	if len(urls) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(urls))
	}
	want := "https://www150.statcan.gc.ca/n1/daily-quotidien/260605/dq260605a-eng.htm"
	if urls[0] != want {
		t.Fatalf("first candidate = %s, want %s", urls[0], want)
	}
}

func TestResultFromWDS(t *testing.T) {
	var parsed []wdsDataResponse
	if err := json.Unmarshal([]byte(wdsFixture), &parsed); err != nil {
		t.Fatalf("fixture unmarshal failed: %v", err)
	}
	result, err := resultFromWDS(tableVectorSource, wdsVectorDataURL, fetchMeta{}, parsed)
	if err != nil {
		t.Fatalf("resultFromWDS returned error: %v", err)
	}
	if result.PeriodYYYYMM != "2026-05" {
		t.Fatalf("period = %s, want 2026-05", result.PeriodYYYYMM)
	}
	if result.Value != "6.6%" {
		t.Fatalf("value = %s, want 6.6%%", result.Value)
	}
	if result.Previous != "6.9%" || result.PreviousPeriod != "April 2026" {
		t.Fatalf("previous = %s %s, want 6.9%% April 2026", result.Previous, result.PreviousPeriod)
	}
}
