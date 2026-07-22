package observehttp

import "net/http"

// withCORS mirrors tower-http's CorsLayer::permissive(): every response
// carries Access-Control-Allow-Origin: * and Access-Control-Allow-Methods:
// *; preflight OPTIONS short-circuits with 204. The dashboard runs on a
// different origin than the observe HTTP port in dev, so permissive CORS
// is required for the SPA to reach the API without a proxy.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
