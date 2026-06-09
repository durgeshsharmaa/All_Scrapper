package main

import (
	"math"
	"strings"
	"testing"
)

const listingFixture = `
<html><body>
<main>
<p>May 27, 2026</p>
<h3><a href="/2026/05/unrelated/">Bank of Canada joins BIS Project Agora</a></h3>
<p>The Bank of Canada announced today it is joining a project.</p>

<p>April 29, 2026</p>
<h3><a href="/2026/04/fad-press-release-2026-04-29/">Bank of Canada maintains policy rate at 2&#188;%</a></h3>
<p>Media Relations Ottawa, Ontario</p>
<p>The Bank of Canada today held its target for the overnight rate at 2.25%, with the Bank Rate at 2.5% and the deposit rate at 2.20%.</p>
</main>
</body></html>`

const releaseDayListingFixture = `
<html><body>
<p>June 10, 2026</p>
<h3><a href="/2026/06/fad-press-release-2026-06-10/">Bank of Canada maintains policy rate at 2&#188;%</a></h3>
<p>The Bank of Canada today held its target for the overnight rate at 2.25%, with the Bank Rate at 2.5% and the deposit rate at 2.20%.</p>
</body></html>`

const reductionFixture = `
<html><body>
<p>October 29, 2025</p>
<h3><a href="/2025/10/fad-press-release-2025-10-29/">Bank of Canada reduces policy rate by 25 basis points</a></h3>
<p>The Bank of Canada today reduced its target for the overnight rate to 2.25%, with the Bank Rate at 2.5% and the deposit rate at 2.20%.</p>
</body></html>`

const policyRateFixture = `
<html><body>
<h1>Policy interest rate</h1>
<p>The Bank carries out monetary policy by adjusting the target for the overnight rate on eight fixed dates each year.</p>
<h2>Recent data</h2>
<table>
<tr><th>Date*</th><th>Target (%)</th><th>Change (%)</th></tr>
<tr><td>April 29, 2026</td><td>2.25</td><td>---</td></tr>
<tr><td>March 18, 2026</td><td>2.25</td><td>---</td></tr>
</table>
<p>*As of 2021, a change takes effect the day after its announcement.</p>
<h2>Schedule for 2026</h2>
<table>
<tr><td>January&nbsp;28</td><td>Interest rate announcement and Monetary Policy Report</td></tr>
<tr><td>March&nbsp;18</td><td>Interest rate announcement</td></tr>
<tr><td>April&nbsp;29</td><td>Interest rate announcement and Monetary Policy Report</td></tr>
<tr><td>June&nbsp;10</td><td>Interest rate announcement</td></tr>
<tr><td>July&nbsp;15</td><td>Interest rate announcement and Monetary Policy Report</td></tr>
<tr><td>September&nbsp;2</td><td>Interest rate announcement</td></tr>
<tr><td>October&nbsp;28</td><td>Interest rate announcement and Monetary Policy Report</td></tr>
<tr><td>December&nbsp;9</td><td>Interest rate announcement</td></tr>
</table>
<p>See Blackout Guidelines for communications around fixed announcement dates.</p>
</body></html>`

const policyInstrumentFixture = `
{
  "groupDetail": {
    "label": "Policy Instrument",
    "description": "Policy Instrument"
  },
  "seriesDetail": {
    "STATIC_ATABLE_V39079": {
      "label": "Overnight rate target (end of month)",
      "description": "Overnight rate target (end of month)"
    },
    "V122514": {
      "label": "Overnight rate",
      "description": "Overnight rate"
    }
  },
  "observations": [
    {
      "d": "2026-05-01",
      "V122514": {
        "v": "2.2422"
      },
      "STATIC_ATABLE_V39079": {
        "v": "2.25"
      }
    }
  ]
}`

