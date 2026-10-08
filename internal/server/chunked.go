package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// isAWSChunked reports whether the body uses the aws-chunked framing SDKs send for streaming uploads
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD, STREAMING-UNSIGNED-PAYLOAD-TRAILER, ...). The Java and Go SDKs pick
// it on plain-HTTP endpoints when they attach a trailing checksum, so a local store sees it constantly.
func isAWSChunked(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return true
	}
	for _, enc := range strings.Split(r.Header.Get("Content-Encoding"), ",") {
		if strings.TrimSpace(enc) == "aws-chunked" {
			return true
		}
	}
	return false
}

// storedContentEncoding drops the transport-only aws-chunked token from Content-Encoding.
func storedContentEncoding(v string) string {
	var keep []string
	for _, enc := range strings.Split(v, ",") {
		if enc = strings.TrimSpace(enc); enc != "" && enc != "aws-chunked" {
			keep = append(keep, enc)
		}
	}
	return strings.Join(keep, ", ")
}

// chunkedReader decodes `<hex-size>[;chunk-signature=...]\r\n<data>\r\n ... 0[;...]\r\n[trailers]\r\n`.
// Chunk signatures and trailing checksums are not verified.
type chunkedReader struct {
	r         *bufio.Reader
	remaining int64
	done      bool
}

func newChunkedReader(r io.Reader) *chunkedReader {
	return &chunkedReader{r: bufio.NewReader(r)}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.remaining == 0 {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("%w: reading chunk header: %v", errIncompleteBody, err)
		}
		sizeHex, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil || size < 0 {
			return 0, fmt.Errorf("%w: bad chunk size %q", errIncompleteBody, sizeHex)
		}
		if size == 0 {
			c.done = true
			// Drain trailers up to the blank line; the payload is complete either way.
			for {
				l, err := c.r.ReadString('\n')
				if err != nil || strings.TrimRight(l, "\r\n") == "" {
					break
				}
			}
			return 0, io.EOF
		}
		c.remaining = size
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining == 0 && err == nil {
		if _, err := c.r.Discard(2); err != nil { // CRLF after the chunk data
			return n, fmt.Errorf("%w: missing chunk terminator: %v", errIncompleteBody, err)
		}
	}
	if err == io.EOF && !c.done {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
