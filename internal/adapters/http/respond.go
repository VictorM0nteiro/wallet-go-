package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"internal_error","message":"Internal Server Error"}}`)
	}
	writeRaw(w, status, body)
}

// writeResult sends the outcome of a money-moving request. On a replay it
// sends the originally stored status and body, flagged by a header.
func writeResult(w http.ResponseWriter, res app.TransferResult) {
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeRaw(w, res.Response.StatusCode, res.Response.Body)
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := statusFor(err)
	msg := err.Error()
	if status >= http.StatusInternalServerError {
		msg = http.StatusText(status)
		h.logger.Error("request failed", "request_id", requestIDFrom(r.Context()), "err", err)
	}
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: msg}})
}
