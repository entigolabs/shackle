// Package cli is the command line of shackle.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/entigolabs/shackle/internal/aws"
	"github.com/entigolabs/shackle/internal/azure"
	"github.com/entigolabs/shackle/internal/cloud"
	"github.com/entigolabs/shackle/internal/execcred"
	"github.com/entigolabs/shackle/internal/google"
	"github.com/entigolabs/shackle/internal/kubeconfig"
	"github.com/entigolabs/shackle/internal/oracle"
	"github.com/entigolabs/shackle/internal/storage"
)

const usage = `shackle gives a CI/CD job access to a cloud's Kubernetes cluster, container
registry and object storage with the job's own cloud identity, without the
cloud's CLI.

Usage:
  shackle kubeconfig <cloud> --cluster NAME [flags]   write a kubeconfig entry
  shackle token <cloud> [flags]                       print a cluster token (kubectl credential plugin)
  shackle registry-token <cloud> [flags]              print a container registry password
  shackle registries <cloud> [flags]                  print the container registry hosts
  shackle cp [--recursive] SOURCE DESTINATION         copy to or from object storage
  shackle sync [--delete] DIRECTORY DESTINATION       make an object storage prefix match a directory
  shackle version

Clouds: aws, google, azure, oracle.
Object storage locations: s3://bucket/key, gs://bucket/key,
az://account/container/key, oci://bucket@namespace/key.

Run "shackle <command> --help" for the flags of a command.
`

// env is what a command needs from the outside; tests replace it.
type env struct {
	providers map[string]cloud.Provider
	open      func(ctx context.Context, loc storage.Location, pathStyle bool) (storage.Store, error)
	cache     *execcred.Cache
	getenv    func(string) string
	stdout    io.Writer
	stderr    io.Writer
	version   string
}

// Main runs shackle and returns its exit code.
func Main(args []string, version string, stdout, stderr io.Writer) int {
	e := &env{
		providers: map[string]cloud.Provider{
			"aws":    aws.Provider{},
			"google": google.Provider{},
			"azure":  azure.Provider{},
			"oracle": oracle.Provider{},
		},
		open:    openStore,
		cache:   execcred.DefaultCache(),
		getenv:  os.Getenv,
		stdout:  stdout,
		stderr:  stderr,
		version: version,
	}
	return e.run(context.Background(), args)
}

func openStore(ctx context.Context, loc storage.Location, pathStyle bool) (storage.Store, error) {
	switch loc.Scheme {
	case "s3":
		return aws.NewStore(ctx, loc.Bucket, pathStyle)
	case "gs":
		return google.NewStore(ctx, loc.Bucket)
	case "az":
		return azure.NewStore(ctx, loc.Account, loc.Bucket)
	case "oci":
		return oracle.NewStore(ctx, loc.Account, loc.Bucket)
	}
	return nil, fmt.Errorf("unknown scheme %q", loc.Scheme)
}

// errHelp is returned when the user asked for a command's help: the help has
// been printed and the exit code is 0.
var errHelp = errors.New("help requested")

func (e *env) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "kubeconfig":
		err = e.kubeconfig(ctx, rest)
	case "token":
		err = e.token(ctx, rest)
	case "registry-token":
		err = e.registryToken(ctx, rest)
	case "registries":
		err = e.registries(ctx, rest)
	case "cp":
		err = e.cp(ctx, rest)
	case "sync":
		err = e.sync(ctx, rest)
	case "version", "--version":
		fmt.Fprintln(e.stdout, e.version)
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usage)
	default:
		fmt.Fprintf(e.stderr, "shackle: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil || errors.Is(err, errHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(e.stderr, "shackle: %v\n", err)
		return 2
	default:
		fmt.Fprintf(e.stderr, "shackle: %v\n", err)
		return 1
	}
}

// usageError is a mistake on the command line.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, args ...any) error {
	return usageError{fmt.Sprintf(format, args...)}
}

// newFlags is a flag set that prints to stderr and does not exit.
func (e *env) newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: shackle %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads flags wherever they stand among the positional arguments and
// returns the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// clusterFlags are the flags that say which cluster or registry is meant.
func clusterFlags(fs *flag.FlagSet, o *cloud.Options) {
	fs.StringVar(&o.Cluster, "cluster", "", "cluster name (oracle: name or OCID)")
	fs.StringVar(&o.Region, "region", "", "region (google: region or zone); default from AWS_REGION, GOOGLE_REGION, OCI_REGION")
	fs.StringVar(&o.Project, "project", "", "google: project; default from GOOGLE_PROJECT")
	fs.StringVar(&o.Subscription, "subscription", "", "azure: subscription ID; default from AZURE_SUBSCRIPTION_ID")
	fs.StringVar(&o.ResourceGroup, "resource-group", "", "azure: resource group; default from AZURE_RESOURCE_GROUP")
	fs.StringVar(&o.Compartment, "compartment", "", "oracle: compartment OCID of a cluster given by name; default from OCI_COMPARTMENT_ID")
	fs.StringVar(&o.ServerID, "server-id", "", "azure: application ID of the cluster's Entra server application, if not the AKS default")
	fs.BoolVar(&o.PrivateEndpoint, "private-endpoint", false, "use the cluster's private endpoint (google, oracle)")
}

