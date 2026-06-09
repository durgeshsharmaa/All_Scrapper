# Canada Unemployment Rate Sniper

Single-file Go scraper for the Canada unemployment rate release.

## Sources

1. Statistics Canada The Daily Labour Force Survey article
2. Statistics Canada Table 14-10-0287-01 via vector `2062815`
3. Statistics Canada WDS product-coordinate backup, product `14100287`, coordinate `1.7.1.1.1.1.0.0.0.0`

Target series: Canada, seasonally adjusted, both sexes, 15 years and over, unemployment rate.

## Update For Next Event

Edit [main.go](main.go):

```go
eventTimeUTCString    = "YYYY-MM-DD 12:30:00"
expectedReleasePeriod = "YYYY-MM"
latestKnownDailyArticleURL = "https://www150.statcan.gc.ca/n1/daily-quotidien/YYMMDD/dqYYMMDDa-eng.htm"
```

The Daily target URL is generated automatically from `eventTimeUTCString`.

## Run

```powershell
go run main.go
```

Build:

```powershell
go build -o unemployment_rate_ca_sniper.exe main.go
```

## Detection

The scraper captures startup data, captures baselines one minute before the event, starts sniper mode two seconds before release, and polls for three minutes after the event.

The Daily source checks headers every 500 ms and parses content every fifth poll. The two WDS JSON sources parse latest-period content every 500 ms because their release signal is the structured value itself.
