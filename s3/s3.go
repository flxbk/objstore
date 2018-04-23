// Package s3 implements common object storage abstractions against s3-compatible APIs.
package s3

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/minio/minio-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	opObjectsList  = "ListBucket"
	opObjectInsert = "PutObject"
	opObjectGet    = "GetObject"
	opObjectStat   = "StatObject"
	opObjectDelete = "DeleteObject"
)

// DirDelim is the delimiter used to model a directory structure in an object store bucket.
const DirDelim = "/"

// Bucket implements the store.Bucket interface against s3-compatible APIs.
type Bucket struct {
	bucket   string
	client   *minio.Client
	opsTotal *prometheus.CounterVec
}

// Config encapsulates the necessary config values to instantiate an s3 client.
type Config struct {
	Bucket      string
	Endpoint    string
	AccessKey   string
	SecretKey   string
	Insecure    bool
	SignatureV2 bool
}

// RegisterS3Params registers the s3 flags and returns an initialized Config struct.
func RegisterS3Params(cmd *kingpin.CmdClause) *Config {
	var s3config Config

	cmd.Flag("s3.bucket", "S3-Compatible API bucket name for stored blocks.").
		PlaceHolder("<bucket>").Envar("S3_BUCKET").StringVar(&s3config.Bucket)

	cmd.Flag("s3.endpoint", "S3-Compatible API endpoint for stored blocks.").
		PlaceHolder("<api-url>").Envar("S3_ENDPOINT").StringVar(&s3config.Endpoint)

	cmd.Flag("s3.access-key", "Access key for an S3-Compatible API.").
		PlaceHolder("<key>").Envar("S3_ACCESS_KEY").StringVar(&s3config.AccessKey)

	s3config.SecretKey = os.Getenv("S3_SECRET_KEY")

	cmd.Flag("s3.insecure", "Whether to use an insecure connection with an S3-Compatible API.").
		Default("false").Envar("S3_INSECURE").BoolVar(&s3config.Insecure)

	cmd.Flag("s3.signature-version2", "Whether to use S3 Signature Version 2; otherwise Signature Version 4 will be used.").
		Default("false").Envar("S3_SIGNATURE_VERSION2").BoolVar(&s3config.SignatureV2)

	return &s3config
}

// Validate checks to see if any of the s3 config options are set.
func (conf *Config) Validate() error {
	if conf.Bucket == "" ||
		conf.Endpoint == "" ||
		conf.AccessKey == "" ||
		conf.SecretKey == "" {
		return errors.New("insufficient s3 configuration information")
	}
	return nil
}

// NewBucket returns a new Bucket using the provided s3 config values.
func NewBucket(conf *Config, reg prometheus.Registerer, component string) (*Bucket, error) {
	var f func(string, string, string, bool) (*minio.Client, error)
	if conf.SignatureV2 {
		f = minio.NewV2
	} else {
		f = minio.NewV4
	}

	client, err := f(conf.Endpoint, conf.AccessKey, conf.SecretKey, !conf.Insecure)
	client.SetAppInfo(fmt.Sprintf("thanos-%s", component), fmt.Sprintf("%s (%s)", version.Version, runtime.Version()))
	if err != nil {
		return nil, errors.Wrap(err, "initialize s3 client")
	}

	bkt := &Bucket{
		bucket: conf.Bucket,
		client: client,
		opsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "thanos_objstore_s3_bucket_operations_total",
			Help:        "Total number of operations that were executed against an s3 bucket.",
			ConstLabels: prometheus.Labels{"bucket": conf.Bucket},
		}, []string{"operation"}),
	}
	if reg != nil {
		reg.MustRegister(bkt.opsTotal)
	}
	return bkt, nil
}

// Iter calls f for each entry in the given directory. The argument to f is the full
// object name including the prefix of the inspected directory.
func (b *Bucket) Iter(ctx context.Context, dir string, f func(string) error) error {
	b.opsTotal.WithLabelValues(opObjectsList).Inc()
	// Ensure the object name actually ends with a dir suffix. Otherwise we'll just iterate the
	// object itself as one prefix item.
	if dir != "" {
		dir = strings.TrimSuffix(dir, DirDelim) + DirDelim
	}

	for object := range b.client.ListObjects(b.bucket, dir, false, ctx.Done()) {
		// this sometimes happens with empty buckets
		if object.Key == "" {
			continue
		}
		if err := f(object.Key); err != nil {
			return err
		}
	}

	return nil
}

// Get returns a reader for the given object name.
func (b *Bucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	b.opsTotal.WithLabelValues(opObjectGet).Inc()
	return b.client.GetObjectWithContext(ctx, b.bucket, name, minio.GetObjectOptions{})
}

// GetRange returns a new range reader for the given object name and range.
func (b *Bucket) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	b.opsTotal.WithLabelValues(opObjectGet).Inc()
	opts := &minio.GetObjectOptions{}
	err := opts.SetRange(off, off+length)
	if err != nil {
		return nil, err
	}
	return b.client.GetObjectWithContext(ctx, b.bucket, name, *opts)
}

// Exists checks if the given object exists.
func (b *Bucket) Exists(ctx context.Context, name string) (bool, error) {
	b.opsTotal.WithLabelValues(opObjectStat).Inc()
	_, err := b.client.StatObject(b.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		errResponse := minio.ToErrorResponse(err)
		if errResponse.Code == "NoSuchKey" {
			return false, nil
		}
		return false, errors.Wrap(err, "stat s3 object")
	}

	return true, nil
}

// Upload the contents of the reader as an object into the bucket.
func (b *Bucket) Upload(ctx context.Context, name string, r io.Reader) error {
	b.opsTotal.WithLabelValues(opObjectInsert).Inc()
	_, err := b.client.PutObjectWithContext(ctx, b.bucket, name, r, -1, minio.PutObjectOptions{})
	return errors.Wrap(err, "upload s3 object")
}

// Delete removes the object with the given name.
func (b *Bucket) Delete(ctx context.Context, name string) error {
	b.opsTotal.WithLabelValues(opObjectDelete).Inc()
	return b.client.RemoveObject(b.bucket, name)
}
