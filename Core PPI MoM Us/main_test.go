package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func restoreEventTimeConfig(t *testing.T) {
	t.Helper()
	oldFlag := *eventTimeUTCFlag
	oldEnv, hadEnv := os.LookupEnv(eventTimeEnv)
	t.Cleanup(func() {
		*eventTimeUTCFlag = oldFlag
		if hadEnv {
			_ = os.Setenv(eventTimeEnv, oldEnv)
			return
		}
		_ = os.Unsetenv(eventTimeEnv)
	})
}

func TestParseConfiguredEventTimeRequiresRuntimeConfig(t *testing.T) {
	restoreEventTimeConfig(t)
	*eventTimeUTCFlag = ""
	_ = os.Unsetenv(eventTimeEnv)

	if _, _, err := parseConfiguredEventTime(); err == nil {
		t.Fatal("expected missing event time to fail")
	}
}

func TestParseConfiguredEventTimeUsesFlagBeforeEnv(t *testing.T) {
	restoreEventTimeConfig(t)
	*eventTimeUTCFlag = "2026-06-11 12:30:00"
	_ = os.Setenv(eventTimeEnv, "2027-06-11 12:30:00")

	got, source, err := parseConfiguredEventTime()
	if err != nil {
		t.Fatalf("parse configured event time: %v", err)
	}
	want := time.Date(2026, time.June, 11, 12, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("event time=%s, want %s", got, want)
	}
	if source != "-event-time-utc" {
		t.Fatalf("source=%q, want flag source", source)
	}
}

func TestParseConfiguredEventTimeUsesEnv(t *testing.T) {
	restoreEventTimeConfig(t)
	*eventTimeUTCFlag = ""
	_ = os.Setenv(eventTimeEnv, "2026-06-11 12:30:00")

	got, source, err := parseConfiguredEventTime()
	if err != nil {
		t.Fatalf("parse configured event time: %v", err)
	}
	want := time.Date(2026, time.June, 11, 12, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("event time=%s, want %s", got, want)
	}
	if source != eventTimeEnv {
		t.Fatalf("source=%q, want %s", source, eventTimeEnv)
	}
}

func TestPrimaryParserWithSampleOfficialHTML(t *testing.T) {
	parsed, warnings, err := parsePPIHTML([]byte(samplePPIHTML("May", 2026, 0.6, 0.4)))
	if err != nil {
		t.Fatalf("parse PPI HTML: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none", warnings)
	}
	if parsed.Period != "2026-05" {
		t.Fatalf("period=%q, want 2026-05", parsed.Period)
	}
	if len(parsed.Measures) != 4 {
		t.Fatalf("measure count=%d, want 4", len(parsed.Measures))
	}
	assertMeasure(t, parsed.Measures, "PPI (MoM)", "0.6%")
	assertMeasure(t, parsed.Measures, "PPI (YoY)", "2.6%")
	assertMeasure(t, parsed.Measures, "Core PPI (MoM)", "0.4%")
	assertMeasure(t, parsed.Measures, "Core PPI (YoY)", "2.8%")
}

func TestPrimaryParserRejectsAmbiguousRows(t *testing.T) {
	body := strings.Replace(samplePPIHTML("May", 2026, 0.6, 0.4), "</table>", `<tr><td>Final demand</td><td>FD</td><td>4</td><td>100</td><td>2.6</td><td>0.1</td><td>0.2</td><td>0.3</td><td>0.4</td><td>0.5</td><td>0.6</td></tr></table>`, 1)
	if _, _, err := parsePPIHTML([]byte(body)); err == nil {
		t.Fatal("expected ambiguous duplicate row to fail")
	}
}

func TestValidateSnapshotRejectsWrongPeriod(t *testing.T) {
	result := sampleSnapshot(t, primarySource, "April", 2026, 0.5, 0.3)
	if err := validateSnapshot(result, "2026-05"); err == nil {
		t.Fatal("expected wrong period to fail validation")
	}
}

func TestValidateSnapshotRejectsWrongColumn(t *testing.T) {
	result := sampleSnapshot(t, primarySource, "May", 2026, 0.6, 0.4)
	result.Measures[0].Column = "wrong column"
	if err := validateSnapshot(result, "2026-05"); err == nil {
		t.Fatal("expected wrong column to fail validation")
	}
}

func TestSecondaryConfirmationConfirmed(t *testing.T) {
	primary := sampleSnapshot(t, primarySource, "May", 2026, 0.6, 0.4)
	pdf := sampleSnapshot(t, pdfSource, "May", 2026, 0.6, 0.4)

	outcome := classifyConfirmation(pdfSource, pdf, primary, "2026-05")
	if outcome.Status != "CONFIRMED" {
		t.Fatalf("status=%s detail=%s, want CONFIRMED", outcome.Status, outcome.Detail)
	}
}

