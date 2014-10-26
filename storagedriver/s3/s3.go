package s3

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/s3"
	"github.com/docker/docker-registry/storagedriver"
)

/* Chunks need to be at least 5MB to store with a multipart upload on S3 */
const minChunkSize = uint64(5 * 1024 * 1024)

/* The largest amount of parts you can request from S3 */
const listPartsMax = 1000

type S3Driver struct {
	S3      *s3.S3
	Bucket  *s3.Bucket
	Encrypt bool
}

func NewDriver(accessKey string, secretKey string, region aws.Region, encrypt bool, bucketName string) (*S3Driver, error) {
	auth := aws.Auth{AccessKey: accessKey, SecretKey: secretKey}
	s3obj := s3.New(auth, region)
	bucket := s3obj.Bucket(bucketName)

	if err := bucket.PutBucket(getPermissions()); err != nil {
		s3Err, ok := err.(*s3.Error)
		if !(ok && s3Err.Code == "BucketAlreadyOwnedByYou") {
			return nil, err
		}
	}

	return &S3Driver{s3obj, bucket, encrypt}, nil
}

func (d *S3Driver) GetContent(path string) ([]byte, error) {
	return d.Bucket.Get(path)
}

func (d *S3Driver) PutContent(path string, contents []byte) error {
	return d.Bucket.Put(path, contents, d.getContentType(), getPermissions(), d.getOptions())
}

func (d *S3Driver) ReadStream(path string, offset uint64) (io.ReadCloser, error) {
	headers := make(http.Header)
	headers.Add("Range", "bytes="+strconv.FormatUint(offset, 10)+"-")

	resp, err := d.Bucket.GetResponseWithHeaders(path, headers)
	if resp != nil {
		return resp.Body, err
	}

	return nil, err
}

func (d *S3Driver) WriteStream(path string, offset, size uint64, reader io.ReadCloser) error {
	defer reader.Close()

	chunkSize := minChunkSize
	for size/chunkSize >= listPartsMax {
		chunkSize *= 2
	}

	partNumber := 1
	totalRead := uint64(0)
	multi, parts, err := d.getAllParts(path)
	if err != nil {
		return err
	}

	if (offset) > uint64(len(parts))*chunkSize || (offset < size && offset%chunkSize != 0) {
		return storagedriver.InvalidOffsetError{path, offset}
	}

	if len(parts) > 0 {
		partNumber = int(offset/chunkSize) + 1
		totalRead = offset
		parts = parts[0 : partNumber-1]
	}

	buf := make([]byte, chunkSize)
	for {
		bytesRead, err := io.ReadFull(reader, buf)
		totalRead += uint64(bytesRead)

		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		} else if (uint64(bytesRead) < chunkSize) && totalRead != size {
			break
		} else {
			part, err := multi.PutPart(int(partNumber), bytes.NewReader(buf[0:bytesRead]))
			if err != nil {

				return err
			}

			parts = append(parts, part)
			if totalRead == size {
				multi.Complete(parts)
				break
			}

			partNumber++
		}
	}

	return nil
}

func (d *S3Driver) ResumeWritePosition(path string) (uint64, error) {
	_, parts, err := d.getAllParts(path)
	if err != nil {
		return 0, err
	}

	if len(parts) == 0 {
		return 0, nil
	}

	return (((uint64(len(parts)) - 1) * uint64(parts[0].Size)) + uint64(parts[len(parts)-1].Size)), nil
}

func (d *S3Driver) List(prefix string) ([]string, error) {
	listResponse, err := d.Bucket.List(prefix+"/", "/", "", listPartsMax)
	if err != nil {
		return nil, err
	}

	files := []string{}
	directories := []string{}

	for len(listResponse.Contents) > 0 || len(listResponse.CommonPrefixes) > 0 {
		for _, key := range listResponse.Contents {
			files = append(files, key.Key)
		}

		for _, commonPrefix := range listResponse.CommonPrefixes {
			directories = append(directories, commonPrefix[0:len(commonPrefix)-1])
		}

		lastFile := ""
		lastDirectory := ""
		lastMarker := ""

		if len(files) > 0 {
			lastFile = files[len(files)-1]
		}

		if len(directories) > 0 {
			lastDirectory = directories[len(directories)-1] + "/"
		}

		if lastDirectory > lastFile {
			lastMarker = lastDirectory
		} else {
			lastMarker = lastFile
		}

		listResponse, err = d.Bucket.List(prefix+"/", "/", lastMarker, listPartsMax)
		if err != nil {
			return nil, err
		}
	}

	return append(files, directories...), nil
}

func (d *S3Driver) Move(sourcePath string, destPath string) error {
	/* This is terrible, but aws doesn't have an actual move. */
	_, err := d.Bucket.PutCopy(destPath, getPermissions(), s3.CopyOptions{d.getOptions(), "", d.getContentType()}, d.Bucket.Name+"/"+sourcePath)
	if err != nil {
		return err
	}

	return d.Delete(sourcePath)
}

func (d *S3Driver) Delete(path string) error {
	listResponse, err := d.Bucket.List(path, "", "", listPartsMax)
	if err != nil || len(listResponse.Contents) == 0 {
		return storagedriver.PathNotFoundError{path}
	}

	s3Objects := make([]s3.Object, listPartsMax)

	for len(listResponse.Contents) > 0 {
		for index, key := range listResponse.Contents {
			s3Objects[index].Key = key.Key
		}

		err := d.Bucket.DelMulti(s3.Delete{false, s3Objects[0:len(listResponse.Contents)]})
		if err != nil {
			return nil
		}

		listResponse, err = d.Bucket.List(path, "", "", listPartsMax)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *S3Driver) getHighestIdMulti(path string) (multi *s3.Multi, err error) {
	multis, _, err := d.Bucket.ListMulti(path, "")
	if err != nil && !hasCode(err, "NoSuchUpload") {
		return nil, err
	}

	uploadId := ""

	if len(multis) > 0 {
		for _, m := range multis {
			if m.Key == path && m.UploadId >= uploadId {
				uploadId = m.UploadId
				multi = m
			}
		}
		return multi, nil
	} else {
		multi, err := d.Bucket.InitMulti(path, d.getContentType(), getPermissions(), d.getOptions())
		return multi, err
	}
}

func (d *S3Driver) getAllParts(path string) (*s3.Multi, []s3.Part, error) {
	multi, err := d.getHighestIdMulti(path)
	if err != nil {
		return nil, nil, err
	}

	parts, err := multi.ListParts()
	return multi, parts, err
}

func hasCode(err error, code string) bool {
	s3err, ok := err.(*aws.Error)
	return ok && s3err.Code == code
}

func (d *S3Driver) getOptions() s3.Options {
	return s3.Options{SSE: d.Encrypt}
}

func getPermissions() s3.ACL {
	return s3.Private
}

func (d *S3Driver) getContentType() string {
	return "application/octet-stream"
}
