# BoC Interest Rate Decision CA Sniper

Single-file Go sniper scraper for the Bank of Canada interest rate decision.

## Event

Current configured event:

```text
Release date: 2026-06-10
Event time:   19:15 IST / 13:45 UTC
```

Edit these constants near the top of [main.go](main.go) for the next release:

```go
const eventTimeUTC = "2026-06-10 13:45:00"
const expectedReleaseDate = "2026-06-10"
```

## Official Sources

1. Bank of Canada press-release listing  
URL: <https://www.bankofcanada.ca/press/press-releases/>  
Use: primary trading trigger. The scraper extracts only the sentence containing `target for the overnight rate`.

2. Bank of Canada policy interest rate page  
URL: <https://www.bankofcanada.ca/core-functions/monetary-policy/key-interest-rate/>  
Use: official schedule validation and current `Target (%)` backup.

3. Bank of Canada policy instrument  
URL: <https://www.bankofcanada.ca/rates/indicators/key-variables/policy-instrument/>  
Data endpoint: <https://www.bankofcanada.ca/valet/observations/group/ATABLE_POLICY_INSTRUMENT/json?recent=1>  
Use: direct current value backup for `Target for the Overnight Rate`.

## Run

```powershell
go run main.go
```

Build for server deployment:

```powershell
go build -o boc_interest_rate_decision_ca_sniper.exe main.go
```

Run the executable:

```powershell
.\boc_interest_rate_decision_ca_sniper.exe
```

## Confirmation Rules

The scraper uses official Bank of Canada sources only. It rejects stale release dates, out-of-range values, missing schedule dates, and source disagreement. Bank Rate and deposit rate are ignored.
