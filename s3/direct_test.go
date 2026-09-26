package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stowcloud/storage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func directResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d", status), Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}
func testDirectRoot(t *testing.T, client *http.Client) *Root {
	t.Helper()
	r := &Root{cfg: Config{Endpoint: "https://objects.example.test", Region: "us-east-1", Bucket: "photos", Prefix: "team", AccessKey: "AKIAEXAMPLE", PathStyle: true}, http: client, endpointScheme: "https", endpointHost: "objects.example.test", scratch: t.TempDir()}
	if err := r.ensureSDK("supersecretkey"); err != nil {
		t.Fatalf("ensureSDK: %v", err)
	}
	return r
}

func TestRedirectPolicyRejectsDowngradeAndUnsupportedS3Redirect(t *testing.T) {
	origin, _ := url.Parse("https://objects.example.test/bucket/source")
	target, _ := url.Parse("http://objects.example.test/bucket/source")
	if err := refuseRedirect(&http.Request{URL: target}, []*http.Request{{URL: origin}}); err == nil {
		t.Fatal("HTTPS to HTTP redirect was accepted")
	}
	target, _ = url.Parse("https://objects.example.test/other-bucket/source")
	if err := refuseRedirect(&http.Request{URL: target}, []*http.Request{{URL: origin}}); err == nil {
		t.Fatal("unsupported same-host S3 redirect was accepted")
	}
}

func TestRequestTargetMapsPathAndVirtualHostStyles(t *testing.T) {
	r := testDirectRoot(t, nil)
	cases := []struct {
		name                    string
		pathStyle               bool
		key, scheme, host, path string
	}{
		{"path object", true, "team/a/b.txt", "https", "objects.example.test", "/photos/team/a/b.txt"},
		{"path bucket", true, "", "https", "objects.example.test", "/photos"},
		{"virtual object", false, "team/a/b.txt", "https", "photos.objects.example.test", "/team/a/b.txt"},
		{"virtual bucket", false, "", "https", "photos.objects.example.test", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r.cfg.PathStyle = tc.pathStyle
			s, h, p := r.requestTarget(tc.key)
			if s != tc.scheme || h != tc.host || p != tc.path {
				t.Fatalf("got %q %q %q", s, h, p)
			}
		})
	}
}

func TestObjectKeyAndDirectoryPrefixIncludeConfiguredPrefix(t *testing.T) {
	r := testDirectRoot(t, nil)
	if got := r.ObjectKey(mustStoragePath(t, "")); got != "team" {
		t.Fatalf("ObjectKey(root)=%q", got)
	}
	if got := r.ObjectKey(mustStoragePath(t, "albums/summer.jpg")); got != "team/albums/summer.jpg" {
		t.Fatalf("ObjectKey(file)=%q", got)
	}
	if got := r.DirectoryPrefix(mustStoragePath(t, "")); got != "team/" {
		t.Fatalf("DirectoryPrefix(root)=%q", got)
	}
	r.cfg.Prefix = ""
	if got := r.DirectoryPrefix(mustStoragePath(t, "")); got != "" {
		t.Fatalf("empty root prefix=%q", got)
	}
}

func mustStoragePath(t *testing.T, s string) storage.Path {
	t.Helper()
	p, err := storage.ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
}

func TestReadBoundedAndClassifyS3Error(t *testing.T) {
	got, err := readBounded(strings.NewReader("12345"), 5, "fixture")
	if err != nil || string(got) != "12345" {
		t.Fatalf("exact=%q,%v", got, err)
	}
	if _, err := readBounded(strings.NewReader("123456"), 5, "fixture"); err == nil {
		t.Fatal("overflow accepted")
	}
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{404, `<Error><Code>Other</Code></Error>`, ErrNotFound},
		{403, `<Error><Code>Other</Code></Error>`, ErrDenied},
		{409, `<Error><Code>Other</Code></Error>`, ErrExists},
		{400, `<Error><Code>NoSuchKey</Code></Error>`, ErrNotFound},
	}
	for _, tc := range cases {
		if err := classifyS3Error(tc.status, []byte(tc.body)); !errors.Is(err, tc.want) {
			t.Fatalf("status %d: %v not %v", tc.status, err, tc.want)
		}
	}
}

