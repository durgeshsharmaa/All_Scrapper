# US CPI Table 1 Sniper

Single-file Go scraper for fast US CPI release capture from BLS Table 1.

## Sources

1. BLS CPI Table 1 HTML (primary official source)  
URL: https://www.bls.gov/news.release/cpi.t01.htm

2. BLS CPI Summary HTML (official release confirmation source)  
URL: https://www.bls.gov/news.release/cpi.nr0.htm

3. BLS CPI PDF Release (official PDF snapshot backup source)  
URL: https://www.bls.gov/news.release/pdf/cpi.pdf

4. BLS Public Data API  
URL: https://api.bls.gov/publicAPI/v2/timeseries/data/CUSR0000SA0L1E

5. Investing.com CPI event pages (third-party confirmation source)  
CPI (MoM): https://www.investing.com/economic-calendar/cpi-69  
CPI (YoY): https://www.investing.com/economic-calendar/cpi-733  
Core CPI (MoM): https://www.investing.com/economic-calendar/united-states-core-consumer-price-index-%28cpi%29-mom-56  
Core CPI (YoY): https://www.investing.com/economic-calendar/united-states-core-consumer-price-index-%28cpi%29-yoy-736

The production fast path uses only BLS Table 1 HTML as the blocking release trigger. It polls Table 1 content every 100ms during sniper mode and emits the final JSON immediately after the expected release period and all four CPI metrics validate. After the official JSON is printed, the scraper starts post-release confirmation polling for BLS Summary, BLS PDF, BLS API, and Investing.com in parallel.

The primary trigger is the BLS Table 1 HTML row data for CPI-U, U.S. city average. The scraper extracts these direct table values from the same table:

```text
CPI (MoM):      All items, latest seasonally adjusted 1-month percent change
CPI (YoY):      All items, unadjusted 12-month percent change
Core CPI (MoM): All items less food and energy, latest seasonally adjusted 1-month percent change
Core CPI (YoY): All items less food and energy, unadjusted 12-month percent change
```

For the April 2026 BLS Table 1 release, those fields are:

```text
CPI (MoM)      0.6%
CPI (YoY)      3.8%
Core CPI (MoM) 0.4%
Core CPI (YoY) 2.8%
```

The PDF source parses the same four Table 1 values from the official PDF snapshot using direct PDF table values. The API source remains a corroboration path for the legacy Core CPI MoM value and calculates it from index levels using:

```text
((current_month_index / previous_month_index) - 1) * 100
```

The PDF parser is dependency-free. It extracts readable PDF text streams and validates the `All items` and `All items less food and energy` Table 1 row shapes before using the values, so no external PDF library is required.

The Investing.com source fetches the four event pages directly and parses each page's title, latest release date, actual, forecast, previous, and historical release rows. It confirms the bracketed history period, such as `(Apr)`, matches the BLS target month. Investing.com actuals are confirmation only; they never override BLS official values. If Investing.com differs from BLS, the scraper prints `INVESTING_MISMATCH`. If Investing.com has not updated to the BLS release date/period, it prints `INVESTING_NOT_UPDATED`.

In sniper mode, BLS Summary, PDF, API, and Investing.com are excluded from the critical output path because they are slower and can add seconds of latency. After the first official Table 1 hit, they are polled concurrently as confirmation sources and print `CONFIRMED`, `MISMATCH`, `NOT_UPDATED`, or `ERROR` with their values as they arrive.

Console output prints the four-value metric line for each source that can provide the full CPI payload:

```text
Metrics: CPI (MoM)=0.6% | CPI (YoY)=3.8% | Core CPI (MoM)=0.4% | Core CPI (YoY)=2.8%
Metrics: CPI (MoM)=0.6% (Forecast 0.6%, Previous 0.9%) | CPI (YoY)=3.8% (Forecast 3.7%, Previous 3.3%) | Core CPI (MoM)=0.4% (Forecast 0.3%, Previous 0.2%) | Core CPI (YoY)=2.8% (Forecast 2.7%, Previous 2.6%)
```

## Update For Next Event

Edit `eventTimeUTC` in `main.go`.

Use UTC format:

```text
YYYY-MM-DD HH:MM:SS
```

For an 08:30 Eastern CPI release during daylight saving time, UTC is usually 12:30:00. During standard time, UTC is usually 13:30:00.

## Run

```powershell
go run main.go
```

At startup the scraper fetches current published values, displays UTC and IST event times, waits for the configured release, captures the BLS Table 1 baseline one minute before release, activates sniper mode two seconds before release, and emits immediately on the first valid BLS Table 1 hit for the expected period. If started within one minute of release, slow preflight sources are skipped automatically so they cannot delay the fast path. Once official output is printed, confirmation sources continue polling in parallel until they confirm, mismatch, error, or the release polling window ends.
