// Package nrc provides a scraper for the NRC COL application listing pages.
// It discovers all plant-specific pages from the main COL index, then extracts
// document metadata (accession numbers, titles, categories) from each plant page.
package nrc

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/nrc-col-crawler/internal/models"
)

const (
	// colIndexURL is the NRC page that lists all COL applications.
	colIndexURL = "https://www.nrc.gov/reactors/new-reactors/large-lwr/col.html"

	// nrcBase is prepended to relative links found on NRC pages.
	nrcBase = "https://www.nrc.gov"
)

// accessionPattern matches ADAMS accession numbers like ML18002A422.
// Format: "ML" followed by exactly 9 alphanumeric characters.
var accessionPattern = regexp.MustCompile(`\bML[0-9A-Z]{9}\b`)

// docketPattern matches COL docket numbers like 52-025.
var docketPattern = regexp.MustCompile(`\b52-\d{3}\b`)

// Scraper fetches NRC pages and extracts COL application data.
type Scraper struct {
	client  *http.Client
	rateMs  int // milliseconds to wait between requests
	lastReq time.Time
}

// NewScraper creates a Scraper. rateMs controls the polite delay between
// HTTP requests (default 1500ms recommended to avoid hammering nrc.gov).
func NewScraper(rateMs int) *Scraper {
	return &Scraper{
		client: &http.Client{Timeout: 30 * time.Second},
		rateMs: rateMs,
	}
}

// wait enforces the polite delay between requests.
// "defer" schedules a function call to run when the surrounding function
// returns — here we use it to always update lastReq after a fetch.
func (s *Scraper) wait() {
	if !s.lastReq.IsZero() {
		elapsed := time.Since(s.lastReq)
		delay := time.Duration(s.rateMs) * time.Millisecond
		if elapsed < delay {
			time.Sleep(delay - elapsed)
		}
	}
}

// fetch performs a GET request and returns the response body as a string.
// It handles the rate-limiting delay automatically.
func (s *Scraper) fetch(url string) (string, error) {
	s.wait()
	defer func() { s.lastReq = time.Now() }()

	resp, err := s.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	// "defer" ensures the body is closed even if we return early with an error.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading body from %s: %w", url, err)
	}
	return string(body), nil
}

// DiscoverPlantPages fetches the COL index page and returns a list of URLs
// pointing to individual plant COL application pages.
func (s *Scraper) DiscoverPlantPages() ([]string, error) {
	body, err := s.fetch(colIndexURL)
	if err != nil {
		return nil, fmt.Errorf("fetching COL index: %w", err)
	}

	// Parse the HTML and walk every <a> tag looking for links to plant pages.
	// Plant page links typically live under /reactors/new-reactors/large-lwr/col/
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parsing COL index HTML: %w", err)
	}

	var urls []string
	seen := make(map[string]bool)

	// walkNode is a recursive function that visits every HTML node.
	// In Go, functions can reference themselves by name if declared with var.
	var walkNode func(*html.Node)
	walkNode = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attrVal(n, "href")
			if strings.Contains(href, "/reactors/new-reactors/large-lwr/col/") {
				fullURL := resolveURL(href)
				if !seen[fullURL] {
					seen[fullURL] = true
					urls = append(urls, fullURL)
				}
			}
		}
		// Recurse into child nodes.
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkNode(c)
		}
	}
	walkNode(doc)

	if len(urls) == 0 {
		return nil, fmt.Errorf("no plant page links found on COL index — page structure may have changed")
	}
	return urls, nil
}

// ScrapePlantPage fetches one plant COL page and returns an ApplicationRecord
// populated with the documents found there. The caller is responsible for
// supplementing with ADAMS API results.
func (s *Scraper) ScrapePlantPage(pageURL string) (*models.ApplicationRecord, error) {
	body, err := s.fetch(pageURL)
	if err != nil {
		return nil, fmt.Errorf("fetching plant page %s: %w", pageURL, err)
	}

	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parsing plant page HTML: %w", err)
	}

	app := models.Application{NRCPageURL: pageURL}
	app.PlantName = extractTitle(doc)
	app.DocketNumbers = extractDockets(body)
	app.Applicant, app.DesignType, app.Status = extractApplicationMeta(doc)

	documents := extractDocuments(doc, body)

	return &models.ApplicationRecord{
		SchemaVersion: "1.0",
		Application:   app,
		CrawledAt:     time.Now().UTC(),
		Documents:     documents,
	}, nil
}

// --- HTML helper functions ---

// attrVal returns the value of a named attribute on an HTML element node,
// or "" if the attribute is not present.
func attrVal(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// resolveURL turns a relative NRC path like "/reactors/..." into a full URL.
func resolveURL(href string) string {
	if strings.HasPrefix(href, "http") {
		return href
	}
	return nrcBase + href
}

// innerText recursively collects all text content within an HTML node.
func innerText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(innerText(c))
	}
	return strings.TrimSpace(sb.String())
}

