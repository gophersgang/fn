// Package s3 implements an s3 api compatible log store
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/fnproject/fn/api/common"
	"github.com/fnproject/fn/api/models"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
)

// TODO we should encrypt these, user will have to supply a key though (or all
// OSS users logs will be encrypted with same key unless they change it which
// just seems mean...)

// TODO do we need to use the v2 API? can't find BMC object store docs :/

const (
	// key prefixes
	callKeyPrefix    = "c/"
	callMarkerPrefix = "m/"
	logKeyPrefix     = "l/"
)

type store struct {
	client     *s3.S3
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
	bucket     string
}

// decorator around the Reader interface that keeps track of the number of bytes read
// in order to avoid double buffering and track Reader size
type countingReader struct {
	r     io.Reader
	count int
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
	n, err = cr.r.Read(p)
	cr.count += n
	return n, err
}

func createStore(bucketName, endpoint, region, accessKeyID, secretAccessKey string, useSSL bool) *store {
	config := &aws.Config{
		Credentials:      credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(region),
		DisableSSL:       aws.Bool(!useSSL),
		S3ForcePathStyle: aws.Bool(true),
	}
	client := s3.New(session.Must(session.NewSession(config)))

	return &store{
		client:     client,
		uploader:   s3manager.NewUploaderWithClient(client),
		downloader: s3manager.NewDownloaderWithClient(client),
		bucket:     bucketName,
	}
}

// s3://access_key_id:secret_access_key@host/region/bucket_name?ssl=true
// Note that access_key_id and secret_access_key must be URL encoded if they contain unsafe characters!
func New(u *url.URL) (models.LogStore, error) {
	endpoint := u.Host

	var accessKeyID, secretAccessKey string
	if u.User != nil {
		accessKeyID = u.User.Username()
		secretAccessKey, _ = u.User.Password()
	}
	useSSL := u.Query().Get("ssl") == "true"

	strs := strings.SplitN(u.Path, "/", 3)
	if len(strs) < 3 {
		return nil, errors.New("must provide bucket name and region in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	}
	region := strs[1]
	bucketName := strs[2]
	if region == "" {
		return nil, errors.New("must provide non-empty region in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	} else if bucketName == "" {
		return nil, errors.New("must provide non-empty bucket name in path of s3 api url. e.g. s3://s3.com/us-east-1/my_bucket")
	}

	logrus.WithFields(logrus.Fields{"bucketName": bucketName, "region": region, "endpoint": endpoint, "access_key_id": accessKeyID, "useSSL": useSSL}).Info("checking / creating s3 bucket")
	store := createStore(bucketName, endpoint, region, accessKeyID, secretAccessKey, useSSL)

	// ensure the bucket exists, creating if it does not
	_, err := store.client.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucketName)})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyOwnedByYou, s3.ErrCodeBucketAlreadyExists:
				// bucket already exists, NO-OP
			default:
				return nil, fmt.Errorf("failed to create bucket %s: %s", bucketName, aerr.Message())
			}
		} else {
			return nil, fmt.Errorf("unexpected error creating bucket %s: %s", bucketName, err.Error())
		}
	}

	return store, nil
}

func (s *store) InsertLog(ctx context.Context, appID, callID string, callLog io.Reader) error {
	ctx, span := trace.StartSpan(ctx, "s3_insert_log")
	defer span.End()

	// wrap original reader in a decorator to keep track of read bytes without buffering
	cr := &countingReader{r: callLog}
	objectName := logKey(appID, callID)
	params := &s3manager.UploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(objectName),
		Body:        cr,
		ContentType: aws.String("text/plain"),
	}

	logrus.WithFields(logrus.Fields{"bucketName": s.bucket, "key": objectName}).Debug("Uploading log")
	_, err := s.uploader.UploadWithContext(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to write log, %v", err)
	}

	stats.Record(ctx, uploadSizeMeasure.M(int64(cr.count)))
	return nil
}

func (s *store) GetLog(ctx context.Context, appID, callID string) (io.Reader, error) {
	ctx, span := trace.StartSpan(ctx, "s3_get_log")
	defer span.End()

	objectName := logKey(appID, callID)
	logrus.WithFields(logrus.Fields{"bucketName": s.bucket, "key": objectName}).Debug("Downloading log")

	// stream the logs to an in-memory buffer
	target := &aws.WriteAtBuffer{}
	size, err := s.downloader.DownloadWithContext(ctx, target, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectName),
	})
	if err != nil {
		aerr, ok := err.(awserr.Error)
		if ok && aerr.Code() == s3.ErrCodeNoSuchKey {
			return nil, models.ErrCallLogNotFound
		}
		return nil, fmt.Errorf("failed to read log, %v", err)
	}

	stats.Record(ctx, downloadSizeMeasure.M(size))
	return bytes.NewReader(target.Bytes()), nil
}

