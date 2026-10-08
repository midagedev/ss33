package server_test

// Request-handling cost without the network: `go test -bench . -cpu 1,8 ./internal/server`.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

func benchServer(b *testing.B) (http.Handler, func(method, path string) *http.Request) {
	st, err := store.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	creds := sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}
	srv := &server.Server{Store: st, Creds: creds, Region: "us-east-1"}
	signed := func(method, path string) *http.Request {
		req := httptest.NewRequest(method, "http://127.0.0.1:9000"+path, strings.NewReader(""))
		sigv4.Sign(req, creds, "us-east-1", time.Now())
		return req
	}
	srv.ServeHTTP(httptest.NewRecorder(), signed(http.MethodPut, "/bkt"))
	return srv, signed
}

// clone copies a signed request so parallel iterations do not share one body or header map.
func clone(r *http.Request) *http.Request {
	c := r.Clone(r.Context())
	c.Body = http.NoBody
	return c
}

func BenchmarkPutEmpty(b *testing.B) {
	srv, signed := benchServer(b)
	req := signed(http.MethodPut, "/bkt/k")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, clone(req))
			if rec.Code != http.StatusOK {
				b.Fatalf("PUT: %d %s", rec.Code, rec.Body)
			}
		}
	})
}

func BenchmarkHead(b *testing.B) {
	srv, signed := benchServer(b)
	srv.ServeHTTP(httptest.NewRecorder(), signed(http.MethodPut, "/bkt/k"))
	req := signed(http.MethodHead, "/bkt/k")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, clone(req))
			if rec.Code != http.StatusOK {
				b.Fatalf("HEAD: %d", rec.Code)
			}
		}
	})
}
