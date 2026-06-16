package service

import (
	"bytes"
	"fmt"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/go-pdf/fpdf"
)

// maxTermsLen is the maximum number of runes included from TermsSummary in the PDF.
// Long terms are truncated with an ellipsis; the full text remains in the contract record.
const maxTermsLen = 2000

// RenderProofPDF renders a tamper-evidence signed-contract proof PDF for the given
// ProofDocument and returns the raw PDF bytes.
//
// The PDF contains:
//   - Human-readable contract identity (id, kind, title, version).
//   - Truncated terms summary.
//   - A table of all signers (user id, role, UTC signed-at timestamp).
//   - The audit chain head as both visible text and PDF metadata (Keywords/Creator).
//
// The function is pure (no I/O) and deterministic-friendly; the only non-determinism
// is gofpdf's internal font subsetting, which does not affect content correctness.
func RenderProofPDF(doc *domain.ProofDocument) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")

	// Disable content-stream compression so the proof is text-extractable
	// (e.g. via pdftotext). For a tamper-evidence legal artifact the full signer
	// roster, timestamps, and audit chain head must be machine-readable offline,
	// not just the metadata. The size cost on a one-page text document is negligible.
	pdf.SetCompression(false)

	// Embed contract identity and audit chain head in PDF metadata for
	// offline verification without parsing the visible text.
	pdf.SetAuthor("CoverOnes Workspace", true)
	pdf.SetTitle(fmt.Sprintf("Signed Contract Proof — %s", doc.ContractID), true)
	pdf.SetSubject(fmt.Sprintf("contract_kind=%s version=%d", doc.ContractKind, doc.Version), true)
	// Keywords carries the audit chain head so a verifier can check it without
	// rendering the PDF — any PDF metadata reader can extract it.
	pdf.SetKeywords(fmt.Sprintf("audit_chain_head=%s contract_id=%s", doc.AuditChainHead, doc.ContractID), true)
	// Creator carries the generation timestamp for machine parsing.
	pdf.SetCreator(fmt.Sprintf("generated_at=%s", doc.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z")), true)

	pdf.AddPage()

	// ---- Title block -------------------------------------------------------
	pdf.SetFont("Helvetica", "B", 18)
	pdf.CellFormat(0, 12, "Signed Contract Proof", "", 1, "C", false, 0, "")
	pdf.Ln(4)

	// ---- Contract identity section -----------------------------------------
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 7, "Contract Identity", "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "", 10)
	addLabelValue(pdf, "Contract ID", doc.ContractID.String())
	addLabelValue(pdf, "Contract Kind", string(doc.ContractKind))
	addLabelValue(pdf, "Title", sanitizePDFText(doc.Title))
	addLabelValue(pdf, "Version", fmt.Sprintf("%d", doc.Version))
	addLabelValue(pdf, "Generated At (UTC)", doc.GeneratedAt.UTC().Format("2006-01-02 15:04:05"))

	pdf.Ln(4)

	// ---- Terms summary section ---------------------------------------------
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 7, "Terms Summary", "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "", 10)

	terms := doc.TermsSummary
	runes := []rune(terms)

	if len(runes) > maxTermsLen {
		terms = string(runes[:maxTermsLen]) + "…"
	}

	// MultiCell handles line wrapping for long text.
	pdf.MultiCell(0, 5, sanitizePDFText(terms), "1", "L", false)
	pdf.Ln(4)

	// ---- Signers table -----------------------------------------------------
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 7, "Signers", "", 1, "L", false, 0, "")

	// Table header.
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetFillColor(220, 220, 220) //nolint:mnd // RGB grey header background
	pdf.CellFormat(80, 7, "User ID", "1", 0, "L", true, 0, "")
	pdf.CellFormat(40, 7, "Role", "1", 0, "L", true, 0, "")
	pdf.CellFormat(65, 7, "Signed At (UTC)", "1", 1, "L", true, 0, "")

	// Table rows.
	pdf.SetFont("Helvetica", "", 9)
	pdf.SetFillColor(255, 255, 255) //nolint:mnd // white row background

	for _, s := range doc.Signers {
		pdf.CellFormat(80, 6, s.UserID.String(), "1", 0, "L", false, 0, "")
		pdf.CellFormat(40, 6, sanitizePDFText(s.Role), "1", 0, "L", false, 0, "")
		pdf.CellFormat(65, 6, s.SignedAt.UTC().Format("2006-01-02 15:04:05"), "1", 1, "L", false, 0, "")
	}

	pdf.Ln(4)

	// ---- Audit chain section -----------------------------------------------
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 7, "Audit Chain Head", "", 1, "L", false, 0, "")

	pdf.SetFont("Courier", "", 9)

	chainHead := doc.AuditChainHead
	if chainHead == "" {
		chainHead = "(none — no audit entries at generation time)"
	}

	// MultiCell handles long hex hashes that exceed line width.
	pdf.MultiCell(0, 5, chainHead, "1", "L", false)

	pdf.Ln(4)

	// ---- Legal notice footer -----------------------------------------------
	pdf.SetFont("Helvetica", "I", 8)
	pdf.MultiCell(0, 4,
		"This document is a machine-generated proof that the above parties have digitally signed "+
			"the identified contract version. The audit chain head is the SHA-256 hash of the last "+
			"entry in the contract audit log at the time of generation and can be independently verified.",
		"", "L", false)

	var buf bytes.Buffer

	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("render proof PDF: %w", err)
	}

	return buf.Bytes(), nil
}

// addLabelValue writes a label: value row in the current font.
// label is printed bold, value in the current weight.
func addLabelValue(pdf *fpdf.Fpdf, label, value string) {
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(45, 6, label+":", "", 0, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	pdf.MultiCell(0, 6, value, "", "L", false)
}

// sanitizePDFText strips characters that gofpdf cannot encode in Latin-1 (the
// default font encoding), and replaces ASCII control characters with a space.
// Non-Latin characters are replaced with '?' to avoid rendering artifacts.
func sanitizePDFText(s string) string {
	out := make([]rune, 0, len(s))

	for _, r := range s {
		// Strip ASCII control characters (< 0x20) except tab and newline,
		// which gofpdf handles via CellFormat/MultiCell respectively.
		if r < 0x20 && r != '\t' && r != '\n' {
			out = append(out, ' ')
			continue
		}

		// gofpdf's built-in Helvetica/Courier use ISO-8859-1 (Latin-1).
		// Only include runes in the Latin-1 range; replace others with '?'.
		if r < 0x100 {
			out = append(out, r)
		} else {
			out = append(out, '?')
		}
	}

	return string(out)
}