func (s *store) InsertCall(ctx context.Context, call *models.Call) error {
	ctx, span := trace.StartSpan(ctx, "s3_insert_call")
	defer span.End()

	byts, err := json.Marshal(call)
	if err != nil {
		return err
	}

	cr := &countingReader{r: bytes.NewReader(byts)}

	objectName := callKey(call.AppID, call.ID)
	fmt.Println("YODAWG", objectName)
	params := &s3manager.UploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(objectName),
		Body:        cr,
		ContentType: aws.String("text/plain"),
	}

	logrus.WithFields(logrus.Fields{"bucketName": s.bucket, "key": objectName}).Debug("Uploading call")
	_, err = s.uploader.UploadWithContext(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to insert call, %v", err)
	}

	// at this point, they can point lookup the log and it will work. now, we can try to upload
	// the marker key. if the marker key upload fails, the user will simply not see this entry
	// when listing AND specifying a route path. (NOTE: this behavior will go away if we stop listing
	// by route -> triggers)

	objectName = callMarkerKey(call.AppID, call.Path, call.ID)
	params = &s3manager.UploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(objectName),
		Body:        bytes.NewReader([]byte{}),
		ContentType: aws.String("text/plain"),
	}

	logrus.WithFields(logrus.Fields{"bucketName": s.bucket, "key": objectName}).Debug("Uploading call")
	_, err = s.uploader.UploadWithContext(ctx, params)
	if err != nil {
		// XXX(reed): we could just log this?
		return fmt.Errorf("failed to write marker key for log, %v", err)
	}

	return nil
}

// GetCall returns a call at a certain id and app name.
func (s *store) GetCall(ctx context.Context, appID, callID string) (*models.Call, error) {
	ctx, span := trace.StartSpan(ctx, "s3_get_call")
	defer span.End()

	objectName := callKey(appID, callID)
	logrus.WithFields(logrus.Fields{"bucketName": s.bucket, "key": objectName}).Debug("Downloading call")

	// stream the logs to an in-memory buffer
	var target aws.WriteAtBuffer
	_, err := s.downloader.DownloadWithContext(ctx, &target, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objectName),
	})
	if err != nil {
		aerr, ok := err.(awserr.Error)
		if ok && aerr.Code() == s3.ErrCodeNoSuchKey {
			return nil, models.ErrCallNotFound
		}
		return nil, fmt.Errorf("failed to read log, %v", err)
	}

	var call models.Call
	err = json.Unmarshal(target.Bytes(), &call)
	if err != nil {
		return nil, err
	}

	return &call, nil
}

func xorCursor(oid string) string {
	// 01C860Z3M9A7WHJ00000000000
	cp := []byte(oid)

	// id are big endian. we need to flip the bytes, they will remain sortable but we'll get a reverse sort.
	for i := range cp[:] {
		cp[i] ^= 0xFF
	}
	return string(cp[:])
}

func callMarkerKey(app, path, id string) string {
	id = xorCursor(id)
	return callMarkerPrefix + app + "/" + path + "/" + id
}

func callKey(app, id string) string {
	id = xorCursor(id)
	return callKeyPrefix + app + "/" + id
}

func logKey(appID, callID string) string {
	return logKeyPrefix + appID + "/" + callID
}