// provider takes the cloud off the positional arguments.
func (e *env) provider(positional []string) (string, cloud.Provider, error) {
	if len(positional) != 1 {
		return "", nil, usagef("expected one cloud (%s), got %d arguments", e.clouds(), len(positional))
	}
	p, ok := e.providers[positional[0]]
	if !ok {
		return "", nil, usagef("unknown cloud %q (%s)", positional[0], e.clouds())
	}
	return positional[0], p, nil
}

func (e *env) clouds() string {
	names := make([]string, 0, len(e.providers))
	for n := range e.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// A cloud call that takes longer than this has hung.
const timeout = 2 * time.Minute

func (e *env) kubeconfig(ctx context.Context, args []string) error {
	fs := e.newFlags("kubeconfig", "kubeconfig <cloud> --cluster NAME [flags]")
	var o cloud.Options
	clusterFlags(fs, &o)
	path := fs.String("kubeconfig", "", "kubeconfig file to write; default: the first file of $KUBECONFIG, or ~/.kube/config")
	name := fs.String("context", "", "name of the context; default: the name the cloud's own CLI would give it")
	static := fs.Bool("static-token", false, "write a token into the kubeconfig instead of the credential plugin; it stops working when the token expires")
	command := fs.String("exec-command", "shackle", "the command the kubeconfig runs to get a token")
	stdout := fs.Bool("stdout", false, "print a kubeconfig with only this cluster instead of writing a file")
	keepCurrent := fs.Bool("keep-current-context", false, "do not make the new context the current one")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	cloudName, p, err := e.provider(positional)
	if err != nil {
		return err
	}
	if o.Cluster == "" {
		return usagef("no cluster: pass --cluster")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := p.Cluster(ctx, o)
	if err != nil {
		return err
	}
	entry := kubeconfig.Entry{Name: c.Name, Server: c.Server, CAData: c.CAData}
	if *name != "" {
		entry.Name = *name
	}
	switch {
	case c.StaticUser != nil:
		entry.User = c.StaticUser
	case *static:
		t, err := p.Token(ctx, tokenOptions(o, c))
		if err != nil {
			return err
		}
		entry.User = kubeconfig.TokenUser(t.Value)
	default:
		entry.User = kubeconfig.ExecUser(*command, append([]string{"token", cloudName}, c.TokenArgs...))
	}
	if *stdout {
		doc, err := kubeconfig.Merge(nil, entry, true)
		if err != nil {
			return err
		}
		_, err = e.stdout.Write(doc)
		return err
	}
	file, err := kubeconfig.Path(*path)
	if err != nil {
		return err
	}
	if err := kubeconfig.Update(file, entry, !*keepCurrent); err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "Added context %s to %s\n", entry.Name, file)
	return nil
}

// tokenOptions are the options that mint a token for a cluster that was just
// described: what the user gave, with what the cluster's TokenArgs say on
// top, the same as `shackle token` would get from the kubeconfig.
func tokenOptions(o cloud.Options, c *cloud.Cluster) cloud.Options {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var parsed cloud.Options
	clusterFlags(fs, &parsed)
	if fs.Parse(c.TokenArgs) != nil {
		return o
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cluster":
			o.Cluster = parsed.Cluster
		case "region":
			o.Region = parsed.Region
		case "server-id":
			o.ServerID = parsed.ServerID
		}
	})
	return o
}

func (e *env) token(ctx context.Context, args []string) error {
	fs := e.newFlags("token", "token <cloud> [flags]")
	var o cloud.Options
	clusterFlags(fs, &o)
	raw := fs.Bool("raw", false, "print only the token instead of an ExecCredential")
	noCache := fs.Bool("no-cache", false, "ask the cloud for a new token even if a cached one is still valid")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	cloudName, p, err := e.provider(positional)
	if err != nil {
		return err
	}
	key := execcred.Key(cloudName, []string{o.Cluster, o.Region, o.ServerID}, e.getenv)
	cache := e.cache
	if *noCache {
		cache = nil
	}
	t, ok := cache.Get(key)
	if !ok {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		fresh, err := p.Token(ctx, o)
		if err != nil {
			return err
		}
		t = *fresh
		cache.Put(key, t)
	}
	if *raw {
		fmt.Fprintln(e.stdout, t.Value)
		return nil
	}
	out, err := execcred.Encode(t, e.getenv("KUBERNETES_EXEC_INFO"))
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, string(out))
	return nil
}

