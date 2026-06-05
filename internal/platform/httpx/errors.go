package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/gin-gonic/gin"
)

// ErrorResponse is the machine-readable error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the stable code, human message, and optional details.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Err sends a structured error response, translating domain errors to HTTP codes.
func Err(c *gin.Context, err error) {
	code, status, message, details := translate(err)
	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// ErrCode sends a raw code/status/message triple (for handler-generated errors
// that don't map cleanly to domain sentinels).
func ErrCode(c *gin.Context, status int, code, message string, details ...any) {
	var d any
	if len(details) > 0 {
		d = details[0]
	}

	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: d,
	}})
}

// translate maps domain / sentinel errors to HTTP status + machine code.
func translate(err error) (code string, status int, message string, details any) {
	if c, s, m, d, ok := translateNotFound(err); ok {
		return c, s, m, d
	}

	if c, s, m, d, ok := translateConflict(err); ok {
		return c, s, m, d
	}

	switch {
	case errors.Is(err, domain.ErrShareSumNotFull):
		return "SHARE_SUM_INVALID", http.StatusUnprocessableEntity, "sum of active party share_bps must equal 10000", nil

	case errors.Is(err, domain.ErrForbidden):
		return "FORBIDDEN", http.StatusForbidden, "forbidden", nil

	case errors.Is(err, domain.ErrUnauthorized):
		return "UNAUTHORIZED", http.StatusUnauthorized, "unauthorized", nil

	case errors.Is(err, domain.ErrKYCTierRequired):
		return "KYC_TIER_REQUIRED", http.StatusForbidden, "kyc verification required", nil

	case errors.Is(err, domain.ErrValidation):
		return "VALIDATION_ERROR", http.StatusBadRequest, err.Error(), nil

	default:
		slog.Error("unhandled internal error", "err", err)
		return "INTERNAL_ERROR", http.StatusInternalServerError, "internal server error", nil
	}
}

// translateNotFound maps domain sentinel errors that indicate a resource was not found.
// Returns (code, status, message, details, true) when matched.
//
//nolint:unparam // details: reserved for structured error payload; always nil today but part of the stable contract
func translateNotFound(err error) (code string, status int, message string, details any, ok bool) {
	switch {
	case errors.Is(err, domain.ErrContractNotFound),
		errors.Is(err, domain.ErrMultipartyContractNotFound),
		errors.Is(err, domain.ErrNotFound),
		errors.Is(err, domain.ErrNotParty),
		errors.Is(err, domain.ErrNotContractOwner),
		errors.Is(err, domain.ErrMilestoneNotFound):
		// ErrNotParty and ErrNotContractOwner return 404 (not 403) to prevent
		// resource-existence enumeration (IDOR guard).
		return "NOT_FOUND", http.StatusNotFound, "resource not found", nil, true

	case errors.Is(err, domain.ErrSignatureNotFound):
		return "SIGNATURE_NOT_FOUND", http.StatusNotFound, "signature not found", nil, true

	case errors.Is(err, domain.ErrTaskNotFound):
		return "TASK_NOT_FOUND", http.StatusNotFound, "task not found", nil, true

	case errors.Is(err, domain.ErrWorklogNotFound):
		return "WORKLOG_NOT_FOUND", http.StatusNotFound, "worklog not found", nil, true

	default:
		return "", 0, "", nil, false
	}
}

// translateConflict maps domain sentinel errors that indicate a conflict / invalid state.
// Returns (code, status, message, details, true) when matched.
//
//nolint:unparam // details: reserved for structured error payload; always nil today but part of the stable contract
func translateConflict(err error) (code string, status int, message string, details any, ok bool) {
	switch {
	case errors.Is(err, domain.ErrInvalidTransition):
		return "INVALID_STATE_TRANSITION", http.StatusConflict, "invalid state transition for current contract status", nil, true

	case errors.Is(err, domain.ErrHashMismatch):
		return "HASH_MISMATCH", http.StatusConflict, "signed content hash does not match current contract hash", nil, true

	case errors.Is(err, domain.ErrAlreadySigned):
		return "ALREADY_SIGNED", http.StatusConflict, "party has already signed this contract version", nil, true

	case errors.Is(err, domain.ErrMilestoneAlreadyDone):
		return "MILESTONE_ALREADY_COMPLETED", http.StatusConflict, "milestone is already completed", nil, true

	case errors.Is(err, domain.ErrStaleVersion):
		return "STALE_VERSION", http.StatusConflict, "signature version does not match current contract version", nil, true

	case errors.Is(err, domain.ErrConflict):
		return "CONFLICT", http.StatusConflict, "conflict detected", nil, true

	default:
		return "", 0, "", nil, false
	}
}
