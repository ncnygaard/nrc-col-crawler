// Package adams provides a client for the NRC ADAMS APS (Advanced Program Search) API.
// It queries ADAMS by docket number to find documents not listed on NRC plant pages,
// particularly RAIs and related correspondence.
//
// API base: https://adams-api.nrc.gov/aps/api/
// Auth:     Ocp-Apim-Subscription-Key header
package adams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nrc-col-crawler/internal/models"
)

const (
	apiBase    = "https://adams-api.nrc.gov/aps/api/"
	maxPerPage = 1000 // ADAMS returns at most 1000 results per query
)

// Client handles communication with the ADAMS APS API.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a Client using the provided API key.
// The key is sent as the Ocp-Apim-Subscription-Key header on every request.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// --- Request / Response types for the ADAMS APS API ---

// searchRequest is the JSON body sent to POST /search.
type searchRequest struct {
	Query  string `json:"query"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// searchResponse is the JSON body returned by POST /search.
// The API wraps results in a "data" array with a total count.
type searchResponse struct {
	Total int              `json:"total"`
	Data  []adamsDocument  `json:"data"`
}

// adamsDocument is one record returned by the ADAMS search API.
// Field names match what the APS API actually returns; adjust if the
// real schema differs (consult https://adams-api-developer.nrc.gov/).
type adamsDocument struct {
	AccessionNumber string `json:"accession_number"`
	Title           string `json:"title"`
	DocumentDate    string `json:"document_date"` // YYYY-MM-DD
	AuthorName      string `json:"author_name"`
	DocketNumbers   string `json:"docket_numbers"` // comma-separated
	DocumentType    string `json:"document_type"`
}

// --- Public methods ---

// SearchByDocket queries ADAMS for all documents associated with a given
// docket number (e.g. "52-025"). It handles pagination automatically,
// fetching up to maxPerPage records at a time until all results are retrieved.
func (c *Client) SearchByDocket(docket string) ([]models.Document, error) {
	var allDocs []models.Document
	offset := 0

	for {
		batch, total, err := c.searchPage(docket, offset)
		if err != nil {
			return nil, fmt.Errorf("ADAMS search for docket %s (offset %d): %w", docket, offset, err)
		}

		for _, d := range batch {
			allDocs = append(allDocs, convertDocument(d))
		}

		offset += len(batch)
		// Stop when we've fetched everything or got an empty page.
		if offset >= total || len(batch) == 0 {
			break
		}
	}

	return allDocs, nil
}

// GetDocument fetches a single document by accession number using
// GET /search/{accessionNumber}.
func (c *Client) GetDocument(accession string) (*models.Document, error) {
	url := apiBase + "search/" + accession

	body, err := c.get(url)
	if err != nil {
		return nil, fmt.Errorf("ADAMS get %s: %w", accession, err)
	}

	var d adamsDocument
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decoding ADAMS document %s: %w", accession, err)
	}

	doc := convertDocument(d)
	return &doc, nil
}

// --- Internal helpers ---

// searchPage fetches one page of ADAMS search results for a docket number.
// Returns the documents on this page, the total result count, and any error.
func (c *Client) searchPage(docket string, offset int) ([]adamsDocument, int, error) {
	// Build the search query. The APS API uses a boolean query syntax;
	// searching by docket number is the most reliable filter.
	reqBody := searchRequest{
		Query:  fmt.Sprintf("docket_number:\"%s\"", docket),
		Offset: offset,
		Limit:  maxPerPage,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("encoding search request: %w", err)
	}

	respBytes, err := c.post(apiBase+"search", bodyBytes)
	if err != nil {
		return nil, 0, err
	}

	var result searchResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, 0, fmt.Errorf("decoding search response: %w", err)
	}

	return result.Data, result.Total, nil
}

// get performs an authenticated GET request and returns the response body.
func (c *Client) get(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building GET request: %w", err)
	}
	c.addAuth(req)

	return c.do(req)
}

// post performs an authenticated POST request with a JSON body.
func (c *Client) post(url string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req)

	return c.do(req)
}

// addAuth injects the ADAMS API subscription key header.
func (c *Client) addAuth(req *http.Request) {
	req.Header.Set("Ocp-Apim-Subscription-Key", c.apiKey)
}

// do executes an HTTP request and returns the body or an error.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s %s: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s %s: status %d", req.Method, req.URL, resp.StatusCode)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return b, nil
}

// convertDocument maps an ADAMS API record to our internal Document type.
func convertDocument(d adamsDocument) models.Document {
	var authors []string
	if d.AuthorName != "" {
		authors = []string{d.AuthorName}
	} else {
		authors = []string{}
	}

	return models.Document{
		ID:       d.AccessionNumber,
		Category: inferCategory(d.Title, d.DocumentType),
		Title:    d.Title,
		Date:     d.DocumentDate,
		Authors:  authors,
		PDFURL:   accessionToPDFURL(d.AccessionNumber),
		Source:   models.SourceADAMSSearch,
	}
}

// accessionToPDFURL converts an ADAMS accession number to its PDF URL.
// Formula: https://www.nrc.gov/docs/{acc[0:6]}/{acc}.pdf
func accessionToPDFURL(acc string) string {
	if len(acc) < 6 {
		return ""
	}
	return fmt.Sprintf("https://www.nrc.gov/docs/%s/%s.pdf", acc[:6], acc)
}

// inferCategory maps ADAMS document type strings and title keywords to our
// internal DocCategory. ADAMS document_type values vary; title-based fallback
// handles cases where document_type is absent or generic.
func inferCategory(title, docType string) models.DocCategory {
	dt := strings.ToLower(docType)
	t := strings.ToLower(title)

	switch {
	case strings.Contains(dt, "rai response") || strings.Contains(t, "response to request"):
		return models.CategoryRAIResponse
	case strings.Contains(dt, "rai") || strings.Contains(t, "request for additional information"):
		return models.CategoryRAI
	case strings.Contains(dt, "ser") || strings.Contains(t, "safety evaluation report"):
		return models.CategorySER
	case strings.Contains(dt, "eis") || strings.Contains(t, "environmental impact statement"):
		return models.CategoryEIS
	case strings.Contains(dt, "nureg") || strings.Contains(t, "nureg"):
		return models.CategoryNUREG
	case strings.Contains(t, "col application") || strings.Contains(t, "fsar"):
		return models.CategoryCOLApplication
	case strings.Contains(dt, "letter") || strings.Contains(t, "letter") || strings.Contains(t, "correspondence"):
		return models.CategoryCorrespondence
	default:
		return models.CategoryOther
	}
}
