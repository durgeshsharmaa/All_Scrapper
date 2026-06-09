package main

import (
	"strings"
	"testing"
)

const articleFixture = `
<html>
<head>
<script>
var dataLayer = [{ contentTitle: htmlUnescape("GDP monthly estimate, UK: March 2026") }];
</script>
<link rel="canonical" href="/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/march2026" />
</head>
<body>
<h1>GDP monthly estimate, UK: March 2026</h1>
<p><span>Release date: </span><br/>14 May 2026</p>
<p><span>Next release: </span><br/>12 June 2026</p>
<ul>
<li><p>Monthly GDP grew by 0.3% in March 2026, following a growth of 0.4% in February 2026 and no growth in January 2026.</p></li>
</ul>
<p>GDP is estimated to be 1.2% higher in March 2026 compared with the same month a year ago.</p>
</body>
</html>`

const articleNoYoYFixture = `
<html><body>
<h1>GDP monthly estimate, UK: April 2026</h1>
<p>Release date: 12 June 2026</p>
<p>Next release: 11 July 2026</p>
<p>Monthly GDP fell by 0.2% in April 2026, following a growth of 0.3% in March 2026.</p>
</body></html>`

const aprilArticleURL = "https://www.ons.gov.uk/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/april2026"

const releaseMarchFixture = `
<html><body>
<h1>GDP monthly estimate, UK: March 2026</h1>
<p>Released: 14 May 2026 7:00am</p>
<p>Next release: 12 June 2026</p>
<h2>Publications</h2>
<ul><li><a href="/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/march2026">GDP monthly estimate, UK: March 2026</a></li></ul>
</body></html>`

const releaseExpectedFixture = `
<html><body>
<h1>GDP monthly estimate, UK: April 2026</h1>
<p>Released: 12 June 2026 7:00am</p>
<p>Next release: 11 July 2026</p>
<h2>Publications</h2>
<ul><li><a href="/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/april2026">GDP monthly estimate, UK: April 2026</a></li></ul>
</body></html>`

const datasetFixture = `"Title","Gross Value Added - Monthly (Index 1dp) :CVM SA","Gross Value Added - Monthly (period on period growth) :CVM SA","Gross Value Added - Monthly (period on period 1 year ago growth ) :CVM SA"
"CDID","ECY2","ECYX","ED2S"
"Release Date","14-05-2026","14-05-2026","14-05-2026"
"Next release","12 June 2026","12 June 2026","12 June 2026"
"2025 MAR","102.2","",""
"2025 APR","102.4","",""
"2026 FEB","103.1","0.4","1.0"
"2026 MAR","103.4","0.3","1.2"`

const datasetExpectedFixture = `"Title","Gross Value Added - Monthly (Index 1dp) :CVM SA","Gross Value Added - Monthly (period on period growth) :CVM SA","Gross Value Added - Monthly (period on period 1 year ago growth ) :CVM SA"
"CDID","ECY2","ECYX","ED2S"
"Release Date","12-06-2026","12-06-2026","12-06-2026"
"Next release","11 July 2026","11 July 2026","11 July 2026"
"2025 APR","102.4","",""
"2026 MAR","103.4","0.3","1.2"
"2026 APR","103.2","-0.2","0.8"`

