#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(git -C "$script_dir" rev-parse --show-toplevel)
audit_dir=$(mktemp -d /tmp/zwfm-dante-inferno.XXXXXX)
revision=9767558e0905c9e9c960f5a60e050fbdc38300b7
inferno_image=zwfm-dante-audit-inferno-release:9767558
runtime_image=zwfm-dante-audit-runtime:9767558
sender_id=
network_id=
cleanup() {
    if [[ -n "$sender_id" ]]; then
        docker logs "$sender_id" > "$audit_dir/sender-console.log" 2>&1 || true
        docker rm -f "$sender_id" >/dev/null || true
    fi
    if [[ -n "$network_id" ]]; then
        docker network rm "$network_id" >/dev/null || true
    fi
    echo "Dante test artifacts: $audit_dir"
}
trap cleanup EXIT

git clone --no-checkout https://github.com/teodly/inferno.git "$audit_dir/inferno"
git -C "$audit_dir/inferno" checkout --detach "$revision"
git -C "$audit_dir/inferno" submodule update --init --recursive
docker build -f "$audit_dir/inferno/Dockerfile.alpine-alsa" \
    --build-arg 'BUILD_FLAGS=--release --locked' --build-arg BUILD_TYPE=release \
    -t "$inferno_image" "$audit_dir/inferno"
cp "$audit_dir/inferno/test/dockerized_trx/fake_usrvclock_server/fake_usrvclock_server.c" "$audit_dir/clock.c"
docker build -f "$script_dir/Dockerfile" --build-arg "INFERNO_IMAGE=$inferno_image" \
    -t "$runtime_image" "$audit_dir"
cp "$script_dir/sender.py" "$script_dir/relay.py" "$audit_dir/"
target_arch=$(docker version --format '{{.Server.Arch}}')
cd "$repo_dir"
go test -race ./internal/dante
GOOS=linux GOARCH="$target_arch" CGO_ENABLED=0 go test -c \
    -o "$audit_dir/dante-linux.test" ./internal/dante
network_id=$(docker network create "zwfm-dante-inferno-$(basename "$audit_dir")")
sender_id=$(docker run -d --init --network "$network_id" \
    --mount "type=bind,src=$audit_dir,dst=/test" \
    "$runtime_image" python3 -u /test/sender.py)
ready=false
for _ in {1..30}; do
    if grep -q '^READY:' <(docker logs "$sender_id"); then
        ready=true
        break
    fi
    sleep 1
done
if [[ "$ready" != true ]]; then
    docker logs "$sender_id"
    echo 'Inferno sender did not become ready' >&2
    exit 1
fi
docker run --rm --network "$network_id" \
    --mount "type=bind,src=$audit_dir,dst=/test,readonly" \
    -e 'DANTE_INFERNO_INPUT=dante://inferno-audit/TX%201/TX%202?interface=eth0' \
    "$runtime_image" /test/dante-linux.test \
    -test.run '^TestInfernoInteroperability$' -test.v -test.timeout=90s 2>&1 | tee "$audit_dir/results.log"
