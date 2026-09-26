package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stowcloud/storage"
)

func (r *Root) listObjects(ctx context.Context, prefix, delimiter, token string, maxKeys int) (*listResult, error) {
	if r.client == nil {
		return nil, errors.New("s3: client is not initialized")
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(r.cfg.Bucket), Prefix: aws.String(prefix)}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	if token != "" {
		in.ContinuationToken = aws.String(token)
	}
	if maxKeys >= 0 {
		n := int32(maxKeys)
		in.MaxKeys = &n
	}
	out, err := r.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, sdkError("list objects", err)
	}
	result := &listResult{IsTruncated: aws.ToBool(out.IsTruncated), NextContinuationToken: aws.ToString(out.NextContinuationToken)}
	for _, o := range out.Contents {
		size := aws.ToInt64(o.Size)
		if size < 0 {
			return nil, errors.New("s3: list returned a negative object size")
		}
		lm := ""
		if o.LastModified != nil {
			lm = o.LastModified.Format(time.RFC3339)
		}
		result.Contents = append(result.Contents, listObject{Key: aws.ToString(o.Key), LastModified: lm, Size: uint64(size), ETag: aws.ToString(o.ETag)})
	}
	for _, p := range out.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, aws.ToString(p.Prefix))
	}
	if len(result.Contents)+len(result.CommonPrefixes) > maxListEntries {
		return nil, errors.New("s3: list response carries too many entries")
	}
	for _, o := range result.Contents {
		if _, err := listedRemainder(o.Key, prefix); err != nil {
			return nil, err
		}
	}
	for _, p := range result.CommonPrefixes {
		if _, err := listedRemainder(p, prefix); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *Root) headObject(ctx context.Context, key string) (bool, int64, time.Time, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		normalized := sdkError("head object", err)
		if errors.Is(normalized, ErrNotFound) {
			return false, 0, time.Time{}, nil
		}
		return false, 0, time.Time{}, normalized
	}
	size := aws.ToInt64(out.ContentLength)
	if size < 0 {
		return false, 0, time.Time{}, errors.New("s3: head returned a negative object size")
	}
	var mt time.Time
	if out.LastModified != nil {
		mt = *out.LastModified
	}
	return true, size, mt, nil
}

func (r *Root) isDirectory(ctx context.Context, key string) (bool, time.Time, error) {
	prefix := key + "/"
	result, err := r.listObjects(ctx, prefix, "", "", 1)
	if err != nil {
		return false, time.Time{}, err
	}
	for _, c := range result.Contents {
		if c.Key == prefix {
			mt, err := parseListedTime(c.LastModified)
			if err != nil {
				return false, time.Time{}, fmt.Errorf("s3: invalid LastModified for %q: %w", c.Key, err)
			}
			return true, mt, nil
		}
	}
	return len(result.CommonPrefixes) > 0 || len(result.Contents) > 0, time.Time{}, nil
}

func (r *Root) getObjectTo(ctx context.Context, key string, dst io.Writer) (int64, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		return 0, sdkError("get object", err)
	}
	n, copyErr := io.Copy(dst, io.LimitReader(out.Body, maxObjectBodyBytes+1))
	closeErr := out.Body.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, fmt.Errorf("s3: closing object body: %w", closeErr)
	}
	if n > maxObjectBodyBytes {
		return n, fmt.Errorf("s3: object %q exceeds download ceiling", key)
	}
	return n, nil
}

func (r *Root) putReader(ctx context.Context, key string, body io.Reader, size int64) error {
	return r.putReaderConditional(ctx, key, body, size, "")
}

func (r *Root) putReaderConditional(ctx context.Context, key string, body io.Reader, size int64, ifNoneMatch string) error {
	if size < 0 {
		return errors.New("s3: negative object size")
	}
	in := &s3.PutObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size)}
	if ifNoneMatch != "" {
		in.IfNoneMatch = aws.String(ifNoneMatch)
	}
	_, err := r.client.PutObject(ctx, in)
	return sdkError("put object", err)
}
func (r *Root) putEmpty(ctx context.Context, key string) error {
	return r.putReader(ctx, key, bytes.NewReader(nil), 0)
}
func (r *Root) deleteObject(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(key)})
	return sdkError("delete object", err)
}
func (r *Root) copyObject(ctx context.Context, src, dst string) error {
	out, err := r.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(r.cfg.Bucket), Key: aws.String(dst), CopySource: aws.String(r.cfg.Bucket + "/" + src)})
	if err != nil {
		return sdkError("copy object", err)
	}
	if out == nil || out.CopyObjectResult == nil || aws.ToString(out.CopyObjectResult.ETag) == "" {
		return errors.New("s3: copy object returned no result")
	}
	return nil
}

func (r *Root) rename(ctx context.Context, from, to storage.Path, noReplace bool) error {
	st, err := r.Stat(ctx, from)
	if err != nil {
		return fmt.Errorf("s3: rename: %w", err)
	}
	if noReplace {
		if _, err := r.Stat(ctx, to); err == nil {
			return fmt.Errorf("s3: rename: %w", ErrExists)
		} else if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("s3: rename: %w", err)
		}
	}
	if st.Kind == storage.KindDirectory {
		return r.renameDir(ctx, from, to)
	}
	if err := r.copyObject(ctx, r.ObjectKey(from), r.ObjectKey(to)); err != nil {
		return fmt.Errorf("s3: rename: %w", err)
	}
	if err := r.deleteObject(ctx, r.ObjectKey(from)); err != nil {
		return fmt.Errorf("s3: rename: %w", err)
	}
	return nil
}
func (r *Root) renameDir(ctx context.Context, from, to storage.Path) error {
	fromPrefix, toPrefix := r.DirectoryPrefix(from), r.DirectoryPrefix(to)
	keys, err := r.listAll(ctx, fromPrefix)
	if err != nil {
		return fmt.Errorf("s3: rename: %w", err)
	}
	for _, key := range keys {
		if err := r.copyObject(ctx, key, toPrefix+strings.TrimPrefix(key, fromPrefix)); err != nil {
			return fmt.Errorf("s3: rename: %w", err)
		}
	}
	for _, key := range keys {
		if err := r.deleteObject(ctx, key); err != nil {
			return fmt.Errorf("s3: rename: %w", err)
		}
	}
	return nil
}
func (r *Root) listAll(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		result, err := r.listObjects(ctx, prefix, "", token, 1000)
		if err != nil {
			return nil, err
		}
		for _, o := range result.Contents {
			keys = append(keys, o.Key)
			if len(keys) > maxRenameEntries {
				return nil, fmt.Errorf("more than %d entries under %q", maxRenameEntries, prefix)
			}
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return keys, nil
		}
		token = result.NextContinuationToken
	}
}