func TestParsePressReleaseListingUsesOvernightTarget(t *testing.T) {
	parsed, err := parsePressReleaseListing(primarySource(), []byte(listingFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-04-29" {
		t.Fatalf("release date = %s, want 2026-04-29", parsed.ReleaseDate)
	}
	if parsed.Value != "2.25%" {
		t.Fatalf("value = %s, want 2.25%%", parsed.Value)
	}
	if math.Abs(parsed.NumericValue-2.25) > 0.000001 {
		t.Fatalf("numeric value = %.4f, want 2.25", parsed.NumericValue)
	}
	if strings.Contains(parsed.Value, "2.5") || strings.Contains(parsed.Value, "2.20") {
		t.Fatalf("parsed Bank Rate or deposit rate instead of overnight target: %s", parsed.Value)
	}
	if !strings.Contains(parsed.Sentence, "target for the overnight rate at 2.25%") {
		t.Fatalf("unexpected sentence: %q", parsed.Sentence)
	}
}

func TestParsePressReleaseListingReductionTitle(t *testing.T) {
	parsed, err := parsePressReleaseListing(primarySource(), []byte(reductionFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	if parsed.ReleaseDate != "2025-10-29" {
		t.Fatalf("release date = %s, want 2025-10-29", parsed.ReleaseDate)
	}
	if parsed.Value != "2.25%" {
		t.Fatalf("value = %s, want 2.25%%", parsed.Value)
	}
}

func TestValidatePrimaryRejectsStaleDate(t *testing.T) {
	parsed, err := parsePressReleaseListing(primarySource(), []byte(listingFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	err = validatePrimary(parsed)
	if err == nil || !strings.Contains(err.Error(), "stale primary source") {
		t.Fatalf("expected stale primary source error, got %v", err)
	}
}

func TestValidatePrimaryAcceptsConfiguredReleaseDate(t *testing.T) {
	parsed, err := parsePressReleaseListing(primarySource(), []byte(releaseDayListingFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	if err := validatePrimary(parsed); err != nil {
		t.Fatalf("validatePrimary returned error: %v", err)
	}
}

func TestParsePolicyRatePageCurrentRateAndSchedule(t *testing.T) {
	parsed, err := parsePolicyRatePage(sources[1], []byte(policyRateFixture))
	if err != nil {
		t.Fatalf("parsePolicyRatePage returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-04-29" {
		t.Fatalf("current date = %s, want 2026-04-29", parsed.ReleaseDate)
	}
	if parsed.Value != "2.25%" {
		t.Fatalf("current rate = %s, want 2.25%%", parsed.Value)
	}
	if !parsed.ScheduleConfirmed {
		t.Fatalf("expected schedule to confirm %s: %v", expectedReleaseDate, parsed.ScheduledDates)
	}
	if len(parsed.ScheduledDates) != 8 {
		t.Fatalf("schedule dates = %v, want 8 dates", parsed.ScheduledDates)
	}
}

func TestParsePolicyInstrumentDataCurrentTarget(t *testing.T) {
	parsed, err := parsePolicyInstrumentData(sources[2], []byte(policyInstrumentFixture))
	if err != nil {
		t.Fatalf("parsePolicyInstrumentData returned error: %v", err)
	}
	if parsed.ReleaseDate != "2026-05-01" {
		t.Fatalf("current date = %s, want 2026-05-01", parsed.ReleaseDate)
	}
	if parsed.Value != "2.25%" {
		t.Fatalf("current rate = %s, want 2.25%%", parsed.Value)
	}
	if math.Abs(parsed.NumericValue-2.25) > 0.000001 {
		t.Fatalf("numeric value = %.4f, want 2.25", parsed.NumericValue)
	}
}

func TestValidateConfirmationAcceptsMatchingBackups(t *testing.T) {
	primary, err := parsePressReleaseListing(primarySource(), []byte(releaseDayListingFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	policy, err := parsePolicyRatePage(sources[1], []byte(policyRateFixture))
	if err != nil {
		t.Fatalf("parsePolicyRatePage returned error: %v", err)
	}
	instrument, err := parsePolicyInstrumentData(sources[2], []byte(policyInstrumentFixture))
	if err != nil {
		t.Fatalf("parsePolicyInstrumentData returned error: %v", err)
	}
	warnings, err := validateConfirmation(primary, map[string]Snapshot{
		"policy-rate-page":  {Result: policy},
		"policy-instrument": {Result: instrument},
	})
	if err != nil {
		t.Fatalf("validateConfirmation returned error: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected backup warnings for older backup dates")
	}
}

func TestValidateConfirmationRejectsSourceDisagreement(t *testing.T) {
	primary, err := parsePressReleaseListing(primarySource(), []byte(releaseDayListingFixture))
	if err != nil {
		t.Fatalf("parsePressReleaseListing returned error: %v", err)
	}
	instrument, err := parsePolicyInstrumentData(sources[2], []byte(policyInstrumentFixture))
	if err != nil {
		t.Fatalf("parsePolicyInstrumentData returned error: %v", err)
	}
	instrument.Value = "2.50%"
	instrument.NumericValue = 2.5
	_, err = validateConfirmation(primary, map[string]Snapshot{
		"policy-rate-page":  {Error: "skipped in test"},
		"policy-instrument": {Result: instrument},
	})
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected source disagreement error, got %v", err)
	}
}
