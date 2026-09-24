// Package main implements a production-ready command-line tool that visits CCSDS
// publication webpages, extracts PDF URLs from their (possibly escaped) response
// bodies, removes duplicate URLs, and downloads the unique PDFs sequentially into
// a local output directory, skipping files that have already been downloaded.
package main

// Import every standard-library package this program depends on.
import (
	"context"       // context is used to propagate cancellation/timeouts through HTTP requests and the retry loop.
	"fmt"           // fmt is used to build formatted error and log messages.
	"io"            // io is used for streaming and size-limited copying of HTTP response bodies.
	"log"           // log is used for leveled, timestamped console logging.
	"math/rand"     // math/rand is used to add jitter to retry backoff delays.
	"net/http"      // net/http is used to issue HTTP requests to webpages and PDF files.
	"net/url"       // net/url is used to parse, resolve, and inspect URLs.
	"os"            // os is used for directory/file creation, existence checks, and process exit codes.
	"os/signal"     // os/signal is used to intercept Ctrl+C / SIGTERM for graceful shutdown.
	"path"          // path is used to extract the final segment of a URL's path component.
	"path/filepath" // path/filepath is used for building and validating local, OS-specific file paths.
	"regexp"        // regexp is used to locate href attributes inside raw response bodies.
	"strings"       // strings is used for string cleanup, replacement, and sanitization.
	"syscall"       // syscall is used to also catch SIGTERM in addition to os.Interrupt.
	"time"          // time is used for timeouts, retry backoff, and measuring total run time.
)

// The following constants hold every tunable setting for the program. They are fixed at
// compile time (there are intentionally no command-line flags), so changing behavior means
// editing these values and rebuilding.
const (
	outputDirectory = "PDFs"                                                                                                           // outputDirectory is where downloaded PDFs are stored.
	requestTimeout  = 30 * time.Second                                                                                                 // requestTimeout bounds how long any single HTTP request may take.
	maxRetries      = 3                                                                                                                // maxRetries is how many attempts are made for a failing HTTP operation.
	retryBaseDelay  = 2 * time.Second                                                                                                  // retryBaseDelay is the starting delay for exponential backoff between retries.
	userAgent       = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36" // userAgent is sent on every outgoing HTTP request.
)

// websiteURLs holds the CCSDS publication webpages that will be scraped.
// Storing them in a slice makes it trivial to add more pages later.
var websiteURLs = []string{
	"https://ccsds.org/publications/bluebooks/",    // The CCSDS Blue Books publication listing page.
	"https://ccsds.org/publications/greenbooks/",   // The CCSDS Green Books publication listing page.
	"https://ccsds.org/publications/magentabooks/", // The CCSDS Magenta Books publication listing page.
	"https://ccsds.org/publications/orangebooks/",  // The CCSDS Orange Books publication listing page.
	"https://ccsds.org/publications/yellow-books/", // The CCSDS Yellow Books publication listing page.
	"https://ccsds.org/publications/silverbooks/",  // The CCSDS Silver Books publication listing page.
	"https://ccsds.org/publications/allpubs/",      // The CCSDS "all publications" listing page.
	"https://ccsds.org/publications/ccsdsallpubs/", // The CCSDS "all CCSDS publications" listing page.
	"https://ccsds.org/publications/sis/",          // The CCSDS Space Internetworking Services (SIS) area listing page.
	"https://ccsds.org/publications/moims/",        // The CCSDS Mission Operations and Information Management Services (MOIMS) area listing page.
	"https://ccsds.org/publications/sois/",         // The CCSDS Spacecraft Onboard Interface Services (SOIS) area listing page.
	"https://ccsds.org/publications/sea/",          // The CCSDS Systems Engineering Area (SEA) listing page.
	"https://ccsds.org/publications/css/",          // The CCSDS Cross Support Services (CSS) area listing page.
	"https://ccsds.org/publications/sls/",          // The CCSDS Space Link Services (SLS) area listing page.
}

// escapedHrefPattern matches href attributes whose surrounding quotes are backslash-escaped,
// which is the format actually returned by the CCSDS website, e.g. href=\"...\".
// The double backslash below matches a single literal backslash character in the input text.
var escapedHrefPattern = regexp.MustCompile(`href=\\"(.*?)\\"`)

// plainHrefPattern matches ordinary, unescaped href attributes, e.g. href="...".
// This is kept as a fallback in case a page ever returns normal, unescaped HTML.
var plainHrefPattern = regexp.MustCompile(`href="([^"]*?)"`)

