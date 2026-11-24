#!/usr/bin/env bash
# Times shackle against a cloud's own CLI, both inside that CLI's image so
# that they share the machine, the network path and the credentials:
#
#   scripts/bench.sh aws|google|azure|oracle
#
# Needs docker, a shackle binary (SHACKLE, default dist/shackle-linux-amd64),
# credentials for the cloud in the environment, and:
#
#   BENCH_CLUSTER   cluster name
#   BENCH_REGION    region (google: region or zone)
#   BENCH_BUCKET    where the copies go, a URL shackle understands, with a
#                   prefix this script may write under and removes afterwards
#                   (s3://bucket/prefix, gs://bucket/prefix,
#                   az://account/container/prefix, oci://bucket@namespace/prefix)
#   BENCH_REGISTRY  azure: registry name
#   BENCH_RUNS      runs per case (default 5); the median is reported
#   BENCH_IMAGE     the CLI image, to pin a version
#   BENCH_DOCKER_ARGS  more arguments for docker run (a user, say)
#
# Per cloud also what its CLI needs: GOOGLE_PROJECT; AZURE_RESOURCE_GROUP;
# OCI_COMPARTMENT_ID and BENCH_OCI_DIR (the directory with the OCI config).
# Every case is skipped when its setting is missing. The table is markdown.
set -euo pipefail

cloud=${1:?usage: scripts/bench.sh aws|google|azure|oracle}

