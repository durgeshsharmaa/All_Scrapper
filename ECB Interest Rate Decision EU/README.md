# ECB Interest Rate Decision EU Sniper

Production Go scraper for the ECB Interest Rate Decision event.

## Configured Event

- Event date: 11 June 2026
- Configured IST time: 2026-06-11 17:45:00 IST
- Configured UTC time: 2026-06-11 12:15:00 UTC
- Actual field: main refinancing operations rate
- Ignored for the calendar actual: deposit facility, marginal lending facility

The code validates that the hardcoded IST and UTC settings match.

## Official Sources

1. ECB monetary policy decisions index
   https://www.ecb.europa.eu/press/govcdec/mopo/html/index.en.html

2. Latest ECB monetary policy press release
   Pattern: https://www.ecb.europa.eu/press/pr/date/YYYY/html/ecb.mpYYMMDD~HASH.en.html

3. ECB key interest rates page
   https://www.ecb.europa.eu/stats/policy_and_exchange_rates/key_ecb_interest_rates/html/index.en.html

## Build

```powershell
go test ./...
go build -o ecb_interest_rate_decision_eu_sniper.exe .
```

## Run

Sniper mode:

```powershell
.\ecb_interest_rate_decision_eu_sniper.exe
```

Current-source smoke test:

```powershell
.\ecb_interest_rate_decision_eu_sniper.exe -once -expected-date=
```

If the server network is slow:

```powershell
.\ecb_interest_rate_decision_eu_sniper.exe -request-timeout 30s
```

## Deployment Notes

- Deploy only `ecb_interest_rate_decision_eu_sniper.exe` if the server is Windows.
- For Linux servers, build a Linux binary on that server or cross-compile.
- Keep server clock synced with NTP.
- Run a live smoke test on the server before event day.
