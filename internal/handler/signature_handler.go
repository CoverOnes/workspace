package handler

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/CoverOnes/workspace/internal/platform/middleware"
	"github.com/CoverOnes/workspace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// reHex64 matches exactly 64 lowercase hexadecimal characters (SHA-256 digest).
var reHex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)

// reANSI matches ANSI/VT100 CSI escape sequences (ESC [ ... final-byte).
// Stripped to prevent log/record injection via crafted User-Agent values.
var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// SignatureHandler handles contract signature endpoints.
type SignatureHandler struct {
	contractSvc  *service.ContractService
	signatureSvc *service.SignatureService
}

// NewSignatureHandler returns a SignatureHandler.
func NewSignatureHandler(contractSvc *service.ContractService, signatureSvc *service.SignatureService) *SignatureHandler {
	return &SignatureHandler{contractSvc: contractSvc, signatureSvc: signatureSvc}
}

// SignRequest is the POST /v1/contracts/:id/sign request body.
type SignRequest struct {
	SignedContentHash string `json:"signedContentHash"`
	// FileID is an optional UUID of a document already uploaded to the file service.
	// When present the workspace service registers it as a signature attachment.
	FileID *uuid.UUID `json:"fileId,omitempty"`
}

// Sign handles POST /v1/contracts/:id/sign.
// signer_ip and user_agent are derived from the request — NEVER from the body.
func (h *SignatureHandler) Sign(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	var req SignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	if req.SignedContentHash == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "signedContentHash is required")
		return
	}

	if !reHex64.MatchString(req.SignedContentHash) {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"signedContentHash must be a 64-character lowercase hex string (SHA-256)")
		return
	}

	// IP and user-agent are derived from the request, NOT from the body.
	ip := c.ClientIP()
	ua := c.GetHeader("User-Agent")
	sanitizedUA := sanitizeUserAgentHeader(ua)

	contract, err := h.contractSvc.SignContract(c.Request.Context(), service.SignContractInput{
		ContractID:        id,
		CallerID:          identity.UserID,
		SignedContentHash: req.SignedContentHash,
		SignerIP:          &ip,
		UserAgent:         sanitizedUA,
		FileID:            req.FileID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, contract)
}

// ListSignatures handles GET /v1/contracts/:id/signatures.
func (h *SignatureHandler) ListSignatures(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	sigs, err := h.signatureSvc.ListSignatures(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if sigs == nil {
		sigs = []*domain.Signature{}
	}

	httpx.OK(c, sigs)
}

// GetAttachmentDownloadURL handles GET /v1/contracts/:id/signatures/:sigId/attachment/download-url.
// Party gate is enforced inside SignatureService.GetAttachmentDownloadURL (IDOR-safe 404).
// Non-party callers and callers requesting a signature with no attachment both receive 404.
func (h *SignatureHandler) GetAttachmentDownloadURL(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	contractID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid contract id")
		return
	}

	sigID, err := uuid.Parse(c.Param("sigId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid signature id")
		return
	}

	result, err := h.signatureSvc.GetAttachmentDownloadURL(c.Request.Context(), service.DownloadURLInput{
		ContractID:  contractID,
		SignatureID: sigID,
		CallerID:    identity.UserID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, result)
}

// sanitizeUserAgentHeader sanitizes the User-Agent header for safe storage and logging:
//  1. Strips ANSI/VT100 CSI escape sequences to prevent terminal injection.
//  2. Removes ASCII control characters (< 0x20, except tab \t) and DEL (0x7f),
//     including null bytes (\x00), CR (\r), LF (\n) — mirroring CONVENTIONS §5.4.
//  3. Caps to 500 runes.
//
// Returns nil if the result is empty.
func sanitizeUserAgentHeader(ua string) *string {
	if ua == "" {
		return nil
	}

	// Step 1: strip ANSI CSI escape sequences.
	ua = reANSI.ReplaceAllString(ua, "")

	// Step 2: remove control characters (< 0x20 except \t) and DEL (0x7f).
	ua = strings.Map(func(r rune) rune {
		if r == '\t' {
			return r // allow tab
		}

		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cc, r) {
			return -1 // drop
		}

		return r
	}, ua)

	if ua == "" {
		return nil
	}

	// Step 3: cap to 500 runes.
	runes := []rune(ua)
	if len(runes) > 500 {
		ua = string(runes[:500])
	}

	return &ua
}
