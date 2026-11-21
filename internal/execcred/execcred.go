// Package execcred speaks the client-go credential plugin protocol: it
// prints a token as an ExecCredential and keeps tokens in a small cache, so
// that a script that runs kubectl a hundred times asks the cloud once.
package execcred

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entigolabs/shackle/internal/cloud"
)

const defaultAPIVersion = "client.authentication.k8s.io/v1"

// minRemaining is how much lifetime a cached token must have left to be
// handed out again.
const minRemaining = 60 * time.Second

// Encode renders the token as the ExecCredential kubectl expects. execInfo
// is the content of $KUBERNETES_EXEC_INFO; when it names an API version the
// answer uses the same one, as the protocol requires.
func Encode(t cloud.Token, execInfo string) ([]byte, error) {
	apiVersion := defaultAPIVersion
	var info struct {
		APIVersion string `json:"apiVersion"`
	}
	if json.Unmarshal([]byte(execInfo), &info) == nil && strings.HasPrefix(info.APIVersion, "client.authentication.k8s.io/") {
		apiVersion = info.APIVersion
	}
	status := map[string]any{"token": t.Value}
	if !t.Expiry.IsZero() {
		status["expirationTimestamp"] = t.Expiry.UTC().Format(time.RFC3339)
	}
	return json.Marshal(map[string]any{
		"apiVersion": apiVersion,
		"kind":       "ExecCredential",
		"status":     status,
	})
}

// Cache keeps tokens in files only the user can read.
type Cache struct {
	Dir string
	Now func() time.Time
}

// DefaultCache is the cache in $SHACKLE_CACHE_DIR or the user's cache
// directory. It returns nil when there is no place for one; a nil cache
// stores nothing.
func DefaultCache() *Cache {
	dir := os.Getenv("SHACKLE_CACHE_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(base, "shackle")
	}
	return &Cache{Dir: dir, Now: time.Now}
}

type entry struct {
	Token  string    `json:"token"`
	Expiry time.Time `json:"expiry"`
}

func (c *Cache) file(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:])+".json")
}

// Get returns the token stored under key if it is still good for a while.
func (c *Cache) Get(key string) (cloud.Token, bool) {
	if c == nil {
		return cloud.Token{}, false
	}
	data, err := os.ReadFile(c.file(key))
	if err != nil {
		return cloud.Token{}, false
	}
	var e entry
	if json.Unmarshal(data, &e) != nil || e.Token == "" || e.Expiry.Sub(c.Now()) < minRemaining {
		return cloud.Token{}, false
	}
	return cloud.Token{Value: e.Token, Expiry: e.Expiry}, true
}

// Put stores the token under key. A token without an expiry is not stored,
// and a failure to store is not an error: the next call asks the cloud again.
func (c *Cache) Put(key string, t cloud.Token) {
	if c == nil || t.Expiry.IsZero() {
		return
	}
	if os.MkdirAll(c.Dir, 0o700) != nil {
		return
	}
	data, err := json.Marshal(entry{Token: t.Value, Expiry: t.Expiry})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(c.Dir, ".token-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if tmp.Chmod(0o600) != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if tmp.Close() != nil {
		return
	}
	_ = os.Rename(tmp.Name(), c.file(key))
}

// identityEnv are the variables that select which cloud identity a token is
// for. They are part of the cache key, so that two identities on one machine
// never get each other's tokens.
var identityEnv = []string{
	"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG",
	"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
	"OCI_CLI_PROFILE", "OCI_CLI_CONFIG_FILE", "OCI_CONFIG_FILE", "OCI_CLI_AUTH", "OCI_RESOURCE_PRINCIPAL_VERSION",
}

// Key builds a cache key from the cloud, the token flags and the identity
// the environment selects.
func Key(cloudName string, args []string, getenv func(string) string) string {
	var b strings.Builder
	b.WriteString(cloudName)
	for _, a := range args {
		b.WriteString("\x00")
		b.WriteString(a)
	}
	for _, name := range identityEnv {
		b.WriteString("\x00")
		b.WriteString(name)
		b.WriteString("=")
		b.WriteString(getenv(name))
	}
	return b.String()
}