func TestPresignedGetAndUploadPartExposeExpectedShape(t *testing.T) {
	r := testDirectRoot(t, nil)
	getURL, err := r.PresignGet(context.Background(), "team/photo.jpg", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(getURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "objects.example.test" || u.Path != "/photos/team/photo.jpg" {
		t.Fatalf("target=%s", u)
	}
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if u.Query().Get(k) == "" {
			t.Fatalf("missing %s", k)
		}
	}
	if strings.Contains(getURL, "supersecretkey") {
		t.Fatal("secret exposed")
	}
	checksum := strings.Repeat("ab", 32)
	partURL, headers, err := r.PresignUploadPart(context.Background(), "team/photo.jpg", "upload-1", 2, 17, checksum, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pu, err := url.Parse(partURL)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Query().Get("partNumber") != "2" || pu.Query().Get("uploadId") != "upload-1" {
		t.Fatalf("query=%s", pu.RawQuery)
	}
	want, _ := checksumHeaderValue(checksum)
	if headers.Get("x-amz-checksum-sha256") != want {
		t.Fatalf("checksum=%q want %q", headers.Get("x-amz-checksum-sha256"), want)
	}
	if !strings.Contains(pu.Query().Get("X-Amz-SignedHeaders"), "x-amz-checksum-sha256") {
		t.Fatal("checksum not signed")
	}
}

func TestMultipartLifecyclePaginationCompleteAndAbort(t *testing.T) {
	var requests []*http.Request
	var bodies []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req)
		body := ""
		if req.Body != nil {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			body = string(b)
		}
		bodies = append(bodies, body)
		switch req.Method {
		case http.MethodPost:
			if req.URL.Query().Get("uploadId") != "" {
				return directResponse(200, `<CompleteMultipartUploadResult><ETag>"final"</ETag></CompleteMultipartUploadResult>`, nil), nil
			}
			return directResponse(200, `<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`, nil), nil
		case http.MethodGet:
			if req.URL.Query().Get("part-number-marker") == "1" {
				return directResponse(200, `<ListPartsResult><IsTruncated>false</IsTruncated><Part><PartNumber>2</PartNumber><ETag>"b"</ETag><Size>20</Size></Part></ListPartsResult>`, nil), nil
			}
			return directResponse(200, `<ListPartsResult><IsTruncated>true</IsTruncated><NextPartNumberMarker>1</NextPartNumberMarker><Part><PartNumber>1</PartNumber><ETag>"a"</ETag><Size>10</Size></Part></ListPartsResult>`, nil), nil
		case http.MethodDelete:
			return directResponse(204, "", nil), nil
		default:
			return nil, fmt.Errorf("unexpected %s", req.Method)
		}
	})}
	r := testDirectRoot(t, client)
	key := "team/photo.jpg"
	id, err := r.BeginMultipart(context.Background(), key, 30, "")
	if err != nil || id != "upload-1" {
		t.Fatalf("begin=%q,%v", id, err)
	}
	parts, err := r.ListParts(context.Background(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].ETag != "a" || parts[1].ETag != "b" {
		t.Fatalf("parts=%+v", parts)
	}
	receipt, err := r.CompleteMultipart(context.Background(), key, id, parts)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ETag != "final" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if err := r.AbortMultipart(context.Background(), key, id); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 {
		t.Fatalf("requests=%d", len(requests))
	}
	if !strings.Contains(bodies[3], `<PartNumber>1</PartNumber>`) || !strings.Contains(bodies[3], `a`) {
		t.Fatalf("complete=%s", bodies[3])
	}
}

func TestSuccessStatusEmbeddedErrorBodyIsNotAcceptedByMultipartCalls(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return directResponse(200, `<Error><Code>AccessDenied</Code><Message>embedded</Message></Error>`, nil), nil
	})}
	r := testDirectRoot(t, client)
	if _, err := r.BeginMultipart(context.Background(), "team/photo.jpg", 1, ""); err == nil {
		t.Fatal("begin accepted embedded error")
	}
	if _, err := r.CompleteMultipart(context.Background(), "team/photo.jpg", "upload-1", []MultipartPart{{PartNumber: 1, ETag: "a", Size: 1}}); err == nil {
		t.Fatal("complete accepted embedded error")
	}
}

func TestSpaceReportsScratchCapacity(t *testing.T) {
	r := testDirectRoot(t, nil)
	s, err := r.Space(context.Background(), storage.RootPath())
	if err != nil {
		t.Fatal(err)
	}
	if s.Total == 0 || s.Free == 0 {
		t.Fatalf("scratch capacity = %+v", s)
	}
}

func TestClassifyS3PreconditionAsExists(t *testing.T) {
	err := classifyS3Error(412, []byte(`<Error><Code>PreconditionFailed</Code></Error>`))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("precondition error = %v, want ErrExists", err)
	}
}
