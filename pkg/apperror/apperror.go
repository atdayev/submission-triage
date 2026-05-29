package apperror

type Code string

const (
	CodeUnauthorized   Code = "unauthorized"
	CodeInternal       Code = "internal_error"
	CodeInvalidPayload Code = "invalid_payload"
)

type ErrorResponse struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func NewErrorResponse(code Code, message string) *ErrorResponse {
	return &ErrorResponse{Code: code, Message: message}
}

func (e *ErrorResponse) WithRequestID(rid string) *ErrorResponse {
	cp := *e
	cp.RequestID = rid
	return &cp
}
