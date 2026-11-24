# shackle

A tiny single binary for cloud access in CI/CD: kubeconfig and cluster tokens,
registry credentials and object storage copy for AWS, Google Cloud, Azure and
Oracle Cloud. No cloud CLIs needed.

A pipeline that only has to reach a Kubernetes cluster, log in to a registry or
fetch a few files from a bucket usually installs a whole cloud CLI to do it:
hundreds of megabytes, and a Python start-up of a few hundred milliseconds on
every call. `shackle` does those three jobs with the job's own cloud identity,
from one static binary of about 35 MB. [Compared with the cloud
CLIs](#compared-with-the-cloud-clis) has the measurements.

| | AWS | Google Cloud | Azure | Oracle Cloud |
| --- | --- | --- | --- | --- |
| `shackle kubeconfig` | EKS | GKE | AKS | OKE |
| `shackle token` | EKS | GKE | AKS | OKE |
| `shackle registry-token`, `shackle registries` | ECR | Artifact Registry | ACR | not available |
| `shackle cp`, `shackle sync` | `s3://` | `gs://` | `az://` | `oci://` |

## Install

### Binary

Binaries for Linux (`amd64`, `arm64`) are attached to every
[release](https://github.com/entigolabs/shackle/releases).

```bash
VERSION=v0.1.4   # the release you want
ARCH=amd64       # or arm64
curl -fsSLO "https://github.com/entigolabs/shackle/releases/download/${VERSION}/shackle-linux-${ARCH}"
curl -fsSLO "https://github.com/entigolabs/shackle/releases/download/${VERSION}/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
install -m 0755 "shackle-linux-${ARCH}" /usr/local/bin/shackle
```

### Container image

```bash
docker run --rm ghcr.io/entigolabs/shackle:v0.1.4 version
```

The image is distroless: it holds the binary and CA certificates, no shell. To
use `shackle` together with `kubectl` or `helm`, copy the binary into your own
image:

```dockerfile
COPY --from=ghcr.io/entigolabs/shackle:v0.1.4 /usr/local/bin/shackle /usr/local/bin/shackle
```

### Verify a release

Every release is signed with [cosign](https://github.com/sigstore/cosign)
keyless signing; the certificate names the release workflow of this repository.

```bash
# The checksum file, which covers the binaries, the SBOM and the image digests (IMAGES)
curl -fsSLO "https://github.com/entigolabs/shackle/releases/download/${VERSION}/SHA256SUMS.sigstore.json"
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/entigolabs/shackle/\.github/workflows/release\.yaml@refs/heads/main$' \
  SHA256SUMS

# The image
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/entigolabs/shackle/\.github/workflows/release\.yaml@refs/heads/main$' \
  "ghcr.io/entigolabs/shackle:${VERSION}"
```

A release carries:

| File | Content |
| --- | --- |
| `shackle-linux-amd64`, `shackle-linux-arm64` | the binaries |
| `SHA256SUMS` | sha256 of the binaries, the SBOM and `IMAGES` |
| `SHA256SUMS.sigstore.json` | the cosign signature of `SHA256SUMS` |
| `IMAGES` | sha256 digests of the multi-arch image and of each platform's image |
| `shackle.cdx.json` | CycloneDX SBOM |

## Identity

`shackle` has no login command and stores no credentials. It uses whatever
identity the job already has, found the way each cloud's SDK finds it:

| Cloud | Identity |
| --- | --- |
| AWS | `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, a profile, web identity (OIDC), or the role of the container or instance |
| Google Cloud | An access token in `GOOGLE_OAUTH_ACCESS_TOKEN` (or `CLOUDSDK_AUTH_ACCESS_TOKEN`), used as it is and never cached; otherwise Application Default Credentials: the key file named by `GOOGLE_APPLICATION_CREDENTIALS`, workload identity federation, or the metadata server |
| Azure | a service principal in `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` and `AZURE_CLIENT_SECRET`, workload identity, or a managed identity |
| Oracle Cloud | the API key of the OCI configuration file (`OCI_CLI_CONFIG_FILE`, `OCI_CLI_PROFILE`), a resource principal, an instance principal with `OCI_CLI_AUTH=instance_principal`, or a session token with `OCI_CLI_AUTH=security_token` |

## Kubernetes access

### `shackle kubeconfig`

Writes a kubeconfig entry for a cluster: its endpoint, its certificate
authority, and a user that gets tokens from `shackle token`.

```bash
shackle kubeconfig aws    --cluster my-eks --region eu-north-1
shackle kubeconfig google --cluster my-gke --region europe-north1 --project my-project
shackle kubeconfig azure  --cluster my-aks --resource-group my-group --subscription 00000000-0000-0000-0000-000000000000
shackle kubeconfig oracle --cluster my-oke --region eu-frankfurt-1 --compartment ocid1.compartment.oc1..aaaa
kubectl get nodes
```

The entry is merged into the kubeconfig (`--kubeconfig`, else the first file of
`$KUBECONFIG`, else `~/.kube/config`); everything else in the file is kept.
Concurrent runs do not corrupt the file. The context gets the name the cloud's
own CLI would give it, so it is a drop-in replacement:

| Cloud | Context name | Replaces |
| --- | --- | --- |
| AWS | the cluster ARN | `aws eks update-kubeconfig` |
| Google Cloud | `gke_<project>_<location>_<cluster>` | `gcloud container clusters get-credentials` |
| Azure | the cluster name | `az aks get-credentials` + `kubelogin convert-kubeconfig` |
| Oracle Cloud | the cluster name | `oci ce cluster create-kubeconfig` |

| Flag | Meaning |
| --- | --- |
| `--cluster` | cluster name; for Oracle a name (with `--compartment`) or the cluster OCID |
| `--region` | region; for Google a region or a zone. Default: `AWS_REGION`, `GOOGLE_REGION`, `OCI_REGION` |
| `--project` | Google project. Default: `GOOGLE_PROJECT` |
| `--subscription`, `--resource-group` | Azure. Default: `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP` |
| `--compartment` | Oracle compartment of a cluster given by name. Default: `OCI_COMPARTMENT_ID` |
| `--private-endpoint` | use the cluster's private endpoint (Google, Oracle) |
| `--kubeconfig` | the file to write |
| `--context` | another name for the context |
| `--keep-current-context` | do not switch the current context to the new one |
| `--stdout` | print a kubeconfig with only this cluster instead of writing a file |
| `--exec-command` | the command the kubeconfig runs for tokens (default `shackle`, found on `PATH`) |
| `--static-token` | write a token into the kubeconfig instead of the plugin, see below |

The kubeconfig works wherever `shackle` is on the `PATH`. Where it is not, use
`--static-token`: the kubeconfig then needs nothing but itself, and stops
working when the token expires (EKS: 14 minutes, OKE: 4 minutes, GKE and AKS:
about an hour).

An AKS cluster with local accounts hands out a client certificate instead of
tokens; that goes into the kubeconfig as it is.

### `shackle token`

Prints a token for a cluster as a
[client-go credential plugin](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins)
answer. The kubeconfig that `shackle kubeconfig` writes calls it; there is
rarely a reason to run it by hand.

```bash
shackle token aws --cluster my-eks --region eu-north-1          # ExecCredential JSON
shackle token aws --cluster my-eks --region eu-north-1 --raw    # only the token
```

Tokens are cached until a minute before they expire, in
`$SHACKLE_CACHE_DIR` or `~/.cache/shackle`, in files only the user can read,
so a script that calls `kubectl` many times asks the cloud once. `--no-cache`
skips the cache.

## Registry credentials

### `shackle registry-token`

Prints a password for the cloud's container registry.

```bash
shackle registry-token aws --region eu-north-1 \
  | helm registry login 123456789012.dkr.ecr.eu-north-1.amazonaws.com --username AWS --password-stdin

shackle registry-token google --region europe-north1 \
  | docker login europe-north1-docker.pkg.dev --username oauth2accesstoken --password-stdin

shackle registry-token azure --registry myregistry.azurecr.io \
  | docker login myregistry.azurecr.io --username 00000000-0000-0000-0000-000000000000 --password-stdin
```

On Azure `--registry` may also be a bare registry name, which is looked up in
the subscription; without it, `--resource-group` selects the one registry in
that group. `--output json` then tells which host that is.

`--output json` prints the registry, the user name, the password and, where the
cloud tells it, the expiry:

```json
{
  "registry": "123456789012.dkr.ecr.eu-north-1.amazonaws.com",
  "username": "AWS",
  "password": "...",
  "expiresAt": "2030-01-01T12:00:00Z"
}
```

Oracle's registry takes an auth token that a user creates by hand; there is
nothing to mint from the job's identity.

### `shackle registries`

Prints the host of the registry, one a line, without logging in:

```bash
shackle registries aws --region eu-north-1                  # 123456789012.dkr.ecr.eu-north-1.amazonaws.com
shackle registries google --region europe-north1            # europe-north1-docker.pkg.dev
shackle registries azure --resource-group my-group          # every registry of the group; none prints nothing
shackle registries azure --registry myregistry              # myregistry.azurecr.io, looked up by name
```

On AWS this asks only who the identity is, so it needs no permission.

## Object storage

### `shackle cp`

Copies between the local disk and object storage, in either direction.

```bash
shackle cp s3://my-bucket/steps/net ./steps/net --recursive --exclude '*.terraform/*'
shackle cp plan.json gs://my-bucket/net/plan.json
shackle cp ./out az://myaccount/mycontainer/out --recursive
shackle cp oci://my-bucket@my-namespace/config.yaml .
```

| Location | Store |
| --- | --- |
| `s3://bucket/key` | Amazon S3, and S3-compatible stores |
| `gs://bucket/key` | Google Cloud Storage |
| `az://account/container/key` | Azure Blob Storage |
| `oci://bucket@namespace/key` | Oracle Object Storage |

| Flag | Meaning |
| --- | --- |
| `--recursive`, `-r` | copy everything under a directory or key prefix |
| `--exclude PATTERN` | leave out relative paths matching the pattern; `*` matches any characters including `/`, `?` matches one. May be repeated |
| `--s3-path-style` | address the bucket in the path, for S3-compatible stores; their endpoint comes from `AWS_ENDPOINT_URL_S3` |
| `--quiet` | do not print the files copied |

A prefix is a directory: `s3://bucket/steps/a` does not match
`steps/ab/file`. A key that would land outside the destination directory
(`../`) is refused. Symbolic links are followed on upload. A recursive
download gives each file the modification time of its object. A recursive
copy, like `sync`, transfers sixteen files at a time and stops at the first
failure. There is no copy from one bucket to another.

### `shackle sync`

Makes an object storage prefix hold what a local directory holds.

```bash
shackle sync .terraform s3://my-bucket/steps/net/.terraform --delete
```

A file is uploaded when the prefix lacks it, when the sizes differ, or when the
file is newer than the object; with the modification times `cp --recursive`
sets, a directory that was downloaded and not touched uploads nothing.
`--delete` removes the objects the directory does not have. `--exclude`
patterns are neither uploaded nor deleted. Sync goes one way, from the
directory to the bucket; `cp --recursive` is the other direction.

## Permissions

What the identity needs for each command:

| Cloud | `kubeconfig` | `token` | `registry-token` | `cp`, `sync` |
| --- | --- | --- | --- | --- |
| AWS | `eks:DescribeCluster` | none in IAM; the identity must be mapped in the cluster (access entry or `aws-auth`) | `ecr:GetAuthorizationToken` | `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`; `s3:DeleteObject` for `sync --delete` |
| Google Cloud | `container.clusters.get` | access to the cluster through IAM or RBAC | Artifact Registry reader or writer | `storage.objects.list`, `get`, `create` |
| Azure | `Microsoft.ContainerService/managedClusters/listClusterUserCredential/action` | access to the cluster through Azure RBAC or Kubernetes RBAC | `AcrPull` or `AcrPush` | Storage Blob Data Reader or Contributor |
| Oracle Cloud | `use clusters` in the compartment | access to the cluster | – | `read objects` or `manage objects` on the bucket |

## In a pipeline

GitHub Actions, with an identity from OIDC:

```yaml
permissions:
  id-token: write
  contents: read
steps:
  - uses: aws-actions/configure-aws-credentials@v6
    with:
      role-to-assume: arn:aws:iam::123456789012:role/deploy
      aws-region: eu-north-1
  - name: Install shackle
    run: |
      curl -fsSL -o /usr/local/bin/shackle \
        https://github.com/entigolabs/shackle/releases/download/v0.1.4/shackle-linux-amd64
      chmod +x /usr/local/bin/shackle
  - run: |
      shackle kubeconfig aws --cluster my-eks
      kubectl get nodes
```

GitLab CI, with the binary copied into the job's image:

```yaml
deploy:
  image: registry.example.com/deploy:1   # kubectl, helm and shackle
  script:
    - shackle kubeconfig google --cluster my-gke --region europe-north1 --project my-project
    - helm upgrade --install my-app ./chart
```

## Compared with the cloud CLIs

What a pipeline downloads (compressed size, `linux/amd64`):

| | Size |
| --- | --- |
| `ghcr.io/entigolabs/shackle` | 12 MB |
| shackle binary | 36 MB (11 MB gzipped) |
| `amazon/aws-cli` 2.42.2 | 136 MB |
| `google-cloud-cli` 594.0.1 | 854 MB (`slim`, without the GKE credential plugin: 246 MB) |
| `azure-cli` 2.93.0 | 147 MB, without `kubelogin` |
| `oci-cli` 3.96.3 | 192 MB |

How long the same job takes, in milliseconds, written as shackle / the
cloud's CLI. Both ran inside the CLI's image, from a GitHub-hosted runner,
against clusters and buckets in northern and central Europe; each number is
the median of five runs (shackle v0.1.8, November 2026).

| | AWS | Google Cloud | Azure | Oracle Cloud |
| --- | --- | --- | --- | --- |
| Start (print the version) | 10 / 484 | 8 / 340 | 12 / 210 | 9 / 300 |
| Kubernetes token | 11 / 611 | 67 / 413 | 577 / 394 | 10 / 349 |
| Kubernetes token, cached | 10 / none | 8 / 4 | 12 / 584 | 8 / none |
| Write a kubeconfig | 536 / 1130 | 775 / 1333 | 934 / 1047 | 883 / 1187 |
| Registry password | 383 / 959 | 69 / 389 | 1880 / 2345 | not available |
| Upload one 10 MiB file | 1464 / 2689 | 1556 / 3748 | 2025 / 2404 | 2190 / 2606 |
| Upload 200 files of 4 KiB | 2139 / 3749 | 5245 / 6423 | 2345 / 23878 | 3834 / 4213 |
| Download 200 files of 4 KiB | 2343 / 3931 | 4953 / 5285 | 3243 / 24219 | 2770 / 4134 |
| Sync 200 files: 50 deleted, 50 changed, 50 new | 3044 / 4832 | 5019 / 6113 | 3763 / 12713 | 5046 / 8722 |

- **Where the time goes.** A cloud CLI spends 0.1 to 0.5 s starting before it
  does anything. An EKS or OKE token is a signature made locally, so with no
  start-up cost it takes milliseconds. `kubectl` asks for a token on every
  call, which is where this adds up: `aws`, `oci` and `kubelogin` (for a
  service principal) keep no token between calls, shackle caches it.
- **Where shackle is not faster.** A new Azure token is one round trip to
  Entra ID for either tool. `gke-gcloud-auth-plugin` answers from its own
  cache as fast as shackle does. Copies of many small files to Google Cloud
  Storage take about the same time with both.
- **The sync case.** A prefix holds 200 files; the directory has lost 50 of
  them, has 50 with other content and 50 new ones. Timed is `sync --delete`
  bringing the prefix in line, and what it left there is compared with the
  directory afterwards.
- **Variation.** The rows that talk to a cloud moved by up to 40 % between two
  runs an hour apart; a difference smaller than that, such as the Azure
  kubeconfig, is not a difference.
- **The CLIs compared:** `aws eks get-token`, `aws eks update-kubeconfig`,
  `aws ecr get-login-password`, `aws s3 cp` and `sync`; `gke-gcloud-auth-plugin`,
  `gcloud container clusters get-credentials`, `gcloud auth
  print-access-token`, `gcloud storage cp` and `rsync`; `kubelogin get-token`, `az aks
  get-credentials` with `kubelogin convert-kubeconfig`, `az acr login
  --expose-token`, `az storage blob upload-batch`, `download-batch` and `sync`; `oci ce
  cluster generate-token`, `oci ce cluster create-kubeconfig`, `oci os object
  put`, `bulk-upload`, `bulk-download` and `sync`.

`scripts/bench.sh` is the script behind the table. It runs against your own
cluster and bucket:

```bash
scripts/build.sh
BENCH_CLUSTER=my-cluster BENCH_REGION=eu-north-1 \
BENCH_BUCKET=s3://my-bucket/shackle-bench scripts/bench.sh aws
```

## Limits

- Azure: the public cloud only, not the sovereign clouds.
- Oracle Cloud: no registry credentials.
- `sync` uploads only; there is no sync from a bucket to a directory.
- Binaries are built for Linux. `go build` works on other platforms.

## Development

```bash
go test ./...          # unit tests; they need no cloud and no network
scripts/build.sh dev   # binaries and checksums into dist/
docker build .         # the image, from dist/
```

Every pull request is tested and built for both architectures. Every commit on
`main` is released as `v0.1.<run number>`: the binaries, their checksums and a
CycloneDX SBOM on the GitHub Release, the image in GHCR, all signed.

## Licence

[GNU Affero General Public License v3.0](LICENSE). Third-party modules are
listed with their licences in the SBOM of each release.
