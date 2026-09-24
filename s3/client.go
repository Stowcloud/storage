package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aws/smithy-go"
)

const (
	dialTimeout            = 10 * time.Second
	tlsHandshakeTimeout    = 10 * time.Second
	responseHeaderTimeout  = 30 * time.Second
	idleConnTimeout        = 90 * time.Second
	metadataRequestTimeout = 30 * time.Second
	maxMetadataBodyBytes   = 1 << 20
	maxListResponseBytes   = 8 << 20
	maxListEntries         = 100_000
	maxObjectKeyBytes      = 1024
	maxRenameEntries       = 10_000
	maxObjectBodyBytes     = 64 << 30
)

var (
	ErrNotFound                  = errors.New("s3: not found")
	ErrDenied                    = errors.New("s3: denied")
	ErrExists                    = errors.New("s3: already exists")
	ErrNotEmpty                  = errors.New("s3: directory not empty")
	ErrDirectTransferUnsupported = errors.New("s3: direct transfer unsupported")
)

func defaultHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: refuseRedirect, Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ForceAttemptHTTP2:     true,
	}}
}

func refuseRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("s3: too many redirects")
	}
	if len(via) == 0 {
		return errors.New("s3: refusing a redirect without an origin request")
	}
	previous := via[len(via)-1]
	if previous.URL.Scheme == "https" && req.URL.Scheme == "http" {
		return fmt.Errorf("s3: refusing an HTTPS to HTTP redirect to %s", req.URL.Host)
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("s3: refusing a redirect from %s to a different host %s", via[0].URL.Host, req.URL.Host)
	}
	return errors.New("s3: refusing an unsupported S3 redirect")
}

func readBounded(r io.Reader, limit int64, what string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("s3: reading %s: %w", what, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("s3: %s exceeds %d bytes", what, limit)
	}
	return body, nil
}

type s3ErrorXML struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func classifyS3Error(status int, body []byte) error {
	var e s3ErrorXML
	code, msg := "", ""
	if len(body) > 0 && xml.Unmarshal(body, &e) == nil {
		code, msg = e.Code, e.Message
	}
	detail := fmt.Sprintf("status %d", status)
	if code != "" {
		detail = fmt.Sprintf("status %d, %s: %s", status, code, msg)
	}
	switch {
	case status == 404 || code == "NoSuchKey" || code == "NoSuchBucket":
		return fmt.Errorf("s3: %s: %w", detail, ErrNotFound)
	case status == 401 || status == 403 || code == "AccessDenied":
		return fmt.Errorf("s3: %s: %w", detail, ErrDenied)
	case status == 409 || code == "BucketAlreadyExists" || code == "BucketAlreadyOwnedByYou":
		return fmt.Errorf("s3: %s: %w", detail, ErrExists)
	default:
		return fmt.Errorf("s3: %s", detail)
	}
}

func sdkError(op string, err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return fmt.Errorf("s3: %s: %w", op, ErrNotFound)
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("s3: %s: %w", op, ErrDenied)
		case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
			return fmt.Errorf("s3: %s: %w", op, ErrExists)
		}
	}
	return fmt.Errorf("s3: %s: %w", op, err)
}
