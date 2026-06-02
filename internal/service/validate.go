// Package service implements the workspace business logic.
package service

import (
	"fmt"
	"unicode/utf8"

	"github.com/CoverOnes/workspace/internal/domain"
	"github.com/shopspring/decimal"
)

// maxNumeric14_2 is the maximum value representable by numeric(14,2): 999999999999.99.
var maxNumeric14_2 = decimal.NewFromFloat(999999999999.99) //nolint:gochecknoglobals // package-level sentinel; immutable after init

// sanitizeText rejects null bytes, carriage returns, newlines, and ASCII control chars
// in user-supplied strings (backend-security §5.4).
func sanitizeText(s string) error {
	for _, r := range s {
		if r == '\x00' || r == '\r' || r == '\n' {
			return fmt.Errorf("contains illegal control characters (null, CR, LF)")
		}

		if r < 0x20 && r != '\t' {
			return fmt.Errorf("contains ASCII control characters")
		}
	}

	return nil
}

// validateTitle validates a title field: 1-200 chars, sanitized.
func validateTitle(title string) error {
	if err := sanitizeText(title); err != nil {
		return fmt.Errorf("%w: title: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > 200 {
		return fmt.Errorf("%w: title must be 1-200 characters", domain.ErrValidation)
	}

	return nil
}

// validateTerms validates contract terms: 0-50000 chars, sanitized.
func validateTerms(terms string) error {
	if err := sanitizeText(terms); err != nil {
		return fmt.Errorf("%w: terms: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(terms) > 50000 {
		return fmt.Errorf("%w: terms exceeds 50000 characters", domain.ErrValidation)
	}

	return nil
}

// validateDescription validates a description field: 0-10000 chars, sanitized.
func validateDescription(desc string) error {
	if err := sanitizeText(desc); err != nil {
		return fmt.Errorf("%w: description: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(desc) > 10000 {
		return fmt.Errorf("%w: description exceeds 10000 characters", domain.ErrValidation)
	}

	return nil
}

// validateCurrency validates a 3-letter currency code.
func validateCurrency(currency string) error {
	if len(currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter code", domain.ErrValidation)
	}

	return nil
}

// validateAmount validates an amount is positive and within numeric(14,2) range.
func validateAmount(amount decimal.Decimal) error {
	if amount.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("%w: amount must be greater than 0", domain.ErrValidation)
	}

	if amount.GreaterThan(maxNumeric14_2) {
		return fmt.Errorf("%w: amount exceeds maximum allowed value", domain.ErrValidation)
	}

	return nil
}
