# US PPI Group Sniper

Single-file Go scraper for the U.S. PPI event group. The production entry point is `main.go`.

- `PPI (MoM)`
- `PPI (YoY)`
- `Core PPI (MoM)`
- `Core PPI (YoY)`

## Primary Source

BLS Table 1 HTML:

```text
https://www.bls.gov/news.release/ppi.t01.htm
```

Targets:

```text
Table: Table 1
Source: https://www.bls.gov/news.release/ppi.t01.htm
Value method: direct_table_value
Unit: %

1. PPI (MoM)
   Row: Final demand
   Group code: FD
   Item code: 4
   Column: latest seasonally adjusted 1-month percent change

2. PPI (YoY)
   Row: Final demand
   Group code: FD
   Item code: 4
   Column: unadjusted 12-month percent change

3. Core PPI (MoM)
   Row: Final demand less foods and energy
   Group code: FD
   Item code: 49104
   Column: latest seasonally adjusted 1-month percent change

4. Core PPI (YoY)
   Row: Final demand less foods and energy
   Group code: FD
   Item code: 49104
   Column: unadjusted 12-month percent change
```

For the April 2026 page:

```text
PPI (MoM):      1.4%
PPI (YoY):      6.0%
Core PPI (MoM): 1.0%
Core PPI (YoY): 5.2%
```

## Update For Next Event

Edit `eventTimeUTC` in `main.go`.

Use UTC format:

```text
YYYY-MM-DD HH:MM:SS
```

The configured event is the May 2026 PPI release on June 11, 2026 at 08:30 Eastern, which is `2026-06-11 12:30:00` UTC.

## Production Pipeline

`main.go` runs all official sources in one pipeline:

1. Source 1 primary value trigger: `https://www.bls.gov/news.release/ppi.t01.htm`
2. Source 2 official PDF value backup: `https://www.bls.gov/news.release/pdf/ppi.pdf`
3. Source 3 release confirmation only: `https://www.bls.gov/news.release/ppi.nr0.htm`

The production scraper:

- fetches all sources concurrently at startup
- captures baseline headers, content hashes, period, and value signatures
- polls every 500 ms per source
- checks headers every poll and content every fifth poll
- fetches content immediately when `ETag` or `Last-Modified` changes
- selects Source 1 as the primary low-latency value source
- rejects same-period official value disagreement between Source 1 and Source 2
- never uses Source 3 summary values for the grouped extraction

## Run

Run sniper mode:

```powershell
go run main.go
```

At startup the scraper fetches the current BLS Table 1 values, displays UTC and IST event times, validates the configured date against the BLS PPI release schedule, captures baseline headers/content one minute before release, activates sniper mode two seconds before release, polls for three minutes, and prints `NOT_CONFIRMED` if validation fails or the expected release period is not detected.

The scraper has no required command-line arguments. Edit `eventTimeUTC` in `main.go` for the next release.

Source 2 PDF parsing and Source 3 release-summary confirmation are implemented inside `main.go`. Source 3 checks release availability, release month/date, and narrative signals, but it does not supply grouped values. This avoids substituting the summary measure `Final demand less foods, energy, and trade services` for the target Core PPI row `FD 49104`.
