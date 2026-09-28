package ingest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipIngestPreservesBytesAndRollback(t *testing.T) {
	for _, mode := range []string{"gzip", "unsupported", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			raw := `{"data":{"command":"echo hello | cat; curl https://example.com","n":1234567890123456789,"text":"é🦋"}}`
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body io.Reader = r.Body
				if r.Header.Get("Content-Encoding") == "gzip" {
					if mode == "unsupported" {
						t.Error("compressed without advertisement")
					}
					if mode == "rollback" {
						w.WriteHeader(400)
						return
					}
					g, err := gzip.NewReader(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					defer g.Close()
					body = g
				} else if mode == "gzip" {
					t.Error("missing gzip encoding")
				}
				got, _ := io.ReadAll(body)
				if string(got) != raw {
					t.Errorf("changed signed bytes: %s", got)
				}
				if r.Header.Get("X-API-Key") != "synthetic-key" {
					t.Error("lost authentication header")
				}
				w.WriteHeader(201)
			}))
			defer srv.Close()
			client := WithGzip(srv.Client(), func() bool { return mode != "unsupported" })
			req, _ := http.NewRequest("POST", srv.URL+"/v1/teams/ingest", strings.NewReader(raw))
			req.Header.Set("X-API-Key", "synthetic-key")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			want := 1
			if mode == "rollback" {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}
