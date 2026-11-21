package aws

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestPresignToken(t *testing.T) {
	cfg := awssdk.Config{
		Region:      "eu-north-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "session"),
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tok, err := presignToken(context.Background(), cfg, "my-cluster", now)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Expiry.Equal(now.Add(14 * time.Minute)) {
		t.Errorf("expiry = %v", tok.Expiry)
	}
	encoded, ok := strings.CutPrefix(tok.Value, "k8s-aws-v1.")
	if !ok {
		t.Fatalf("token has no k8s-aws-v1. prefix: %s", tok.Value)
	}
	if strings.ContainsAny(encoded, "=+/") {
		t.Errorf("token is not unpadded URL-safe base64: %s", encoded)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "sts.eu-north-1.amazonaws.com" {
		t.Errorf("host = %s, want the regional STS endpoint", u.Host)
	}
	q := u.Query()
	if q.Get("Action") != "GetCallerIdentity" {
		t.Errorf("Action = %q", q.Get("Action"))
	}
	if q.Get("X-Amz-Expires") != "60" {
		t.Errorf("X-Amz-Expires = %q, want 60 in the query string", q.Get("X-Amz-Expires"))
	}
	// The cluster name must be covered by the signature, or a token for one
	// cluster would be good for every cluster.
	signed := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	found := false
	for _, h := range signed {
		if h == "x-k8s-aws-id" {
			found = true
		}
	}
	if !found {
		t.Errorf("x-k8s-aws-id is not a signed header: %v", signed)
	}
	if q.Get("X-Amz-Security-Token") != "session" {
		t.Error("the session token is missing from the URL")
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("the URL is not signed")
	}
}

func TestPresignTokenNeedsCluster(t *testing.T) {
	cfg := awssdk.Config{Region: "eu-north-1", Credentials: credentials.NewStaticCredentialsProvider("a", "b", "")}
	if _, err := presignToken(context.Background(), cfg, "", time.Now()); err == nil {
		t.Error("a token without a cluster name was minted")
	}
}

func TestDecodeECRToken(t *testing.T) {
	expiry := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	cred, err := decodeECRToken(base64.StdEncoding.EncodeToString([]byte("AWS:pass:with:colons")), "https://123456789012.dkr.ecr.eu-north-1.amazonaws.com", expiry)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "AWS" || cred.Password != "pass:with:colons" {
		t.Errorf("credential = %+v", cred)
	}
	if cred.Registry != "123456789012.dkr.ecr.eu-north-1.amazonaws.com" {
		t.Errorf("registry = %s", cred.Registry)
	}
	if _, err := decodeECRToken("not base64!", "", expiry); err == nil {
		t.Error("an undecodable token was accepted")
	}
	if _, err := decodeECRToken(base64.StdEncoding.EncodeToString([]byte("nocolon")), "", expiry); err == nil {
		t.Error("a token without a password was accepted")
	}
}

func TestECRHost(t *testing.T) {
	if got := ecrHost("123456789012", "eu-north-1", "arn:aws:sts::123456789012:assumed-role/r/s"); got != "123456789012.dkr.ecr.eu-north-1.amazonaws.com" {
		t.Errorf("ecrHost = %s", got)
	}
	if got := ecrHost("123456789012", "cn-north-1", "arn:aws-cn:iam::123456789012:user/u"); got != "123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn" {
		t.Errorf("ecrHost in China = %s", got)
	}
}
