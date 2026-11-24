// Package google is the Google Cloud provider: GKE, Artifact Registry and
// Cloud Storage. It uses the REST clients, which keep the binary small.
package google

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/oauth2"
	googleauth "golang.org/x/oauth2/google"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	gcs "google.golang.org/api/storage/v1"

	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/storage"
)

const scope = "https://www.googleapis.com/auth/cloud-platform"

// accessTokenEnv names the variables holding an OAuth access token to use as it
// is: the one the OpenTofu Google provider reads, and gcloud's own.
var accessTokenEnv = []string{"GOOGLE_OAUTH_ACCESS_TOKEN", "CLOUDSDK_AUTH_ACCESS_TOKEN"}

// Provider implements cloud.Provider with an access token from the
// environment (GOOGLE_OAUTH_ACCESS_TOKEN) or Application Default
// Credentials: the key file GOOGLE_APPLICATION_CREDENTIALS names, workload
// identity federation, or the metadata server.
type Provider struct{}

func tokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	for _, name := range accessTokenEnv {
		if t := os.Getenv(name); t != "" {
			// The token's lifetime is not known, so it has no expiry: it is
			// handed on as it is and never cached.
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: t}), nil
		}
	}
	creds, err := googleauth.FindDefaultCredentials(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("no Google credentials: %w", err)
	}
	return creds.TokenSource, nil
}

func project(o cloud.Options) string {
	for _, p := range []string{o.Project, os.Getenv("GOOGLE_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT")} {
		if p != "" {
			return p
		}
	}
	return ""
}

func location(o cloud.Options) string {
	for _, l := range []string{o.Region, os.Getenv("GOOGLE_REGION"), os.Getenv("GOOGLE_ZONE")} {
		if l != "" {
			return l
		}
	}
	return ""
}

// clusterPath is the cluster's resource name.
func clusterPath(project, location, name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, location, name)
}

// contextName is what `gcloud container clusters get-credentials` calls the
// context.
func contextName(project, location, name string) string {
	return fmt.Sprintf("gke_%s_%s_%s", project, location, name)
}

// Cluster describes a GKE cluster.
func (Provider) Cluster(ctx context.Context, o cloud.Options) (*cloud.Cluster, error) {
	proj, loc := project(o), location(o)
	if proj == "" {
		return nil, errors.New("no project: pass --project or set GOOGLE_PROJECT")
	}
	if loc == "" {
		return nil, errors.New("no location: pass --region (a region or a zone) or set GOOGLE_REGION")
	}
	ts, err := tokenSource(ctx)
	if err != nil {
		return nil, err
	}
	svc, err := container.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	c, err := svc.Projects.Locations.Clusters.Get(clusterPath(proj, loc, o.Cluster)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get GKE cluster %s in %s/%s: %w", o.Cluster, proj, loc, err)
	}
	return fromAPI(c, proj, loc, o)
}

func fromAPI(c *container.Cluster, proj, loc string, o cloud.Options) (*cloud.Cluster, error) {
	endpoint := c.Endpoint
	if o.PrivateEndpoint {
		if c.PrivateClusterConfig == nil || c.PrivateClusterConfig.PrivateEndpoint == "" {
			return nil, fmt.Errorf("GKE cluster %s has no private endpoint", o.Cluster)
		}
		endpoint = c.PrivateClusterConfig.PrivateEndpoint
	}
	if endpoint == "" || c.MasterAuth == nil {
		return nil, fmt.Errorf("GKE cluster %s has no endpoint yet (status %s)", o.Cluster, c.Status)
	}
	ca, err := base64.StdEncoding.DecodeString(c.MasterAuth.ClusterCaCertificate)
	if err != nil {
		return nil, fmt.Errorf("GKE cluster %s: certificate authority: %w", o.Cluster, err)
	}
	return &cloud.Cluster{
		Name:   contextName(proj, loc, o.Cluster),
		Server: "https://" + endpoint,
		CAData: ca,
		// A Google access token is not bound to a cluster.
	}, nil
}

// Token returns an OAuth access token, which is what GKE accepts.
func (Provider) Token(ctx context.Context, _ cloud.Options) (*cloud.Token, error) {
	ts, err := tokenSource(ctx)
	if err != nil {
		return nil, err
	}
	t, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("get a Google access token: %w", err)
	}
	return &cloud.Token{Value: t.AccessToken, Expiry: t.Expiry}, nil
}

// RegistryCredential returns a login for Artifact Registry: the access token
// with the fixed user name the registry expects.
func (p Provider) RegistryCredential(ctx context.Context, o cloud.Options) (*cloud.RegistryCredential, error) {
	registries, err := p.Registries(ctx, o)
	if err != nil {
		return nil, err
	}
	registry := registries[0]
	t, err := p.Token(ctx, o)
	if err != nil {
		return nil, err
	}
	return &cloud.RegistryCredential{Registry: registry, Username: "oauth2accesstoken", Password: t.Value, Expiry: t.Expiry}, nil
}

// Registries returns the Artifact Registry host of the region, or the
// registry that was given. It calls nothing.
func (Provider) Registries(_ context.Context, o cloud.Options) ([]string, error) {
	if o.Registry != "" {
		return []string{o.Registry}, nil
	}
	region := firstNonEmpty(o.Region, os.Getenv("GOOGLE_REGION"))
	if region == "" {
		return nil, errors.New("no registry: pass --registry or --region")
	}
	return []string{region + "-docker.pkg.dev"}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// NewStore opens a Cloud Storage bucket.
func NewStore(ctx context.Context, bucket string) (storage.Store, error) {
	ts, err := tokenSource(ctx)
	if err != nil {
		return nil, err
	}
	svc, err := gcs.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	return &store{svc: svc, bucket: bucket}, nil
}

type store struct {
	svc    *gcs.Service
	bucket string
}

func (s *store) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	err := s.svc.Objects.List(s.bucket).Prefix(prefix).Fields("nextPageToken", "items(name,size,updated)").Pages(ctx, func(page *gcs.Objects) error {
		for _, o := range page.Items {
			// A time that does not parse is the zero time: unknown.
			updated, _ := time.Parse(time.RFC3339, o.Updated)
			out = append(out, storage.Object{Key: o.Name, Size: int64(o.Size), ModTime: updated})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list gs://%s/%s: %w", s.bucket, prefix, err)
	}
	return out, nil
}

func (s *store) Download(ctx context.Context, key string, w io.Writer) error {
	resp, err := s.svc.Objects.Get(s.bucket, key).Context(ctx).Download()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (s *store) Upload(ctx context.Context, key string, r io.ReadSeeker, _ int64) error {
	_, err := s.svc.Objects.Insert(s.bucket, &gcs.Object{Name: key}).Media(r).Context(ctx).Do()
	return err
}

func (s *store) Delete(ctx context.Context, key string) error {
	return s.svc.Objects.Delete(s.bucket, key).Context(ctx).Do()
}
