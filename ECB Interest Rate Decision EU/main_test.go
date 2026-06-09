package main

import (
	"math"
	"testing"
	"time"
)

const indexFixture = `
<dl id="lazyload-container" data-snippets='../2026/html/index_include.en.html,../2025/html/index_include.en.html'></dl>
`

const snippetFixture = `
<dt isoDate="2026-04-30"><div class="date">30 April 2026</div></dt>
<dd><div class="title"><a href="/press/pr/date/2026/html/ecb.mp260430~81b7179e6f.en.html">Monetary policy decisions</a></div>
<div class='accordion'><div class="content-box"><dl>
<dt isoDate="2026-04-30"><div class="date">30 April 2026</div></dt>
<dd><div class="title"><a href="/press/press_conference/monetary-policy-statement/shared/pdf/ecb.ds260430~1c397fa90c.en.pdf">Combined monetary policy decisions and statement</a></div></dd>
</dl></div></div></dd>
<dt isoDate="2026-03-19"><div class="date">19 March 2026</div></dt>
<dd><div class="title"><a href="/press/pr/date/2026/html/ecb.mp260319~3057739775.en.html">Monetary policy decisions</a></div></dd>
`

const releaseFixture = `
<html>
<head><meta property="article:published_time" content="2026-04-30"></head>
<body>
<h1>Monetary policy decisions</h1>
<p>30 April 2026</p>
<h2>Key ECB interest rates</h2>
<p>The interest rates on the deposit facility, the main refinancing operations and the marginal lending facility will remain unchanged at 2.00%, 2.15% and 2.40% respectively.</p>
</body>
</html>
`

const keyRatesFixture = `
<table>
<tbody>
<tr>
  <td class="number"><strong>2025</strong></td>
  <td class="number">11 Jun.</td>
  <td class="number">2.00</td>
  <td class="number">2.15</td>
  <td class="number">-</td>
  <td class="number">2.40</td>
</tr>
<tr>
  <td class="number">&nbsp;</td>
  <td class="number">23 Apr.</td>
  <td class="number">2.25</td>
  <td class="number">2.40</td>
  <td class="number">-</td>
  <td class="number">2.65</td>
</tr>
</tbody>
</table>
`

func TestConfiguredEventTimeISTMatchesUTC(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	ist, err := time.ParseInLocation("2006-01-02 15:04:05", defaultEventTimeIST, loc)
	if err != nil {
		t.Fatalf("parse defaultEventTimeIST: %v", err)
	}
	utc, err := time.ParseInLocation("2006-01-02 15:04:05", defaultEventTimeUTC, time.UTC)
	if err != nil {
		t.Fatalf("parse defaultEventTimeUTC: %v", err)
	}
	if !ist.UTC().Equal(utc) {
		t.Fatalf("IST config %s converts to %s UTC, want %s", defaultEventTimeIST, ist.UTC().Format("2006-01-02 15:04:05"), defaultEventTimeUTC)
	}
}

