package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

// S3Options select the bucket and, for S3 compatible storage, the endpoint.
type S3Options struct {
	Bucket         string
	Prefix         string
	Region         string
	Endpoint       string
	ForcePathStyle bool
}

// S3Store is a Store on S3 or S3 compatible storage. Credentials come from the
// AWS SDK default chain: IRSA, Pod Identity or AWS_* environment variables.
type S3Store struct {
	client *s3.Client
	up     *transfermanager.Client
	bucket string
	prefix string
}

// NewS3Store builds an S3Store.
func NewS3Store(ctx context.Context, o S3Options) (*S3Store, error) {
	if o.Bucket == "" {
		return nil, errors.New("s3 bucket is required")
	}
	var loadOpts []func(*config.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.ForcePathStyle
		// Many S3 compatible servers reject the default CRC checksums of the newer SDK.
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3Store{
		client: client,
		up: transfermanager.New(client, func(o *transfermanager.Options) {
			// Same reason as for the S3 client above.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
		bucket: o.Bucket,
		prefix: strings.Trim(o.Prefix, "/"),
	}, nil
}

func (s *S3Store) key(parts ...string) string {
	k := strings.Join(parts, "/")
	if s.prefix != "" {
		return s.prefix + "/" + k
	}
	return k
}

func (s *S3Store) dataKey(name string) string     { return s.key("snapshots", name, "data.tar.zst") }
func (s *S3Store) manifestKey(name string) string { return s.key("snapshots", name, "manifest.json") }
func (s *S3Store) tagKey(tag string) string       { return s.key("tags", tag) }

func notFound(err error) bool {
	if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
		return true
	}
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

func (s *S3Store) get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *S3Store) getAll(ctx context.Context, key string) ([]byte, error) {
	r, err := s.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

func (s *S3Store) put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.up.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: &s.bucket, Key: &key, Body: r})
	return err
}

func (s *S3Store) Resolve(ctx context.Context, tag string) (string, error) {
	b, err := s.getAll(ctx, s.tagKey(tag))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *S3Store) Manifest(ctx context.Context, name string) (manifest.Manifest, error) {
	b, err := s.getAll(ctx, s.manifestKey(name))
	if err != nil {
		return manifest.Manifest{}, err
	}
	return manifest.Parse(b)
}

func (s *S3Store) Fetch(ctx context.Context, name string) (io.ReadCloser, error) {
	return s.get(ctx, s.dataKey(name))
}

func (s *S3Store) PutData(ctx context.Context, name string, r io.Reader) error {
	return s.put(ctx, s.dataKey(name), r)
}

func (s *S3Store) PutManifest(ctx context.Context, m manifest.Manifest) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	return s.put(ctx, s.manifestKey(m.Name), bytes.NewReader(b))
}

func (s *S3Store) PutTag(ctx context.Context, tag, name string) error {
	return s.put(ctx, s.tagKey(tag), strings.NewReader(name+"\n"))
}

// Delete removes the snapshot objects and the tags that point to it.
func (s *S3Store) Delete(ctx context.Context, name string) error {
	var keys []string
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket, Prefix: aws.String(s.key("snapshots", name) + "/"),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			keys = append(keys, *o.Key)
		}
	}
	tagPager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket, Prefix: aws.String(s.key("tags") + "/"),
	})
	for tagPager.HasMorePages() {
		page, err := tagPager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			b, err := s.getAll(ctx, *o.Key)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			if strings.TrimSpace(string(b)) == name {
				keys = append(keys, *o.Key)
			}
		}
	}
	for _, k := range keys {
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &k}); err != nil {
			return err
		}
	}
	return nil
}
