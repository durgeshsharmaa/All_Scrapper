package main

import (
	"math"
	"strings"
	"testing"
)

const tableFixture = `
<html>
<head><title>Table B-1. Employees on nonfarm payrolls by industry sector and selected industry detail - 2026 M04 Results</title></head>
<body>
<pre>
 ESTABLISHMENT DATA
 Table B-1. Employees on nonfarm payrolls by industry sector and selected industry detail
 [In thousands]

 Industry                                      Not seasonally adjusted             Seasonally adjusted
                                               Apr. 2025 Feb. 2026 Mar. 2026(p) Apr. 2026(p) Apr. 2025 Feb. 2026 Mar. 2026(p) Apr. 2026(p) Change from:
                                                                                                                                                Mar. 2026 - Apr. 2026(p)

 Total nonfarm
 158,368 157,214 157,769 158,695 158,485 158,436 158,621 158,736 115

 Total private

 134,457 133,640 134,125 135,049 134,917 135,115 135,305 135,428 123

 Goods-producing
 21,430 21,126 21,239 21,401 21,555 21,480 21,513 21,523 10
</pre>
</body>
</html>`

const cesFixture = `series_id        	year	period	       value	footnote_codes
CES0500000001    	2026	M01	      135263
CES0500000001    	2026	M02	      135115
CES0500000001    	2026	M03	      135305	P
CES0500000001    	2026	M04	      135428	P
`

const htmlRowFixture = `
<html>
<head><title>Table B-1. Employees on nonfarm payrolls by industry sector and selected industry detail - 2026 M04 Results</title></head>
<body>
<h1>Table B-1. Employees on nonfarm payrolls by industry sector and selected industry detail</h1>
<table>
<caption>ESTABLISHMENT DATA Table B-1. Employees on nonfarm payrolls by industry sector and selected industry detail [In thousands]</caption>
<thead><tr><th>Change from:</th><th>Mar. 2026 - Apr. 2026(p)</th></tr></thead>
<tbody>
<tr class="greenbar">
	<th><p class="sub1">Total private</p></th>
	<td><span class="datavalue">134,457</span></td>
	<td><span class="datavalue">133,640</span></td>
	<td><span class="datavalue">134,125</span></td>
	<td><span class="datavalue">135,049</span></td>
	<td><span class="datavalue">134,917</span></td>
	<td><span class="datavalue">135,115</span></td>
	<td><span class="datavalue">135,305</span></td>
	<td><span class="datavalue">135,428</span></td>
	<td><span class="datavalue">123</span></td>
</tr>
</tbody>
</table>
</body>
</html>`

func TestParseBLSTableB1(t *testing.T) {
	parsed, err := parseBLSTableB1(primarySource, []byte(tableFixture))
	if err != nil {
		t.Fatalf("parseBLSTableB1 returned error: %v", err)
	}
	if parsed.PeriodYYYYMM != "2026-04" {
		t.Fatalf("period = %s, want 2026-04", parsed.PeriodYYYYMM)
	}
	if parsed.FromPeriodYYYYMM != "2026-03" {
		t.Fatalf("from period = %s, want 2026-03", parsed.FromPeriodYYYYMM)
	}
	if parsed.PagePeriodYYYYMM != "2026-04" {
		t.Fatalf("page period = %s, want 2026-04", parsed.PagePeriodYYYYMM)
	}
	if math.Abs(parsed.ActualThousands-123) > 0.000001 {
		t.Fatalf("actual = %.1f, want 123", parsed.ActualThousands)
	}
}

func TestValidateExpectedPeriod(t *testing.T) {
	parsed, err := parseBLSTableB1(primarySource, []byte(tableFixture))
	if err != nil {
		t.Fatalf("parseBLSTableB1 returned error: %v", err)
	}
	confidence, warnings, err := validate(parsed, "2026-04", false)
	if err != nil {
		t.Fatalf("validate returned error: %v", err)
	}
	if confidence != "HIGH" {
		t.Fatalf("confidence = %s, want HIGH; warnings=%v", confidence, warnings)
	}
	_, _, err = validate(parsed, "2026-05", false)
	if err == nil || !strings.Contains(err.Error(), "stale source") {
		t.Fatalf("expected stale source error, got %v", err)
	}
}

func TestParseBLSTableB1HTMLRow(t *testing.T) {
	parsed, err := parseBLSTableB1(primarySource, []byte(htmlRowFixture))
	if err != nil {
		t.Fatalf("parseBLSTableB1 returned error: %v", err)
	}
	if parsed.PeriodYYYYMM != "2026-04" {
		t.Fatalf("period = %s, want 2026-04", parsed.PeriodYYYYMM)
	}
	if math.Abs(parsed.ActualThousands-123) > 0.000001 {
		t.Fatalf("actual = %.1f, want 123", parsed.ActualThousands)
	}
}

func TestParseCESFlatFileFallback(t *testing.T) {
	parsed, err := parseCESFlatFile(fallbackSource, []byte(cesFixture))
	if err != nil {
		t.Fatalf("parseCESFlatFile returned error: %v", err)
	}
	if parsed.PeriodYYYYMM != "2026-04" {
		t.Fatalf("period = %s, want 2026-04", parsed.PeriodYYYYMM)
	}
	if parsed.FromPeriodYYYYMM != "2026-03" {
		t.Fatalf("from period = %s, want 2026-03", parsed.FromPeriodYYYYMM)
	}
	if math.Abs(parsed.ActualThousands-123) > 0.000001 {
		t.Fatalf("actual = %.1f, want 123", parsed.ActualThousands)
	}
	confidence, _, err := validate(parsed, "Apr 2026", true)
	if err != nil {
		t.Fatalf("fallback validate returned error: %v", err)
	}
	if confidence != "MEDIUM" {
		t.Fatalf("fallback confidence = %s, want MEDIUM", confidence)
	}
}
