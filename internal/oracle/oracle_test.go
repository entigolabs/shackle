package oracle

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"

	"github.com/entigolabs/shackle/internal/cloud"
)

// keyProvider is an API key identity made up for the test.
type keyProvider struct{ key *rsa.PrivateKey }

func (k keyProvider) PrivateRSAKey() (*rsa.PrivateKey, error) { return k.key, nil }
func (k keyProvider) KeyID() (string, error)                  { return "tenancy/user/fingerprint", nil }

func TestSignToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	const id = "ocid1.cluster.oc1.eu-frankfurt-1.aaaa"
	tok, err := signToken(context.Background(), common.DefaultRequestSigner(keyProvider{key}), "eu-frankfurt-1", id, now)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Expiry.Equal(now.Add(4 * time.Minute)) {
		t.Errorf("expiry = %v", tok.Expiry)
	}
	raw, err := base64.URLEncoding.DecodeString(tok.Value)
	if err != nil {
		t.Fatalf("token is not URL-safe base64: %v", err)
	}
	u, err := url.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "containerengine.eu-frankfurt-1.oraclecloud.com" || u.Path != "/cluster_request/"+id {
		t.Errorf("token URL = %s", u)
	}
	q := u.Query()
	if q.Get("date") != "Wed, 02 Jan 2030 03:04:05 GMT" {
		t.Errorf("date = %q", q.Get("date"))
	}
	authz := q.Get("authorization")
	for _, want := range []string{`Signature version="1"`, `keyId="tenancy/user/fingerprint"`, `algorithm="rsa-sha256"`, `signature="`} {
		if !strings.Contains(authz, want) {
			t.Errorf("authorization lacks %s: %s", want, authz)
		}
	}
	// The server replays a GET with only these headers, so nothing else may
	// be part of the signature.
	if !strings.Contains(authz, `headers="date (request-target) host"`) {
		t.Errorf("signed headers are not date, (request-target) and host: %s", authz)
	}
}

func TestTokenNeedsClusterID(t *testing.T) {
	if _, err := (Provider{}).Token(context.Background(), cloud.Options{Cluster: "a-name"}); err == nil {
		t.Error("a token was minted for a cluster name")
	}
}

func TestIsClusterID(t *testing.T) {
	if !isClusterID("ocid1.cluster.oc1.eu-frankfurt-1.aaaa") || isClusterID("my-cluster") || isClusterID("ocid1.compartment.oc1..aaaa") {
		t.Error("isClusterID tells names and OCIDs apart wrongly")
	}
}

func TestRegistryCredentialIsRefused(t *testing.T) {
	if _, err := (Provider{}).RegistryCredential(context.Background(), cloud.Options{}); err == nil {
		t.Error("a registry credential was returned")
	}
}
