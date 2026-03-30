// Package models defines the shared data types used throughout the crawler.
// All other packages import from here so that the JSON output format stays
// consistent in one place.
package models

import "time"

// DocCategory classifies each document so the downstream AI pipeline can
// filter or weight document types differently.
type DocCategory string

const (
	CategoryCOLApplication DocCategory = "COL_APPLICATION"
	CategoryRAI            DocCategory = "RAI"
	CategoryRAIResponse    DocCategory = "RAI_RESPONSE"
	CategorySER            DocCategory = "SER"
	CategoryEIS            DocCategory = "EIS"
	CategoryNUREG          DocCategory = "NUREG"
	CategoryCorrespondence DocCategory = "CORRESPONDENCE"
	CategoryOther          DocCategory = "OTHER"
)

// DocSource records where the crawler found the document — either scraped
// from the NRC plant page or retrieved via the ADAMS APS search API.
type DocSource string

const (
	SourceNRCPlantPage DocSource = "nrc_plant_page"
	SourceADAMSSearch  DocSource = "adams_search"
)

// Document represents a single NRC document (COL part, RAI, SER, etc.).
// No binary content is stored here — only metadata and a URL to the PDF.
type Document struct {
	// ID is the ADAMS accession number, e.g. "ML18002A422"
	ID string `json:"id"`

	// Category classifies the document type for downstream filtering.
	Category DocCategory `json:"category"`

	// Part is an optional free-text label for multi-part submissions,
	// e.g. "Part 2 - FSAR". Null when not applicable.
	Part *string `json:"part"`

	// Title is the document title extracted from the NRC page or ADAMS metadata.
	Title string `json:"title"`

	// Date is the document date in YYYY-MM-DD format.
	Date string `json:"date"`

	// Authors lists document authors when available (often empty).
	Authors []string `json:"authors"`

	// PDFURL is the direct link to the document PDF on nrc.gov.
	// Formula: https://www.nrc.gov/docs/{acc[0:6]}/{acc}.pdf
	PDFURL string `json:"pdf_url"`

	// Source indicates whether this document was found via the NRC plant
	// page scraper or the ADAMS API search.
	Source DocSource `json:"source"`
}

// Application holds all metadata for one COL application (one plant).
// A plant may have multiple docket numbers (e.g. Vogtle 3 & 4).
type Application struct {
	// DocketNumbers lists all 10 CFR Part 52 docket numbers for this plant,
	// e.g. ["52-025", "52-026"]. ADAMS is queried once per docket.
	DocketNumbers []string `json:"docket_numbers"`

	// PlantName is the human-readable plant name, e.g. "Vogtle Units 3 and 4".
	PlantName string `json:"plant_name"`

	// Applicant is the company that filed the COL application.
	Applicant string `json:"applicant"`

	// DesignType is the reactor design, e.g. "AP1000", "ESBWR".
	DesignType string `json:"design_type"`

	// Status is the current license status, e.g. "COL Issued", "Application Withdrawn".
	Status string `json:"status"`

	// NRCPageURL is the URL of the NRC plant-specific COL page.
	NRCPageURL string `json:"nrc_page_url"`
}

// ApplicationRecord is the top-level structure written to each per-plant
// JSON output file. It wraps Application metadata with its document list
// and crawl timestamp.
type ApplicationRecord struct {
	// SchemaVersion lets downstream consumers detect breaking format changes.
	SchemaVersion string `json:"schema_version"`

	Application Application `json:"application"`

	// CrawledAt records when this record was generated (UTC).
	CrawledAt time.Time `json:"crawled_at"`

	// Documents is a flat list of all documents found for this application.
	// All categories (COL parts, RAIs, SERs, etc.) live together here.
	Documents []Document `json:"documents"`
}

// CatalogEntry is a lightweight summary used in the master catalog.json index.
type CatalogEntry struct {
	PlantName     string   `json:"plant_name"`
	DocketNumbers []string `json:"docket_numbers"`
	OutputFile    string   `json:"output_file"`
	DocumentCount int      `json:"document_count"`
	CrawledAt     time.Time `json:"crawled_at"`
}

// Catalog is the master index written to catalog.json.
type Catalog struct {
	SchemaVersion string         `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Entries       []CatalogEntry `json:"entries"`
}
