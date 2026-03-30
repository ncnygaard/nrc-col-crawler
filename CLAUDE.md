# NRC COL Crawler — Project Context for Claude Code

## What This Project Is

This is a Go-based web crawler that collects NRC (Nuclear Regulatory Commission) Combined
Operating License (COL) application documents and their associated RAIs (Requests for
Additional Information). The output feeds a downstream AI vectorization pipeline that helps
companies assess the likelihood of success for nuclear reactor license applications.

This is a fresh implementation — there is no existing code to build on. Start from scratch.

---

## Developer Background

The developer is a software engineering graduate (BYU-I, 2022) with a background in C++ and
Java, returning to coding after several years away. This is their first Go project. Please:
- Explain Go-specific syntax and patterns as you introduce them
- Prefer clarity over cleverness — well-commented, readable code over terse idioms
- When introducing a new Go concept (goroutines, interfaces, defer, etc.), briefly explain
  what it does before using it

---

## Project Structure

```
nrc-col-crawler/
├── cmd/
│   └── crawler/
│       └── main.go           # Entry point, CLI flags, orchestration
├── internal/
│   ├── models/
│   │   └── types.go          # Shared data types
│   ├── nrc/
│   │   └── scraper.go        # NRC website scraper
│   └── adams/
│       └── client.go         # ADAMS APS API client
├── output/                   # Generated JSON files (git-ignored)
├── .env                      # API keys — never commit this (git-ignored)
├── .env.example              # Safe template to commit
├── .gitignore
├── go.mod
└── go.sum
```

---

## Configuration

API keys and config are loaded from a `.env` file at startup using the `godotenv` package.
Keys are **never** hardcoded or passed via command line.

`.env` file format:
```
ADAMS_API_KEY=your_subscription_key_here
```

`.env.example` (safe to commit):
```
ADAMS_API_KEY=your_subscription_key_here
```

---

## Data Sources

### 1. NRC Website Scraper
- Entry point: `https://www.nrc.gov/reactors/new-reactors/large-lwr/col.html`
- Dynamically discovers all COL application page links — no hardcoded plant names or docket numbers
- For each plant page, extracts:
  - Docket numbers (`52-XXX` format per 10 CFR Part 52)
  - ADAMS accession numbers (linked and plain-text, format: `MLxxxxxxxxx`)
  - Document titles from surrounding HTML context
  - Applicant name, reactor design type, application status
- Infers document category from title/label text:
  `COL_APPLICATION`, `RAI`, `RAI_RESPONSE`, `SER`, `EIS`, `NUREG`, `CORRESPONDENCE`, `OTHER`
- Polite rate limiting: default 1500ms delay between requests

### 2. ADAMS APS API Client
- Queries ADAMS by docket number to find RAIs not listed on the NRC plant page
- **API base URL:** `https://adams-api.nrc.gov/aps/api/`
- **Auth header:** `Ocp-Apim-Subscription-Key: {ADAMS_API_KEY}`
- **Endpoints:**
  - `GET /search/{accessionNumber}` — fetch single document by accession number
  - `POST /search` — boolean search across the ADAMS Public Library
- **PDF URL formula:** `https://www.nrc.gov/docs/{accession[0:6]}/{accession}.pdf`
  - Example: `ML18002A422` → `https://www.nrc.gov/docs/ML1800/ML18002A422.pdf`
- ADAMS search returns max 1000 results per query — implement pagination
- The old WBA API (`adams.nrc.gov/wba`) was retired 2026-02-27 — do not use it

---

## JSON Output Format

**No binary content is downloaded at crawl time.** The crawler outputs metadata and PDF URLs
only. Downloading and chunking PDFs is a separate downstream step.

One JSON file per application, plus a master `catalog.json` index.

### Per-application file:
```json
{
  "schema_version": "1.0",
  "application": {
    "docket_numbers": ["52-025", "52-026"],
    "plant_name": "Vogtle Units 3 and 4",
    "applicant": "Southern Nuclear Operating Company",
    "design_type": "AP1000",
    "status": "COL Issued",
    "nrc_page_url": "https://www.nrc.gov/reactors/new-reactors/large-lwr/col/vogtle.html"
  },
  "crawled_at": "2026-03-12T14:22:01Z",
  "documents": [
    {
      "id": "ML11180A100",
      "category": "COL_APPLICATION",
      "part": "Part 2 - FSAR",
      "title": "Final Safety Analysis Report, Revision 5",
      "date": "2011-06-29",
      "authors": [],
      "pdf_url": "https://www.nrc.gov/docs/ML1118/ML11180A100.pdf",
      "source": "nrc_plant_page"
    },
    {
      "id": "ML090140527",
      "category": "RAI",
      "part": null,
      "title": "Request for Additional Information - Chapter 2",
      "date": "2009-01-14",
      "authors": ["NRC Staff"],
      "pdf_url": "https://www.nrc.gov/docs/ML0901/ML090140527.pdf",
      "source": "adams_search"
    }
  ]
}
```

All document types (COL parts, RAIs, RAI responses, SERs, EIS, etc.) live in one flat
`documents` array, distinguished by `category`. This makes downstream iteration for
chunking and embedding straightforward.

---

## CLI Flags

The crawler entrypoint (`cmd/crawler/main.go`) supports these flags:
- `--out` — output directory (default: `./output`)
- `--rate-ms` — delay in milliseconds between NRC page requests (default: `1500`)
- `--skip-adams` — skip ADAMS API queries, scrape NRC pages only

---

## Known Complexity / Things to Get Right

- **Multi-unit plants:** Some plants (e.g. Vogtle 3 & 4) have multiple docket numbers.
  ADAMS must be queried once per docket, with deduplication when merging results.
- **Pagination:** ADAMS search is capped at 1000 results per query. Use offset pagination
  to fetch all results for high-volume applications.
- **Resumable crawling:** For later — a state file or SQLite DB to track what has already
  been crawled, plus a `--since` flag and exponential backoff retry logic.
- **HTML variability:** NRC plant pages are not perfectly uniform. The scraper needs to
  handle older page layouts gracefully without crashing.

---

## Key Reference Data

| Topic | Detail |
|---|---|
| COL docket format | `52-XXX` per 10 CFR Part 52 |
| ADAMS accession format | `MLxxxxxxxxx` (9 chars after ML, e.g. `ML18002A422`) |
| PDF URL formula | `https://www.nrc.gov/docs/{acc[0:6]}/{acc}.pdf` |
| ADAMS APS API base | `https://adams-api.nrc.gov/aps/api/` |
| APS auth header | `Ocp-Apim-Subscription-Key` |
| APS developer portal | `https://adams-api-developer.nrc.gov/` |
| NRC COL listing page | `https://www.nrc.gov/reactors/new-reactors/large-lwr/col.html` |
| Rate limit | 1000 results max per ADAMS query; 1500ms polite delay for NRC pages |

---

## Out of Scope for This Crawler

The following are downstream steps handled by a separate pipeline (Ray's AI system):
- Downloading PDFs
- Chunking PDFs by FSAR chapter/section
- Embedding chunks with a text embedding model
- Storing vectors in a vector database
- Building retrieval and scoring logic

---

## Where to Start

1. Initialize the Go module: `go mod init github.com/nrc-col-crawler`
2. Create the project directory structure above
3. Create `.gitignore` (ignore `.env` and `output/`)
4. Create `.env.example`
5. Build `internal/models/types.go` first — all other files depend on the shared types
6. Then `internal/nrc/scraper.go`
7. Then `internal/adams/client.go`
8. Finally wire everything together in `cmd/crawler/main.go`
