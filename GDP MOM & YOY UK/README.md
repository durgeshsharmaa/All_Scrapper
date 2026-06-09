# UK GDP MoM and YoY Sniper

Single-file Go sniper scraper for the Office for National Statistics monthly UK GDP release.

## Event

Current configured event:

```text
Release date: 2026-06-12
Release time: 07:00 UK / 06:00 UTC / 11:30 IST
Expected period: April 2026
```

Edit these constants near the top of [main.go](main.go) for the next release:

```go
const eventTimeUTC = "2026-06-12 06:00:00"
const expectedReleaseDate = "2026-06-12"
const expectedPeriod = "2026-04"
```

## Official Sources

1. ONS GDP monthly estimate bulletin  
URL: <https://www.ons.gov.uk/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk>  
Fetch URL: <https://www.ons.gov.uk/economy/grossdomesticproductgdp/bulletins/gdpmonthlyestimateuk/latest>  
Use: primary official source. The scraper extracts the direct `Monthly GDP grew/fell by X.X% in {month}` sentence for GDP MoM, and the same-month-a-year-ago sentence for GDP YoY when present.

2. ONS Monthly gross domestic product: time series dataset  
URL: <https://www.ons.gov.uk/economy/grossdomesticproductgdp/datasets/gdpmonthlyestimateuktimeseriesdataset/current>  
CSV: <https://www.ons.gov.uk/file?uri=/economy/grossdomesticproductgdp/datasets/gdpmonthlyestimateuktimeseriesdataset/current/mgdp.csv>  
Use: official MGDP confirmation source. The scraper reads total `Gross Value Added - Monthly` period-on-period growth for MoM and period-on-period one-year-ago growth for YoY. If the article does not include the YoY sentence, the MGDP YoY value is used as the fallback.

3. ONS releases discovery page  
URL: <https://www.ons.gov.uk/releases>  
Expected release detail URL: <https://www.ons.gov.uk/releases/gdpmonthlyestimateukapril2026>  
Use: official release discovery and schedule validation. Before the expected release detail page is live, the scraper falls back to the previous release detail page to validate `Next release`. During sniper polling, it checks the expected release detail page and uses its `Publications` link to validate the exact article URL.

## Run

```powershell
go run main.go
```

Build for server deployment:

```powershell
go build -o gdp_mom_yoy_uk_sniper.exe main.go
```

Run the executable:

```powershell
.\gdp_mom_yoy_uk_sniper.exe
```

## Confirmation Rules

The scraper requires the primary ONS bulletin to publish the expected release date and period. The expected ONS releases detail page must also be live, have the configured 7:00 UK release timestamp, and link to the same exact bulletin URL. GDP MoM must come from the article. GDP YoY comes from the article if present, otherwise from the official MGDP dataset. When MGDP is available, MoM and YoY must match the article or the scraper returns `NOT_CONFIRMED`.
