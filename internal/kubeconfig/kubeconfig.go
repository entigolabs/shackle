// Package kubeconfig writes cluster entries into kubeconfig files.
//
// It works on the YAML document as a generic tree instead of typed structs,
// so that whatever else a kubeconfig holds (other clusters, extensions,
// fields of a newer Kubernetes) is kept as it is.
package kubeconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

// Entry is one cluster with the user that reaches it, under one name.
type Entry struct {
	Name   string
	Server string
	CAData []byte
	User   map[string]any
}

// ExecUser is a kubeconfig user that gets its token from a credential plugin.
func ExecUser(command string, args []string) map[string]any {
	a := make([]any, len(args))
	for i, s := range args {
		a[i] = s
	}
	return map[string]any{
		"exec": map[string]any{
			"apiVersion":         "client.authentication.k8s.io/v1",
			"command":            command,
			"args":               a,
			"interactiveMode":    "Never",
			"provideClusterInfo": false,
		},
	}
}

// TokenUser is a kubeconfig user with a fixed token.
func TokenUser(token string) map[string]any {
	return map[string]any{"token": token}
}

// Merge adds the entry to a kubeconfig document, replacing a cluster, user
// and context of the same name, and makes its context the current one when
// setCurrent is true or the document has none. An empty document is a new
// kubeconfig.
func Merge(existing []byte, e Entry, setCurrent bool) ([]byte, error) {
	if e.Name == "" || e.Server == "" {
		return nil, errors.New("kubeconfig entry needs a name and a server")
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("existing kubeconfig is not valid YAML: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if _, ok := doc["apiVersion"]; !ok {
		doc["apiVersion"] = "v1"
	}
	if _, ok := doc["kind"]; !ok {
		doc["kind"] = "Config"
	}
	if _, ok := doc["preferences"]; !ok {
		doc["preferences"] = map[string]any{}
	}
	cluster := map[string]any{"server": e.Server}
	if len(e.CAData) > 0 {
		cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString(e.CAData)
	}
	var err error
	if doc["clusters"], err = upsert(doc["clusters"], "clusters", e.Name, "cluster", cluster); err != nil {
		return nil, err
	}
	if doc["users"], err = upsert(doc["users"], "users", e.Name, "user", e.User); err != nil {
		return nil, err
	}
	context := map[string]any{"cluster": e.Name, "user": e.Name}
	if doc["contexts"], err = upsert(doc["contexts"], "contexts", e.Name, "context", context); err != nil {
		return nil, err
	}
	if cur, _ := doc["current-context"].(string); setCurrent || cur == "" {
		doc["current-context"] = e.Name
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), enc.Close()
}

// upsert replaces the item called name in a kubeconfig list, or appends it.
func upsert(list any, listName, name, key string, value map[string]any) ([]any, error) {
	var items []any
	if list != nil {
		var ok bool
		if items, ok = list.([]any); !ok {
			return nil, fmt.Errorf("existing kubeconfig: %s is not a list", listName)
		}
	}
	item := map[string]any{"name": name, key: value}
	for i, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			items[i] = item
			return items, nil
		}
	}
	return append(items, item), nil
}

// First reads the first cluster and the first user of a kubeconfig that a
// cloud API returned: the API server URL, its certificate authority and the
// user's fields.
func First(data []byte) (server string, ca []byte, user map[string]any, err error) {
	var doc struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
				CA     string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User map[string]any `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", nil, nil, fmt.Errorf("kubeconfig from the cloud API is not valid YAML: %w", err)
	}
	if len(doc.Clusters) == 0 || doc.Clusters[0].Cluster.Server == "" {
		return "", nil, nil, errors.New("kubeconfig from the cloud API names no cluster")
	}
	c := doc.Clusters[0].Cluster
	if c.CA != "" {
		if ca, err = base64.StdEncoding.DecodeString(c.CA); err != nil {
			return "", nil, nil, fmt.Errorf("kubeconfig from the cloud API: certificate authority: %w", err)
		}
	}
	if len(doc.Users) > 0 {
		user = doc.Users[0].User
	}
	return c.Server, ca, user, nil
}

// Path is the kubeconfig file to write: the flag, or the first file of
// $KUBECONFIG, or ~/.kube/config.
func Path(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if list := filepath.SplitList(os.Getenv("KUBECONFIG")); len(list) > 0 && list[0] != "" {
		return list[0], nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no --kubeconfig, no $KUBECONFIG and no home directory: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// lockTimeout is how long Update waits for another writer.
var lockTimeout = 30 * time.Second

// Update merges the entry into the kubeconfig file at path, creating it when
// it does not exist. Concurrent writers are kept apart with the same lock
// file kubectl uses (<path>.lock), and the file is replaced atomically, so a
// reader never sees half a kubeconfig.
func Update(path string, e Entry, setCurrent bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := Merge(existing, e, setCurrent)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".shackle-kubeconfig-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(merged); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func lock(path string) (func(), error) {
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another process for more than %s; remove it if that process is gone", path, lockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