// infoLogger, warnLogger, and errorLogger are leveled loggers used throughout the program.
// Each writes timestamped, prefixed lines so that log severity is immediately visible.
// infoLogger and warnLogger write to standard output; errorLogger writes to standard error,
// which follows the Unix convention of separating diagnostics from normal program output.
var (
	infoLogger  = log.New(os.Stdout, "INFO  ", log.Ldate|log.Ltime) // infoLogger reports normal progress.
	warnLogger  = log.New(os.Stdout, "WARN  ", log.Ldate|log.Ltime) // warnLogger reports recoverable problems (e.g. a retry).
	errorLogger = log.New(os.Stderr, "ERROR ", log.Ldate|log.Ltime) // errorLogger reports failures that affect the result.
)

// stats accumulates counters describing the outcome of a run. The program downloads
// sequentially, so these are plain integers with no need for concurrency-safe access.
type stats struct {
	TotalFound int // TotalFound is the total number of PDF URLs found before deduplication.
	Duplicates int // Duplicates is how many of those URLs were duplicates of an already-seen URL.
	Unique     int // Unique is how many distinct PDF URLs remained after deduplication.
	Downloaded int // Downloaded is how many PDFs were newly downloaded during this run.
	Skipped    int // Skipped is how many PDFs were already present locally and were left untouched.
	Failed     int // Failed is how many PDFs could not be downloaded after all retry attempts.
}

// main is the program's entry point: it wires up graceful shutdown, runs the full
// scrape-and-download pipeline, and prints a final summary.
func main() {
	// Create a context that is automatically cancelled when the process receives SIGINT or SIGTERM,
	// allowing in-flight HTTP requests to be aborted cleanly instead of the process dying abruptly.
	ctx, stopSignalHandling := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Ensure the signal handling is released once main returns.
	defer stopSignalHandling()

	// Record the start time so total elapsed run time can be reported at the end.
	startTime := time.Now()

	// Run the full pipeline and capture the resulting statistics and any fatal error.
	runStats, runError := run(ctx)

	// Compute how long the entire run took.
	elapsedTime := time.Since(startTime)

	// Print the final summary report, even if the run ended early due to an error.
	printSummary(runStats, elapsedTime)

	// Check whether the pipeline returned a fatal error (as opposed to per-file failures, which are just counted).
	if runError != nil {
		// Log the fatal error clearly before exiting.
		errorLogger.Printf("run aborted: %v", runError)
		// Exit with a non-zero status code so calling scripts/CI can detect the failure.
		os.Exit(1)
	}

	// Check whether any individual PDF downloads failed, and exit non-zero to signal partial failure.
	if runStats != nil && runStats.Failed > 0 {
		// Exit with a non-zero status code to indicate that not everything succeeded.
		os.Exit(1)
	}
}