func TestParseArticleMarch2026(t *testing.T) {
	parsed, err := parseArticle(sources[0], []byte(articleFixture), "https://example.test/march2026")
	if err != nil {
		t.Fatalf("parseArticle returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-05-14" {
		t.Fatalf("ReleaseDate=%q, want 2026-05-14", parsed.ReleaseDate)
	}
	if parsed.NextRelease != "2026-06-12" {
		t.Fatalf("NextRelease=%q, want 2026-06-12", parsed.NextRelease)
	}
	if parsed.PeriodYYYYMM != "2026-03" {
		t.Fatalf("PeriodYYYYMM=%q, want 2026-03", parsed.PeriodYYYYMM)
	}
	if parsed.MoM.Actual != "0.3%" {
		t.Fatalf("MoM=%q, want 0.3%%", parsed.MoM.Actual)
	}
	if !parsed.HasYoY || parsed.YoY.Actual != "1.2%" {
		t.Fatalf("YoY=%q HasYoY=%v, want 1.2%% true", parsed.YoY.Actual, parsed.HasYoY)
	}
	if !strings.Contains(parsed.MoM.Sentence, "Monthly GDP grew by 0.3% in March 2026") {
		t.Fatalf("unexpected MoM sentence: %q", parsed.MoM.Sentence)
	}
}

func TestParseArticleAllowsMissingYoY(t *testing.T) {
	parsed, err := parseArticle(sources[0], []byte(articleNoYoYFixture), aprilArticleURL)
	if err != nil {
		t.Fatalf("parseArticle returned error: %v", err)
	}
	if parsed.PeriodYYYYMM != "2026-04" {
		t.Fatalf("PeriodYYYYMM=%q, want 2026-04", parsed.PeriodYYYYMM)
	}
	if parsed.MoM.Actual != "-0.2%" {
		t.Fatalf("MoM=%q, want -0.2%%", parsed.MoM.Actual)
	}
	if parsed.HasYoY {
		t.Fatalf("HasYoY=true, want false")
	}
	if len(parsed.Warnings) == 0 {
		t.Fatal("expected YoY fallback warning")
	}
}

func TestParseReleaseDiscoveryMarch2026(t *testing.T) {
	parsed, err := parseReleaseDiscovery(sources[2], []byte(releaseMarchFixture), "https://www.ons.gov.uk/releases/gdpmonthlyestimateukmarch2026")
	if err != nil {
		t.Fatalf("parseReleaseDiscovery returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-05-14" {
		t.Fatalf("ReleaseDate=%q, want 2026-05-14", parsed.ReleaseDate)
	}
	if parsed.ReleasedAtUTC != "2026-05-14 06:00:00" {
		t.Fatalf("ReleasedAtUTC=%q, want 2026-05-14 06:00:00", parsed.ReleasedAtUTC)
	}
	if parsed.NextRelease != "2026-06-12" {
		t.Fatalf("NextRelease=%q, want 2026-06-12", parsed.NextRelease)
	}
	if parsed.ArticleURL != "https://www.ons.gov.uk/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/march2026" {
		t.Fatalf("ArticleURL=%q", parsed.ArticleURL)
	}
	if !parsed.ScheduleConfirmed {
		t.Fatal("expected previous release page to confirm next release schedule")
	}
}

func TestParseDatasetMarch2026(t *testing.T) {
	parsed, err := parseDataset(sources[1], []byte(datasetFixture), datasetCSVURL)
	if err != nil {
		t.Fatalf("parseDataset returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-05-14" {
		t.Fatalf("ReleaseDate=%q, want 2026-05-14", parsed.ReleaseDate)
	}
	if parsed.PeriodYYYYMM != "2026-03" {
		t.Fatalf("PeriodYYYYMM=%q, want 2026-03", parsed.PeriodYYYYMM)
	}
	if parsed.MoM.Actual != "0.3%" {
		t.Fatalf("MoM=%q, want 0.3%%", parsed.MoM.Actual)
	}
	if parsed.YoY.Actual != "1.2%" {
		t.Fatalf("YoY=%q, want 1.2%%", parsed.YoY.Actual)
	}
}

func TestComposeConfirmedUsesDatasetYoYFallback(t *testing.T) {
	article, err := parseArticle(sources[0], []byte(articleNoYoYFixture), aprilArticleURL)
	if err != nil {
		t.Fatalf("parseArticle returned error: %v", err)
	}
	dataset, err := parseDataset(sources[1], []byte(datasetExpectedFixture), datasetCSVURL)
	if err != nil {
		t.Fatalf("parseDataset returned error: %v", err)
	}
	release, err := parseReleaseDiscovery(sources[2], []byte(releaseExpectedFixture), "https://www.ons.gov.uk/releases/gdpmonthlyestimateukapril2026")
	if err != nil {
		t.Fatalf("parseReleaseDiscovery returned error: %v", err)
	}
	confirmed, err := composeConfirmed(article, dataset, release, map[string]Snapshot{
		"dataset":           {Result: dataset},
		"release-discovery": {Result: release},
	})
	if err != nil {
		t.Fatalf("composeConfirmed returned error: %v", err)
	}
	if confirmed.MoM.Actual != "-0.2%" {
		t.Fatalf("MoM=%q, want -0.2%%", confirmed.MoM.Actual)
	}
	if confirmed.YoY.Actual != "0.8%" {
		t.Fatalf("YoY=%q, want 0.8%%", confirmed.YoY.Actual)
	}
	if confirmed.YoY.ValueMethod != "Official MGDP dataset fallback" {
		t.Fatalf("YoY ValueMethod=%q", confirmed.YoY.ValueMethod)
	}
	if !confirmed.ScheduleConfirmed {
		t.Fatal("expected schedule to be confirmed")
	}
}

func TestComposeConfirmedRejectsDatasetDisagreement(t *testing.T) {
	article, err := parseArticle(sources[0], []byte(`
<html><body>
<h1>GDP monthly estimate, UK: April 2026</h1>
<p>Release date: 12 June 2026</p>
<p>Next release: 11 July 2026</p>
<p>Monthly GDP grew by 0.1% in April 2026.</p>
<p>GDP is estimated to be 0.8% higher in April 2026 compared with the same month a year ago.</p>
</body></html>`), aprilArticleURL)
	if err != nil {
		t.Fatalf("parseArticle returned error: %v", err)
	}
	dataset, err := parseDataset(sources[1], []byte(datasetExpectedFixture), datasetCSVURL)
	if err != nil {
		t.Fatalf("parseDataset returned error: %v", err)
	}
	release, err := parseReleaseDiscovery(sources[2], []byte(releaseExpectedFixture), "https://www.ons.gov.uk/releases/gdpmonthlyestimateukapril2026")
	if err != nil {
		t.Fatalf("parseReleaseDiscovery returned error: %v", err)
	}
	_, err = composeConfirmed(article, dataset, release, map[string]Snapshot{
		"dataset":           {Result: dataset},
		"release-discovery": {Result: release},
	})
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected disagreement error, got %v", err)
	}
}
