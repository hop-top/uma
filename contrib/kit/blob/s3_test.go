package blob_test

import (
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	kits3 "hop.top/kit/go/storage/blob/s3"

	"hop.top/uma/contrib/kit/blob"
	"hop.top/uma/ports"
)

// The S3 backend plugs in through an *s3.Client the host builds. Building
// one does no I/O; no request is made here.
func TestS3BackendCompiles(t *testing.T) {
	client := awss3.New(awss3.Options{Region: "us-east-1"})
	var b ports.BlobStore
	b, err := blob.New(kits3.New(client, "uma-bucket", "attachments"))
	if err != nil || b == nil {
		t.Fatalf("New over s3 = %v, %v", b, err)
	}
}
