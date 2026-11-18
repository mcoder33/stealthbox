//go:build !darwin

package stealthbox

import "net/http"

func servePastedImage(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "pasted images require a macOS bridge", http.StatusNotImplemented)
}
