package ingest

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
)

// WithGzip returns an independent client that compresses replayable ingest
// requests only while the server advertises support. Signed JSON bytes are
// untouched after decompression. Other requests and the original client stay
// unchanged.
func WithGzip(client *http.Client, supported func() bool) *http.Client {
	copy := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &gzipTransport{base: base, supported: supported}
	return &copy
}

type gzipTransport struct {
	base      http.RoundTripper
	supported func() bool
}

func (t *gzipTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.GetBody == nil ||
		req.Header.Get("Content-Encoding") != "" ||
		(req.URL.Path != ingestEndpoint() && req.URL.Path != "/v1/teams/ingest/batch") ||
		t.supported == nil || !t.supported() {
		return t.base.RoundTrip(req)
	}
	defer req.Body.Close()
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := io.Copy(writer, req.Body); err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	compressed := req.Clone(req.Context())
	compressed.Header.Set("Content-Encoding", "gzip")
	compressed.Body = io.NopCloser(bytes.NewReader(encoded.Bytes()))
	compressed.ContentLength = int64(encoded.Len())
	compressed.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(encoded.Bytes())), nil
	}
	resp, err := t.base.RoundTrip(compressed)
	// A rollback can remove gzip support between policy refreshes. Retry the
	// original bytes before an old parser's 400 can be mistaken for an invalid
	// event and cause the outbox to discard it. Ingest is idempotent.
	if err == nil && (resp.StatusCode == 400 || resp.StatusCode == 415) {
		resp.Body.Close()
		plain := req.Clone(req.Context())
		plain.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		return t.base.RoundTrip(plain)
	}
	return resp, err
}