func TestSecondaryConfirmationMismatch(t *testing.T) {
	primary := sampleSnapshot(t, primarySource, "May", 2026, 0.6, 0.4)
	pdf := sampleSnapshot(t, pdfSource, "May", 2026, 0.7, 0.4)

	outcome := classifyConfirmation(pdfSource, pdf, primary, "2026-05")
	if outcome.Status != "MISMATCH" {
		t.Fatalf("status=%s detail=%s, want MISMATCH", outcome.Status, outcome.Detail)
	}
}

func TestSecondaryConfirmationNotUpdated(t *testing.T) {
	primary := sampleSnapshot(t, primarySource, "May", 2026, 0.6, 0.4)
	pdf := sampleSnapshot(t, pdfSource, "April", 2026, 0.5, 0.3)

	outcome := classifyConfirmation(pdfSource, pdf, primary, "2026-05")
	if outcome.Status != "NOT_UPDATED" {
		t.Fatalf("status=%s detail=%s, want NOT_UPDATED", outcome.Status, outcome.Detail)
	}
}

func TestFastPathPrimaryOnlySuccess(t *testing.T) {
	eventTime := time.Date(2026, time.June, 11, 12, 30, 0, 0, time.UTC)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != tableURL {
			t.Fatalf("requested URL=%s, want %s", req.URL.String(), tableURL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":  []string{"text/html"},
				"ETag":          []string{`"fresh"`},
				"Last-Modified": []string{"Thu, 11 Jun 2026 12:30:00 GMT"},
				"Cache-Control": []string{"max-age=0"},
				"Date":          []string{"Thu, 11 Jun 2026 12:30:00 GMT"},
			},
			Body: io.NopCloser(strings.NewReader(samplePPIHTML("May", 2026, 0.6, 0.4))),
		}, nil
	})}

	states := map[string]*SourceResult{}
	got, err := runPrimaryFastPath(client, states, eventTime, time.Now().UTC().Add(2*time.Second), "2026-05", logDiscard())
	if err != nil {
		t.Fatalf("run primary fast path: %v", err)
	}
	if got.Source != primarySource.Name {
		t.Fatalf("source=%s, want %s", got.Source, primarySource.Name)
	}
	if got.DetectionMethod != "primary-content" {
		t.Fatalf("method=%s, want primary-content", got.DetectionMethod)
	}
	assertMeasure(t, got.Measures, "Core PPI (MoM)", "0.4%")
}

func sampleSnapshot(t *testing.T, source Source, month string, year int, ppiMoM, coreMoM float64) *SnapshotResult {
	t.Helper()
	parsed, warnings, err := parsePPIHTML([]byte(samplePPIHTML(month, year, ppiMoM, coreMoM)))
	if err != nil {
		t.Fatalf("parse sample snapshot: %v", err)
	}
	applyMeasureMetadata(parsed.Measures, source.ValueMethod, source.Confidence)
	result := parsedSnapshotToResult(source, parsed, warnings)
	result.URL = source.URL
	result.SourceType = source.SourceType
	result.Timestamp = time.Now().UTC()
	return result
}

func samplePPIHTML(month string, year int, ppiMoM, coreMoM float64) string {
	return fmt.Sprintf(`
<html>
<body>
<h1>Producer Price Indexes</h1>
<table>
<caption>Table 1. Producer Price Indexes [ %s %d ]</caption>
<tr><th>Group</th><th>Code</th><th>Item</th><th>Relative importance</th><th>Unadjusted 12-month percent change</th><th colspan="5">Seasonally adjusted 1-month percent change</th></tr>
<tr><td>Final demand</td><td>FD</td><td>4</td><td>100.000</td><td>2.6</td><td>0.1</td><td>0.2</td><td>0.3</td><td>0.4</td><td>0.5</td><td>%.1f</td></tr>
<tr><td>Final demand less foods and energy</td><td>FD</td><td>49104</td><td>70.000</td><td>2.8</td><td>0.1</td><td>0.1</td><td>0.2</td><td>0.2</td><td>0.3</td><td>%.1f</td></tr>
</table>
<p>%d M%02d Results</p>
</body>
</html>`, month, year, ppiMoM, coreMoM, year, monthNumber(month))
}

func assertMeasure(t *testing.T, measures []MeasureResult, name, want string) {
	t.Helper()
	for _, measure := range measures {
		if measure.EventName == name {
			if measure.Actual != want {
				t.Fatalf("%s actual=%s, want %s", name, measure.Actual, want)
			}
			return
		}
	}
	t.Fatalf("measure %q not found", name)
}

func logDiscard() *log.Logger {
	return log.New(io.Discard, "", 0)
}
