package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Vectors from the AWS SigV4 test suite (service "service", us-east-1).
func TestSignPublishedVectors(t *testing.T) {
	s := signer{awsKeys{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "service"}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, tc := range []struct{ name, method, url, want string }{
		{"get-vanilla", "GET", "https://example.amazonaws.com/",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"post-vanilla", "POST", "https://example.amazonaws.com/",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"},
		{"query order", "GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.Sign(req, emptyPayload, at)
			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestPresignShape(t *testing.T) {
	s := signer{awsKeys{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}, "us-east-1", "sts"}
	u, _ := url.Parse("https://sts.us-east-1.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15")
	got := s.Presign("GET", u, http.Header{"X-K8s-Aws-Id": {"of-local"}}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), time.Minute)
	for _, p := range []string{
		"Action=GetCallerIdentity", "X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKID%2F20260102%2Fus-east-1%2Fsts%2Faws4_request", "X-Amz-Date=20260102T030405Z",
		"X-Amz-Expires=60", "X-Amz-Security-Token=tok", "X-Amz-SignedHeaders=host%3Bx-k8s-aws-id", "X-Amz-Signature=",
	} {
		if !strings.Contains(got, p) {
			t.Errorf("presigned URL %s lacks %s", got, p)
		}
	}
	if strings.Contains(got, "secret") {
		t.Error("presigned URL has the secret key")
	}
}
