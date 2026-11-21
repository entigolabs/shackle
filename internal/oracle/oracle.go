// Package oracle is the Oracle Cloud Infrastructure provider: OKE and Object
// Storage.
package oracle

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/kubeconfig"
	"github.com/entigolabs/shackle/internal/storage"
)

// Provider implements cloud.Provider. The identity is a resource principal
// when the runtime provides one, an instance principal when
// OCI_CLI_AUTH=instance_principal, a session token when
// OCI_CLI_AUTH=security_token, and otherwise the API key of the OCI
// configuration file (OCI_CLI_CONFIG_FILE, OCI_CLI_PROFILE).
type Provider struct{}

func configProvider() (common.ConfigurationProvider, error) {
	switch {
	case os.Getenv("OCI_RESOURCE_PRINCIPAL_VERSION") != "":
		return auth.ResourcePrincipalConfigurationProvider()
	case os.Getenv("OCI_CLI_AUTH") == "instance_principal":
		return auth.InstancePrincipalConfigurationProvider()
	}
	file := firstNonEmpty(os.Getenv("OCI_CLI_CONFIG_FILE"), os.Getenv("OCI_CONFIG_FILE"))
	profile := firstNonEmpty(os.Getenv("OCI_CLI_PROFILE"), "DEFAULT")
	if os.Getenv("OCI_CLI_AUTH") == "security_token" {
		// A session of `oci session authenticate`: the profile names a
		// token file instead of a user.
		if file == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			file = filepath.Join(home, ".oci", "config")
		}
		return common.CustomProfileSessionTokenConfigProvider(file, profile), nil
	}
	if file != "" {
		return common.CustomProfileConfigProvider(file, profile), nil
	}
	if profile != "DEFAULT" {
		return common.CustomProfileConfigProvider("", profile), nil
	}
	return common.DefaultConfigProvider(), nil
}

func region(o cloud.Options, p common.ConfigurationProvider) (string, error) {
	if r := firstNonEmpty(o.Region, os.Getenv("OCI_REGION")); r != "" {
		return r, nil
	}
	r, err := p.Region()
	if err != nil || r == "" {
		return "", errors.New("no region: pass --region or set OCI_REGION")
	}
	return r, nil
}

func isClusterID(s string) bool { return strings.HasPrefix(s, "ocid1.cluster.") }

// Cluster describes an OKE cluster, given by OCID or by name within a
// compartment. The name of the entry is what was given.
func (Provider) Cluster(ctx context.Context, o cloud.Options) (*cloud.Cluster, error) {
	p, err := configProvider()
	if err != nil {
		return nil, fmt.Errorf("no Oracle credentials: %w", err)
	}
	reg, err := region(o, p)
	if err != nil {
		return nil, err
	}
	client, err := containerengine.NewContainerEngineClientWithConfigurationProvider(p)
	if err != nil {
		return nil, err
	}
	client.SetRegion(reg)
	id := o.Cluster
	if !isClusterID(id) {
		compartment := firstNonEmpty(o.Compartment, os.Getenv("OCI_COMPARTMENT_ID"))
		if compartment == "" {
			return nil, errors.New("a cluster name needs a compartment: pass --compartment or set OCI_COMPARTMENT_ID, or pass the cluster OCID")
		}
		list, err := client.ListClusters(ctx, containerengine.ListClustersRequest{
			CompartmentId:  &compartment,
			Name:           &o.Cluster,
			LifecycleState: []containerengine.ClusterLifecycleStateEnum{containerengine.ClusterLifecycleStateActive},
		})
		if err != nil {
			return nil, fmt.Errorf("list OKE clusters named %s: %w", o.Cluster, err)
		}
		if len(list.Items) == 0 || list.Items[0].Id == nil {
			return nil, fmt.Errorf("no active OKE cluster named %s in the compartment", o.Cluster)
		}
		id = *list.Items[0].Id
	}
	endpoint := containerengine.CreateClusterKubeconfigContentDetailsEndpointPublicEndpoint
	if o.PrivateEndpoint {
		endpoint = containerengine.CreateClusterKubeconfigContentDetailsEndpointPrivateEndpoint
	}
	resp, err := client.CreateKubeconfig(ctx, containerengine.CreateKubeconfigRequest{
		ClusterId: &id,
		CreateClusterKubeconfigContentDetails: containerengine.CreateClusterKubeconfigContentDetails{
			TokenVersion: common.String("2.0.0"),
			Endpoint:     endpoint,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create the kubeconfig of OKE cluster %s: %w", o.Cluster, err)
	}
	defer resp.Content.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Content, 8<<20))
	if err != nil {
		return nil, err
	}
	server, ca, _, err := kubeconfig.First(raw)
	if err != nil {
		return nil, err
	}
	return &cloud.Cluster{
		Name:      o.Cluster,
		Server:    server,
		CAData:    ca,
		TokenArgs: []string{"--cluster", id, "--region", reg},
	}, nil
}

