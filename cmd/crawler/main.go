// Command crawler is the entry point for the NRC COL document crawler.
//
// It discovers all COL application pages on the NRC website, scrapes document
// metadata from each plant page, optionally supplements with ADAMS API results,
// and writes one JSON file per plant plus a master catalog.json index.
//
// Usage:
//
//	go run ./cmd/crawler [flags]
//
// Flags:
//
//	--out        Output directory (default: ./output)
//	--rate-ms    Delay between NRC page requests in milliseconds (default: 1500)
//	--skip-adams Skip ADAMS API queries; scrape NRC pages only
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/nrc-col-crawler/internal/adams"
	"github.com/nrc-col-crawler/internal/models"
	"github.com/nrc-col-crawler/internal/nrc"
)

func main() {
	// --- Parse CLI flags ---
	outDir := flag.String("out", "./output", "output directory for JSON files")
	rateMs := flag.Int("rate-ms", 1500, "milliseconds to wait between NRC page requests")
	skipAdams := flag.Bool("skip-adams", false, "skip ADAMS API queries (scrape NRC pages only)")
	flag.Parse()

	// --- Load environment variables from .env ---
	// godotenv.Load reads key=value pairs from .env into the process environment.
	// It's fine if .env doesn't exist (e.g. in CI where env vars are injected directly).
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found — relying on environment variables")
	}

	adamsAPIKey := os.Getenv("ADAMS_API_KEY")
	if adamsAPIKey == "" && !*skipAdams {
		log.Fatal("ADAMS_API_KEY is not set. Add it to .env or set it in your environment. " +
			"Use --skip-adams to run without it.")
	}

	// --- Ensure output directory exists ---
	// os.MkdirAll creates the directory and any missing parents.
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("Creating output directory %s: %v", *outDir, err)
	}

	// --- Build clients ---
	scraper := nrc.NewScraper(*rateMs)
	var adamsClient *adams.Client
	if !*skipAdams {
		adamsClient = adams.NewClient(adamsAPIKey)
	}

	// --- Discover all plant pages ---
	log.Println("Discovering COL plant pages...")
	plantURLs, err := scraper.DiscoverPlantPages()
	if err != nil {
		log.Fatalf("Discovering plant pages: %v", err)
	}
	log.Printf("Found %d plant pages\n", len(plantURLs))

	// catalog collects a summary entry for each successfully crawled plant.
	var catalog models.Catalog
	catalog.SchemaVersion = "1.0"

	// --- Process each plant page ---
	for i, pageURL := range plantURLs {
		log.Printf("[%d/%d] Scraping %s", i+1, len(plantURLs), pageURL)

		record, err := scraper.ScrapePlantPage(pageURL)
		if err != nil {
			log.Printf("  ERROR scraping %s: %v (skipping)", pageURL, err)
			continue
		}

		// --- Supplement with ADAMS results ---
		if adamsClient != nil {
			record.Documents = supplementWithADAMS(adamsClient, record)
		}

		// --- Write per-plant JSON file ---
		outFile := plantOutputFilename(record.Application.PlantName, *outDir)
		if err := writeJSON(outFile, record); err != nil {
			log.Printf("  ERROR writing %s: %v", outFile, err)
			continue
		}
		log.Printf("  Wrote %d documents → %s", len(record.Documents), outFile)

		catalog.Entries = append(catalog.Entries, models.CatalogEntry{
			PlantName:     record.Application.PlantName,
			DocketNumbers: record.Application.DocketNumbers,
			OutputFile:    filepath.Base(outFile),
			DocumentCount: len(record.Documents),
			CrawledAt:     record.CrawledAt,
		})
	}

	// --- Write master catalog ---
	catalog.GeneratedAt = time.Now().UTC()
	catalogFile := filepath.Join(*outDir, "catalog.json")
	if err := writeJSON(catalogFile, catalog); err != nil {
		log.Fatalf("Writing catalog: %v", err)
	}
	log.Printf("Catalog written → %s (%d plants)", catalogFile, len(catalog.Entries))
}

// supplementWithADAMS queries the ADAMS API for each docket number on the
// application and merges results into the document list, deduplicating by
// accession number.
func supplementWithADAMS(client *adams.Client, record *models.ApplicationRecord) []models.Document {
	// Build a set of accession numbers already found on the NRC page.
	existing := make(map[string]bool)
	for _, d := range record.Documents {
		existing[d.ID] = true
	}

	merged := record.Documents // start with what we already have

	for _, docket := range record.Application.DocketNumbers {
		log.Printf("  ADAMS search: docket %s", docket)
		docs, err := client.SearchByDocket(docket)
		if err != nil {
			log.Printf("  WARNING: ADAMS search failed for docket %s: %v", docket, err)
			continue
		}
		log.Printf("  ADAMS returned %d documents for docket %s", len(docs), docket)

		for _, d := range docs {
			if !existing[d.ID] {
				existing[d.ID] = true
				merged = append(merged, d)
			}
		}
	}

	return merged
}

// plantOutputFilename generates a safe filesystem name for a plant's JSON file.
// Example: "Vogtle Units 3 and 4" → "<outDir>/vogtle-units-3-and-4.json"
func plantOutputFilename(plantName, outDir string) string {
	// Replace non-alphanumeric characters with hyphens and lowercase everything.
	re := regexp.MustCompile(`[^a-z0-9]+`)
	safe := re.ReplaceAllString(strings.ToLower(plantName), "-")
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = fmt.Sprintf("plant-%d", time.Now().UnixNano())
	}
	return filepath.Join(outDir, safe+".json")
}

// writeJSON marshals v to indented JSON and writes it to path.
func writeJSON(path string, v any) error {
	// json.MarshalIndent produces human-readable JSON with 2-space indentation.
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON: %w", err)
	}
	// os.WriteFile creates or truncates the file and writes the data atomically.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