// run executes the full pipeline: create the output directory, scrape every configured
// webpage for PDF URLs, deduplicate them, and download the unique PDFs one at a time.
func run(ctx context.Context) (*stats, error) {
	// Initialize a fresh stats value to track this run's outcome.
	runStats := &stats{}

	// Log the effective configuration so operators can see exactly what will happen.
	infoLogger.Printf("starting run: output-dir=%s timeout=%s retries=%d",
		outputDirectory, requestTimeout, maxRetries)

	// Create the output directory up front, so the whole run fails fast if that is not possible.
	if directoryError := createOutputDirectory(outputDirectory); directoryError != nil {
		// Wrap and return the directory error, since nothing else can proceed without it.
		return runStats, fmt.Errorf("failed to prepare output directory: %w", directoryError)
	}

	// Build a single, reusable HTTP client with a sane timeout for every request it issues.
	httpClient := &http.Client{Timeout: requestTimeout}

	// allDiscoveredPDFURLs accumulates every PDF URL found across all scraped webpages.
	var allDiscoveredPDFURLs []string

	// Iterate over every webpage that should be scraped.
	for _, currentWebsiteURL := range websiteURLs {
		// Stop scraping additional pages if the context has already been cancelled (e.g. Ctrl+C).
		if ctx.Err() != nil {
			// Log that scraping is stopping early due to cancellation.
			warnLogger.Printf("stopping page scraping early: %v", ctx.Err())
			// Break out of the loop since no further work should be started.
			break
		}

		// Log which webpage is about to be visited.
		infoLogger.Printf("visiting page: %s", currentWebsiteURL)

		// Fetch the webpage body, retrying on transient failures with exponential backoff.
		var responseBodyText string
		fetchOperation := func() error {
			// Perform a single fetch attempt and capture the result in the enclosing variable.
			body, fetchError := fetchWebpage(ctx, httpClient, currentWebsiteURL)
			// Store the fetched body for use outside the closure.
			responseBodyText = body
			// Propagate any error so retryWithBackoff can decide whether to retry.
			return fetchError
		}
		// Run the fetch operation with retry-and-backoff semantics.
		if fetchError := retryWithBackoff(ctx, "fetch "+currentWebsiteURL, fetchOperation); fetchError != nil {
			// Log the failure and move on to the next webpage rather than aborting the whole run.
			errorLogger.Printf("giving up on page %s: %v", currentWebsiteURL, fetchError)
			// Skip extraction for this page since there is no body to process.
			continue
		}

		// Extract every PDF URL discovered within this webpage's response body.
		pdfURLsFromThisPage, extractionError := extractPDFURLs(responseBodyText, currentWebsiteURL)
		// Check whether extraction failed outright (e.g. the base URL itself was invalid).
		if extractionError != nil {
			// Log the extraction error and continue with the remaining webpages.
			errorLogger.Printf("failed to extract PDF URLs from %s: %v", currentWebsiteURL, extractionError)
			// Move on to the next webpage.
			continue
		}

		// Log how many PDF URLs were found on this particular webpage.
		infoLogger.Printf("found %d PDF URLs on %s", len(pdfURLsFromThisPage), currentWebsiteURL)

		// Append the newly found URLs onto the overall accumulator slice.
		allDiscoveredPDFURLs = append(allDiscoveredPDFURLs, pdfURLsFromThisPage...)
	}

	// Record the total number of PDF URLs discovered before deduplication.
	runStats.TotalFound = len(allDiscoveredPDFURLs)

	// Deduplicate the discovered PDF URLs, obtaining the unique set and a duplicate count.
	uniquePDFURLs, duplicateCount := removeDuplicateURLs(allDiscoveredPDFURLs)

	// Record the duplicate and unique counts on the stats value.
	runStats.Duplicates = duplicateCount
	runStats.Unique = len(uniquePDFURLs)

	// Log the deduplication results.
	infoLogger.Printf("discovered %d PDF URLs total, %d duplicates removed, %d unique URLs remain",
		len(allDiscoveredPDFURLs), duplicateCount, len(uniquePDFURLs))

	// Download every unique PDF URL, one at a time, in order.
	downloadAllPDFs(ctx, httpClient, uniquePDFURLs, runStats)

	// Return the accumulated stats with no fatal error, since per-file failures are only counted, not fatal.
	return runStats, nil
}

// fetchWebpage sends a single HTTP GET request to the given webpage URL, using the provided
// context for cancellation/timeout, and returns the full response body as a string.
func fetchWebpage(ctx context.Context, client *http.Client, webpageURL string) (string, error) {
	// Build an HTTP request bound to the given context so it can be cancelled or time out.
	httpRequest, requestBuildError := http.NewRequestWithContext(ctx, http.MethodGet, webpageURL, nil)
	// Check whether building the request itself failed (e.g. a malformed URL).
	if requestBuildError != nil {
		// Wrap and return the request-construction error with context.
		return "", fmt.Errorf("failed to build request for %s: %w", webpageURL, requestBuildError)
	}
	// Set a descriptive User-Agent header, which many servers expect from well-behaved clients.
	httpRequest.Header.Set("User-Agent", userAgent)

	// Execute the HTTP request.
	httpResponse, requestError := client.Do(httpRequest)
	// Check whether the request failed at the transport level (network error, timeout, DNS failure, etc.).
	if requestError != nil {
		// Wrap and return the transport error with additional context.
		return "", fmt.Errorf("failed to fetch webpage %s: %w", webpageURL, requestError)
	}
	// Ensure the response body is always closed, even if a later step returns early.
	defer httpResponse.Body.Close()

	// Check whether the HTTP status code indicates a failure (anything other than 200 OK).
	if httpResponse.StatusCode != http.StatusOK {
		// Return a descriptive error including the unexpected status code.
		return "", fmt.Errorf("unexpected HTTP status %d while fetching %s", httpResponse.StatusCode, webpageURL)
	}

	// Read the entire response body into memory.
	responseBodyBytes, readError := io.ReadAll(httpResponse.Body)
	// Check whether reading the response body failed partway through.
	if readError != nil {
		// Wrap and return the read error with additional context.
		return "", fmt.Errorf("failed to read response body from %s: %w", webpageURL, readError)
	}

	// Convert the response body bytes into a string and return it successfully.
	return string(responseBodyBytes), nil
}

