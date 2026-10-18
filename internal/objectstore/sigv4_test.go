package objectstore

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Vectors from the AWS SigV4 test suite (service "service", us-east-1) and
// the Amazon S3 SigV4 documentation examples.
func TestSigV4PublishedVectors(t *testing.T) {
	suite := awsCreds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	s3doc := awsCreds{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	suiteTime := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	s3Time := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		signer  sigV4
		method  string
		url     string
		headers map[string]string
		payload string
		at      time.Time
		want    string
	}{
		{
			name:   "aws suite get-vanilla",
			signer: sigV4{suite, "us-east-1", "service"},
			method: "GET", url: "https://example.amazonaws.com/",
			payload: emptyPayloadHash, at: suiteTime,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:   "aws suite post-vanilla",
			signer: sigV4{suite, "us-east-1", "service"},
			method: "POST", url: "https://example.amazonaws.com/",
			payload: emptyPayloadHash, at: suiteTime,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		},
		{
			name:   "aws suite get-vanilla-query-order-key-case",
			signer: sigV4{suite, "us-east-1", "service"},
			method: "GET", url: "https://example.amazonaws.com/?Param2=value2&Param1=value1",
			payload: emptyPayloadHash, at: suiteTime,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			name:   "s3 docs GET object with range",
			signer: sigV4{s3doc, "us-east-1", "s3"},
			method: "GET", url: "https://examplebucket.s3.amazonaws.com/test.txt",
			headers: map[string]string{"Range": "bytes=0-9"},
			payload: emptyPayloadHash, at: s3Time,
			want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "s3 docs list objects",
			signer: sigV4{s3doc, "us-east-1", "s3"},
			method: "GET", url: "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			payload: emptyPayloadHash, at: s3Time,
			want: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			tc.signer.Sign(req, tc.payload, tc.at)
			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// Example from "Authenticating Requests: Using Query Parameters" in the S3
// documentation.
func TestSigV4PresignPublishedVector(t *testing.T) {
	s := sigV4{awsCreds{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}, "us-east-1", "s3"}
	u, _ := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	got := s.Presign("GET", u, nil, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), 86400*time.Second)
	const wantSig = "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.Contains(got, wantSig) {
		t.Fatalf("presigned URL %s\nlacks %s", got, wantSig)
	}
	for _, p := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request",
		"X-Amz-Date=20130524T000000Z", "X-Amz-Expires=86400", "X-Amz-SignedHeaders=host",
	} {
		if !strings.Contains(got, p) {
			t.Errorf("presigned URL lacks %s", p)
		}
	}
}

func TestAWSURIEncode(t *testing.T) {
	for in, want := range map[string]string{
		"a b+c/ü~._-": "a%20b%2Bc/%C3%BC~._-",
		"x=y&z":       "x%3Dy%26z",
	} {
		if got := awsURIEncode(in, true); got != want {
			t.Errorf("awsURIEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := awsURIEncode("a/b", false); got != "a%2Fb" {
		t.Errorf("slash not encoded: %q", got)
	}
}
