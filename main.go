// Package main implements a program that visits CCSDS publication webpages,
// extracts PDF URLs from their (possibly escaped) response bodies, removes
// duplicate URLs, and downloads the unique PDFs into a local "PDFs/" directory.
package main

// Import the standard library packages this program depends on.
import (
	"fmt"      // fmt is used for printing progress and error messages to the console.
	"io"       // io is used for copying HTTP response bodies into files.
	"net/http" // net/http is used for issuing HTTP GET requests to the webpages and PDF URLs.
	"net/url"  // net/url is used for parsing and resolving URLs (relative -> absolute, path inspection).
	"os"       // os is used for creating directories, creating files, and checking file existence.
	"path"     // path is used for extracting the base filename component from a URL path.
	"regexp"   // regexp is used for finding href attributes embedded in the raw response bodies.
	"strings"  // strings is used for string manipulation such as replacing escaped characters.
)

// websiteURLs holds the list of CCSDS publication webpages that will be scraped for PDF links.
// Storing them in a slice makes it easy to add more webpages later.
var websiteURLs = []string{
	"https://ccsds.org/publications/bluebooks/",  // The CCSDS Blue Books publication listing page.
	"https://ccsds.org/publications/greenbooks/", // The CCSDS Green Books publication listing page.
}

// pdfDirectoryName is the name of the local directory where downloaded PDFs will be stored.
const pdfDirectoryName = "PDFs"

// escapedHrefPattern matches href attributes whose surrounding quotes are backslash-escaped,
// which is the format observed in the actual CCSDS website responses, e.g. href=\"...\".
// The double backslash below matches a single literal backslash character in the input text.
var escapedHrefPattern = regexp.MustCompile(`href=\\"(.*?)\\"`)

// plainHrefPattern matches ordinary, unescaped href attributes, e.g. href="...".
// This is kept as a fallback in case a webpage returns normal HTML instead of escaped text.
var plainHrefPattern = regexp.MustCompile(`href="([^"]*?)"`)

// main is the program's entry point and drives the overall workflow described in the requirements.
func main() {
	// Announce that the program is creating the PDFs directory.
	fmt.Println("Creating PDFs directory...")
	// Create the PDFs directory (or confirm it already exists), stopping the program on failure.
	if creationError := createPDFDirectory(pdfDirectoryName); creationError != nil {
		// Print the directory-creation error using the standard error stream convention.
		fmt.Println("Error creating PDFs directory:", creationError)
		// Exit the program early because we cannot proceed without the destination directory.
		return
	}

	// discoveredPDFURLs accumulates every PDF URL found across all visited webpages.
	var discoveredPDFURLs []string

	// Iterate over every configured webpage URL in order.
	for _, currentWebsiteURL := range websiteURLs {
		// Print which webpage is about to be visited.
		fmt.Println("\nVisiting:")
		// Print the actual URL being visited on its own line, matching the requested format.
		fmt.Println(currentWebsiteURL)

		// Fetch the raw response body text for the current webpage.
		responseBodyText, fetchError := fetchWebpage(currentWebsiteURL)
		// Check whether fetching the webpage failed.
		if fetchError != nil {
			// Report the fetch error but continue on to the next webpage rather than aborting entirely.
			fmt.Println("Error fetching webpage:", fetchError)
			// Skip the rest of this loop iteration since there is no response body to process.
			continue
		}

		// Extract every candidate PDF URL found within the response body text.
		pdfURLsFromThisPage, extractionError := extractPDFURLs(responseBodyText, currentWebsiteURL)
		// Check whether URL extraction encountered an unrecoverable error.
		if extractionError != nil {
			// Report the extraction error but continue processing the remaining webpages.
			fmt.Println("Error extracting PDF URLs:", extractionError)
			// Skip the rest of this loop iteration since extraction failed.
			continue
		}

		// Print how many PDF URLs were found on this particular webpage.
		fmt.Printf("Found %d PDF URLs.\n", len(pdfURLsFromThisPage))

		// Append the newly found URLs onto the overall slice of discovered PDF URLs.
		discoveredPDFURLs = append(discoveredPDFURLs, pdfURLsFromThisPage...)
	}

	// Print the total number of PDF URLs discovered across all webpages, before deduplication.
	fmt.Printf("\nTotal PDF URLs found: %d\n", len(discoveredPDFURLs))

	// Remove duplicate URLs from the discovered list, obtaining the unique URLs and a duplicate count.
	uniquePDFURLs, duplicateCount := removeDuplicateURLs(discoveredPDFURLs)

	// Print how many duplicate URLs were removed during deduplication.
	fmt.Printf("Duplicate URLs removed: %d\n", duplicateCount)
	// Print how many unique PDF URLs remain after deduplication.
	fmt.Printf("Unique PDF URLs: %d\n", len(uniquePDFURLs))

	// Iterate over every unique PDF URL to determine whether it needs downloading.
	for _, currentPDFURL := range uniquePDFURLs {
		// Determine the local filename that this PDF URL should be saved as.
		localFilename, filenameError := getPDFFilename(currentPDFURL)
		// Check whether the filename could not be determined.
		if filenameError != nil {
			// Report the error and move on to the next PDF URL.
			fmt.Println("Error determining filename for URL:", currentPDFURL, "-", filenameError)
			// Skip to the next iteration of the loop.
			continue
		}

		// Build the full local destination path by joining the PDFs directory with the filename.
		destinationPath := path.Join(pdfDirectoryName, localFilename)

		// Check whether this PDF has already been downloaded previously.
		if isPDFAlreadyDownloaded(destinationPath) {
			// Print the "already exists" message using the requested format.
			fmt.Println("\nPDF already exists, skipping:")
			// Print the destination path that already contains the file.
			fmt.Println(destinationPath)
			// Skip downloading this PDF since it already exists locally.
			continue
		}

		// Print that the program is about to download this PDF.
		fmt.Println("\nDownloading:")
		// Print the URL that is being downloaded.
		fmt.Println(currentPDFURL)

		// Attempt to download the PDF to the computed destination path.
		downloadError := downloadPDF(currentPDFURL, destinationPath)
		// Check whether the download failed.
		if downloadError != nil {
			// Report the download error and continue with the next PDF URL.
			fmt.Println("Error downloading PDF:", downloadError)
			// Move on to the next PDF URL in the loop.
			continue
		}

		// Print confirmation that the PDF was saved successfully.
		fmt.Println("Saved:")
		// Print the local path where the PDF was saved.
		fmt.Println(destinationPath)
	}

	// Announce that the program has completed all work.
	fmt.Println("\nFinished.")
}