// extractPDFURLs scans the given response body text for href attributes, normalizes each
// candidate URL, resolves relative URLs against the base webpage URL, and returns only
// those absolute URLs that point to PDF files.
func extractPDFURLs(responseBodyText string, baseWebpageURL string) ([]string, error) {
	// Parse the base webpage URL so relative URLs can later be resolved against it.
	parsedBaseURL, baseParseError := url.Parse(baseWebpageURL)
	// Check whether the base webpage URL itself failed to parse.
	if baseParseError != nil {
		// Return the parse error with additional context, since resolution would be impossible.
		return nil, fmt.Errorf("failed to parse base webpage URL %s: %w", baseWebpageURL, baseParseError)
	}

	// rawHrefMatches collects every raw href value found by either regex pattern.
	var rawHrefMatches []string

	// Find all matches of the escaped href pattern (href=\"...\") in the response body.
	for _, matchGroups := range escapedHrefPattern.FindAllStringSubmatch(responseBodyText, -1) {
		// Append the captured URL (the first capture group) to the raw matches slice.
		rawHrefMatches = append(rawHrefMatches, matchGroups[1])
	}

	// Find all matches of the plain href pattern (href="...") as a fallback for unescaped HTML.
	for _, matchGroups := range plainHrefPattern.FindAllStringSubmatch(responseBodyText, -1) {
		// Append the captured URL (the first capture group) to the raw matches slice.
		rawHrefMatches = append(rawHrefMatches, matchGroups[1])
	}

	// extractedPDFURLs collects the final list of confirmed, absolute PDF URLs found on this page.
	var extractedPDFURLs []string

	// Process every raw href value that was found by either pattern.
	for _, rawHrefValue := range rawHrefMatches {
		// Normalize the raw href value by converting escaped characters into their normal form.
		normalizedURL := normalizeEscapedURL(rawHrefValue)

		// Attempt to parse the normalized URL to determine whether it is relative or absolute.
		parsedCandidateURL, candidateParseError := url.Parse(normalizedURL)
		// Check whether the candidate URL failed to parse at all.
		if candidateParseError != nil {
			// Skip this candidate since it cannot be interpreted as a valid URL.
			continue
		}

		// Resolve the candidate URL against the base webpage URL, turning relative URLs into absolute ones.
		resolvedURL := parsedBaseURL.ResolveReference(parsedCandidateURL)

		// Obtain the fully resolved, absolute URL as a string.
		absoluteURLString := resolvedURL.String()

		// Check whether the resolved URL actually points to a PDF file.
		if isPDFURL(absoluteURLString) {
			// Append the confirmed PDF URL to the list of extracted PDF URLs.
			extractedPDFURLs = append(extractedPDFURLs, absoluteURLString)
		}
	}

	// Return the collected PDF URLs with no error, since extraction completed successfully.
	return extractedPDFURLs, nil
}

// normalizeEscapedURL converts backslash-escaped characters commonly found in JSON-like or
// escaped-HTML responses (such as "\/") into their normal, unescaped equivalents.
func normalizeEscapedURL(escapedURL string) string {
	// Replace every occurrence of the escaped forward slash "\/" with a normal forward slash "/".
	normalizedURL := strings.ReplaceAll(escapedURL, `\/`, "/")
	// Replace every occurrence of an escaped double quote with a normal double quote, in case one slipped through.
	normalizedURL = strings.ReplaceAll(normalizedURL, `\"`, `"`)
	// Trim any leading or trailing whitespace that may have been captured accidentally.
	normalizedURL = strings.TrimSpace(normalizedURL)
	// Return the fully normalized URL string.
	return normalizedURL
}

// isPDFURL reports whether the given URL string points to a PDF file, based on its path
// (ignoring any query parameters), and is case-insensitive with respect to the ".pdf" extension.
func isPDFURL(candidateURL string) bool {
	// Parse the candidate URL so that its path component can be inspected independently of the query string.
	parsedURL, parseError := url.Parse(candidateURL)
	// Check whether parsing failed, in which case this cannot be confirmed as a PDF URL.
	if parseError != nil {
		// Return false because an unparseable URL cannot be treated as a valid PDF link.
		return false
	}

	// Convert the URL's path to lowercase so the comparison is case-insensitive (e.g. ".PDF" vs ".pdf").
	lowercasePath := strings.ToLower(parsedURL.Path)
	// Return whether the lowercase path ends with the ".pdf" file extension.
	return strings.HasSuffix(lowercasePath, ".pdf")
}

