// Package controller is the chat's REST side: it decodes requests, calls one service method and
// encodes the answer or the error (ARC-001).
package controller

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"

	"github.com/mxcd/inkling-chat/internal/model"
)

// ErrorResponse is the body of every error response, and the data of a stream's error event
// (API-001). Message is safe to show a user.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RespondError answers a request that sent nothing yet with err's status and body, and records
// err on the request's wide event (LOG-002).
func RespondError(ginContext *gin.Context, err error) {
	wideeventginadapter.FromContext(ginContext).Failure(err)
	status, response := errorResponse(err)
	ginContext.AbortWithStatusJSON(status, response)
}

// errorResponse is the one place that maps the model's errors to status codes and to texts
// without internals (API-002).
func errorResponse(err error) (int, ErrorResponse) {
	var invalid *model.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		return http.StatusBadRequest, ErrorResponse{Code: "invalid_argument", Message: invalid.Message}
	case errors.Is(err, model.ErrInvalid):
		return http.StatusBadRequest, ErrorResponse{Code: "invalid_argument", Message: "The request is not valid."}
	case errors.Is(err, model.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, ErrorResponse{Code: "too_large", Message: "The message is too long for the model. Shorten it or send it in parts."}
	case errors.Is(err, model.ErrForbidden):
		return http.StatusForbidden, ErrorResponse{Code: "forbidden", Message: "The server answers only its own pages on this computer."}
	case errors.Is(err, model.ErrUnsupportedMediaType):
		return http.StatusUnsupportedMediaType, ErrorResponse{Code: "unsupported_media_type", Message: "Send the request as application/json."}
	case errors.Is(err, model.ErrNotFound):
		return http.StatusNotFound, ErrorResponse{Code: "not_found", Message: "Not found."}
	case errors.Is(err, model.ErrRejected):
		return http.StatusUnprocessableEntity, ErrorResponse{Code: "rejected", Message: "The model server rejected the request. The conversation may be longer than the model's context: start a new chat."}
	case errors.Is(err, model.ErrUnavailable):
		return http.StatusBadGateway, ErrorResponse{Code: "unavailable", Message: "The model server did not answer. Is it running?"}
	default:
		return http.StatusInternalServerError, ErrorResponse{Code: "internal", Message: "Internal error."}
	}
}
