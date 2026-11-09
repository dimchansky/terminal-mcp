package viewer

import (
	"io"
	"net/http"
	"time"
)

// Administrative paths. They are served behind the MCP bearer guard, never
// through the viewer guard, and the embedded tunnel cannot reach them.
const (
	AdminGrantPath  = "/admin/viewer-grant"
	AdminRevokePath = "/admin/viewer-revoke"
)

// AdminGrant issues a pairing code to the owner's CLI (POST, empty body).
// The code appears only in this response, never in logs.
func AdminGrant(r *Registry) http.Handler {
	return adminPOST(func(w http.ResponseWriter) {
		code, expires, err := r.Issue()
		if err != nil {
			writeRegistryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Code      string    `json:"code"`
			ExpiresAt time.Time `json:"expires_at"`
		}{code, expires.UTC()})
	})
}

// AdminRevoke ends every viewer session and drops pending codes (POST).
func AdminRevoke(r *Registry) http.Handler {
	return adminPOST(func(w http.ResponseWriter) {
		r.RevokeAll()
		w.WriteHeader(http.StatusNoContent)
	})
}

func adminPOST(serve func(http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 64))
		if err != nil || (len(body) != 0 && string(body) != "{}") {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		serve(w)
	})
}