// registryFlags are the flags that say which registry is meant.
func registryFlags(fs *flag.FlagSet, o *cloud.Options) {
	fs.StringVar(&o.Region, "region", "", "region of the registry (aws, google); default from AWS_REGION, GOOGLE_REGION")
	fs.StringVar(&o.Registry, "registry", "", "registry host (google: default <region>-docker.pkg.dev; azure: <name>.azurecr.io or a registry name)")
	fs.StringVar(&o.Subscription, "subscription", "", "azure: subscription ID, to look a registry up; default from AZURE_SUBSCRIPTION_ID")
	fs.StringVar(&o.ResourceGroup, "resource-group", "", "azure: without --registry, the resource group of the registry; default from AZURE_RESOURCE_GROUP")
}

// registries prints the registry hosts, one a line; none is not an error.
func (e *env) registries(ctx context.Context, args []string) error {
	fs := e.newFlags("registries", "registries <cloud> [flags]")
	var o cloud.Options
	registryFlags(fs, &o)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	_, p, err := e.provider(positional)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hosts, err := p.Registries(ctx, o)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		fmt.Fprintln(e.stdout, h)
	}
	return nil
}

func (e *env) registryToken(ctx context.Context, args []string) error {
	fs := e.newFlags("registry-token", "registry-token <cloud> [flags]")
	var o cloud.Options
	registryFlags(fs, &o)
	output := fs.String("output", "password", "what to print: password, or json with registry, username, password and expiresAt")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	_, p, err := e.provider(positional)
	if err != nil {
		return err
	}
	if *output != "password" && *output != "json" {
		return usagef("unknown --output %q (password, json)", *output)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cred, err := p.RegistryCredential(ctx, o)
	if err != nil {
		return err
	}
	if *output == "json" {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cred)
	}
	fmt.Fprintln(e.stdout, cred.Password)
	return nil
}

// multiFlag is a flag that may be given more than once.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func (e *env) cp(ctx context.Context, args []string) error {
	fs := e.newFlags("cp", "cp [flags] SOURCE DESTINATION")
	recursive := fs.Bool("recursive", false, "copy everything under a directory or key prefix")
	fs.BoolVar(recursive, "r", false, "short for --recursive")
	var exclude multiFlag
	fs.Var(&exclude, "exclude", "leave out relative paths matching this pattern (* matches any characters, including /); may be repeated")
	pathStyle := fs.Bool("s3-path-style", false, "s3: address the bucket in the path, for S3-compatible stores (endpoint from AWS_ENDPOINT_URL_S3)")
	quiet := fs.Bool("quiet", false, "do not print the files copied")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("expected SOURCE and DESTINATION, got %d arguments", len(positional))
	}
	o := storage.CopyOptions{Source: positional[0], Destination: positional[1], Recursive: *recursive, Exclude: exclude}
	if !*quiet {
		o.Log = func(format string, a ...any) { fmt.Fprintf(e.stderr, format+"\n", a...) }
	}
	return storage.Copy(ctx, o, func(ctx context.Context, loc storage.Location) (storage.Store, error) {
		return e.open(ctx, loc, *pathStyle)
	})
}

func (e *env) sync(ctx context.Context, args []string) error {
	fs := e.newFlags("sync", "sync [flags] DIRECTORY DESTINATION")
	del := fs.Bool("delete", false, "delete objects under the destination that the directory does not have")
	var exclude multiFlag
	fs.Var(&exclude, "exclude", "neither upload nor delete relative paths matching this pattern (* matches any characters, including /); may be repeated")
	pathStyle := fs.Bool("s3-path-style", false, "s3: address the bucket in the path, for S3-compatible stores (endpoint from AWS_ENDPOINT_URL_S3)")
	quiet := fs.Bool("quiet", false, "do not print the files uploaded and deleted")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("expected DIRECTORY and DESTINATION, got %d arguments", len(positional))
	}
	o := storage.SyncOptions{Source: positional[0], Destination: positional[1], Delete: *del, Exclude: exclude}
	if !*quiet {
		o.Log = func(format string, a ...any) { fmt.Fprintf(e.stderr, format+"\n", a...) }
	}
	return storage.Sync(ctx, o, func(ctx context.Context, loc storage.Location) (storage.Store, error) {
		return e.open(ctx, loc, *pathStyle)
	})
}
