package s3

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stowcloud/storage"
)

const (
	maxPresignTTL     = 7 * 24 * time.Hour
	maxUploadIDBytes  = 1024
	maxMultipartParts = 10000
)

func validateDirectTTL(t time.Duration) (int, error) {
	if t <= 0 || t > maxPresignTTL {
		return 0, errors.New("s3: invalid presign expiry")
	}
	return int(t / time.Second), nil
}
func (r *Root) validateDirectKey(key string) error {
	if r == nil || !r.DirectTransfer() {
		return ErrDirectTransferUnsupported
	}
	if key == "" || len(key) > maxObjectKeyBytes {
		return errors.New("s3: invalid direct-transfer object key")
	}
	if r.cfg.Prefix != "" && key != r.cfg.Prefix && !strings.HasPrefix(key, r.cfg.Prefix+"/") {
		return errors.New("s3: direct-transfer object key is outside configured prefix")
	}
	if _, err := storage.ParsePath(key); err != nil {
		return err
	}
	return nil
}
func validateUploadID(id string) error {
	if id == "" || len(id) > maxUploadIDBytes {
		return errors.New("s3: invalid multipart upload id")
	}
	return nil
}
func validateChecksum(c string) error {
	if c == "" {
		return nil
	}
	if len(c) != 64 {
		return errors.New("s3: checksum must be a SHA-256 hex digest")
	}
	_, err := hex.DecodeString(c)
	return err
}
func normalizeETag(e string) string {
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(e), "W/"), "\"")
}
func checksumHeaderValue(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	b, err := hex.DecodeString(c)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (r *Root) BeginMultipart(ctx context.Context, key string, size int64, checksum string) (string, error) {
	if err := r.validateDirectKey(key); err != nil {
		return "", err
	}
	if err := validateChecksum(checksum); err != nil {
		return "", err
	}
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key)}
	if checksum != "" {
		in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
	}
	out, err := r.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return "", sdkError("create multipart upload", err)
	}
	if aws.ToString(out.UploadId) == "" {
		return "", errors.New("s3: create multipart upload returned no upload id")
	}
	return aws.ToString(out.UploadId), nil
}
func (r *Root) AbortMultipart(ctx context.Context, key, id string) error {
	if err := r.validateDirectKey(key); err != nil {
		return err
	}
	if err := validateUploadID(id); err != nil {
		return err
	}
	_, err := r.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), UploadId: aws.String(id)})
	return sdkError("abort multipart upload", err)
}
func (r *Root) ListParts(ctx context.Context, key, id string) ([]MultipartPart, error) {
	if err := r.validateDirectKey(key); err != nil {
		return nil, err
	}
	if err := validateUploadID(id); err != nil {
		return nil, err
	}
	pager := s3.NewListPartsPaginator(r.client, &s3.ListPartsInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), UploadId: aws.String(id)})
	out := make([]MultipartPart, 0)
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, sdkError("list multipart parts", err)
		}
		for _, part := range page.Parts {
			size := aws.ToInt64(part.Size)
			if size < 0 {
				return nil, errors.New("s3: multipart part has negative size")
			}
			out = append(out, MultipartPart{PartNumber: int(aws.ToInt32(part.PartNumber)), ETag: normalizeETag(aws.ToString(part.ETag)), Size: size, Checksum: aws.ToString(part.ChecksumSHA256)})
			if len(out) > maxMultipartParts {
				return nil, errors.New("s3: too many multipart parts")
			}
		}
	}
	return out, nil
}
func (r *Root) CompleteMultipart(ctx context.Context, key, id string, parts []MultipartPart) (TransferReceipt, error) {
	if err := r.validateDirectKey(key); err != nil {
		return TransferReceipt{}, err
	}
	if err := validateUploadID(id); err != nil {
		return TransferReceipt{}, err
	}
	if len(parts) == 0 || len(parts) > maxMultipartParts {
		return TransferReceipt{}, errors.New("s3: invalid multipart part list")
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	last := 0
	for _, p := range parts {
		etag := normalizeETag(p.ETag)
		if p.PartNumber <= last || p.PartNumber > maxMultipartParts || etag == "" {
			return TransferReceipt{}, errors.New("s3: invalid multipart part list")
		}
		last = p.PartNumber
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(int32(p.PartNumber)), ETag: aws.String(`"` + etag + `"`)})
	}
	out, err := r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), UploadId: aws.String(id), MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
	if err != nil {
		return TransferReceipt{}, sdkError("complete multipart upload", err)
	}
	if out == nil {
		return TransferReceipt{}, errors.New("s3: complete multipart upload returned no result")
	}
	return TransferReceipt{ETag: normalizeETag(aws.ToString(out.ETag))}, nil
}
func (r *Root) PresignUploadPart(ctx context.Context, key, id string, n int, size int64, checksum string, expiry time.Duration) (string, http.Header, error) {
	if err := r.validateDirectKey(key); err != nil {
		return "", nil, err
	}
	if err := validateUploadID(id); err != nil {
		return "", nil, err
	}
	if n < 1 || n > maxMultipartParts || size < 0 {
		return "", nil, errors.New("s3: invalid multipart part")
	}
	if err := validateChecksum(checksum); err != nil {
		return "", nil, err
	}
	secs, err := validateDirectTTL(expiry)
	if err != nil {
		return "", nil, err
	}
	in := &s3.UploadPartInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), UploadId: aws.String(id), PartNumber: aws.Int32(int32(n)), ContentLength: aws.Int64(size)}
	headers := http.Header{}
	if checksum != "" {
		v, err := checksumHeaderValue(checksum)
		if err != nil {
			return "", nil, err
		}
		in.ChecksumSHA256 = aws.String(v)
		headers.Set("x-amz-checksum-sha256", v)
	}
	out, err := r.presign.PresignUploadPart(ctx, in, func(o *s3.PresignOptions) { o.Expires = time.Duration(secs) * time.Second })
	if err != nil {
		return "", nil, err
	}
	return out.URL, headers, nil
}
func (r *Root) ObjectMetadata(ctx context.Context, key string) (uint64, string, string, bool, error) {
	if err := r.validateDirectKey(key); err != nil {
		return 0, "", "", false, err
	}
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		normalized := sdkError("head object metadata", err)
		if errors.Is(normalized, ErrNotFound) {
			return 0, "", "", false, nil
		}
		return 0, "", "", false, normalized
	}
	size := aws.ToInt64(out.ContentLength)
	if size < 0 {
		return 0, "", "", true, errors.New("s3: invalid object size")
	}
	return uint64(size), normalizeETag(aws.ToString(out.ETag)), aws.ToString(out.ChecksumSHA256), true, nil
}
func (r *Root) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if err := r.validateDirectKey(key); err != nil {
		return "", err
	}
	secs, err := validateDirectTTL(expiry)
	if err != nil {
		return "", err
	}
	out, err := r.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key)}, func(o *s3.PresignOptions) { o.Expires = time.Duration(secs) * time.Second })
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

var _ = strconv.Itoa
var _ = fmt.Sprintf