// fetchWebpage sends an HTTP GET request to the given webpage URL and returns its response body as a string.
func fetchWebpage(webpageURL string) (string, error) {
	// Perform the HTTP GET request against the given webpage URL.
	httpResponse, requestError := http.Get(webpageURL)
	// Check whether the HTTP request itself failed (e.g. network error, DNS failure).
	if requestError != nil {
		// Wrap and return the underlying request error with additional context.
		return "", fmt.Errorf("failed to fetch webpage %s: %w", webpageURL, requestError)
	}
	// Ensure the response body is closed once this function returns, to avoid leaking resources.
	defer httpResponse.Body.Close()

	// Check whether the HTTP status code indicates a failure (anything other than 200 OK).
	if httpResponse.StatusCode != http.StatusOK {
		// Return a descriptive error including the unexpected status code.
		return "", fmt.Errorf("unexpected HTTP status %d while fetching %s", httpResponse.StatusCode, webpageURL)
	}

	// Read the entire response body into a byte slice.
	responseBodyBytes, readError := io.ReadAll(httpResponse.Body)
	// Check whether reading the response body failed.
	if readError != nil {
		// Wrap and return the read error with additional context.
		return "", fmt.Errorf("failed to read response body from %s: %w", webpageURL, readError)
	}

	// Convert the response body bytes into a string and return it.
	return string(responseBodyBytes), nil
}

