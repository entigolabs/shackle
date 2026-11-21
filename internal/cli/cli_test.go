package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/execcred"
	"github.com/entigolabs/shackle/internal/storage"
)

// fakeProvider records what it was asked and answers from its fields.
type fakeProvider struct {
	cluster    *cloud.Cluster
	token      *cloud.Token
	registry   *cloud.RegistryCredential
	err        error
	tokenCalls int
	lastToken  cloud.Options
	lastOpts   cloud.Options
}

func (f *fakeProvider) Cluster(_ context.Context, o cloud.Options) (*cloud.Cluster, error) {
	f.lastOpts = o
	return f.cluster, f.err
}

func (f *fakeProvider) Token(_ context.Context, o cloud.Options) (*cloud.Token, error) {
	f.tokenCalls++
	f.lastToken = o
	return f.token, f.err
}

func (f *fakeProvider) RegistryCredential(_ context.Context, o cloud.Options) (*cloud.RegistryCredential, error) {
	f.lastOpts = o
	return f.registry, f.err
}

func newEnv(t *testing.T, p *fakeProvider) (*env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	return &env{
		providers: map[string]cloud.Provider{"aws": p, "oracle": p},
		open: func(context.Context, storage.Location, bool) (storage.Store, error) {
			return nil, errors.New("no store in this test")
		},
		cache:   &execcred.Cache{Dir: t.TempDir(), Now: time.Now},
		getenv:  func(string) string { return "" },
		stdout:  &out,
		stderr:  &errOut,
		version: "1.2.3",
	}, &out, &errOut
}

