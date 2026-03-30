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
			if strings.Contains(href, "/reactors/new-reactors/large-lwr/col/") &&
				!strings.Contains(href, "new-reactor-map") {
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

	// Always check for an "Application Documents" sub-page and merge whatever
	// is found there. Some plants (e.g. River Bend) have documents on the main
	// page AND more documents on the sub-page; others only have the sub-page.
	// scrapeApplicationDocumentsPage returns nil immediately if no link is found,
	// so this costs nothing for pages that don't have a sub-page.
	extra := s.scrapeApplicationDocumentsPage(doc, pageURL)
	if len(extra) > 0 {
		// Merge, deduplicating by accession number (ID).
		seen := make(map[string]bool, len(documents))
		for _, d := range documents {
			seen[d.ID] = true
		}
		for _, d := range extra {
			if !seen[d.ID] {
				seen[d.ID] = true
				documents = append(documents, d)
			}
		}
	}

	return &models.ApplicationRecord{
		SchemaVersion: "1.0",
		Application:   app,
		CrawledAt:     time.Now().UTC(),
		Documents:     documents,
	}, nil
}

// scrapeApplicationDocumentsPage looks for an "Application Documents" link on
// a plant page and follows it to extract accession numbers from PDF links.
func (s *Scraper) scrapeApplicationDocumentsPage(doc *html.Node, pageURL string) []models.Document {
	// Step 1: Find the "Application Documents" link on the plant page.
	var appDocsURL string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			text := strings.ToLower(innerText(n))
			if strings.Contains(text, "application documents") {
				href := attrVal(n, "href")
				if href != "" {
					appDocsURL = resolveURL(href)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if appDocsURL == "" {
		return nil
	}

	// Step 2: Fetch the Application Documents page.
	body, err := s.fetch(appDocsURL)
	if err != nil {
		return nil
	}

	// Step 3: Extract accession numbers from PDF URLs on that page.
	// The URLs follow the pattern: /docs/ML1209/ML12095A144.pdf
	var docs []models.Document
	seen := make(map[string]bool)

	for _, acc := range accessionPattern.FindAllString(body, -1) {
		if seen[acc] {
			continue
		}
		seen[acc] = true
		docs = append(docs, models.Document{
			ID:       acc,
			Category: models.CategoryCOLApplication,
			Title:    acc, // will be enriched by ADAMS later
			Authors:  []string{},
			PDFURL:   accessionToPDFURL(acc),
			Source:   models.SourceNRCPlantPage,
		})
	}

	return docs
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

// extractTitle finds the <h1 class="page-title"> element used on NRC plant pages.
func extractTitle(doc *html.Node) string {
	var title string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "h1" {
			if strings.Contains(attrVal(n, "class"), "page-title") {
				t := innerText(n)
				if t != "" {
					title = t
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(title)
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

// extractDocuments walks the parsed HTML to find all ADAMS documents on a plant page.
//
// NRC plant pages are structured as a series of <h2> section headings (e.g.
// "Application Information", "Final Safety Evaluation Report") each followed
// by a table of documents. We track which heading we are currently under so
// that every document in a section automatically gets the right category —
// no keyword-guessing required.
//
// Within each table row we look at all the cells: one cell contains the PDF
// link (giving us the accession number), and the other cells contain the
// document description (giving us the title).
//
// A plain-text fallback at the end catches any accession numbers we missed.
func extractDocuments(doc *html.Node, body string) []models.Document {
	seen := make(map[string]bool)
	var docs []models.Document

	// currentCategory updates every time we pass a new <h2> heading.
	// In Go, closures capture variables by reference, so when the walk
	// function modifies currentCategory the change sticks for future calls.
	currentCategory := models.CategoryOther

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type != html.ElementNode {
			// Text nodes and comments have no children to recurse into.
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}

		switch n.Data {
		case "h2":
			// Entering a new section — update the category for all documents below.
			currentCategory = inferCategoryFromSection(attrVal(n, "id"), innerText(n))

		case "tr":
			// A table row may contain a PDF link and a description cell.
			// extractDocFromRow handles the row and marks the accession in seen.
			if d := extractDocFromRow(n, currentCategory, seen); d != nil {
				docs = append(docs, *d)
			}
			// Return here so we don't double-visit the <td> children below.
			return

		case "li":
			// List items appear in sections like "Combined Licenses" that use
			// a bullet list instead of a table.
			if d := extractDocFromInline(n, currentCategory, seen); d != nil {
				docs = append(docs, *d)
			}
			return
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Fallback: catch any accession numbers embedded in plain text that the
	// structured walk above didn't reach (e.g. inside a <p> with no table).
	for _, acc := range accessionPattern.FindAllString(body, -1) {
		if seen[acc] {
			continue
		}
		seen[acc] = true
		docs = append(docs, models.Document{
			ID:       acc,
			Category: models.CategoryOther,
			Title:    acc, // no surrounding context available
			Authors:  []string{},
			PDFURL:   accessionToPDFURL(acc),
			Source:   models.SourceNRCPlantPage,
		})
	}

	return docs
}

// extractDocFromRow examines one <tr> (table row) for a PDF link with an
// accession number. If found, it picks the best title from the other cells.
//
// Table rows on NRC pages typically look like one of these two patterns:
//
//	[Part] [Description ← title we want] [Rev. X ← link to PDF]
//	[Date ← link to PDF] [Description ← title we want]
//
// Our strategy: find which cell has the PDF link (that gives us the accession),
// then take the longest text from the remaining cells as the title.
func extractDocFromRow(row *html.Node, category models.DocCategory, seen map[string]bool) *models.Document {
	type cellInfo struct {
		text string // visible text content of this cell
		acc  string // accession number found in a PDF link, or ""
	}
	var cells []cellInfo

	// Collect every <td> cell in this row.
	for c := row.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.Data != "td" {
			continue
		}
		info := cellInfo{text: strings.TrimSpace(innerText(c))}

		// Walk inside the cell looking for a link whose href is a PDF with
		// an accession number, e.g. /docs/ML1118/ML11180A098.pdf
		var walkCell func(*html.Node)
		walkCell = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "a" {
				if acc := accessionPattern.FindString(attrVal(n, "href")); acc != "" {
					info.acc = acc
				}
			}
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				walkCell(ch)
			}
		}
		walkCell(c)
		cells = append(cells, info)
	}

	// Find the cell that holds the accession link.
	accIdx := -1
	for i, cl := range cells {
		if cl.acc != "" {
			accIdx = i
			break
		}
	}
	if accIdx == -1 {
		return nil // this row has no PDF link — skip it
	}
	acc := cells[accIdx].acc
	if seen[acc] {
		return nil
	}
	seen[acc] = true

	// Use the longest text from the non-link cells as the document title.
	// "Longest" is a good heuristic because short cells tend to be part numbers
	// or dates, while the description cell has the most text.
	title := ""
	for i, cl := range cells {
		if i != accIdx && len(cl.text) > len(title) {
			title = cl.text
		}
	}
	if title == "" {
		title = cells[accIdx].text // last resort: use the link text itself
	}

	return &models.Document{
		ID:       acc,
		Category: category,
		Title:    title,
		Authors:  []string{},
		PDFURL:   accessionToPDFURL(acc),
		Source:   models.SourceNRCPlantPage,
	}
}

// extractDocFromInline handles accession links that appear outside of tables,
// for example in a <li> bullet list like the "Combined Licenses" section.
func extractDocFromInline(n *html.Node, category models.DocCategory, seen map[string]bool) *models.Document {
	var acc, linkText string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			if a := accessionPattern.FindString(attrVal(n, "href")); a != "" && acc == "" {
				acc = a
				linkText = strings.TrimSpace(innerText(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)

	if acc == "" || seen[acc] {
		return nil
	}
	seen[acc] = true

	// Prefer the full text of the <li> over just the link text, since the
	// <li> may have additional context around the link (e.g. "Vogtle Unit 3 LWA").
	title := strings.TrimSpace(innerText(n))
	if title == "" {
		title = linkText
	}

	return &models.Document{
		ID:       acc,
		Category: category,
		Title:    title,
		Authors:  []string{},
		PDFURL:   accessionToPDFURL(acc),
		Source:   models.SourceNRCPlantPage,
	}
}

// inferCategoryFromSection maps an <h2> heading to a document category.
// The id attribute (e.g. id="fser") is NRC's machine-readable label for the
// section; the heading text is the human-readable version. We check the id
// first (more reliable), then fall back to keywords in the heading text.
func inferCategoryFromSection(id, text string) models.DocCategory {
	id = strings.ToLower(strings.TrimSpace(id))
	t := strings.ToLower(strings.TrimSpace(text))

	switch {
	case id == "fser" || strings.Contains(t, "safety evaluation"):
		return models.CategorySER
	case id == "feis" || id == "eis" || strings.Contains(t, "environmental impact"):
		return models.CategoryEIS
	case strings.Contains(id, "rai") || strings.Contains(t, "request for additional"):
		return models.CategoryRAI
	case strings.Contains(t, "response to rai") || strings.Contains(t, "rai response"):
		return models.CategoryRAIResponse
	case strings.Contains(t, "nureg"):
		return models.CategoryNUREG
	// "application" and "col" both map to COL_APPLICATION. Check these last
	// because "application" is a broad word that could appear in other headings.
	case id == "application" || id == "col" ||
		strings.Contains(t, "application information") ||
		strings.Contains(t, "combined license"):
		return models.CategoryCOLApplication
	default:
		return models.CategoryOther
	}
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
