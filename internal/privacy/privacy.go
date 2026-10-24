// Package privacy derives the opaque IDs used in URLs and titles, and sets
// security headers.
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// PIDLength is the number of hex characters in a public media ID.
const PIDLength = 16

// NewSalt returns a random salt as 64 hex characters.
func NewSalt() string {
	return hex.EncodeToString(randBytes(32))
}

// PID derives the public ID of a media file from its content fingerprint.
func PID(salt string, fingerprint []byte) string {
	return mac(salt, "media", fingerprint)[:PIDLength]
}

// DirID derives the public ID of a library folder.
func DirID(salt, root, relDir string) string {
	return mac(salt, "dir", []byte(root+"\x00"+relDir))[:12]
}

func mac(salt, domain string, data []byte) string {
	m := hmac.New(sha256.New, []byte(salt))
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error since Go 1.24.
	_, _ = rand.Read(b)
	return b
}

// contentSecurityPolicy allows same-origin resources only, without inline
// scripts or styles.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; media-src 'self'; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Headers sets security headers, including Cache-Control: no-store (handlers
// may override it).
func Headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
