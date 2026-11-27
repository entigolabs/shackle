package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/entigolabs/shackle/internal/cloud"
)

// fakeCredential hands out a token that names the scope it was asked for.
type fakeCredential struct{ scopes []string }

func (f *fakeCredential) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.scopes = append(f.scopes, o.Scopes...)
	return azcore.AccessToken{Token: "token-for-" + o.Scopes[0], ExpiresOn: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func credentialsAnswer(kubeconfig string) string {
	b, _ := json.Marshal(map[string]any{"kubeconfigs": []map[string]string{{"name": "clusterUser", "value": base64.StdEncoding.EncodeToString([]byte(kubeconfig))}}})
	return string(b)
}

const entraKubeconfig = `clusters:
- name: c
  cluster:
    server: https://c.hcp.example.azmk8s.io:443
    certificate-authority-data: Q0E=
users:
- name: u
  user:
    exec:
      command: kubelogin
      args: [get-token, --environment, AzurePublicCloud, --server-id, SERVERID, --client-id, x, --tenant-id, y, --login, devicecode]
`

const localKubeconfig = `clusters:
- name: c
  cluster:
    server: https://c.hcp.example.azmk8s.io:443
    certificate-authority-data: Q0E=
users:
- name: u
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
    token: static
`

func TestCluster(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth, gotMethod = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Method
		_, _ = io.WriteString(w, credentialsAnswer(strings.Replace(entraKubeconfig, "SERVERID", defaultServerID, 1)))
	}))
	defer srv.Close()
	cred := &fakeCredential{}
	p := Provider{ARM: srv.URL, HTTP: srv.Client(), Credential: cred}
	c, err := p.Cluster(context.Background(), cloud.Options{Cluster: "my-aks", Subscription: "sub", ResourceGroup: "rg"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/my-aks/listClusterUserCredential" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.HasPrefix(gotQuery, "api-version=") {
		t.Errorf("query = %s", gotQuery)
	}
	if gotAuth != "Bearer token-for-"+armScope {
		t.Errorf("Authorization = %s", gotAuth)
	}
	if c.Name != "my-aks" || c.Server != "https://c.hcp.example.azmk8s.io:443" || string(c.CAData) != "CA" {
		t.Errorf("cluster = %+v", c)
	}
	if c.StaticUser != nil || len(c.TokenArgs) != 0 {
		t.Errorf("an Entra cluster on the default server application needs no token flags and no static user: %+v", c)
	}
}

func TestClusterNeedsSubscriptionAndGroup(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	p := Provider{Credential: &fakeCredential{}}
	if _, err := p.Cluster(context.Background(), cloud.Options{Cluster: "c", ResourceGroup: "rg"}); err == nil {
		t.Error("no subscription was accepted")
	}
	if _, err := p.Cluster(context.Background(), cloud.Options{Cluster: "c", Subscription: "sub"}); err == nil {
		t.Error("no resource group was accepted")
	}
}

func TestClusterReportsAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"AuthorizationFailed"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	p := Provider{ARM: srv.URL, HTTP: srv.Client(), Credential: &fakeCredential{}}
	_, err := p.Cluster(context.Background(), cloud.Options{Cluster: "c", Subscription: "s", ResourceGroup: "g"})
	if err == nil || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("err = %v, want the API's reason", err)
	}
	if cloud.IsTransient(err) {
		t.Error("a refusal is not worth another try")
	}
}

func TestClusterMarksBusyAnswersAsTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "busy", status)
		}))
		p := Provider{ARM: srv.URL, HTTP: srv.Client(), Credential: &fakeCredential{}}
		_, err := p.Cluster(context.Background(), cloud.Options{Cluster: "c", Subscription: "s", ResourceGroup: "g"})
		srv.Close()
		if !cloud.IsTransient(err) {
			t.Errorf("status %d: err = %v, want it marked as worth another try", status, err)
		}
	}
}

