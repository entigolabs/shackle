// Package azure is the Microsoft Azure provider: AKS, ACR and Blob Storage.
// Only the public Azure cloud is supported.
package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/kubeconfig"
	"github.com/entigolabs/shackle/internal/storage"
)

const (
	armScope = "https://management.azure.com/.default"
	// The Entra application every AKS cluster with Entra authentication
	// trusts, unless the cluster was set up with a server application of
	// its own.
	defaultServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"
	// The user name a registry expects with a refresh token.
	acrUsername = "00000000-0000-0000-0000-000000000000"
	apiVersion  = "2024-09-01"
	// API version of Microsoft.ContainerRegistry.
	registryAPIVersion = "2023-07-01"
)

// Provider implements cloud.Provider with DefaultAzureCredential: a service
// principal in AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_CLIENT_SECRET,
// workload identity, or a managed identity.
type Provider struct {
	// ARM is the Resource Manager endpoint; empty means the public cloud.
	ARM string
	// HTTP is the client for the calls made without the SDK.
	HTTP *http.Client
	// Credential replaces DefaultAzureCredential in tests.
	Credential azcore.TokenCredential
}

func (p Provider) arm() string {
	if p.ARM != "" {
		return p.ARM
	}
	return "https://management.azure.com"
}

func (p Provider) http() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p Provider) credential() (azcore.TokenCredential, error) {
	if p.Credential != nil {
		return p.Credential, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("no Azure credentials: %w", err)
	}
	return cred, nil
}

func (p Provider) token(ctx context.Context, scope string) (azcore.AccessToken, error) {
	cred, err := p.credential()
	if err != nil {
		return azcore.AccessToken{}, err
	}
	return cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
}

// Cluster describes an AKS cluster. The name is the cluster's name, which is
// what `az aks get-credentials` calls the context.
func (p Provider) Cluster(ctx context.Context, o cloud.Options) (*cloud.Cluster, error) {
	sub := firstNonEmpty(o.Subscription, os.Getenv("AZURE_SUBSCRIPTION_ID"))
	group := firstNonEmpty(o.ResourceGroup, os.Getenv("AZURE_RESOURCE_GROUP"))
	if sub == "" {
		return nil, errors.New("no subscription: pass --subscription or set AZURE_SUBSCRIPTION_ID")
	}
	if group == "" {
		return nil, errors.New("no resource group: pass --resource-group or set AZURE_RESOURCE_GROUP")
	}
	t, err := p.token(ctx, armScope)
	if err != nil {
		return nil, fmt.Errorf("get a token for Azure Resource Manager: %w", err)
	}
	u := fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s/listClusterUserCredential?api-version=%s",
		p.arm(), url.PathEscape(sub), url.PathEscape(group), url.PathEscape(o.Cluster), apiVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	body, err := do(p.http(), req)
	if err != nil {
		return nil, fmt.Errorf("list the user credentials of AKS cluster %s in %s: %w", o.Cluster, group, err)
	}
	return parseCredentials(body, o)
}

