package v1

import (
	"time"
)

// Standard Error Codes
const (
	ErrCodeValidationFailed    = "VALIDATION_FAILED"
	ErrCodeJobNotFound         = "JOB_NOT_FOUND"
	ErrCodeGPUNotFound         = "GPU_NOT_FOUND"
	ErrCodeArtifactNotFound    = "ARTIFACT_NOT_FOUND"
	ErrCodeDuplicateRequestID  = "DUPLICATE_REQUEST_ID"
	ErrCodeQuotaExceeded       = "QUOTA_EXCEEDED"
	ErrCodeInternalServerError = "INTERNAL_SERVER_ERROR"
)

// ValidationErrorDetail provides field-level feedback for 400 Bad Request.
type ValidationErrorDetail struct {
	Field         string      `json:"field"`
	RejectedValue interface{} `json:"rejected_value,omitempty"`
	Message       string      `json:"message"`
}

// ErrorResponse is the standardized JSON envelope returned on API errors.
type ErrorResponse struct {
	ErrorCode string                  `json:"error_code"`
	Message   string                  `json:"message"`
	Details   []ValidationErrorDetail `json:"details,omitempty"`
	Timestamp time.Time               `json:"timestamp"`
}

// NewValidationError constructs an ErrorResponse with field details.
func NewValidationError(message string, details ...ValidationErrorDetail) *ErrorResponse {
	return &ErrorResponse{
		ErrorCode: ErrCodeValidationFailed,
		Message:   message,
		Details:   details,
		Timestamp: time.Now().UTC(),
	}
}

// NewNotFoundError constructs an ErrorResponse for 404s.
func NewNotFoundError(code, message string) *ErrorResponse {
	return &ErrorResponse{
		ErrorCode: code,
		Message:   message,
		Timestamp: time.Now().UTC(),
	}
}