# ---------------------------------------------------------------- in the image
if [ "${2:-}" = --inner ]; then
  runs=${BENCH_RUNS:-5}
  work=$(mktemp -d)
  export SHACKLE_CACHE_DIR=$work/cache

  # median_ms COMMAND...: the median wall time of the command, in ms; "failed"
  # if any run fails, with the output of that run on stderr.
  median_ms() {
    local i s e out times=()
    for i in $(seq "$runs"); do
      s=$(date +%s%N)
      if ! out=$("$@" 2>&1 >/dev/null); then echo "failed: $* : $out" >&2; echo failed; return 0; fi
      e=$(date +%s%N)
      times+=($(((e - s) / 1000000)))
    done
    printf '%s\n' "${times[@]}" | sort -n | sed -n "$(((runs + 1) / 2))p"
  }

  # row LABEL SHACKLE_COMMAND CLI_COMMAND: one table line. Commands are shell
  # snippets; "-" means the tool has nothing to compare.
  row() {
    local label=$1 a=- b=-
    [ "$2" = - ] || a=$(median_ms bash -c "$2")
    [ "$3" = - ] || b=$(median_ms bash -c "$3")
    printf '| %s | %s | %s |\n' "$label" "$a" "$b"
  }

  mkdir -p "$work/small" "$work/down-s" "$work/down-c"
  head -c 10485760 /dev/urandom > "$work/big"
  for i in $(seq 200); do head -c 4096 /dev/urandom > "$work/small/file-$i"; done

  c=${BENCH_CLUSTER:-} r=${BENCH_REGION:-} b=${BENCH_BUCKET:-}
  echo "| ${cloud}, median of ${runs} runs, ms | shackle | cloud CLI |"
  echo "| --- | --- | --- |"
  case $cloud in
    aws)
      row "Start (print the version)" "shackle version" "aws --version"
      if [ -n "$c" ]; then
        row "Kubernetes token" "shackle token aws --cluster $c --region $r --no-cache" "aws eks get-token --cluster-name $c --region $r"
        row "Kubernetes token, cached" "shackle token aws --cluster $c --region $r" -
        row "Write a kubeconfig" "shackle kubeconfig aws --cluster $c --region $r --kubeconfig $work/kc-s" "aws eks update-kubeconfig --name $c --region $r --kubeconfig $work/kc-c"
      fi
      row "Registry password" "shackle registry-token aws --region $r" "aws ecr get-login-password --region $r"
      if [ -n "$b" ]; then
        row "Upload one 10 MiB file" "shackle cp $work/big $b/s/big --quiet" "aws s3 cp $work/big $b/c/big --only-show-errors"
        row "Upload 200 files of 4 KiB" "shackle cp -r $work/small $b/s/small --quiet" "aws s3 cp --recursive $work/small $b/c/small --only-show-errors"
        row "Download 200 files of 4 KiB" "shackle cp -r $b/s/small $work/down-s --quiet" "aws s3 cp --recursive $b/c/small $work/down-c --only-show-errors"
        aws s3 rm --recursive "$b" --only-show-errors
      fi ;;
    google)
      p=${GOOGLE_PROJECT:-}
      row "Start (print the version)" "shackle version" "gcloud --version"
      if [ -n "$c" ]; then
        # The plugin keeps its token in a file next to the kubeconfig, if
        # that directory exists; without the file it asks gcloud.
        mkdir -p "$HOME/.kube"
        row "Kubernetes token" "shackle token google --no-cache" "rm -f \$HOME/.kube/gke_gcloud_auth_plugin_cache; gke-gcloud-auth-plugin"
        row "Kubernetes token, cached" "shackle token google" "gke-gcloud-auth-plugin"
        row "Write a kubeconfig" "shackle kubeconfig google --cluster $c --region $r --project $p --kubeconfig $work/kc-s" "KUBECONFIG=$work/kc-c gcloud container clusters get-credentials $c --location $r --project $p"
      fi
      row "Registry password" "shackle registry-token google --region ${r%-[a-z]}" "gcloud auth print-access-token"
      if [ -n "$b" ]; then
        row "Upload one 10 MiB file" "shackle cp $work/big $b/s/big --quiet" "gcloud storage cp $work/big $b/c/big --quiet"
        row "Upload 200 files of 4 KiB" "shackle cp -r $work/small $b/s/small --quiet" "gcloud storage cp --recursive $work/small $b/c/ --quiet"
        row "Download 200 files of 4 KiB" "shackle cp -r $b/s/small $work/down-s --quiet" "gcloud storage cp --recursive $b/c/small $work/down-c --quiet"
        gcloud storage rm --recursive "$b" --quiet >/dev/null 2>&1
      fi ;;
    azure)
      g=${AZURE_RESOURCE_GROUP:-} sub=${AZURE_SUBSCRIPTION_ID:-}
      az login --service-principal -u "$AZURE_CLIENT_ID" -p "$AZURE_CLIENT_SECRET" --tenant "$AZURE_TENANT_ID" --output none
      az account set --subscription "$sub"
      account=${b#az://}; account=${account%%/*}; rest=${b#az://*/}; container=${rest%%/*}; prefix=${rest#*/}
      row "Start (print the version)" "shackle version" "az version"
      if [ -n "$c" ]; then
        row "Write a kubeconfig" "shackle kubeconfig azure --cluster $c --resource-group $g --subscription $sub --kubeconfig $work/kc-s" "az aks get-credentials --name $c --resource-group $g --file $work/kc-c --overwrite-existing && kubelogin convert-kubeconfig -l spn --kubeconfig $work/kc-c"
        # kubelogin reads its secret from AAD_SERVICE_PRINCIPAL_CLIENT_SECRET
        # and keeps tokens in a directory; without it, it asks Entra.
        export AAD_SERVICE_PRINCIPAL_CLIENT_ID=$AZURE_CLIENT_ID AAD_SERVICE_PRINCIPAL_CLIENT_SECRET=$AZURE_CLIENT_SECRET
        kl="kubelogin get-token -l spn --server-id 6dae42f8-4368-4678-94ff-3960e28e3630 --tenant-id $AZURE_TENANT_ID --token-cache-dir $work/kubelogin"
        row "Kubernetes token" "shackle token azure --no-cache" "rm -rf $work/kubelogin; $kl"
        row "Kubernetes token, cached" "shackle token azure" "$kl"
      fi
      if [ -n "${BENCH_REGISTRY:-}" ]; then
        row "Registry password" "shackle registry-token azure --registry $BENCH_REGISTRY" "az acr login --name $BENCH_REGISTRY --expose-token --output none"
      fi
      if [ -n "$b" ]; then
        row "Upload one 10 MiB file" "shackle cp $work/big $b/s/big --quiet" "az storage blob upload --auth-mode login --account-name $account -c $container -f $work/big -n $prefix/c/big --overwrite --only-show-errors --output none"
        row "Upload 200 files of 4 KiB" "shackle cp -r $work/small $b/s/small --quiet" "az storage blob upload-batch --auth-mode login --account-name $account -d $container --destination-path $prefix/c/small -s $work/small --overwrite --only-show-errors --output none"
        row "Download 200 files of 4 KiB" "shackle cp -r $b/s/small $work/down-s --quiet" "az storage blob download-batch --auth-mode login --account-name $account -s $container --pattern '$prefix/c/small/*' -d $work/down-c --overwrite --only-show-errors --output none"
        az storage blob delete-batch --auth-mode login --account-name "$account" -s "$container" --pattern "$prefix/*" --only-show-errors --output none
      fi ;;
    oracle)
      comp=${OCI_COMPARTMENT_ID:-}
      bucket=${b#oci://}; bucket=${bucket%%@*}; rest=${b#oci://*@}; ns=${rest%%/*}; prefix=${rest#*/}
      row "Start (print the version)" "shackle version" "oci --version"
      if [ -n "$c" ]; then
        id=$(oci ce cluster list --compartment-id "$comp" --name "$c" --lifecycle-state ACTIVE --query 'data[0].id' --raw-output)
        row "Kubernetes token" "shackle token oracle --cluster $id --region $r --no-cache" "oci ce cluster generate-token --cluster-id $id --region $r"
        row "Kubernetes token, cached" "shackle token oracle --cluster $id --region $r" -
        row "Write a kubeconfig" "shackle kubeconfig oracle --cluster $id --region $r --kubeconfig $work/kc-s" "oci ce cluster create-kubeconfig --cluster-id $id --region $r --file $work/kc-c --token-version 2.0.0 --kube-endpoint PUBLIC_ENDPOINT --overwrite"
      fi
      if [ -n "$b" ]; then
        row "Upload one 10 MiB file" "shackle cp $work/big $b/s/big --quiet" "oci os object put -ns $ns -bn $bucket --file $work/big --name $prefix/c/big --force"
        row "Upload 200 files of 4 KiB" "shackle cp -r $work/small $b/s/small --quiet" "oci os object bulk-upload -ns $ns -bn $bucket --src-dir $work/small --object-prefix $prefix/c/small/ --overwrite"
        row "Download 200 files of 4 KiB" "shackle cp -r $b/s/small $work/down-s --quiet" "oci os object bulk-download -ns $ns -bn $bucket --prefix $prefix/c/small/ --download-dir $work/down-c --overwrite"
        oci os object bulk-delete -ns "$ns" -bn "$bucket" --prefix "$prefix/" --force >/dev/null 2>&1
      fi ;;
  esac
  exit 0
fi

# -------------------------------------------------------------------- on the host
root=$(cd "$(dirname "$0")/.." && pwd)
shackle=${SHACKLE:-$root/dist/shackle-linux-amd64}
[ -x "$shackle" ] || { echo "no shackle binary at $shackle: run scripts/build.sh, or set SHACKLE" >&2; exit 1; }

args=(--rm -v "$shackle:/usr/local/bin/shackle:ro" -v "$root/scripts/bench.sh:/bench.sh:ro" --entrypoint bash)
for v in BENCH_CLUSTER BENCH_REGION BENCH_BUCKET BENCH_REGISTRY BENCH_RUNS; do
  [ -z "${!v:-}" ] || args+=(-e "$v")
done
pass() { local v; for v in "$@"; do [ -z "${!v:-}" ] || args+=(-e "$v"); done; }
case $cloud in
  aws)
    image=${BENCH_IMAGE:-amazon/aws-cli:latest}
    pass AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_REGION ;;
  google)
    image=${BENCH_IMAGE:-gcr.io/google.com/cloudsdktool/google-cloud-cli:latest}
    pass GOOGLE_PROJECT GOOGLE_OAUTH_ACCESS_TOKEN CLOUDSDK_AUTH_ACCESS_TOKEN
    if [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
      args+=(-v "$GOOGLE_APPLICATION_CREDENTIALS:/creds/google.json:ro" -e GOOGLE_APPLICATION_CREDENTIALS=/creds/google.json -e CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=/creds/google.json)
    fi ;;
  azure)
    # The Azure CLI image has no kubelogin; BENCH_IMAGE must add it.
    image=${BENCH_IMAGE:-mcr.microsoft.com/azure-cli:latest}
    pass AZURE_TENANT_ID AZURE_CLIENT_ID AZURE_CLIENT_SECRET AZURE_SUBSCRIPTION_ID AZURE_RESOURCE_GROUP ;;
  oracle)
    image=${BENCH_IMAGE:-ghcr.io/oracle/oci-cli:latest}
    pass OCI_COMPARTMENT_ID OCI_CLI_AUTH OCI_CLI_PROFILE
    # At the path it has here, so that the key_file of the config resolves.
    oci_dir=${BENCH_OCI_DIR:-$HOME/.oci}
    args+=(-v "$oci_dir:$oci_dir:ro" -e "OCI_CLI_CONFIG_FILE=$oci_dir/config") ;;
  *) echo "unknown cloud $cloud" >&2; exit 1 ;;
esac
# shellcheck disable=SC2086
exec docker run "${args[@]}" ${BENCH_DOCKER_ARGS:-} "$image" /bench.sh "$cloud" --inner
