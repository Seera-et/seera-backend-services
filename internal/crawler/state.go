package crawler

// State is where a document has reached in the acquisition pipeline.
//
// Recorded per document so a crawl is resumable and a failure is attributable:
// at corpus scale, "some documents did not make it" is only actionable if each
// one says how far it got and why it stopped.
//
// The states past PDF_VALIDATED are reached by workers/extraction, not by this
// package. They are declared here because they belong to the same document's
// life, and because the crawler writes the record that tracks it.
type State string

const (
	StateDiscovered  State = "DISCOVERED"
	StateFetchedPage State = "FETCHED_PAGE"
	StatePDFFound    State = "PDF_LINK_FOUND"
	StatePDFDownload State = "PDF_DOWNLOADED"
	StatePDFValid    State = "PDF_VALIDATED"

	StateTextExtracted State = "TEXT_EXTRACTED"
	StateOCRRequired   State = "OCR_REQUIRED"
	StateOCRCompleted  State = "OCR_COMPLETED"
	StateStructured    State = "STRUCTURED"
	StateReady         State = "READY_FOR_INGESTION"

	StateFailed State = "FAILED"
)

// FailureReason categorises why a document stopped.
//
// A category rather than free text, so a run of a thousand documents can be
// counted by cause. The specific error is kept alongside it in Record.Error —
// the category says what kind of problem it is, the message says what happened.
type FailureReason string

const (
	// ReasonNoPDFLink — the law page carried no link that resolved to a PDF.
	ReasonNoPDFLink FailureReason = "PDF_LINK_NOT_FOUND"

	// ReasonJetDownloadFailed — the only candidate was the JetElements
	// endpoint and it did not serve a PDF. Distinguished from
	// ReasonNoPDFLink because it is the Ministry's own broken button rather
	// than a page that never had a document, and because it is common enough
	// that it should be countable on its own.
	ReasonJetDownloadFailed FailureReason = "JET_DOWNLOAD_FAILED"

	// ReasonDownloadFailed — the transfer itself failed: network, timeout,
	// or a status the client gave up on.
	ReasonDownloadFailed FailureReason = "PDF_DOWNLOAD_FAILED"

	// ReasonInvalidPDF — something arrived, and it was not a PDF. Usually an
	// HTML error page served with HTTP 200.
	ReasonInvalidPDF FailureReason = "INVALID_PDF"

	// ReasonPageFetchFailed — the law page could not be retrieved at all.
	ReasonPageFetchFailed FailureReason = "PAGE_FETCH_FAILED"

	// Reached by workers/extraction rather than by this package.
	ReasonParseFailed FailureReason = "PDF_PARSE_FAILED"
	ReasonOCRFailed   FailureReason = "OCR_FAILED"
)

// Terminal reports whether a state ends a document's progress through the
// crawler. READY_FOR_INGESTION is terminal here because what follows it is
// ingestion, which is a different pipeline.
func (s State) Terminal() bool {
	return s == StateReady || s == StateFailed
}
