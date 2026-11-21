package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"
)

func load(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output is not YAML: %v\n%s", err, data)
	}
	return doc
}

func names(doc map[string]any, list string) []string {
	var out []string
	items, _ := doc[list].([]any)
	for _, it := range items {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

func TestMergeNewFile(t *testing.T) {
	out, err := Merge(nil, Entry{Name: "c1", Server: "https://one", CAData: []byte("CA"), User: ExecUser("shackle", []string{"token", "aws", "--cluster", "c1"})}, false)
	if err != nil {
		t.Fatal(err)
	}
	doc := load(t, out)
	if doc["apiVersion"] != "v1" || doc["kind"] != "Config" {
		t.Errorf("not a kubeconfig header: %v %v", doc["apiVersion"], doc["kind"])
	}
	if doc["current-context"] != "c1" {
		t.Errorf("a new kubeconfig gets a current context, got %v", doc["current-context"])
	}
	cluster := doc["clusters"].([]any)[0].(map[string]any)["cluster"].(map[string]any)
	if cluster["server"] != "https://one" {
		t.Errorf("server = %v", cluster["server"])
	}
	// "CA" in base64, the encoding kubectl expects for the field.
	if !strings.Contains(string(out), "certificate-authority-data: Q0E=") {
		t.Errorf("certificate authority is not base64:\n%s", out)
	}
	exec := doc["users"].([]any)[0].(map[string]any)["user"].(map[string]any)["exec"].(map[string]any)
	if exec["command"] != "shackle" || exec["apiVersion"] != "client.authentication.k8s.io/v1" || exec["interactiveMode"] != "Never" {
		t.Errorf("exec = %v", exec)
	}
	if got := exec["args"].([]any); len(got) != 4 || got[1] != "aws" {
		t.Errorf("exec args = %v", got)
	}
}

const existing = `apiVersion: v1
kind: Config
current-context: other
clusters:
- name: other
  cluster:
    server: https://other
- name: c1
  cluster:
    server: https://stale
users:
- name: other
  user:
    token: keep-me
- name: c1
  user:
    token: stale
contexts:
- name: other
  context:
    cluster: other
    user: other
    namespace: kept
extensions:
- name: something
  extension: {a: b}
`

func TestMergeKeepsTheRest(t *testing.T) {
	out, err := Merge([]byte(existing), Entry{Name: "c1", Server: "https://new", User: TokenUser("fresh")}, false)
	if err != nil {
		t.Fatal(err)
	}
	doc := load(t, out)
	if got := names(doc, "clusters"); len(got) != 2 || got[0] != "other" || got[1] != "c1" {
		t.Errorf("clusters = %v, want the entry replaced in place", got)
	}
	if got := names(doc, "contexts"); len(got) != 2 {
		t.Errorf("contexts = %v, want other and c1", got)
	}
	if doc["current-context"] != "other" {
		t.Errorf("current-context = %v, want it left alone", doc["current-context"])
	}
	s := string(out)
	for _, want := range []string{"keep-me", "namespace: kept", "extensions", "https://new", "fresh"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	for _, gone := range []string{"https://stale", "token: stale"} {
		if strings.Contains(s, gone) {
			t.Errorf("output still has %q", gone)
		}
	}
}

func TestMergeSetCurrent(t *testing.T) {
	out, err := Merge([]byte(existing), Entry{Name: "c1", Server: "https://new", User: TokenUser("t")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := load(t, out)["current-context"]; got != "c1" {
		t.Errorf("current-context = %v, want c1", got)
	}
}

func TestMergeRejects(t *testing.T) {
	if _, err := Merge(nil, Entry{Name: "c1"}, false); err == nil {
		t.Error("an entry without a server was accepted")
	}
	if _, err := Merge([]byte("clusters: nope\n"), Entry{Name: "c1", Server: "https://x"}, false); err == nil {
		t.Error("a kubeconfig whose clusters is not a list was accepted")
	}
	if _, err := Merge([]byte("{not yaml"), Entry{Name: "c1", Server: "https://x"}, false); err == nil {
		t.Error("invalid YAML was accepted")
	}
}

func TestFirst(t *testing.T) {
	server, ca, user, err := First([]byte(`clusters:
- name: x
  cluster:
    server: https://api.example
    certificate-authority-data: Q0E=
users:
- name: u
  user:
    exec:
      command: kubelogin
      args: [get-token, --server-id, abc]
`))
	if err != nil {
		t.Fatal(err)
	}
	if server != "https://api.example" || string(ca) != "CA" {
		t.Errorf("server %q ca %q", server, ca)
	}
	if _, ok := user["exec"]; !ok {
		t.Errorf("user = %v", user)
	}
	if _, _, _, err := First([]byte("clusters: []\n")); err == nil {
		t.Error("a kubeconfig without clusters was accepted")
	}
}

func TestPath(t *testing.T) {
	t.Setenv("KUBECONFIG", "/a/one"+string(os.PathListSeparator)+"/a/two")
	if p, _ := Path("/flag"); p != "/flag" {
		t.Errorf("flag: %s", p)
	}
	if p, _ := Path(""); p != "/a/one" {
		t.Errorf("$KUBECONFIG: %s", p)
	}
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", "/home/someone")
	if p, _ := Path(""); p != "/home/someone/.kube/config" {
		t.Errorf("default: %s", p)
	}
}

func TestUpdateConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config")
	var wg sync.WaitGroup
	clusters := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, n := range clusters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(path, Entry{Name: n, Server: "https://" + n, User: TokenUser(n)}, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(load(t, data), "clusters"); len(got) != len(clusters) {
		t.Errorf("clusters after concurrent updates = %v, want all of %v", got, clusters)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Error("the lock file was left behind")
	}
}
