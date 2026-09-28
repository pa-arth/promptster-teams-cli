package ingest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
)

// WithGzip returns an independent client that compresses replayable ingest
// requests only while the server advertises support. Signed JSON bytes are
// untouched after decompression. Other requests and the original client stay
// unchanged.
func WithGzip(client *http.Client, supported func() (batchEndpoint string, ok bool)) *http.Client {
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
	supported func() (batchEndpoint string, ok bool)
}

func (t *gzipTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var batchEndpoint string
	var supported bool
	if t.supported != nil {
		batchEndpoint, supported = t.supported()
	}
	if req.Method != http.MethodPost || req.GetBody == nil ||
		req.Header.Get("Content-Encoding") != "" ||
		(req.URL.Path != ingestEndpoint() && (batchEndpoint == "" || req.URL.Path != batchEndpoint)) ||
		!supported {
		return t.base.RoundTrip(req)
	}
	defer req.Body.Close()
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := io.Copy(writer, req.Body); err != nil {
		_ = writer.Close()
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
	if err == nil && gzipUnsupported(resp) {
		_ = resp.Body.Close()
		plain := req.Clone(req.Context())
		plain.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		return t.base.RoundTrip(plain)
	}
	return resp, err
}

// Only retry the old Fastify parser's transport errors, not real event-shape
// rejections. Preserve the response body for the caller when it is not retried.
func gzipUnsupported(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnsupportedMediaType {
		return true
	}
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	original := resp.Body
	prefix, err := io.ReadAll(io.LimitReader(original, 4096))
	resp.Body = &prefixedResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
	if err != nil {
		return false
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(prefix, &body) != nil {
		return false
	}
	return body.Code == "FST_ERR_CTP_INVALID_CONTENT_LENGTH" || body.Code == "FST_ERR_CTP_INVALID_JSON_BODY"
}

type prefixedResponseBody struct {
	io.Reader
	io.Closer
}