// removeDuplicateURLs takes a slice of URLs and returns a new slice containing only the
// unique URLs (preserving first-seen order), along with a count of how many duplicates were removed.
func removeDuplicateURLs(allURLs []string) ([]string, int) {
	// seenURLs is a map used as a set to efficiently track which URLs have already been encountered.
	seenURLs := make(map[string]bool, len(allURLs))

	// uniqueURLs collects the URLs that have not been seen before, in their original order.
	uniqueURLs := make([]string, 0, len(allURLs))

	// duplicateCount tracks how many URLs were found to be duplicates of an already-seen URL.
	duplicateCount := 0

	// Iterate over every URL in the input slice.
	for _, currentURL := range allURLs {
		// Check whether this URL has already been recorded as seen.
		if seenURLs[currentURL] {
			// Increment the duplicate counter since this URL was already seen before.
			duplicateCount++
			// Skip adding this URL again since it is a duplicate.
			continue
		}

		// Mark this URL as seen for future iterations.
		seenURLs[currentURL] = true
		// Append this newly seen URL to the slice of unique URLs.
		uniqueURLs = append(uniqueURLs, currentURL)
	}

	// Return the slice of unique URLs along with the total duplicate count.
	return uniqueURLs, duplicateCount
}

// createOutputDirectory creates the given directory (including any necessary parent
// directories) if it does not already exist, returning an error if creation fails.
func createOutputDirectory(directoryPath string) error {
	// Attempt to create the directory, along with any missing parent directories, using standard permissions.
	creationError := os.MkdirAll(directoryPath, 0o755)
	// Check whether the directory creation failed.
	if creationError != nil {
		// Wrap and return the creation error with additional context.
		return fmt.Errorf("failed to create directory %s: %w", directoryPath, creationError)
	}

	// Log that the directory is ready for use.
	infoLogger.Printf("output directory ready: %s", directoryPath)
	// Return nil to indicate the directory was created successfully (or already existed).
	return nil
}

// getPDFFilename determines a safe local filename for the PDF located at the given URL,
// based on the final path segment of that URL, sanitized against unsafe characters.
func getPDFFilename(pdfURL string) (string, error) {
	// Parse the PDF URL so that its path component can be extracted independently of the query string.
	parsedURL, parseError := url.Parse(pdfURL)
	// Check whether parsing the PDF URL failed.
	if parseError != nil {
		// Wrap and return the parse error with additional context.
		return "", fmt.Errorf("failed to parse PDF URL %s: %w", pdfURL, parseError)
	}

	// Extract the final path segment (base name) from the URL's path, which serves as the raw filename.
	rawFilename := path.Base(parsedURL.Path)

	// Sanitize the raw filename to guard against path traversal or otherwise unsafe names.
	sanitizedFilename := sanitizeFilename(rawFilename)

	// Check whether sanitization left an unusable, empty filename.
	if sanitizedFilename == "" {
		// Return an error because no sensible, safe filename could be derived from this URL.
		return "", fmt.Errorf("could not determine a valid filename from URL %s", pdfURL)
	}

	// Return the sanitized filename with no error.
	return sanitizedFilename, nil
}

// sanitizeFilename strips any directory separators, parent-directory references, and other
// unsafe characters from a filename, so it cannot be used to escape the output directory.
func sanitizeFilename(rawFilename string) string {
	// Keep only the final path element according to the OS-specific rules, discarding any directory components.
	cleanedFilename := filepath.Base(rawFilename)

	// Treat certain sentinel values returned by filepath.Base as "no usable filename".
	if cleanedFilename == "." || cleanedFilename == string(filepath.Separator) || cleanedFilename == ".." {
		// Return an empty string to signal that no safe filename could be derived.
		return ""
	}

	// Replace any remaining ".." sequences defensively, even though filepath.Base should have removed them.
	cleanedFilename = strings.ReplaceAll(cleanedFilename, "..", "_")

	// Return the fully sanitized filename.
	return cleanedFilename
}

// isPathWithinDirectory reports whether candidatePath resolves to a location inside parentDirectory,
// which guards against path traversal even if a filename were somehow crafted maliciously.
func isPathWithinDirectory(candidatePath string, parentDirectory string) bool {
	// Resolve the parent directory to an absolute, cleaned path.
	absoluteParent, parentError := filepath.Abs(parentDirectory)
	// Check whether resolving the parent directory failed.
	if parentError != nil {
		// Treat resolution failure as "not safe" out of an abundance of caution.
		return false
	}

	// Resolve the candidate path to an absolute, cleaned path.
	absoluteCandidate, candidateError := filepath.Abs(candidatePath)
	// Check whether resolving the candidate path failed.
	if candidateError != nil {
		// Treat resolution failure as "not safe" out of an abundance of caution.
		return false
	}

	// Compute the relative path from the parent directory to the candidate path.
	relativePath, relError := filepath.Rel(absoluteParent, absoluteCandidate)
	// Check whether computing the relative path failed.
	if relError != nil {
		// Treat a failure to compute a relative path as "not safe".
		return false
	}

	// Reject the path if it needs to climb out of the parent directory (contains "..") or is absolute.
	return relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) && !filepath.IsAbs(relativePath)
}

