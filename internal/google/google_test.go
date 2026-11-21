package google

import (
	"encoding/base64"
	"testing"

	container "google.golang.org/api/container/v1"

	"github.com/entigolabs/shackle/internal/cloud"
)

func TestNames(t *testing.T) {
	if got := clusterPath("proj", "europe-north1", "c"); got != "projects/proj/locations/europe-north1/clusters/c" {
		t.Errorf("clusterPath = %s", got)
	}
	if got := contextName("proj", "europe-north1-a", "c"); got != "gke_proj_europe-north1-a_c" {
		t.Errorf("contextName = %s", got)
	}
}

func TestProjectAndLocationDefaults(t *testing.T) {
	t.Setenv("GOOGLE_PROJECT", "from-env")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "other")
	t.Setenv("GOOGLE_REGION", "")
	t.Setenv("GOOGLE_ZONE", "zone-a")
	if got := project(cloud.Options{}); got != "from-env" {
		t.Errorf("project = %s", got)
	}
	if got := project(cloud.Options{Project: "flag"}); got != "flag" {
		t.Errorf("project = %s, want the flag to win", got)
	}
	if got := location(cloud.Options{}); got != "zone-a" {
		t.Errorf("location = %s", got)
	}
}

func TestFromAPI(t *testing.T) {
	api := &container.Cluster{
		Endpoint:             "203.0.113.10",
		MasterAuth:           &container.MasterAuth{ClusterCaCertificate: base64.StdEncoding.EncodeToString([]byte("CA"))},
		PrivateClusterConfig: &container.PrivateClusterConfig{PrivateEndpoint: "10.0.0.2"},
	}
	c, err := fromAPI(api, "proj", "region", cloud.Options{Cluster: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "https://203.0.113.10" || string(c.CAData) != "CA" || c.Name != "gke_proj_region_c" {
		t.Errorf("cluster = %+v", c)
	}
	if len(c.TokenArgs) != 0 {
		t.Errorf("TokenArgs = %v, want none: the token is not bound to a cluster", c.TokenArgs)
	}
	c, err = fromAPI(api, "proj", "region", cloud.Options{Cluster: "c", PrivateEndpoint: true})
	if err != nil || c.Server != "https://10.0.0.2" {
		t.Errorf("private endpoint: %+v %v", c, err)
	}
	api.PrivateClusterConfig = nil
	if _, err := fromAPI(api, "proj", "region", cloud.Options{Cluster: "c", PrivateEndpoint: true}); err == nil {
		t.Error("a private endpoint was invented for a public cluster")
	}
	if _, err := fromAPI(&container.Cluster{Status: "PROVISIONING"}, "proj", "region", cloud.Options{Cluster: "c"}); err == nil {
		t.Error("a cluster without an endpoint was accepted")
	}
}
