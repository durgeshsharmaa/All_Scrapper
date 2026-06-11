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

Do not edit source code for the release time. Pass it at runtime with `-event-time-utc` or set `PPI_EVENT_TIME_UTC`.

Use UTC format:

```text
YYYY-MM-DD HH:MM:SS
```

Example for the May 2026 PPI release on June 11, 2026 at 08:30 Eastern:

```powershell
go run . -event-time-utc "2026-06-11 12:30:00"
```

## Production Pipeline

`main.go` runs all official sources in one pipeline:

1. Source 1 primary value trigger: `https://www.bls.gov/news.release/ppi.t01.htm`
2. Source 2 official PDF value backup: `https://www.bls.gov/news.release/pdf/ppi.pdf`
3. Source 3 release confirmation only: `https://www.bls.gov/news.release/ppi.nr0.htm`

The production scraper:

- fetches all sources concurrently at startup
- captures baseline headers, content hashes, period, and value signatures
- uses Source 1 as the low-latency primary value source
- polls Source 1 content every 100 ms during the release window
- races Source 2 PDF as an official value backup on a slower cadence
- validates the new expected period, exact rows, columns, units, source identity, and value bounds before printing JSON
- polls remaining official sources after the first valid value hit for confirmation
- reports Source 2 disagreement as `MISMATCH`
- never uses Source 3 summary values for the grouped extraction

## Run

Run sniper mode:

```powershell
go run . -event-time-utc "2026-06-11 12:30:00"
```

Or:

```powershell
$env:PPI_EVENT_TIME_UTC = "2026-06-11 12:30:00"
go run .
```

At startup the scraper fetches the current BLS sources, displays UTC and IST event times, validates the configured date against the BLS PPI release schedule, captures baseline headers/content one minute before release, activates sniper mode two seconds before release, polls for three minutes, and prints `NOT_CONFIRMED` if validation fails or the expected release period is not detected.

Source 2 PDF parsing and Source 3 release-summary confirmation are implemented inside `main.go`. Source 3 checks release availability, release month/date, and narrative signals, but it does not supply grouped values. This avoids substituting the summary measure `Final demand less foods, energy, and trade services` for the target Core PPI row `FD 49104`.

## Build And Test

```powershell
go test ./...
go vet ./...
go build -o ppi_sniper.exe .
```