// extractPDFURLs scans the given response body text for href attributes, normalizes each
// candidate URL, resolves relative URLs against the base webpage URL, and returns only
// those URLs that point to PDF files.
func extractPDFURLs(responseBodyText string, baseWebpageURL string) ([]string, error) {
	// Parse the base webpage URL so relative URLs can later be resolved against it.
	parsedBaseURL, baseParseError := url.Parse(baseWebpageURL)
	// Check whether the base webpage URL itself failed to parse.
	if baseParseError != nil {
		// Return the parse error with additional context, since resolution would be impossible.
		return nil, fmt.Errorf("failed to parse base webpage URL %s: %w", baseWebpageURL, baseParseError)
	}

	// extractedPDFURLs collects the final list of confirmed PDF URLs found on this page.
	var extractedPDFURLs []string

	// rawHrefMatches will collect every raw href value found using either regex pattern.
	var rawHrefMatches []string

	// Find all matches of the escaped href pattern (href=\"...\") in the response body.
	for _, matchGroups := range escapedHrefPattern.FindAllStringSubmatch(responseBodyText, -1) {
		// Append the captured URL (the first capture group) to the raw matches slice.
		rawHrefMatches = append(rawHrefMatches, matchGroups[1])
	}

	// Find all matches of the plain href pattern (href="...") in the response body as a fallback.
	for _, matchGroups := range plainHrefPattern.FindAllStringSubmatch(responseBodyText, -1) {
		// Append the captured URL (the first capture group) to the raw matches slice.
		rawHrefMatches = append(rawHrefMatches, matchGroups[1])
	}

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

// normalizeEscapedURL converts backslash-escaped characters commonly found in JSON-like
// or escaped-HTML responses (such as "\/") into their normal, unescaped equivalents.
func normalizeEscapedURL(escapedURL string) string {
	// Replace every occurrence of the escaped forward slash "\/" with a normal forward slash "/".
	normalizedURL := strings.ReplaceAll(escapedURL, `\/`, "/")
	// Replace every occurrence of an escaped double quote "\\\"" with a normal double quote, just in case one slipped through.
	normalizedURL = strings.ReplaceAll(normalizedURL, `\"`, `"`)
	// Trim any leading or trailing whitespace that may have been captured accidentally.
	normalizedURL = strings.TrimSpace(normalizedURL)
	// Return the fully normalized URL string.
	return normalizedURL
}

// isPDFURL reports whether the given URL string points to a PDF file, based on its path
// (ignoring any query parameters), and is case-insensitive with respect to the ".pdf" extension.
func isPDFURL(candidateURL string) bool {
	// Parse the candidate URL so that its path component can be inspected independently of any query string.
	parsedURL, parseError := url.Parse(candidateURL)
	// Check whether parsing failed, in which case this cannot be treated as a valid PDF URL.
	if parseError != nil {
		// Return false because an unparseable URL cannot be confirmed as a PDF.
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
	seenURLs := make(map[string]bool)

	// uniqueURLs collects the URLs that have not been seen before, in their original order.
	var uniqueURLs []string

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

// createPDFDirectory creates the given directory (including any necessary parent directories)
// if it does not already exist, and returns an error if directory creation fails.
func createPDFDirectory(directoryPath string) error {
	// Attempt to create the directory, along with any missing parent directories, using standard permissions.
	creationError := os.MkdirAll(directoryPath, 0o755)
	// Check whether the directory creation failed.
	if creationError != nil {
		// Wrap and return the creation error with additional context.
		return fmt.Errorf("failed to create directory %s: %w", directoryPath, creationError)
	}

	// Return nil to indicate the directory was created successfully (or already existed).
	return nil
}

// getPDFFilename determines the local filename that should be used when saving the PDF
// located at the given URL, based on the final path segment of that URL.
func getPDFFilename(pdfURL string) (string, error) {
	// Parse the PDF URL so that its path component can be extracted independently of the query string.
	parsedURL, parseError := url.Parse(pdfURL)
	// Check whether parsing the PDF URL failed.
	if parseError != nil {
		// Wrap and return the parse error with additional context.
		return "", fmt.Errorf("failed to parse PDF URL %s: %w", pdfURL, parseError)
	}

	// Extract the final path segment (base name) from the URL's path, which serves as the filename.
	baseFilename := path.Base(parsedURL.Path)

	// Check whether the extracted filename is empty or otherwise unusable.
	if baseFilename == "" || baseFilename == "." || baseFilename == "/" {
		// Return an error because no sensible filename could be derived from this URL.
		return "", fmt.Errorf("could not determine a valid filename from URL %s", pdfURL)
	}

	// Return the derived filename with no error.
	return baseFilename, nil
}

// isPDFAlreadyDownloaded reports whether a file already exists at the given local destination path.
func isPDFAlreadyDownloaded(destinationPath string) bool {
	// Attempt to retrieve filesystem information about the destination path.
	_, statError := os.Stat(destinationPath)
	// Return true only if os.Stat succeeded (no error), meaning the file already exists.
	return statError == nil
}

// downloadPDF downloads the PDF located at pdfURL and saves it to the given destinationPath,
// reporting any errors encountered while requesting, reading, or writing the file.
func downloadPDF(pdfURL string, destinationPath string) error {
	// Perform the HTTP GET request to retrieve the PDF file.
	httpResponse, requestError := http.Get(pdfURL)
	// Check whether the HTTP request itself failed.
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

	// Create the destination file on disk, truncating it if it somehow already exists.
	destinationFile, fileCreationError := os.Create(destinationPath)
	// Check whether creating the destination file failed.
	if fileCreationError != nil {
		// Wrap and return the file creation error with additional context.
		return fmt.Errorf("failed to create destination file %s: %w", destinationPath, fileCreationError)
	}
	// Ensure the destination file is closed once this function returns, to flush and release the file handle.
	defer destinationFile.Close()

	// Copy the entire response body directly into the destination file.
	_, copyError := io.Copy(destinationFile, httpResponse.Body)
	// Check whether copying the response body into the file failed.
	if copyError != nil {
		// Wrap and return the copy error with additional context.
		return fmt.Errorf("failed to write PDF data to %s: %w", destinationPath, copyError)
	}

	// Return nil to indicate the PDF was downloaded and saved successfully.
	return nil
}
