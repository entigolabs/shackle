// Package cloud holds the types the cloud providers share.
package cloud

import (
	"context"
	"time"
)

// Options is what the command line tells a provider. A provider reads the
// fields its cloud uses and ignores the rest.
type Options struct {
	Cluster         string // cluster name (OKE: name or OCID)
	Region          string // region (GKE: region or zone)
	Project         string // Google project
	Subscription    string // Azure subscription ID
	ResourceGroup   string // Azure resource group
	Compartment     string // Oracle compartment OCID
	ServerID        string // AKS: application ID of the cluster's Entra server app
	Registry        string // registry host, where it cannot be derived
	PrivateEndpoint bool   // use the cluster's private endpoint
}

// Cluster is what a kubeconfig entry needs to know about a cluster.
type Cluster struct {
	// Name is the default name of the context, the cluster and the user. It
	// is the name the cloud's own CLI would give the context.
	Name string
	// Server is the API server URL.
	Server string
	// CAData is the PEM encoded certificate authority of the API server.
	CAData []byte
	// TokenArgs are the flags `shackle token <cloud>` needs to mint a token
	// for this cluster.
	TokenArgs []string
	// StaticUser is set when the cluster hands out credentials that are not
	// tokens of the cloud identity (AKS local accounts); they go into the
	// kubeconfig as they are.
	StaticUser map[string]any
}

// Token is a bearer token for a cluster's API server.
type Token struct {
	Value string
	// Expiry is zero when the token's lifetime is unknown.
	Expiry time.Time
}

// RegistryCredential is a login for a container registry.
type RegistryCredential struct {
	Registry string    `json:"registry"`
	Username string    `json:"username"`
	Password string    `json:"password"`
	Expiry   time.Time `json:"expiresAt,omitzero"`
}

// Provider is one cloud.
type Provider interface {
	// Cluster describes the cluster.
	Cluster(ctx context.Context, o Options) (*Cluster, error)
	// Token mints a token for the cluster's API server.
	Token(ctx context.Context, o Options) (*Token, error)
	// RegistryCredential returns a login for the cloud's container registry.
	RegistryCredential(ctx context.Context, o Options) (*RegistryCredential, error)
}