// extractTitle tries to find the page <h1> or <title> as the plant name.
func extractTitle(doc *html.Node) string {
	var title string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "h1" || n.Data == "h2") {
			t := innerText(n)
			if t != "" && title == "" {
				title = t
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title
}

// extractDockets scans the raw HTML body for docket numbers (52-XXX format).
func extractDockets(body string) []string {
	matches := docketPattern.FindAllString(body, -1)
	seen := make(map[string]bool)
	var unique []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	return unique
}

// extractApplicationMeta tries to pull applicant, design type, and status
// from common NRC page patterns (a definition list or labeled paragraphs).
// NRC pages vary in structure so this is best-effort.
func extractApplicationMeta(doc *html.Node) (applicant, designType, status string) {
	// Walk looking for <dt>/<dd> pairs or bold labels followed by text.
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "dt" {
			label := strings.ToLower(innerText(n))
			dd := nextSiblingElement(n)
			if dd == nil {
				return
			}
			val := innerText(dd)
			switch {
			case strings.Contains(label, "applicant"):
				applicant = val
			case strings.Contains(label, "design") || strings.Contains(label, "reactor"):
				designType = val
			case strings.Contains(label, "status"):
				status = val
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return
}

// nextSiblingElement returns the next sibling HTML element node, skipping text nodes.
func nextSiblingElement(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			return s
		}
	}
	return nil
}

// extractDocuments walks the parsed HTML and raw body to find all ADAMS
// accession numbers, building Document records for each one found.
func extractDocuments(doc *html.Node, body string) []Document {
	// Collect all <a> tags that link to ADAMS documents.
	type linkDoc struct {
		accession string
		title     string
		href      string
	}
	var linked []linkDoc

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attrVal(n, "href")
			text := innerText(n)
			// Accession numbers in the link text
			if acc := accessionPattern.FindString(text); acc != "" {
				linked = append(linked, linkDoc{acc, text, href})
			}
			// Or accession number embedded in the href URL
			if acc := accessionPattern.FindString(href); acc != "" {
				linked = append(linked, linkDoc{acc, text, href})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Also find any plain-text accession numbers not inside links.
	allAccessions := accessionPattern.FindAllString(body, -1)

	// Build a set of all accessions from linked docs.
	seen := make(map[string]bool)
	var docs []models.Document

	for _, ld := range linked {
		if seen[ld.accession] {
			continue
		}
		seen[ld.accession] = true
		docs = append(docs, models.Document{
			ID:       ld.accession,
			Category: inferCategory(ld.title),
			Title:    ld.title,
			Authors:  []string{},
			PDFURL:   accessionToPDFURL(ld.accession),
			Source:   models.SourceNRCPlantPage,
		})
	}

	// Add any plain-text accessions not already captured via links.
	for _, acc := range allAccessions {
		if seen[acc] {
			continue
		}
		seen[acc] = true
		docs = append(docs, models.Document{
			ID:       acc,
			Category: models.CategoryOther,
			Title:    acc, // best we can do without surrounding context
			Authors:  []string{},
			PDFURL:   accessionToPDFURL(acc),
			Source:   models.SourceNRCPlantPage,
		})
	}

	return docs
}

// Document alias to avoid repeating the full package path inside this file.
type Document = models.Document

// accessionToPDFURL converts an ADAMS accession number to its PDF URL.
// Formula: https://www.nrc.gov/docs/{acc[0:6]}/{acc}.pdf
// Example: ML18002A422 → https://www.nrc.gov/docs/ML1800/ML18002A422.pdf
func accessionToPDFURL(acc string) string {
	if len(acc) < 6 {
		return ""
	}
	return fmt.Sprintf("https://www.nrc.gov/docs/%s/%s.pdf", acc[:6], acc)
}

// inferCategory guesses a document category from its title or surrounding
// label text. Falls back to OTHER when no keywords match.
func inferCategory(title string) models.DocCategory {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "request for additional information") || strings.HasPrefix(t, "rai"):
		return models.CategoryRAI
	case strings.Contains(t, "response to request") || strings.Contains(t, "rai response"):
		return models.CategoryRAIResponse
	case strings.Contains(t, "safety evaluation report") || strings.Contains(t, "ser"):
		return models.CategorySER
	case strings.Contains(t, "environmental impact statement") || strings.Contains(t, "eis"):
		return models.CategoryEIS
	case strings.Contains(t, "nureg"):
		return models.CategoryNUREG
	case strings.Contains(t, "combined license application") ||
		strings.Contains(t, "col application") ||
		strings.Contains(t, "fsar") ||
		strings.Contains(t, "final safety analysis"):
		return models.CategoryCOLApplication
	case strings.Contains(t, "letter") || strings.Contains(t, "correspondence"):
		return models.CategoryCorrespondence
	default:
		return models.CategoryOther
	}
}