// GetCalls returns a list of calls that satisfy the given CallFilter. If no
// calls exist, an empty list and a nil error are returned.
func (s *store) GetCalls(ctx context.Context, filter *models.CallFilter) ([]*models.Call, error) {
	ctx, span := trace.StartSpan(ctx, "s3_get_calls")
	defer span.End()

	// NOTE:
	// if filter.Path != ""
	//   find marker from marker keys, start there, list keys, get next marker from there
	// else
	//   use marker for keys

	// NOTE we need marker keys to support (app is REQUIRED):
	// 1) quick iteration per path
	// 2) sorted by id across all path
	// marker key: m : {app} : {path} : {id}
	// key: s: {app} : {id}
	//
	// also s3 api is returns sorted in lexicographic order, we need the reverse of this.

	// we need to use first 48 bits of id to approximate created_at, to skip over records
	// if looking far into the past.
	id := ""
	if t := time.Time(filter.FromTime); !t.IsZero() {
		ms := uint64(t.Unix()*1000) + uint64(t.Nanosecond()/int(time.Millisecond))
		var buf [6]byte
		buf[0] = byte(ms >> 40)
		buf[1] = byte(ms >> 32)
		buf[2] = byte(ms >> 24)
		buf[3] = byte(ms >> 16)
		buf[4] = byte(ms >> 8)
		buf[5] = byte(ms)
		id = xorCursor(string(buf[:]))
	}

	prefix := callKey(filter.AppID, id)
	if filter.Path != "" {
		prefix = callMarkerKey(filter.AppID, filter.Path, id)
	}

	// filter.Cursor is a call id, translate to our key format. if a path is
	// provided, we list keys from markers instead.
	var marker string
	if filter.Cursor != "" {
		cursor := xorCursor(filter.Cursor)
		marker = "s:" + filter.AppID + cursor
		if filter.Path != "" {
			marker = "m:" + filter.AppID + ":" + filter.Path + ":" + cursor
		}
	}

	input := &s3.ListObjectsInput{
		Bucket:  aws.String(s.bucket),
		MaxKeys: aws.Int64(int64(filter.PerPage)),
		Marker:  aws.String(marker),
		Prefix:  aws.String(prefix),
	}

	result, err := s.client.ListObjects(input)
	if err != nil {
		return nil, fmt.Errorf("failed to list logs: %v", err)
	}

	// TODO we could add an additional check here to slice to per page if the api doesn't
	// implement the max keys parameter (and we probably should...)
	calls := make([]*models.Call, 0, len(result.Contents))

	for _, obj := range result.Contents {

		var app, id string
		if filter.Path != "" {
			fields := strings.Split(*obj.Key, ":")
			// XXX(reed): validate
			app = fields[1]
			id = fields[3]
		} else {
			fields := strings.Split(*obj.Key, ":")
			// XXX(reed): validate
			app = fields[1]
			id = fields[2]
		}

		// we have to flip the bits back in order to use GetCall directly
		id = xorCursor(id)

		// NOTE: s3 doesn't have a way to get multiple objects so just use GetCall
		// TODO we should reuse the buffer to decode these
		call, err := s.GetCall(ctx, app, id)
		if err != nil {
			common.Logger(ctx).WithError(err).WithFields(logrus.Fields{"app": app, "id": id}).Error("error filling call object")
			continue
		}

		if t := time.Time(filter.ToTime); !t.IsZero() && !time.Time(call.CreatedAt).Before(t) {
			// make sure it fits our time bounds. break, and assume this is it, even
			// though that may not be the case since id and created at are not the same, it's a
			// pretty good approximation
			break
		}

		calls = append(calls, call)
	}

	return calls, nil
}

var (
	uploadSizeMeasure   *stats.Int64Measure
	downloadSizeMeasure *stats.Int64Measure
)

func init() {
	// TODO(reed): do we have to do this? the measurements will be tagged on the context, will they be propagated
	// or we have to white list them in the view for them to show up? test...
	var err error
	appKey, err := tag.NewKey("fn_appname")
	if err != nil {
		logrus.Fatal(err)
	}
	pathKey, err := tag.NewKey("fn_path")
	if err != nil {
		logrus.Fatal(err)
	}

	{
		uploadSizeMeasure, err = stats.Int64("s3_log_upload_size", "uploaded log size", "byte")
		if err != nil {
			logrus.Fatal(err)
		}
		v, err := view.New(
			"s3_log_upload_size",
			"uploaded log size",
			[]tag.Key{appKey, pathKey},
			uploadSizeMeasure,
			view.Distribution(),
		)
		if err != nil {
			logrus.Fatalf("cannot create view: %v", err)
		}
		if err := v.Subscribe(); err != nil {
			logrus.Fatal(err)
		}
	}

	{
		downloadSizeMeasure, err = stats.Int64("s3_log_download_size", "downloaded log size", "byte")
		if err != nil {
			logrus.Fatal(err)
		}
		v, err := view.New(
			"s3_log_download_size",
			"downloaded log size",
			[]tag.Key{appKey, pathKey},
			downloadSizeMeasure,
			view.Distribution(),
		)
		if err != nil {
			logrus.Fatalf("cannot create view: %v", err)
		}
		if err := v.Subscribe(); err != nil {
			logrus.Fatal(err)
		}
	}
}