// isPDFAlreadyDownloaded reports whether a non-empty file already exists at the given local path.
func isPDFAlreadyDownloaded(destinationPath string) bool {
	// Attempt to retrieve filesystem information about the destination path.
	fileInfo, statError := os.Stat(destinationPath)
	// Check whether the stat call failed, meaning the file does not exist (or is inaccessible).
	if statError != nil {
		// Return false since the file cannot be confirmed to already exist.
		return false
	}

	// Treat a zero-byte file as not properly downloaded, so a previously interrupted download is retried.
	return fileInfo.Size() > 0
}

// downloadPDF downloads the PDF located at pdfURL and atomically saves it to destinationPath:
// it streams the response into a temporary file in the same directory and only renames that
// temporary file into place once the download has completed successfully in full.
func downloadPDF(ctx context.Context, client *http.Client, pdfURL string, destinationPath string) error {
	// Build an HTTP request bound to the given context so it can be cancelled or time out.
	httpRequest, requestBuildError := http.NewRequestWithContext(ctx, http.MethodGet, pdfURL, nil)
	// Check whether building the request itself failed.
	if requestBuildError != nil {
		// Wrap and return the request-construction error with context.
		return fmt.Errorf("failed to build request for %s: %w", pdfURL, requestBuildError)
	}
	// Set the configured User-Agent header on the outgoing request.
	httpRequest.Header.Set("User-Agent", userAgent)

	// Execute the HTTP request to retrieve the PDF file.
	httpResponse, requestError := client.Do(httpRequest)
	// Check whether the HTTP request itself failed at the transport level.
	if requestError != nil {
		// Wrap and return the request error with additional context.
		return fmt.Errorf("failed to request PDF %s: %w", pdfURL, requestError)
	}
	// Ensure the response body is closed once this function returns, to avoid leaking resources.
	defer httpResponse.Body.Close()

	// Check whether the HTTP status code indicates a failure (anything other than 200 OK).
	if httpResponse.StatusCode != http.StatusOK {
		// Return a descriptive error including the unexpected status code.
		return fmt.Errorf("unexpected HTTP status %d while downloading %s", httpResponse.StatusCode, pdfURL)
	}

	// Determine the directory that will hold the temporary file, so it lives alongside the final destination
	// (which keeps the later rename on the same filesystem, making it atomic on POSIX systems).
	destinationDirectory := filepath.Dir(destinationPath)

	// Create a temporary file in the destination directory to stream the download into.
	temporaryFile, temporaryFileError := os.CreateTemp(destinationDirectory, "*.pdf.downloading")
	// Check whether creating the temporary file failed.
	if temporaryFileError != nil {
		// Wrap and return the temporary-file creation error with additional context.
		return fmt.Errorf("failed to create temporary file for %s: %w", pdfURL, temporaryFileError)
	}
	// temporaryFilePath remembers the temporary file's path for cleanup and renaming below.
	temporaryFilePath := temporaryFile.Name()
	// downloadSucceeded tracks whether the copy step completed successfully, for cleanup purposes.
	downloadSucceeded := false
	// Ensure the temporary file is removed on any exit path where the download did not succeed.
	defer func() {
		// Close the temporary file if it has not already been closed, ignoring any close error here.
		_ = temporaryFile.Close()
		// Check whether the download did not succeed, in which case the temp file must be cleaned up.
		if !downloadSucceeded {
			// Remove the leftover temporary file, ignoring errors since this is best-effort cleanup.
			_ = os.Remove(temporaryFilePath)
		}
	}()

	// Copy the entire response body into the temporary file, with no size limit.
	_, copyError := io.Copy(temporaryFile, httpResponse.Body)
	// Check whether copying the response body into the file failed.
	if copyError != nil {
		// Wrap and return the copy error with additional context.
		return fmt.Errorf("failed to write PDF data for %s: %w", pdfURL, copyError)
	}

	// Flush and close the temporary file explicitly, checking for any write-back error before renaming.
	if closeError := temporaryFile.Close(); closeError != nil {
		// Wrap and return the close error with additional context.
		return fmt.Errorf("failed to finalize temporary file for %s: %w", pdfURL, closeError)
	}

	// Atomically move the completed temporary file into its final destination path.
	if renameError := os.Rename(temporaryFilePath, destinationPath); renameError != nil {
		// Wrap and return the rename error with additional context.
		return fmt.Errorf("failed to move downloaded PDF into place for %s: %w", pdfURL, renameError)
	}

	// Mark the download as successful so the deferred cleanup does not remove the (now-renamed) temp file.
	downloadSucceeded = true
	// Return nil to indicate the PDF was downloaded and saved successfully.
	return nil
}

