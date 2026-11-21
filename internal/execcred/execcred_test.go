package execcred

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entigolabs/shackle/internal/cloud"
)

func TestEncode(t *testing.T) {
	expiry := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	out, err := Encode(cloud.Token{Value: "tok", Expiry: expiry}, "")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Status     struct {
			Token               string `json:"token"`
			ExpirationTimestamp string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != "client.authentication.k8s.io/v1" || got.Kind != "ExecCredential" {
		t.Errorf("header = %s %s", got.APIVersion, got.Kind)
	}
	if got.Status.Token != "tok" || got.Status.ExpirationTimestamp != "2030-01-02T02:04:05Z" {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestEncodeFollowsExecInfo(t *testing.T) {
	out, _ := Encode(cloud.Token{Value: "tok"}, `{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential"}`)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["apiVersion"] != "client.authentication.k8s.io/v1beta1" {
		t.Errorf("apiVersion = %v, want the one kubectl asked for", got["apiVersion"])
	}
	if _, has := got["status"].(map[string]any)["expirationTimestamp"]; has {
		t.Error("a token without an expiry got an expirationTimestamp")
	}
	out, _ = Encode(cloud.Token{Value: "tok"}, `{"apiVersion":"something/else"}`)
	_ = json.Unmarshal(out, &got)
	if got["apiVersion"] != "client.authentication.k8s.io/v1" {
		t.Errorf("apiVersion = %v, want the default for a foreign exec info", got["apiVersion"])
	}
}

func TestCache(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &Cache{Dir: filepath.Join(t.TempDir(), "cache"), Now: func() time.Time { return now }}
	if _, ok := c.Get("k"); ok {
		t.Fatal("an empty cache returned a token")
	}
	c.Put("k", cloud.Token{Value: "tok", Expiry: now.Add(10 * time.Minute)})
	got, ok := c.Get("k")
	if !ok || got.Value != "tok" {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if _, ok := c.Get("other"); ok {
		t.Error("another key returned the token")
	}
	files, _ := os.ReadDir(c.Dir)
	if len(files) != 1 {
		t.Fatalf("cache files = %d, want 1", len(files))
	}
	if info, _ := files[0].Info(); info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode = %v, want 0600", info.Mode().Perm())
	}
	// Close to its expiry the token is not handed out again.
	now = now.Add(9*time.Minute + 30*time.Second)
	if _, ok := c.Get("k"); ok {
		t.Error("a token with 30 seconds left was handed out")
	}
}

func TestCacheSkips(t *testing.T) {
	c := &Cache{Dir: t.TempDir(), Now: time.Now}
	c.Put("k", cloud.Token{Value: "tok"})
	if files, _ := os.ReadDir(c.Dir); len(files) != 0 {
		t.Error("a token without an expiry was stored")
	}
	var none *Cache
	none.Put("k", cloud.Token{Value: "tok", Expiry: time.Now().Add(time.Hour)})
	if _, ok := none.Get("k"); ok {
		t.Error("a nil cache returned a token")
	}
}

func TestKey(t *testing.T) {
	env := map[string]string{"AWS_PROFILE": "one"}
	get := func(k string) string { return env[k] }
	a := Key("aws", []string{"c", "r"}, get)
	if a != Key("aws", []string{"c", "r"}, get) {
		t.Error("the key is not stable")
	}
	if a == Key("aws", []string{"c2", "r"}, get) {
		t.Error("another cluster has the same key")
	}
	if a == Key("aws", []string{"cr", ""}, get) {
		t.Error("the arguments run into each other")
	}
	env["AWS_PROFILE"] = "two"
	if a == Key("aws", []string{"c", "r"}, get) {
		t.Error("another identity has the same key")
	}
}
