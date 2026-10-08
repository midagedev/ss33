package sigv4

import (
	"fmt"
	"testing"
)

// The scope is chosen by the client before its signature is checked, so the key cache must stay bounded
// however many distinct scopes arrive.
func TestSigningKeyCacheIsBounded(t *testing.T) {
	for i := 0; i < 1000; i++ {
		signingKey("secret", fmt.Sprintf("20260101/region-%d/s3/aws4_request", i), "20260101")
	}
	signingKeysMu.Lock()
	defer signingKeysMu.Unlock()
	if n := len(signingKeys); n > 64 {
		t.Fatalf("%d cached signing keys", n)
	}
}