// Token mints an OKE token.
func (Provider) Token(ctx context.Context, o cloud.Options) (*cloud.Token, error) {
	if !isClusterID(o.Cluster) {
		return nil, errors.New("an OKE token needs the cluster OCID in --cluster")
	}
	p, err := configProvider()
	if err != nil {
		return nil, fmt.Errorf("no Oracle credentials: %w", err)
	}
	reg, err := region(o, p)
	if err != nil {
		return nil, err
	}
	return signToken(ctx, common.DefaultRequestSigner(p), reg, o.Cluster, time.Now())
}

// The lifetime `oci ce cluster generate-token` states for its tokens.
const tokenLifetime = 4 * time.Minute

// signToken builds the token OKE expects: the URL of a signed request to the
// cluster's token endpoint, with the signature and its date moved into the
// query string. The API server replays the request to learn who signed it.
func signToken(ctx context.Context, signer common.HTTPRequestSigner, region, clusterID string, now time.Time) (*cloud.Token, error) {
	host := common.StringToRegion(region).Endpoint("containerengine")
	u := fmt.Sprintf("https://%s/cluster_request/%s", host, clusterID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	if err := signer.Sign(req); err != nil {
		return nil, fmt.Errorf("sign the OKE token request: %w", err)
	}
	q := url.Values{
		"authorization": {req.Header.Get("Authorization")},
		"date":          {req.Header.Get("Date")},
	}
	return &cloud.Token{
		Value:  base64.URLEncoding.EncodeToString([]byte(u + "?" + q.Encode())),
		Expiry: now.Add(tokenLifetime),
	}, nil
}

// RegistryCredential is not available: Oracle's registry takes an auth token
// that a user creates by hand, there is none to mint from the identity.
func (Provider) RegistryCredential(context.Context, cloud.Options) (*cloud.RegistryCredential, error) {
	return nil, errors.New("Oracle Cloud has no registry token to mint: its registry takes a user's auth token")
}

// Registries is not available, for the same reason.
func (Provider) Registries(context.Context, cloud.Options) ([]string, error) {
	return nil, errors.New("Oracle Cloud registries are not supported")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// NewStore opens an Object Storage bucket.
func NewStore(_ context.Context, namespace, bucket string) (storage.Store, error) {
	p, err := configProvider()
	if err != nil {
		return nil, fmt.Errorf("no Oracle credentials: %w", err)
	}
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(p)
	if err != nil {
		return nil, err
	}
	if r := os.Getenv("OCI_REGION"); r != "" {
		client.SetRegion(r)
	}
	return &store{client: client, namespace: namespace, bucket: bucket}, nil
}

type store struct {
	client            objectstorage.ObjectStorageClient
	namespace, bucket string
}

func (s *store) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	var start *string
	for {
		resp, err := s.client.ListObjects(ctx, objectstorage.ListObjectsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.bucket,
			Prefix:        &prefix,
			Start:         start,
			Fields:        common.String("name,size,timeModified"),
		})
		if err != nil {
			return nil, fmt.Errorf("list oci://%s@%s/%s: %w", s.bucket, s.namespace, prefix, err)
		}
		for _, o := range resp.Objects {
			if o.Name == nil {
				continue
			}
			var size int64
			if o.Size != nil {
				size = *o.Size
			}
			var modified time.Time
			if o.TimeModified != nil {
				modified = o.TimeModified.Time
			}
			out = append(out, storage.Object{Key: *o.Name, Size: size, ModTime: modified})
		}
		if resp.NextStartWith == nil {
			return out, nil
		}
		start = resp.NextStartWith
	}
}

func (s *store) Download(ctx context.Context, key string, w io.Writer) error {
	resp, err := s.client.GetObject(ctx, objectstorage.GetObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: &key})
	if err != nil {
		return err
	}
	defer resp.Content.Close()
	_, err = io.Copy(w, resp.Content)
	return err
}

func (s *store) Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	_, err := s.client.PutObject(ctx, objectstorage.PutObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		ObjectName:    &key,
		ContentLength: &size,
		PutObjectBody: io.NopCloser(r),
	})
	return err
}

func (s *store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: &key})
	return err
}
