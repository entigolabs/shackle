// Package aws is the Amazon Web Services provider: EKS, ECR and S3.
package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/storage"
)

// Provider implements cloud.Provider with the SDK's default credential
// chain: environment, shared config, web identity, container and instance
// roles.
type Provider struct{}

func loadConfig(ctx context.Context, region string) (awssdk.Config, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return cfg, err
	}
	if cfg.Region == "" {
		return cfg, errors.New("no region: pass --region or set AWS_REGION")
	}
	return cfg, nil
}

// Cluster describes an EKS cluster. The name is the cluster's ARN, which is
// what `aws eks update-kubeconfig` calls the context.
func (Provider) Cluster(ctx context.Context, o cloud.Options) (*cloud.Cluster, error) {
	cfg, err := loadConfig(ctx, o.Region)
	if err != nil {
		return nil, err
	}
	out, err := eks.NewFromConfig(cfg).DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &o.Cluster})
	if err != nil {
		return nil, fmt.Errorf("describe EKS cluster %s in %s: %w", o.Cluster, cfg.Region, err)
	}
	c := out.Cluster
	if c == nil || c.Endpoint == nil || c.CertificateAuthority == nil || c.CertificateAuthority.Data == nil {
		status := "unknown"
		if c != nil {
			status = string(c.Status)
		}
		return nil, fmt.Errorf("EKS cluster %s has no endpoint yet (status %s)", o.Cluster, status)
	}
	ca, err := base64.StdEncoding.DecodeString(*c.CertificateAuthority.Data)
	if err != nil {
		return nil, fmt.Errorf("EKS cluster %s: certificate authority: %w", o.Cluster, err)
	}
	return &cloud.Cluster{
		Name:      awssdk.ToString(c.Arn),
		Server:    *c.Endpoint,
		CAData:    ca,
		TokenArgs: []string{"--cluster", o.Cluster, "--region", cfg.Region},
	}, nil
}

// Token mints an EKS token.
func (Provider) Token(ctx context.Context, o cloud.Options) (*cloud.Token, error) {
	cfg, err := loadConfig(ctx, o.Region)
	if err != nil {
		return nil, err
	}
	return presignToken(ctx, cfg, o.Cluster, time.Now())
}

const (
	tokenPrefix = "k8s-aws-v1."
	// The API server accepts a token for 15 minutes from the time it was
	// signed; a minute is kept in hand, as aws-iam-authenticator does.
	tokenLifetime = 14 * time.Minute
)

// presignToken builds the token EKS expects: a presigned STS
// GetCallerIdentity URL that carries the cluster name in a signed header.
// The API server replays the request to learn who signed it.
func presignToken(ctx context.Context, cfg awssdk.Config, cluster string, now time.Time) (*cloud.Token, error) {
	if cluster == "" {
		return nil, errors.New("no cluster: pass --cluster")
	}
	client := sts.NewPresignClient(sts.NewFromConfig(cfg))
	req, err := client.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(po *sts.PresignOptions) {
		po.ClientOptions = append(po.ClientOptions, sts.WithAPIOptions(
			smithyhttp.AddHeaderValue("x-k8s-aws-id", cluster),
			// A presigned X-Amz-* header becomes a query parameter.
			smithyhttp.AddHeaderValue("X-Amz-Expires", "60"),
		))
	})
	if err != nil {
		return nil, fmt.Errorf("presign the EKS token: %w", err)
	}
	return &cloud.Token{
		Value:  tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(req.URL)),
		Expiry: now.Add(tokenLifetime),
	}, nil
}

// RegistryCredential returns a login for the account's private ECR registry
// in the region.
func (Provider) RegistryCredential(ctx context.Context, o cloud.Options) (*cloud.RegistryCredential, error) {
	cfg, err := loadConfig(ctx, o.Region)
	if err != nil {
		return nil, err
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("get the ECR authorization token: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return nil, errors.New("ECR returned no authorization token")
	}
	data := out.AuthorizationData[0]
	return decodeECRToken(*data.AuthorizationToken, awssdk.ToString(data.ProxyEndpoint), awssdk.ToTime(data.ExpiresAt))
}

// decodeECRToken splits ECR's base64 "user:password" token.
func decodeECRToken(token, endpoint string, expiry time.Time) (*cloud.RegistryCredential, error) {
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("ECR authorization token: %w", err)
	}
	user, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		return nil, errors.New("ECR authorization token is not user:password")
	}
	return &cloud.RegistryCredential{
		Registry: strings.TrimPrefix(endpoint, "https://"),
		Username: user,
		Password: password,
		Expiry:   expiry,
	}, nil
}

// NewStore opens an S3 bucket. pathStyle addresses the bucket in the path
// instead of the host name, which S3-compatible stores usually need; the
// endpoint of such a store comes from AWS_ENDPOINT_URL_S3.
func NewStore(ctx context.Context, bucket string, pathStyle bool) (storage.Store, error) {
	cfg, err := loadConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = pathStyle })
	return &store{client: client, bucket: bucket}, nil
}

type store struct {
	client *s3.Client
	bucket string
}

func (s *store) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, storage.Object{Key: awssdk.ToString(o.Key), Size: awssdk.ToInt64(o.Size), ModTime: awssdk.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

func (s *store) Download(ctx context.Context, key string, w io.Writer) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	_, err = io.Copy(w, out.Body)
	return err
}

func (s *store) Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: r, ContentLength: &size})
	return err
}

func (s *store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}