func TestParseCredentials(t *testing.T) {
	// A cluster with a server application of its own: the token must be
	// asked for that application.
	c, err := parseCredentials([]byte(credentialsAnswer(strings.Replace(entraKubeconfig, "SERVERID", "custom-app", 1))), cloud.Options{Cluster: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TokenArgs) != 2 || c.TokenArgs[0] != "--server-id" || c.TokenArgs[1] != "custom-app" {
		t.Errorf("TokenArgs = %v", c.TokenArgs)
	}
	// The flag wins over what the kubeconfig says.
	c, _ = parseCredentials([]byte(credentialsAnswer(entraKubeconfig)), cloud.Options{Cluster: "c", ServerID: "from-flag"})
	if len(c.TokenArgs) != 2 || c.TokenArgs[1] != "from-flag" {
		t.Errorf("TokenArgs = %v", c.TokenArgs)
	}
	// Local accounts: the credentials are kept as they are.
	c, err = parseCredentials([]byte(credentialsAnswer(localKubeconfig)), cloud.Options{Cluster: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if c.StaticUser["token"] != "static" || c.StaticUser["client-key-data"] != "a2V5" {
		t.Errorf("StaticUser = %v", c.StaticUser)
	}
	for _, bad := range []string{`{"kubeconfigs":[]}`, `not json`, `{"kubeconfigs":[{"value":"!!!"}]}`} {
		if _, err := parseCredentials([]byte(bad), cloud.Options{Cluster: "c"}); err == nil {
			t.Errorf("answer %q was accepted", bad)
		}
	}
}

func TestEntraServerID(t *testing.T) {
	legacy := map[string]any{"auth-provider": map[string]any{"name": "azure", "config": map[string]any{"apiserver-id": "legacy-app"}}}
	if id, entra := entraServerID(legacy); !entra || id != "legacy-app" {
		t.Errorf("auth-provider: %q %v", id, entra)
	}
	if _, entra := entraServerID(map[string]any{"token": "x"}); entra {
		t.Error("a static user was taken for an Entra user")
	}
}

func TestToken(t *testing.T) {
	cred := &fakeCredential{}
	p := Provider{Credential: cred}
	tok, err := p.Token(context.Background(), cloud.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "token-for-"+defaultServerID+"/.default" || tok.Expiry.IsZero() {
		t.Errorf("token = %+v", tok)
	}
	tok, _ = p.Token(context.Background(), cloud.Options{ServerID: "custom"})
	if tok.Value != "token-for-custom/.default" {
		t.Errorf("token = %+v", tok)
	}
}

func TestRegistryCredential(t *testing.T) {
	t.Setenv("AZURE_TENANT_ID", "tenant")
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/exchange" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = io.WriteString(w, `{"refresh_token":"refresh"}`)
	}))
	defer srv.Close()
	p := Provider{HTTP: srv.Client(), Credential: &fakeCredential{}}
	cred, err := p.RegistryCredential(context.Background(), cloud.Options{Registry: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != acrUsername || cred.Password != "refresh" {
		t.Errorf("credential = %+v", cred)
	}
	if form.Get("grant_type") != "access_token" || form.Get("access_token") != "token-for-"+armScope || form.Get("tenant") != "tenant" || form.Get("service") != srv.URL {
		t.Errorf("exchange form = %v", form)
	}
	if _, err := p.RegistryCredential(context.Background(), cloud.Options{}); err == nil {
		t.Error("no registry was accepted")
	}
}

func TestRegistryLookup(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/oauth2/exchange":
			_, _ = io.WriteString(w, `{"refresh_token":"refresh"}`)
		case strings.Contains(r.URL.Path, "/resourceGroups/one/"):
			_, _ = io.WriteString(w, `{"value":[{"name":"MyReg","properties":{"loginServer":"`+r.Host+`.azurecr.io"}}]}`)
		case strings.Contains(r.URL.Path, "/resourceGroups/two/"):
			_, _ = io.WriteString(w, `{"value":[{"name":"a","properties":{"loginServer":"a.azurecr.io"}},{"name":"b","properties":{"loginServer":"b.azurecr.io"}}]}`)
		case strings.Contains(r.URL.Path, "/resourceGroups/none/"):
			_, _ = io.WriteString(w, `{"value":[]}`)
		default: // the whole subscription
			_, _ = io.WriteString(w, `{"value":[{"name":"other","properties":{"loginServer":"other.azurecr.io"}},{"name":"MyReg","properties":{"loginServer":"myreg-abc.azurecr.io"}}]}`)
		}
	}))
	defer srv.Close()
	p := Provider{ARM: srv.URL, HTTP: srv.Client(), Credential: &fakeCredential{}}
	ctx := context.Background()

	// A bare name is found in the subscription, whatever its case, and the
	// login server is what the API says, not a guess from the name.
	got, err := p.loginServer(ctx, "t", cloud.Options{Registry: "myreg", Subscription: "sub"})
	if err != nil || got != "myreg-abc.azurecr.io" {
		t.Errorf("by name: %q %v", got, err)
	}
	if last := paths[len(paths)-1]; last != "/subscriptions/sub/providers/Microsoft.ContainerRegistry/registries" {
		t.Errorf("looked up at %s", last)
	}
	// No registry: the one in the resource group.
	got, err = p.loginServer(ctx, "t", cloud.Options{Subscription: "sub", ResourceGroup: "one"})
	if err != nil || !strings.HasSuffix(got, ".azurecr.io") {
		t.Errorf("the one in the group: %q %v", got, err)
	}
	if _, err := p.loginServer(ctx, "t", cloud.Options{Subscription: "sub", ResourceGroup: "two"}); err == nil || !strings.Contains(err.Error(), "a.azurecr.io, b.azurecr.io") {
		t.Errorf("several registries: %v", err)
	}
	if _, err := p.loginServer(ctx, "t", cloud.Options{Subscription: "sub", ResourceGroup: "none"}); err == nil {
		t.Error("an empty resource group gave a registry")
	}
	// Listing is not choosing: none and several are both fine.
	if list, err := p.Registries(ctx, cloud.Options{Subscription: "sub", ResourceGroup: "none"}); err != nil || len(list) != 0 {
		t.Errorf("Registries of an empty group = %v %v", list, err)
	}
	if list, err := p.Registries(ctx, cloud.Options{Subscription: "sub", ResourceGroup: "two"}); err != nil || len(list) != 2 {
		t.Errorf("Registries of a group with two = %v %v", list, err)
	}
	if _, err := p.loginServer(ctx, "t", cloud.Options{Registry: "absent", Subscription: "sub"}); err == nil {
		t.Error("an unknown registry name was found")
	}
	// A host name needs no lookup and no subscription.
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	if got, err := p.loginServer(ctx, "t", cloud.Options{Registry: "x.azurecr.io"}); err != nil || got != "x.azurecr.io" {
		t.Errorf("host name: %q %v", got, err)
	}
	if _, err := p.loginServer(ctx, "t", cloud.Options{}); err == nil {
		t.Error("no registry, no subscription and no group was accepted")
	}
}