func TestParseSnippetURLs(t *testing.T) {
	got, err := parseSnippetURLs(indexURL, []byte(indexFixture))
	if err != nil {
		t.Fatalf("parseSnippetURLs returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("snippet count=%d, want 2", len(got))
	}
	if got[0] != "https://www.ecb.europa.eu/press/govcdec/mopo/2026/html/index_include.en.html" {
		t.Fatalf("first snippet=%q", got[0])
	}
}

func TestParseDecisionItems(t *testing.T) {
	got, err := parseDecisionItems("https://www.ecb.europa.eu/press/govcdec/mopo/2026/html/index_include.en.html", []byte(snippetFixture))
	if err != nil {
		t.Fatalf("parseDecisionItems returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("item count=%d, want 2", len(got))
	}
	if got[0].DateISO != "2026-04-30" {
		t.Fatalf("latest date=%q, want 2026-04-30", got[0].DateISO)
	}
	if got[0].LinkSlug != "ecb.mp260430~81b7179e6f.en.html" {
		t.Fatalf("link slug=%q", got[0].LinkSlug)
	}
	if got[0].URL != "https://www.ecb.europa.eu/press/pr/date/2026/html/ecb.mp260430~81b7179e6f.en.html" {
		t.Fatalf("absolute URL=%q", got[0].URL)
	}
}

func TestParseReleaseTitleDateAndRates(t *testing.T) {
	if got := parseTitle([]byte(releaseFixture)); got != "Monetary policy decisions" {
		t.Fatalf("title=%q", got)
	}
	if got := parseReleaseDate([]byte(releaseFixture)); got != "2026-04-30" {
		t.Fatalf("date=%q", got)
	}
	rates, err := parseKeyRates([]byte(releaseFixture))
	if err != nil {
		t.Fatalf("parseKeyRates returned error: %v", err)
	}
	if math.Abs(rates.DepositFacility-2.00) > 0.000001 {
		t.Fatalf("deposit=%.2f", rates.DepositFacility)
	}
	if math.Abs(rates.MainRefinancingOperation-2.15) > 0.000001 {
		t.Fatalf("mro=%.2f", rates.MainRefinancingOperation)
	}
	if math.Abs(rates.MarginalLendingFacility-2.40) > 0.000001 {
		t.Fatalf("mlf=%.2f", rates.MarginalLendingFacility)
	}
}

func TestParseRatesWhenOrderChanges(t *testing.T) {
	body := []byte(`<p>The interest rate on the main refinancing operations and the interest rates on the marginal lending facility and the deposit facility will be increased to 3.10%, 3.35% and 2.95% respectively.</p>`)
	rates, err := parseKeyRates(body)
	if err != nil {
		t.Fatalf("parseKeyRates returned error: %v", err)
	}
	if rates.MainRefinancingOperation != 3.10 || rates.MarginalLendingFacility != 3.35 || rates.DepositFacility != 2.95 {
		t.Fatalf("unexpected rates: %+v", rates)
	}
}

func TestParseSplitRateSentences(t *testing.T) {
	body := []byte(`
<p>The Governing Council today decided to keep the three key ECB interest rates unchanged.</p>
<p>The interest rate on the main refinancing operations will remain unchanged at 2.15%.</p>
<p>The interest rates on the marginal lending facility and the deposit facility will remain unchanged at 2.40% and 2.00% respectively.</p>
`)
	rates, err := parseKeyRates(body)
	if err != nil {
		t.Fatalf("parseKeyRates returned error: %v", err)
	}
	if rates.MainRefinancingOperation != 2.15 || rates.MarginalLendingFacility != 2.40 || rates.DepositFacility != 2.00 {
		t.Fatalf("unexpected rates: %+v", rates)
	}
}

func TestActualUsesMainRefinancingOperations(t *testing.T) {
	rates := ParsedRates{
		DepositFacility:          2.00,
		MainRefinancingOperation: 2.15,
		MarginalLendingFacility:  2.40,
	}
	if got := actualFromRates(rates); got != "2.15" {
		t.Fatalf("actual=%q, want 2.15", got)
	}
	if actualRateField != "main_refinancing_operations" {
		t.Fatalf("actualRateField=%q", actualRateField)
	}
}

func TestParseKeyInterestRatesPage(t *testing.T) {
	got, err := parseKeyInterestRatesPage([]byte(keyRatesFixture))
	if err != nil {
		t.Fatalf("parseKeyInterestRatesPage returned error: %v", err)
	}
	if got.EffectiveDate != "2025-06-11" {
		t.Fatalf("effective date=%q, want 2025-06-11", got.EffectiveDate)
	}
	if got.Actual != "2.15" || got.MainRefinancingOperationsRate != "2.15" {
		t.Fatalf("actual/MRO=%q/%q, want 2.15/2.15", got.Actual, got.MainRefinancingOperationsRate)
	}
	if got.DepositFacilityRate != "2.00" || got.MarginalLendingFacilityRate != "2.40" {
		t.Fatalf("deposit/marginal=%q/%q, want 2.00/2.40", got.DepositFacilityRate, got.MarginalLendingFacilityRate)
	}
}

func TestSource3ConfirmsDecision(t *testing.T) {
	decision := RateDecision{
		Actual:                        "2.15",
		MainRefinancingOperationsRate: "2.15",
		DepositFacilityRate:           "2.00",
		MarginalLendingFacilityRate:   "2.40",
	}
	keyRates := KeyRatesPageResult{
		Actual:                        "2.15",
		MainRefinancingOperationsRate: "2.15",
		DepositFacilityRate:           "2.00",
		MarginalLendingFacilityRate:   "2.40",
	}
	if !source3ConfirmsDecision(decision, keyRates) {
		t.Fatal("expected Source 3 to confirm Source 2 decision rates")
	}
}

func TestIsNewDecision(t *testing.T) {
	baseline := DecisionItem{DateISO: "2026-04-30", URL: "old"}
	latest := DecisionItem{DateISO: "2026-06-11", URL: "new"}
	if !isNewDecision(latest, baseline, "2026-06-11") {
		t.Fatal("expected new decision for expected date")
	}
	if isNewDecision(latest, baseline, "2026-07-23") {
		t.Fatal("did not expect trigger for a different expected date")
	}
}
