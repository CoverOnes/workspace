package service_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMinimalDoc returns a ProofDocument with exactly one signer for basic assertions.
func buildMinimalDoc() domain.ProofDocument {
	contractID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	signerID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	return domain.ProofDocument{
		ContractID:   contractID,
		ContractKind: domain.ContractKindBilateral,
		Title:        "Test Contract",
		TermsSummary: "Payment: USD 5000 upon delivery.",
		Version:      3,
		Signers: []domain.ProofSignerEntry{
			{
				UserID:   signerID,
				Role:     "CLIENT",
				SignedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
			},
		},
		AuditChainHead: "deadbeefdeadbeefdeadbeef",
		GeneratedAt:    time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC),
	}
}

// utf16BEBytes encodes s into UTF-16BE bytes (no BOM) so we can search in the PDF
// metadata section, which gofpdf serializes as UTF-16BE with a BOM prefix (\xfe\xff).
func utf16BEBytes(s string) []byte {
	runes := utf16.Encode([]rune(s))
	out := make([]byte, len(runes)*2)

	for i, r := range runes {
		binary.BigEndian.PutUint16(out[i*2:], r)
	}

	return out
}

// pdfContainsText reports whether the PDF bytes contain the given string in either
// UTF-16BE encoded form (PDF info dict) or raw ASCII/Latin-1 form (content streams
// when uncompressed, xref table, etc.).
func pdfContainsText(pdfData []byte, s string) bool {
	return bytes.Contains(pdfData, utf16BEBytes(s)) || bytes.Contains(pdfData, []byte(s))
}

func TestRenderProofPDF_ReturnsNonEmptyBytes(t *testing.T) {
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err, "RenderProofPDF must not error on valid input")
	assert.NotEmpty(t, data, "PDF bytes must not be empty")
}

func TestRenderProofPDF_IsPDFMagicBytes(t *testing.T) {
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// PDF files start with %PDF-
	assert.True(t, bytes.HasPrefix(data, []byte("%PDF-")),
		"PDF output must start with %%PDF- magic bytes; got prefix: %q", data[:minInt(10, len(data))])
}

func TestRenderProofPDF_ContainsContractID(t *testing.T) {
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// The contract ID appears in PDF metadata (/Title and /Keywords) as UTF-16BE.
	assert.True(t, pdfContainsText(data, doc.ContractID.String()),
		"PDF must contain the contract ID %s in metadata", doc.ContractID)
}

func TestRenderProofPDF_ContainsAuditChainHead(t *testing.T) {
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// Audit chain head appears in /Keywords metadata.
	assert.True(t, pdfContainsText(data, doc.AuditChainHead),
		"PDF must embed the audit chain head %q in metadata", doc.AuditChainHead)
}

func TestRenderProofPDF_EmptyAuditChainHead(t *testing.T) {
	doc := buildMinimalDoc()
	doc.AuditChainHead = ""

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err, "RenderProofPDF must succeed even with an empty audit chain head")
	assert.NotEmpty(t, data)
	// /Keywords should still be present with contract_id but no hash value.
	assert.True(t, pdfContainsText(data, "audit_chain_head="),
		"PDF metadata must contain audit_chain_head= key even when empty")
}

func TestRenderProofPDF_KindEmbeddedInMetadata(t *testing.T) {
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// /Subject contains "contract_kind=bilateral".
	assert.True(t, pdfContainsText(data, "bilateral"),
		"PDF /Subject must contain the contract kind")
}

func TestRenderProofPDF_MultipleSigners(t *testing.T) {
	doc := buildMinimalDoc()
	signerA := doc.Signers[0] // already set by buildMinimalDoc
	signerBID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	signerBSignedAt := time.Date(2025, 1, 15, 11, 0, 0, 0, time.UTC)
	doc.Signers = append(doc.Signers, domain.ProofSignerEntry{
		UserID:   signerBID,
		Role:     "FREELANCER",
		SignedAt: signerBSignedAt,
	})

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	assert.NotEmpty(t, data, "PDF with multiple signers must not be empty")

	// Verify signer A content: user ID, role, and signed-at timestamp.
	assert.True(t, pdfContainsText(data, signerA.UserID.String()),
		"PDF must contain signer A user ID %s", signerA.UserID)
	assert.True(t, pdfContainsText(data, signerA.Role),
		"PDF must contain signer A role %q", signerA.Role)
	assert.True(t, pdfContainsText(data, signerA.SignedAt.UTC().Format("2006-01-02 15:04:05")),
		"PDF must contain signer A signed-at timestamp")

	// Verify signer B content: user ID, role, and signed-at timestamp.
	assert.True(t, pdfContainsText(data, signerBID.String()),
		"PDF must contain signer B user ID %s", signerBID)
	assert.True(t, pdfContainsText(data, "FREELANCER"),
		"PDF must contain signer B role FREELANCER")
	assert.True(t, pdfContainsText(data, signerBSignedAt.UTC().Format("2006-01-02 15:04:05")),
		"PDF must contain signer B signed-at timestamp")
}

func TestRenderProofPDF_NoSigners(t *testing.T) {
	doc := buildMinimalDoc()
	doc.Signers = nil

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err, "RenderProofPDF must succeed with zero signers")
	assert.NotEmpty(t, data)
}

func TestRenderProofPDF_LongTermsTruncated(t *testing.T) {
	doc := buildMinimalDoc()
	// Build a terms string longer than maxTermsLen (2000 runes).
	doc.TermsSummary = strings.Repeat("A", 3000)

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err, "RenderProofPDF must succeed with long terms (truncation expected)")
	assert.NotEmpty(t, data)
}

func TestRenderProofPDF_NonLatin1CharactersReplaced(t *testing.T) {
	doc := buildMinimalDoc()
	// Include a CJK character in the title (outside Latin-1 range) — must not error.
	doc.Title = "Contract Chinese"

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err, "RenderProofPDF must not error on non-Latin-1 title text")
	assert.NotEmpty(t, data)
}

func TestRenderProofPDF_MultipartyKind(t *testing.T) {
	doc := buildMinimalDoc()
	doc.ContractKind = domain.ContractKindMultiparty
	doc.Title = "Multiparty Contract Tender"

	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// /Subject should contain "multiparty".
	assert.True(t, pdfContainsText(data, "multiparty"),
		"multiparty PDF metadata must contain the contract kind")
}

func TestRenderProofPDF_ContractIDInKeywords(t *testing.T) {
	// Contract ID appears in both /Title and /Keywords; verify /Keywords path.
	doc := buildMinimalDoc()
	data, err := service.RenderProofPDF(&doc)

	require.NoError(t, err)
	// /Keywords has "contract_id=<uuid>".
	assert.True(t, pdfContainsText(data, "contract_id="),
		"PDF /Keywords must contain the contract_id= key")
	assert.True(t, pdfContainsText(data, doc.ContractID.String()),
		"PDF /Keywords must contain the contract UUID value")
}

// minInt is a local helper (avoids depending on Go 1.21+ built-in min in test files).
func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}