func TestKubeconfigWritesExecEntry(t *testing.T) {
	p := &fakeProvider{cluster: &cloud.Cluster{Name: "arn:cluster/c", Server: "https://c", CAData: []byte("CA"), TokenArgs: []string{"--cluster", "c", "--region", "r"}}}
	e, _, errOut := newEnv(t, p)
	file := filepath.Join(t.TempDir(), "config")
	// Flags after the cloud, as people type them.
	if code := e.run(context.Background(), []string{"kubeconfig", "aws", "--cluster", "c", "--region", "r", "--kubeconfig", file}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if p.lastOpts.Cluster != "c" || p.lastOpts.Region != "r" {
		t.Errorf("options = %+v", p.lastOpts)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"name: arn:cluster/c", "server: https://c", "command: shackle", "- token\n", "- aws\n", "- --cluster\n", "- r\n", "current-context: arn:cluster/c"} {
		if !strings.Contains(s, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, s)
		}
	}
	if p.tokenCalls != 0 {
		t.Error("a token was minted for a kubeconfig that uses the credential plugin")
	}
	if !strings.Contains(errOut.String(), "Added context arn:cluster/c") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestKubeconfigStaticToken(t *testing.T) {
	p := &fakeProvider{
		cluster: &cloud.Cluster{Name: "c", Server: "https://c", TokenArgs: []string{"--cluster", "ocid1.cluster.x", "--region", "resolved"}},
		token:   &cloud.Token{Value: "tok"},
	}
	e, out, errOut := newEnv(t, p)
	if code := e.run(context.Background(), []string{"kubeconfig", "--static-token", "--stdout", "--context", "mine", "oracle", "--cluster", "by-name", "--compartment", "comp"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// The token is asked with what the cluster lookup resolved, not with
	// what the user typed.
	if p.lastToken.Cluster != "ocid1.cluster.x" || p.lastToken.Region != "resolved" || p.lastToken.Compartment != "comp" {
		t.Errorf("token options = %+v", p.lastToken)
	}
	s := out.String()
	if !strings.Contains(s, "token: tok") || !strings.Contains(s, "name: mine") || strings.Contains(s, "exec") {
		t.Errorf("kubeconfig:\n%s", s)
	}
}

func TestKubeconfigStaticUser(t *testing.T) {
	p := &fakeProvider{cluster: &cloud.Cluster{Name: "c", Server: "https://c", StaticUser: map[string]any{"client-key-data": "a2V5"}}}
	e, out, _ := newEnv(t, p)
	if code := e.run(context.Background(), []string{"kubeconfig", "aws", "--cluster", "c", "--stdout"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "client-key-data: a2V5") || strings.Contains(out.String(), "exec") {
		t.Errorf("kubeconfig:\n%s", out)
	}
}

func TestTokenPrintsExecCredentialAndCaches(t *testing.T) {
	p := &fakeProvider{token: &cloud.Token{Value: "tok", Expiry: time.Now().Add(10 * time.Minute)}}
	e, out, errOut := newEnv(t, p)
	args := []string{"token", "aws", "--cluster", "c", "--region", "r"}
	for i := 0; i < 3; i++ {
		out.Reset()
		if code := e.run(context.Background(), args); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		var cred struct {
			Kind   string `json:"kind"`
			Status struct {
				Token string `json:"token"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out.Bytes(), &cred); err != nil || cred.Kind != "ExecCredential" || cred.Status.Token != "tok" {
			t.Fatalf("output = %s (%v)", out, err)
		}
	}
	if p.tokenCalls != 1 {
		t.Errorf("the cloud was asked %d times, want once", p.tokenCalls)
	}
	// Another cluster does not get the cached token.
	_ = e.run(context.Background(), []string{"token", "aws", "--cluster", "other", "--region", "r"})
	if p.tokenCalls != 2 {
		t.Errorf("another cluster was served from the cache")
	}
	_ = e.run(context.Background(), append(args, "--no-cache"))
	if p.tokenCalls != 3 {
		t.Errorf("--no-cache was served from the cache")
	}
	out.Reset()
	_ = e.run(context.Background(), append(args, "--raw"))
	if out.String() != "tok\n" {
		t.Errorf("--raw printed %q", out)
	}
}

func TestRegistryToken(t *testing.T) {
	p := &fakeProvider{registry: &cloud.RegistryCredential{Registry: "r.example", Username: "AWS", Password: "pw"}}
	e, out, _ := newEnv(t, p)
	if code := e.run(context.Background(), []string{"registry-token", "aws", "--region", "eu"}); code != 0 || out.String() != "pw\n" {
		t.Errorf("exit %d, output %q", code, out)
	}
	if p.lastOpts.Region != "eu" {
		t.Errorf("options = %+v", p.lastOpts)
	}
	out.Reset()
	_ = e.run(context.Background(), []string{"registry-token", "aws", "--output", "json"})
	var cred map[string]any
	if err := json.Unmarshal(out.Bytes(), &cred); err != nil || cred["username"] != "AWS" || cred["registry"] != "r.example" {
		t.Errorf("json = %s (%v)", out, err)
	}
	if _, has := cred["expiresAt"]; has {
		t.Error("an unknown expiry was printed")
	}
	if code := e.run(context.Background(), []string{"registry-token", "aws", "--output", "xml"}); code != 2 {
		t.Errorf("unknown output: exit %d, want 2", code)
	}
}

func TestExitCodes(t *testing.T) {
	p := &fakeProvider{err: errors.New("cloud says no")}
	e, out, errOut := newEnv(t, p)
	tests := []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "Usage:"},
		{[]string{"nonsense"}, 2, "unknown command"},
		{[]string{"kubeconfig", "aws"}, 2, "no cluster"},
		{[]string{"kubeconfig", "--cluster", "c"}, 2, "expected one cloud"},
		{[]string{"kubeconfig", "mars", "--cluster", "c"}, 2, "unknown cloud"},
		{[]string{"kubeconfig", "aws", "--cluster", "c", "--bogus"}, 2, "bogus"},
		{[]string{"kubeconfig", "aws", "--cluster", "c", "--stdout"}, 1, "cloud says no"},
		{[]string{"token", "aws", "--cluster", "c"}, 1, "cloud says no"},
		{[]string{"cp", "only-one"}, 2, "SOURCE and DESTINATION"},
		{[]string{"cp", "/a", "/b"}, 1, "object storage location"},
		{[]string{"kubeconfig", "--help"}, 0, "Flags:"},
	}
	for _, tt := range tests {
		out.Reset()
		errOut.Reset()
		if code := e.run(context.Background(), tt.args); code != tt.code {
			t.Errorf("%v: exit %d, want %d (%s)", tt.args, code, tt.code, errOut)
		}
		if !strings.Contains(errOut.String(), tt.want) {
			t.Errorf("%v: stderr %q lacks %q", tt.args, errOut, tt.want)
		}
	}
	out.Reset()
	if code := e.run(context.Background(), []string{"version"}); code != 0 || out.String() != "1.2.3\n" {
		t.Errorf("version: exit %d, output %q", code, out)
	}
}

func TestCpPassesOptions(t *testing.T) {
	e, _, errOut := newEnv(t, &fakeProvider{})
	var gotLoc storage.Location
	var gotPathStyle bool
	e.open = func(_ context.Context, loc storage.Location, pathStyle bool) (storage.Store, error) {
		gotLoc, gotPathStyle = loc, pathStyle
		return nil, errors.New("stop here")
	}
	code := e.run(context.Background(), []string{"cp", "s3://bucket/steps/a", t.TempDir(), "--recursive", "--exclude", "*.terraform/*", "--s3-path-style"})
	if code != 1 || !strings.Contains(errOut.String(), "stop here") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if gotLoc.Bucket != "bucket" || gotLoc.Key != "steps/a" || !gotPathStyle {
		t.Errorf("store opened with %+v pathStyle=%v", gotLoc, gotPathStyle)
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a := fs.String("a", "", "")
	b := fs.Bool("b", false, "")
	positional, err := parse(fs, []string{"one", "--a", "1", "two", "--b", "three"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(positional, ",") != "one,two,three" || *a != "1" || !*b {
		t.Errorf("positional %v a=%q b=%v", positional, *a, *b)
	}
}

func TestSyncPassesOptions(t *testing.T) {
	e, _, errOut := newEnv(t, &fakeProvider{})
	var gotLoc storage.Location
	e.open = func(_ context.Context, loc storage.Location, _ bool) (storage.Store, error) {
		gotLoc = loc
		return nil, errors.New("stop here")
	}
	code := e.run(context.Background(), []string{"sync", t.TempDir(), "az://acct/tfstate/steps/x/.terraform", "--delete"})
	if code != 1 || !strings.Contains(errOut.String(), "stop here") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if gotLoc.Account != "acct" || gotLoc.Bucket != "tfstate" || gotLoc.Key != "steps/x/.terraform" {
		t.Errorf("store opened with %+v", gotLoc)
	}
	if code := e.run(context.Background(), []string{"sync", "only-one"}); code != 2 {
		t.Errorf("one argument: exit %d, want 2", code)
	}
}
