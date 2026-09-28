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
	for _, mode := range []string{"gzip", "unsupported", "rollback", "custom-batch", "invalid-event"} {
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
						_, _ = io.WriteString(w, `{"code":"FST_ERR_CTP_INVALID_CONTENT_LENGTH"}`)
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
				} else if mode == "gzip" || mode == "custom-batch" || mode == "invalid-event" {
					t.Error("missing gzip encoding")
				}
				got, _ := io.ReadAll(body)
				if string(got) != raw {
					t.Errorf("changed signed bytes: %s", got)
				}
				if r.Header.Get("X-API-Key") != "synthetic-key" {
					t.Error("lost authentication header")
				}
				if mode == "invalid-event" {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid event"}`)
					return
				}
				w.WriteHeader(201)
			}))
			defer srv.Close()
			client := WithGzip(srv.Client(), func() (string, bool) { return "/custom/batch", mode != "unsupported" })
			path := "/v1/teams/ingest"
			if mode == "custom-batch" {
				path = "/custom/batch"
			}
			req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(raw))
			req.Header.Set("X-API-Key", "synthetic-key")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			expectedStatus := 201
			if mode == "invalid-event" {
				expectedStatus = 400
				if string(responseBody) != `{"error":"invalid event"}` {
					t.Fatal("lost rejection body")
				}
			}
			if resp.StatusCode != expectedStatus {
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