// downloadAllPDFs downloads every URL in pdfURLs one at a time, in order, skipping files
// that already exist locally, retrying transient failures, and updating runStats as each
// URL is processed.
func downloadAllPDFs(ctx context.Context, client *http.Client, pdfURLs []string, runStats *stats) {
	// totalCount is the total number of unique PDF URLs to be processed, used for progress logging.
	totalCount := len(pdfURLs)
	// Check whether there is nothing to download, in which case we can return immediately.
	if totalCount == 0 {
		// Log that there is no work to do and return early.
		infoLogger.Printf("no PDF URLs to download")
		// Nothing further to do.
		return
	}

	// Log that sequential downloading is about to begin.
	infoLogger.Printf("downloading %d unique PDFs", totalCount)

	// Iterate over every unique PDF URL in order.
	for pdfIndex, currentPDFURL := range pdfURLs {
		// Stop processing further downloads if the context has already been cancelled.
		if ctx.Err() != nil {
			// Log that remaining downloads are being skipped due to cancellation.
			warnLogger.Printf("stopping remaining downloads due to cancellation: %v", ctx.Err())
			// Break out of the loop since no further downloads should be attempted.
			break
		}

		// Process this single PDF URL (skip-if-exists, download-with-retry, and stats bookkeeping).
		processSinglePDFURL(ctx, client, currentPDFURL, runStats)

		// Log overall progress, using a 1-based index so counts read naturally (e.g. "1/180").
		infoLogger.Printf("progress: %d/%d PDFs processed", pdfIndex+1, totalCount)
	}
}

// processSinglePDFURL handles the full per-file workflow for one PDF URL: determining its
// filename, skipping it if already downloaded, downloading it with retries otherwise, and
// updating the shared stats counters accordingly.
func processSinglePDFURL(ctx context.Context, client *http.Client, pdfURL string, runStats *stats) {
	// Determine the sanitized local filename that this PDF should be saved as.
	localFilename, filenameError := getPDFFilename(pdfURL)
	// Check whether a safe filename could not be determined.
	if filenameError != nil {
		// Log the error, count it as a failure, and stop processing this URL.
		errorLogger.Printf("skipping invalid PDF URL %s: %v", pdfURL, filenameError)
		// Increment the failure counter.
		runStats.Failed++
		// Nothing more to do for this URL.
		return
	}

	// Build the full local destination path by joining the output directory with the sanitized filename.
	destinationPath := filepath.Join(outputDirectory, localFilename)

	// Verify the resolved destination path actually stays within the configured output directory.
	if !isPathWithinDirectory(destinationPath, outputDirectory) {
		// Log the rejected path, count it as a failure, and stop processing this URL as a safety measure.
		errorLogger.Printf("refusing unsafe destination path for %s: %s", pdfURL, destinationPath)
		// Increment the failure counter.
		runStats.Failed++
		// Nothing more to do for this URL.
		return
	}

	// Check whether this PDF has already been downloaded previously.
	if isPDFAlreadyDownloaded(destinationPath) {
		// Log that this file is being skipped because it already exists.
		infoLogger.Printf("already exists, skipping: %s", destinationPath)
		// Increment the skipped counter.
		runStats.Skipped++
		// Nothing more to do for this URL.
		return
	}

	// Log that a download is starting for this URL.
	infoLogger.Printf("downloading: %s", pdfURL)

	// downloadOperation performs one attempt at downloading this PDF, for use with retryWithBackoff.
	downloadOperation := func() error {
		// Delegate to downloadPDF, passing through the shared context and client.
		return downloadPDF(ctx, client, pdfURL, destinationPath)
	}

	// Run the download with retry-and-backoff semantics to smooth over transient network issues.
	if downloadError := retryWithBackoff(ctx, "download "+pdfURL, downloadOperation); downloadError != nil {
		// Log the final failure after all retries were exhausted.
		errorLogger.Printf("failed to download %s: %v", pdfURL, downloadError)
		// Increment the failure counter.
		runStats.Failed++
		// Nothing more to do for this URL.
		return
	}

	// Log that the download completed and was saved successfully.
	infoLogger.Printf("saved: %s", destinationPath)
	// Increment the downloaded counter.
	runStats.Downloaded++
}

