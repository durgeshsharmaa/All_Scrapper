# US Core CPI MoM Sniper

Single-file Go scraper for the US Core CPI MoM release.

## Sources

1. BLS CPI Table 1 HTML
URL: https://www.bls.gov/news.release/cpi.t01.htm

2. BLS CPI PDF Release
URL: https://www.bls.gov/news.release/pdf/cpi.pdf

3. BLS Public Data API
URL: https://api.bls.gov/publicAPI/v2/timeseries/data/CUSR0000SA0L1E

## Update For Next Event

Edit `eventTimeUTC` in `main.go`.

Use UTC format:

YYYY-MM-DD HH:MM:SS

## Dependencies

This scraper uses two external dependencies to ensure robust HTML and PDF parsing:
- `golang.org/x/net/html` for structured HTML table parsing.
- `github.com/ledongthuc/pdf` for PDF text extraction.

## Run

```powershell
go run main.go
```
