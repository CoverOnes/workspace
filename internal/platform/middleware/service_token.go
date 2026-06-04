package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/CoverOnes/workspace/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

const headerServiceToken = "X-Service-Token"

// RequireServiceToken returns a middleware that validates a pre-shared service
// token supplied in the X-Service-Token header. This is used to restrict the
// internal contract-create endpoint to calls from the marketplace service only.
//
// constant-time comparison via crypto/subtle.ConstantTimeCompare prevents
// timing-based token enumeration.
func RequireServiceToken(expected string) gin.HandlerFunc {
	expectedBytes := []byte(expected)

	return func(c *gin.Context) {
		supplied := c.GetHeader(headerServiceToken)
		if supplied == "" {
			c.Abort()
			httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "service token required")

			return
		}

		if subtle.ConstantTimeCompare([]byte(supplied), expectedBytes) != 1 {
			c.Abort()
			httpx.ErrCode(c, http.StatusForbidden, "FORBIDDEN", "invalid service token")

			return
		}

		c.Next()
	}
}