// parseCredentials reads the listClusterUserCredential answer: a kubeconfig
// whose user either asks an Entra plugin for a token or carries the static
// credentials of a cluster with local accounts.
func parseCredentials(body []byte, o cloud.Options) (*cloud.Cluster, error) {
	var res struct {
		Kubeconfigs []struct {
			Value string `json:"value"`
		} `json:"kubeconfigs"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("AKS credentials answer: %w", err)
	}
	if len(res.Kubeconfigs) == 0 {
		return nil, fmt.Errorf("AKS cluster %s returned no kubeconfig", o.Cluster)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Kubeconfigs[0].Value)
	if err != nil {
		return nil, fmt.Errorf("AKS credentials answer: %w", err)
	}
	server, ca, user, err := kubeconfig.First(raw)
	if err != nil {
		return nil, err
	}
	c := &cloud.Cluster{Name: o.Cluster, Server: server, CAData: ca}
	serverID, entra := entraServerID(user)
	if !entra {
		c.StaticUser = user
		return c, nil
	}
	if o.ServerID != "" {
		serverID = o.ServerID
	}
	if serverID != "" && serverID != defaultServerID {
		c.TokenArgs = []string{"--server-id", serverID}
	}
	return c, nil
}

// entraServerID reports whether a kubeconfig user authenticates with Entra
// ID and, if the kubeconfig says so, against which server application.
func entraServerID(user map[string]any) (id string, entra bool) {
	if exec, ok := user["exec"].(map[string]any); ok {
		args, _ := exec["args"].([]any)
		for i, a := range args {
			if a == "--server-id" && i+1 < len(args) {
				id, _ = args[i+1].(string)
			}
		}
		return id, true
	}
	if ap, ok := user["auth-provider"].(map[string]any); ok {
		if cfg, ok := ap["config"].(map[string]any); ok {
			id, _ = cfg["apiserver-id"].(string)
		}
		return id, true
	}
	return "", false
}

// Token returns an Entra ID token for the cluster's server application.
func (p Provider) Token(ctx context.Context, o cloud.Options) (*cloud.Token, error) {
	t, err := p.token(ctx, firstNonEmpty(o.ServerID, defaultServerID)+"/.default")
	if err != nil {
		return nil, fmt.Errorf("get a token for the AKS cluster: %w", err)
	}
	return &cloud.Token{Value: t.Token, Expiry: t.ExpiresOn}, nil
}

// RegistryCredential exchanges the identity's Entra token for a refresh
// token of the registry, which is what `az acr login --expose-token` prints.
func (p Provider) RegistryCredential(ctx context.Context, o cloud.Options) (*cloud.RegistryCredential, error) {
	t, err := p.token(ctx, armScope)
	if err != nil {
		return nil, fmt.Errorf("get a token for Azure Resource Manager: %w", err)
	}
	if o.Registry, err = p.loginServer(ctx, t.Token, o); err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"access_token"}, "service": {o.Registry}, "access_token": {t.Token}}
	if tenant := os.Getenv("AZURE_TENANT_ID"); tenant != "" {
		form.Set("tenant", tenant)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.registryURL(o.Registry)+"/oauth2/exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err := do(p.http(), req)
	if err != nil {
		return nil, fmt.Errorf("exchange a token with %s: %w", o.Registry, err)
	}
	var res struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.RefreshToken == "" {
		return nil, fmt.Errorf("%s returned no refresh token", o.Registry)
	}
	return &cloud.RegistryCredential{Registry: o.Registry, Username: acrUsername, Password: res.RefreshToken}, nil
}

// loginServer is the host of the registry to log in to. A host name is taken
// as it is. A bare registry name is looked up in the subscription, and with
// no registry at all the resource group must hold exactly one.
func (p Provider) loginServer(ctx context.Context, armToken string, o cloud.Options) (string, error) {
	if strings.Contains(o.Registry, ".") || strings.Contains(o.Registry, "://") {
		return o.Registry, nil
	}
	sub := firstNonEmpty(o.Subscription, os.Getenv("AZURE_SUBSCRIPTION_ID"))
	group := firstNonEmpty(o.ResourceGroup, os.Getenv("AZURE_RESOURCE_GROUP"))
	if sub == "" {
		return "", errors.New("no subscription: pass --subscription or set AZURE_SUBSCRIPTION_ID, or pass --registry <name>.azurecr.io")
	}
	scope := "/subscriptions/" + url.PathEscape(sub)
	if o.Registry == "" {
		if group == "" {
			return "", errors.New("no registry: pass --registry, or --resource-group to use the one registry in it")
		}
		scope += "/resourceGroups/" + url.PathEscape(group)
	}
	next := p.arm() + scope + "/providers/Microsoft.ContainerRegistry/registries?api-version=" + registryAPIVersion
	var servers []string
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+armToken)
		body, err := do(p.http(), req)
		if err != nil {
			return "", fmt.Errorf("list the container registries: %w", err)
		}
		var page struct {
			Value []struct {
				Name       string `json:"name"`
				Properties struct {
					LoginServer string `json:"loginServer"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return "", fmt.Errorf("container registries answer: %w", err)
		}
		for _, r := range page.Value {
			if o.Registry == "" || strings.EqualFold(r.Name, o.Registry) {
				servers = append(servers, r.Properties.LoginServer)
			}
		}
		next = page.NextLink
	}
	switch {
	case len(servers) == 1:
		return servers[0], nil
	case o.Registry != "":
		return "", fmt.Errorf("no container registry named %s in the subscription", o.Registry)
	case len(servers) == 0:
		return "", fmt.Errorf("no container registry in resource group %s", group)
	default:
		return "", fmt.Errorf("several container registries in resource group %s, pass --registry: %s", group, strings.Join(servers, ", "))
	}
}

// registryURL is the registry's base URL; a test server's URL is kept as is.
func (p Provider) registryURL(registry string) string {
	if strings.Contains(registry, "://") {
		return registry
	}
	return "https://" + registry
}

func do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// NewStore opens a Blob Storage container.
func NewStore(_ context.Context, account, containerName string) (storage.Store, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("no Azure credentials: %w", err)
	}
	client, err := azblob.NewClient("https://"+account+".blob.core.windows.net/", cred, nil)
	if err != nil {
		return nil, err
	}
	return &store{client: client, account: account, container: containerName}, nil
}

type store struct {
	client             *azblob.Client
	account, container string
}

func (s *store) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	var out []storage.Object
	pager := s.client.NewListBlobsFlatPager(s.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list az://%s/%s/%s: %w", s.account, s.container, prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			if b.Name == nil {
				continue
			}
			var size int64
			if b.Properties != nil && b.Properties.ContentLength != nil {
				size = *b.Properties.ContentLength
			}
			var modified time.Time
			if b.Properties != nil && b.Properties.LastModified != nil {
				modified = *b.Properties.LastModified
			}
			out = append(out, storage.Object{Key: *b.Name, Size: size, ModTime: modified})
		}
	}
	return out, nil
}

func (s *store) Download(ctx context.Context, key string, w io.Writer) error {
	resp, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (s *store) Upload(ctx context.Context, key string, r io.ReadSeeker, _ int64) error {
	_, err := s.client.UploadStream(ctx, s.container, key, r, nil)
	return err
}

func (s *store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteBlob(ctx, s.container, key, nil)
	return err
}