// retryWithBackoff runs operation up to maxRetries times, waiting an exponentially increasing,
// jittered delay between attempts, and stops early if ctx is cancelled. It returns nil as soon
// as operation succeeds, or a wrapped error describing the final failure if every attempt fails.
func retryWithBackoff(ctx context.Context, description string, operation func() error) error {
	// lastError remembers the most recent failure so it can be reported if every attempt fails.
	var lastError error

	// Attempt the operation up to maxRetries times.
	for attemptNumber := 1; attemptNumber <= maxRetries; attemptNumber++ {
		// Stop immediately if the context has already been cancelled before this attempt.
		if ctxError := ctx.Err(); ctxError != nil {
			// Return the context error directly, since further attempts would be pointless.
			return ctxError
		}

		// Run the operation once and capture whether it succeeded.
		lastError = operation()
		// Check whether this attempt succeeded.
		if lastError == nil {
			// Return nil immediately on success, skipping any further attempts.
			return nil
		}

		// Check whether this was the final allowed attempt, in which case there is no point waiting further.
		if attemptNumber == maxRetries {
			// Break out of the loop so the accumulated lastError can be reported below.
			break
		}

		// Compute an exponentially growing backoff delay based on the attempt number.
		exponentialDelay := retryBaseDelay * time.Duration(1<<uint(attemptNumber-1))
		// Add random jitter (up to one retryBaseDelay) so repeated retries do not all fire at once.
		jitterDelay := time.Duration(rand.Int63n(int64(retryBaseDelay) + 1))
		// Combine the exponential delay and jitter into the total wait duration for this retry.
		waitDuration := exponentialDelay + jitterDelay

		// Log the retry decision so operators can see why a delay is happening.
		warnLogger.Printf("%s failed (attempt %d/%d): %v — retrying in %s", description, attemptNumber, maxRetries, lastError, waitDuration)

		// Wait for either the backoff duration to elapse or the context to be cancelled, whichever comes first.
		select {
		case <-time.After(waitDuration):
			// The backoff delay elapsed normally; continue on to the next attempt.
		case <-ctx.Done():
			// The context was cancelled while waiting; abort the retry loop immediately.
			return ctx.Err()
		}
	}

	// Every attempt failed; return a wrapped error describing the final failure after all retries.
	return fmt.Errorf("%s failed after %d attempts: %w", description, maxRetries, lastError)
}

// printSummary logs a clearly formatted final report of the run's outcome and total elapsed time.
func printSummary(runStats *stats, elapsedTime time.Duration) {
	// Guard against a nil stats value, which could happen if the run failed before any stats were produced.
	if runStats == nil {
		// Log a minimal summary noting that no statistics are available.
		infoLogger.Printf("run ended with no statistics available (elapsed %s)", elapsedTime.Round(time.Millisecond))
		// Nothing further to print.
		return
	}

	// Print a visual separator to make the summary block easy to spot in the console output.
	infoLogger.Printf("========================================")
	// Print the summary section header.
	infoLogger.Printf("SUMMARY")
	// Print another visual separator.
	infoLogger.Printf("========================================")
	// Print the total elapsed run time, rounded to millisecond precision for readability.
	infoLogger.Printf("Elapsed time:        %s", elapsedTime.Round(time.Millisecond))
	// Print how many PDF URLs were found in total, before deduplication.
	infoLogger.Printf("PDF URLs found:      %d", runStats.TotalFound)
	// Print how many duplicate URLs were removed during deduplication.
	infoLogger.Printf("Duplicates removed:  %d", runStats.Duplicates)
	// Print how many unique PDF URLs remained after deduplication.
	infoLogger.Printf("Unique PDF URLs:     %d", runStats.Unique)
	// Print how many PDFs were newly downloaded during this run.
	infoLogger.Printf("Downloaded:          %d", runStats.Downloaded)
	// Print how many PDFs already existed locally and were skipped.
	infoLogger.Printf("Skipped (existing):  %d", runStats.Skipped)
	// Print how many PDFs ultimately failed to download after all retries.
	infoLogger.Printf("Failed:              %d", runStats.Failed)
	// Print a final visual separator to close out the summary block.
	infoLogger.Printf("========================================")
}
