#!/bin/bash
# The session in wherry.cast. Commands are typed out for the recording, then
# run for real against a local daemon:
#
#   make run &      # with a fresh data directory
#   ./bin/wherry login http://127.0.0.1:9000 "$(cat data/admin.token)"
#   ./bin/wherry keys:add me ~/.ssh/id_ed25519.pub
#   asciinema rec --cols 88 --rows 26 -i 1.5 -c demo/demo.sh demo/wherry.cast
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -r "$root/examples/hello-go/." "$work"
cd "$work"

PATH=$root/bin:$PATH
: "${GIT_SSH_COMMAND:=ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR}"
export PATH GIT_SSH_COMMAND

git init -q -b main
git add -A
git commit -q -m first
git remote add wherry ssh://wherry@localhost:2222/hello

run() {
  printf '\033[1;32m$\033[0m '
  for ((i = 0; i < ${#1}; i++)); do
    printf '%s' "${1:i:1}"
    sleep 0.03
  done
  printf '\n'
  sleep 0.3
  bash -c "$1" || true
  sleep 1
}

app=http://hello.localhost:8080/

# Deploy. The filter drops Docker's per-step layer lines and keeps the steps.
run 'wherry apps:create hello'
run "git push wherry main 2>&1 | grep -v ' --->'"
run "curl $app"

# Change it and push again; the new release replaces the old one.
run "sed -i 's/v1/v2/' main.go && git commit -qam 'say v2'"
run 'git push wherry main 2>&1 | tail -n 4'
run "curl $app"

# A release that dies on start is rejected, and v2 keeps serving.
run "echo 'func init() { panic(\"no DATABASE_URL\") }' >> main.go"
run "git commit -qam 'read the database' && git push wherry main 2>&1 | tail -n 8"
run "curl $app"

# Go back to any earlier release.
run 'wherry releases hello'
run 'wherry releases:rollback hello v1'
run "curl $app"
